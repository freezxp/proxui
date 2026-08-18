package command

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// These tests run the install path against an in-memory guest filesystem. The
// thing being proved is not that a file gets written - it is that the portal
// cannot lose a key it did not put there, which is the risk in editing
// somebody's authorized_keys from a web application.

const testPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPortalTestKey proxui-portal"

// --- fakes ---------------------------------------------------------------

type fakeKeyStore struct {
	mu       sync.Mutex
	key      shell.PortalKey
	private  string
	hasKey   bool
	installs map[string]shell.KeyInstall
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{installs: map[string]shell.KeyInstall{}}
}

func (f *fakeKeyStore) withKey() *fakeKeyStore {
	f.key = shell.PortalKey{
		PublicKey: testPublicKey, Algorithm: "ssh-ed25519", Fingerprint: "SHA256:test",
	}
	f.private = "PRIVATE"
	f.hasKey = true
	return f
}

func (f *fakeKeyStore) Get(context.Context) (shell.PortalKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasKey {
		return shell.PortalKey{}, ports.ErrNotFound
	}
	return f.key, nil
}

func (f *fakeKeyStore) PrivateKey(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasKey {
		return "", ports.ErrNotFound
	}
	return f.private, nil
}

func (f *fakeKeyStore) Replace(_ context.Context, key shell.PortalKey, private string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.private, f.hasKey = key, private, true
	return nil
}

func (f *fakeKeyStore) Delete(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasKey = false
	return nil
}

func (f *fakeKeyStore) Installs(_ context.Context, vmID uuid.UUID) ([]shell.KeyInstall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []shell.KeyInstall{}
	for _, in := range f.installs {
		if vmID != uuid.Nil && in.VMID != vmID {
			continue
		}
		in.Stale = !f.hasKey || in.Fingerprint != f.key.Fingerprint
		out = append(out, in)
	}
	return out, nil
}

func (f *fakeKeyStore) RecordInstall(_ context.Context, in shell.KeyInstall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs[in.VMID.String()+"/"+in.SSHUser] = in
	return nil
}

func (f *fakeKeyStore) ForgetInstall(_ context.Context, vmID uuid.UUID, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.installs, vmID.String()+"/"+user)
	return nil
}

type fakeKeyGen struct{ err error }

func (g fakeKeyGen) Generate(comment string) (string, shell.PortalKey, error) {
	if g.err != nil {
		return "", shell.PortalKey{}, g.err
	}
	return "PRIVATE-" + comment, shell.PortalKey{
		PublicKey: testPublicKey, Algorithm: "ssh-ed25519", Fingerprint: "SHA256:generated",
	}, nil
}

// memFS is a guest filesystem in a map. Only the operations the install path
// uses are real; the rest satisfy the port.
type memFS struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
	modes map[string]uint32
	home  string

	appendErr error
	openErr   error
}

func newMemFS() *memFS {
	return &memFS{
		files: map[string]string{}, dirs: map[string]bool{"/root": true},
		modes: map[string]uint32{}, home: "/root",
	}
}

func (m *memFS) Home(context.Context) (string, error) { return m.home, nil }

func (m *memFS) Stat(_ context.Context, p string) (shell.FileEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirs[p] {
		return shell.FileEntry{Path: p, IsDir: true}, nil
	}
	if body, ok := m.files[p]; ok {
		return shell.FileEntry{Path: p, Size: int64(len(body))}, nil
	}
	return shell.FileEntry{}, fs.ErrNotExist
}

func (m *memFS) Open(_ context.Context, p string) (io.ReadCloser, int64, error) {
	if m.openErr != nil {
		return nil, 0, m.openErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.files[p]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
}

func (m *memFS) Create(_ context.Context, p string) (io.WriteCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[p] = ""
	return &memWriter{fs: m, path: p}, nil
}

func (m *memFS) Append(_ context.Context, p string) (io.WriteCloser, error) {
	if m.appendErr != nil {
		return nil, m.appendErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[p]; !ok {
		m.files[p] = ""
	}
	return &memWriter{fs: m, path: p, appending: true}, nil
}

func (m *memFS) Mkdir(_ context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[p] = true
	return nil
}

func (m *memFS) Chmod(_ context.Context, p string, mode uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modes[p] = mode
	return nil
}

func (m *memFS) List(context.Context, string) ([]shell.FileEntry, error) { return nil, nil }
func (m *memFS) Remove(context.Context, string) error                    { return nil }
func (m *memFS) Rename(context.Context, string, string) error            { return nil }

func (m *memFS) content(p string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[p]
}

type memWriter struct {
	fs        *memFS
	path      string
	appending bool
	buf       bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *memWriter) Close() error {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	if w.appending {
		w.fs.files[w.path] += w.buf.String()
	} else {
		w.fs.files[w.path] = w.buf.String()
	}
	return nil
}

// keyConn is a live connection whose only real capability is its filesystem.
type keyConn struct{ files *memFS }

func (c *keyConn) Terminal(ports.TerminalSize) (ports.Terminal, error) {
	return nil, errors.New("no terminal in this test")
}
func (c *keyConn) Files() (ports.RemoteFS, error) { return c.files, nil }
func (c *keyConn) HostKey() shell.HostKey         { return shell.HostKey{} }
func (c *keyConn) Close() error                   { return nil }

// --- harness -------------------------------------------------------------

type keyHarness struct {
	cmd       *PortalKey
	store     *fakeKeyStore
	guest     *memFS
	audit     *fakeAudit
	actor     Actor
	sessionID uuid.UUID
	vmID      uuid.UUID
	authPath  string
}

func newKeyHarness(t *testing.T, withKey bool) *keyHarness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)}
	store := newFakeKeyStore()
	if withKey {
		store.withKey()
	}
	guest := newMemFS()
	registry := shellreg.NewRegistry(clock.Now)
	audit := &fakeAudit{}

	h := &keyHarness{
		cmd: &PortalKey{
			Keys: store, KeyGen: fakeKeyGen{}, Registry: registry,
			Audit: audit, Clock: clock,
		},
		store: store, guest: guest, audit: audit,
		actor:     Actor{UserID: uuid.New(), Username: "opal"},
		sessionID: uuid.New(), vmID: uuid.New(),
		authPath: "/root/.ssh/authorized_keys",
	}

	live := shellreg.NewLive(h.sessionID, h.actor.UserID, h.vmID, "web-01", "root",
		"10.0.0.5:22", &keyConn{files: guest}, clock.Now())
	if err := registry.Add(live); err != nil {
		t.Fatalf("add live session: %v", err)
	}
	return h
}

// --- tests ---------------------------------------------------------------

func TestInstallWritesTheKeyAndTightensTheMode(t *testing.T) {
	h := newKeyHarness(t, true)

	install, err := h.cmd.Install(context.Background(), h.actor, h.sessionID)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if install.SSHUser != "root" || install.VMID != h.vmID {
		t.Fatalf("install recorded the wrong session: %+v", install)
	}
	if !shell.HasAuthorizedKey(h.guest.content(h.authPath), testPublicKey) {
		t.Fatalf("key not installed, file is %q", h.guest.content(h.authPath))
	}
	// An authorized_keys anyone can write is one sshd silently ignores.
	if mode := h.guest.modes[h.authPath]; mode != shell.AuthorizedKeysMode {
		t.Fatalf("authorized_keys mode is %o, want %o", mode, shell.AuthorizedKeysMode)
	}
	if mode := h.guest.modes["/root/.ssh"]; mode != shell.SSHDirMode {
		t.Fatalf(".ssh mode is %o, want %o", mode, shell.SSHDirMode)
	}
	if !h.guest.dirs["/root/.ssh"] {
		t.Fatal(".ssh was not created")
	}
}

func TestInstallKeepsTheKeysAlreadyThere(t *testing.T) {
	// The failure that matters: an operator's own key disappearing because the
	// portal rewrote a file it only meant to add a line to.
	theirs := "ssh-rsa AAAAB3NzaC1yc2ETheirOwnKey alice@laptop"
	h := newKeyHarness(t, true)
	h.guest.files[h.authPath] = theirs // deliberately without a trailing newline

	if _, err := h.cmd.Install(context.Background(), h.actor, h.sessionID); err != nil {
		t.Fatalf("install: %v", err)
	}

	content := h.guest.content(h.authPath)
	if !shell.HasAuthorizedKey(content, theirs) {
		t.Fatalf("the operator's own key was lost: %q", content)
	}
	if !shell.HasAuthorizedKey(content, testPublicKey) {
		t.Fatalf("the portal key was not added: %q", content)
	}
	if strings.Contains(content, theirs+"ssh-ed25519") {
		t.Fatalf("two keys were run together on one line: %q", content)
	}
}

func TestInstallTwiceAddsOneLine(t *testing.T) {
	h := newKeyHarness(t, true)
	ctx := context.Background()

	if _, err := h.cmd.Install(ctx, h.actor, h.sessionID); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first := h.guest.content(h.authPath)
	if _, err := h.cmd.Install(ctx, h.actor, h.sessionID); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := h.guest.content(h.authPath); got != first {
		t.Fatalf("installing twice changed the file:\nfirst:  %q\nsecond: %q", first, got)
	}

	// The second attempt is still audited, and says it changed nothing.
	var installed []map[string]any
	for _, e := range h.audit.entries {
		if e.Action == "ssh_portal_key_installed" {
			installed = append(installed, e.Details)
		}
	}
	if len(installed) != 2 {
		t.Fatalf("want two audit entries, got %d", len(installed))
	}
	if installed[0]["changed"] != true || installed[1]["changed"] != false {
		t.Fatalf("audit should record what actually happened: %v", installed)
	}
}

func TestInstallWithoutAKeyIsRefused(t *testing.T) {
	h := newKeyHarness(t, false)
	_, err := h.cmd.Install(context.Background(), h.actor, h.sessionID)
	if !errors.Is(err, shell.ErrNoPortalKey) {
		t.Fatalf("want ErrNoPortalKey, got %v", err)
	}
	if h.guest.content(h.authPath) != "" {
		t.Fatal("nothing should have been written")
	}
}

func TestInstallRefusesSomebodyElsesSession(t *testing.T) {
	h := newKeyHarness(t, true)
	stranger := Actor{UserID: uuid.New(), Username: "mallory"}

	_, err := h.cmd.Install(context.Background(), stranger, h.sessionID)
	if err == nil {
		t.Fatal("a session id is not a bearer token; this must fail")
	}
	if h.guest.content(h.authPath) != "" {
		t.Fatalf("a stranger wrote to the guest: %q", h.guest.content(h.authPath))
	}
}

func TestInstallRefusesAnAbsurdAuthorizedKeys(t *testing.T) {
	h := newKeyHarness(t, true)
	h.guest.files[h.authPath] = strings.Repeat("x", shell.MaxAuthorizedKeysBytes+1)

	_, err := h.cmd.Install(context.Background(), h.actor, h.sessionID)
	if !errors.Is(err, shell.ErrBadAuthorizedKeys) {
		t.Fatalf("want ErrBadAuthorizedKeys, got %v", err)
	}
}

func TestUninstallRemovesOnlyThePortalLine(t *testing.T) {
	theirs := "ssh-rsa AAAAB3NzaC1yc2ETheirOwnKey alice@laptop"
	h := newKeyHarness(t, true)
	ctx := context.Background()
	h.guest.files[h.authPath] = theirs + "\n"

	if _, err := h.cmd.Install(ctx, h.actor, h.sessionID); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := h.cmd.Uninstall(ctx, h.actor, h.sessionID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	content := h.guest.content(h.authPath)
	if shell.HasAuthorizedKey(content, testPublicKey) {
		t.Fatalf("the portal key survived removal: %q", content)
	}
	if !shell.HasAuthorizedKey(content, theirs) {
		t.Fatalf("removal took the operator's key with it: %q", content)
	}

	installs, _ := h.store.Installs(ctx, uuid.Nil)
	if len(installs) != 0 {
		t.Fatalf("the install record should be gone, got %+v", installs)
	}
}

func TestUninstallForgetsARecordEvenWhenTheLineWasAlreadyGone(t *testing.T) {
	// An operator who deleted the line by hand should not be left with a
	// portal that permanently believes the key is installed.
	h := newKeyHarness(t, true)
	ctx := context.Background()
	if err := h.store.RecordInstall(ctx, shell.KeyInstall{
		VMID: h.vmID, SSHUser: "root", Fingerprint: "SHA256:test",
	}); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	if err := h.cmd.Uninstall(ctx, h.actor, h.sessionID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	installs, _ := h.store.Installs(ctx, uuid.Nil)
	if len(installs) != 0 {
		t.Fatalf("the stale record should be gone, got %+v", installs)
	}
}

func TestGenerateRotatesAndAuditsThePreviousFingerprint(t *testing.T) {
	h := newKeyHarness(t, true)
	ctx := context.Background()
	if err := h.store.RecordInstall(ctx, shell.KeyInstall{
		VMID: h.vmID, SSHUser: "root", Fingerprint: "SHA256:test",
	}); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	key, err := h.cmd.Generate(ctx, h.actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if key.Fingerprint != "SHA256:generated" {
		t.Fatalf("unexpected fingerprint %q", key.Fingerprint)
	}

	var entry *ports.AuditEntry
	for i := range h.audit.entries {
		if h.audit.entries[i].Action == "ssh_portal_key_rotated" {
			entry = &h.audit.entries[i]
		}
	}
	if entry == nil {
		t.Fatalf("a rotation should be audited as one, got %v", h.audit.actions())
	}
	if entry.Details["previous_fingerprint"] != "SHA256:test" {
		t.Fatalf("the replaced key should be named: %v", entry.Details)
	}
	if entry.Details["installs_invalidated"] != 1 {
		t.Fatalf("the rotation should count what it broke: %v", entry.Details)
	}

	// And the install left behind is now visibly stale rather than silently
	// relabelled as an install of the new key.
	installs, _ := h.store.Installs(ctx, uuid.Nil)
	if len(installs) != 1 || !installs[0].Stale {
		t.Fatalf("want one stale install, got %+v", installs)
	}
}

func TestGenerateOnAnEmptyPortalIsACreation(t *testing.T) {
	h := newKeyHarness(t, false)
	if _, err := h.cmd.Generate(context.Background(), h.actor); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, action := range h.audit.actions() {
		if action == "ssh_portal_key_created" {
			return
		}
	}
	t.Fatalf("want a creation entry, got %v", h.audit.actions())
}

func TestNoAuditEntryEverCarriesThePrivateKey(t *testing.T) {
	// The one invariant that must hold across every operation here.
	h := newKeyHarness(t, true)
	ctx := context.Background()
	_, _ = h.cmd.Generate(ctx, h.actor)
	_, _ = h.cmd.Install(ctx, h.actor, h.sessionID)
	_ = h.cmd.Uninstall(ctx, h.actor, h.sessionID)
	_ = h.cmd.Delete(ctx, h.actor)

	private, err := h.store.PrivateKey(ctx)
	if err == nil && private != "" {
		t.Fatal("the key should have been deleted by now")
	}
	for _, e := range h.audit.entries {
		for k, v := range e.Details {
			if text, ok := v.(string); ok && strings.Contains(text, "PRIVATE") {
				t.Fatalf("audit detail %q leaked private key material: %q", k, text)
			}
		}
	}
}
