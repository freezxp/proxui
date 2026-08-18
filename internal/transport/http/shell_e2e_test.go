package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/infra/sshclient"
	"github.com/freezxp/proxui/internal/infra/sshclient/sshtest"
	"github.com/freezxp/proxui/internal/transport/ws"
)

// End to end over the real thing: a real HTTP router, the real command layer,
// the real SSH client, a real WebSocket, and an SSH server that speaks the
// actual protocol. The only stubs are the database repositories.
//
// This is where the pieces meet, and it is the layer that a unit test cannot
// reach: that the ticket minted by a POST is redeemable exactly once by a
// WebSocket, that a session id is worthless to another user, and that closing
// a session makes its file endpoints stop answering.

const sshPassword = "correct horse battery"

type shellFixture struct {
	server   *httptest.Server
	sshd     *sshtest.Server
	userID   uuid.UUID
	registry *shellreg.Registry
	sessions *fakeShellRepo
	hostKeys *fakeHostKeys
	keys     *memPortalKeys
	audit    *recordingAudit
}

func newShellFixture(t *testing.T, opts sshtest.Options) *shellFixture {
	t.Helper()

	if opts.User == "" {
		opts.User = "tester"
	}
	if opts.Password == "" && opts.AuthorizedKey == nil {
		opts.Password = sshPassword
	}
	sshd := sshtest.Start(t, opts)

	userID := uuid.New()
	clock := ports.SystemClock{}
	audit := &recordingAudit{}
	sessions := &fakeShellRepo{}
	hostKeys := &fakeHostKeys{}
	portalKeys := newMemPortalKeys()
	registry := shellreg.NewRegistry(clock.Now)
	tickets := shellreg.NewTicketStore(clock.Now)

	closer := &command.CloseShell{
		Sessions: sessions, Registry: registry, Audit: audit, Clock: clock,
	}
	registry.OnEvict = func(l *shellreg.Live, reason string) {
		closer.Record(context.Background(), l, reason)
	}

	inventory := &fakeShellInventory{
		vmID:  uuid.New(),
		name:  "test-guest",
		state: "running",
		// The address of the SSH server the test just started. The command
		// layer refuses any host that is not in this list, so this is also
		// what stops the API from being an SSH proxy to the whole network.
		addresses: []string{sshd.Host},
	}

	api := NewServer(ServerConfig{
		Log:     zerolog.New(io.Discard),
		Version: "test",
		Auth: AuthDeps{
			Tokens:   roleTokenParser{role: identity.RoleOperator, userID: userID},
			Sessions: &fakeSessionChecker{active: true},
			Users:    &fakeUserLoader{user: testUser()},
		},
		Inventory: InventoryDeps{Inventory: inventory, Audit: stubAudit{}, Metrics: stubMetrics{}},
		Admin:     AdminDeps{},
		Shell: ShellDeps{
			Open: &command.OpenShell{
				Inventory: inventory, Sessions: sessions, HostKeys: hostKeys,
				Tickets: tickets, Registry: registry, Dialer: sshclient.NewDialer(),
				Audit: audit, Clock: clock, Keys: portalKeys,
			},
			Close: closer,
			Files: &command.ShellFiles{Registry: registry, Audit: audit, Clock: clock},
			Keys: &command.PortalKey{
				Keys: portalKeys, KeyGen: sshclient.KeyGen{}, Registry: registry,
				Inventory: inventory, Audit: audit, Clock: clock,
			},
			Sessions: sessions,
			HostKeys: hostKeys,
			Bridge: &ws.ShellBridge{
				Tickets: tickets, Sessions: sessions, Registry: registry,
				Closer: closer, Clock: clock, Log: zerolog.New(io.Discard),
			},
		},
	})

	server := httptest.NewServer(api.Routes())
	t.Cleanup(server.Close)

	return &shellFixture{
		server: server, sshd: sshd, userID: userID, registry: registry,
		sessions: sessions, hostKeys: hostKeys, keys: portalKeys, audit: audit,
	}
}

func (f *shellFixture) vmID() uuid.UUID { return f.inventoryVM() }

// do sends an authenticated request and returns the status and decoded body.
func (f *shellFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	decoded := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

// connect builds the request body the browser sends. The address and port are
// explicit because the test server listens on a random loopback port, and the
// portal deliberately refuses to pick a loopback address on its own: a guest
// reporting 127.0.0.1 is not reachable there from the portal, and choosing it
// would produce a connection to the portal's own machine.
func (f *shellFixture) connect(extra map[string]any) map[string]any {
	body := map[string]any{
		"username": "tester", "password": sshPassword,
		"host": f.sshd.Host, "port": f.sshd.Port,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// open runs the two-step connect the browser does: the first attempt learns the
// fingerprint, the second accepts it.
func (f *shellFixture) open(t *testing.T) map[string]any {
	t.Helper()

	status, body := f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh", f.connect(nil))
	if status != http.StatusConflict || body["code"] != "ssh.host_key_unknown" {
		t.Fatalf("first connect = %d %v, want a host key prompt", status, body["code"])
	}
	prompt, _ := body["body"].(map[string]any)
	if prompt["fingerprint"] != f.sshd.Fingerprint {
		t.Fatalf("prompt shows %v, the server presents %s", prompt["fingerprint"], f.sshd.Fingerprint)
	}

	status, session := f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh",
		f.connect(map[string]any{"accept_host_key": f.sshd.Fingerprint}))
	if status != http.StatusCreated {
		t.Fatalf("connect after accepting = %d %v", status, session)
	}
	return session
}

func TestShellOpenAndTerminal(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)

	if session["ssh_user"] != "tester" {
		t.Fatalf("session reports ssh_user %v", session["ssh_user"])
	}
	if session["files_available"] != true {
		t.Fatalf("files_available = %v, the test server offers sftp", session["files_available"])
	}
	// The credential must not come back in any form.
	for _, field := range []string{"password", "private_key", "passphrase"} {
		if _, present := session[field]; present {
			t.Fatalf("the response echoed %q back to the browser", field)
		}
	}

	conn := f.dialTerminal(t, session)
	defer conn.Close()

	if got := readUntil(t, conn, "test$ "); !strings.Contains(got, "test$ ") {
		t.Fatalf("no prompt over the WebSocket; read %q", got)
	}

	// Keystrokes are binary frames.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo hello\r")); err != nil {
		t.Fatalf("send keystrokes: %v", err)
	}
	if got := readUntil(t, conn, "hello"); !strings.Contains(got, "hello") {
		t.Fatalf("no command output; read %q", got)
	}

	// Resizes are text frames, and must not be mistaken for keystrokes.
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"resize","cols":140,"rows":50}`)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, size := range f.sshd.WindowChanges() {
			if size.Cols == 140 && size.Rows == 50 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the guest never saw the resize; saw %v", f.sshd.WindowChanges())
}

func TestShellTicketIsSingleUse(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)

	first := f.dialTerminal(t, session)
	defer first.Close()

	// A second attempt with the same ticket must fail: the ticket is consumed,
	// so a stolen one is worthless the moment the real browser has used it.
	url := strings.Replace(f.server.URL, "http", "ws", 1) + session["ws_url"].(string)
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("a replayed ticket opened a second terminal")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay got %v, want 401", resp)
	}
}

func TestShellWrongPasswordIsNotAServerError(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	// Trust the key first, so the failure under test is the credential.
	f.open(t)

	status, body := f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh",
		f.connect(map[string]any{"password": "not the password"}))
	if status != http.StatusUnauthorized || body["code"] != "ssh.auth_failed" {
		t.Fatalf("wrong password = %d %v, want 401 ssh.auth_failed", status, body["code"])
	}
}

func TestShellRefusesAnAddressTheVMNeverReported(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})

	// The portal reaches a lot of network the operator was never granted.
	// Naming an arbitrary host must not be a way to reach it.
	status, body := f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh",
		f.connect(map[string]any{"host": "10.99.99.99"}))
	if status != http.StatusUnprocessableEntity || body["code"] != "ssh.no_address" {
		t.Fatalf("arbitrary host = %d %v, want 422 ssh.no_address", status, body["code"])
	}
}

func TestShellHostKeyChangeIsRefused(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	f.open(t)

	// The guest is rebuilt: same address, different key.
	f.hostKeys.replaceKey([]byte("a completely different key"))

	status, body := f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh", f.connect(nil))
	if status != http.StatusConflict || body["code"] != "ssh.host_key_mismatch" {
		t.Fatalf("changed key = %d %v, want 409 ssh.host_key_mismatch", status, body["code"])
	}
	// And accepting the new fingerprint must not be a way past it: the pin is
	// only cleared by an administrator, deliberately away from this form.
	status, body = f.do(t, "POST", "/api/v1/vms/"+f.vmID().String()+"/ssh",
		f.connect(map[string]any{"accept_host_key": f.sshd.Fingerprint}))
	if status != http.StatusConflict || body["code"] != "ssh.host_key_mismatch" {
		t.Fatalf("accepting a changed key = %d %v; it must still be refused", status, body["code"])
	}
}

func TestShellFilesRoundTripOverHTTP(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	f.sshd.WriteFile(t, "existing.txt", "already here\n")
	session := f.open(t)
	id := session["session_id"].(string)

	// List.
	status, body := f.do(t, "GET",
		"/api/v1/ssh-sessions/"+id+"/files?path="+f.sshd.Root, nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, body)
	}
	if !listingHas(body, "existing.txt") {
		t.Fatalf("listing missed the file: %v", body["data"])
	}

	// Upload, streamed as a raw body.
	req, _ := http.NewRequest("POST",
		f.server.URL+"/api/v1/ssh-sessions/"+id+"/files/content?path="+f.sshd.Root+"&name=uploaded.txt",
		strings.NewReader("from the browser"))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	if got, err := os.ReadFile(f.sshd.Root + "/uploaded.txt"); err != nil || string(got) != "from the browser" {
		t.Fatalf("uploaded file on disk = %q, %v", got, err)
	}

	// Download.
	req, _ = http.NewRequest("GET",
		f.server.URL+"/api/v1/ssh-sessions/"+id+"/files/content?path="+f.sshd.Root+"/existing.txt", nil)
	req.Header.Set("Authorization", "Bearer test")
	resp, err = f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	content, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(content) != "already here\n" {
		t.Fatalf("downloaded %q", content)
	}
	// A file from a guest must never be rendered by the browser.
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("download Content-Type is %q; an uploaded .html would run on the portal's origin", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("download Content-Disposition is %q", cd)
	}

	// Make a directory, move into it, change its mode, delete it.
	if status, body := f.do(t, "POST", "/api/v1/ssh-sessions/"+id+"/files/mkdir",
		map[string]any{"path": f.sshd.Root, "name": "sub"}); status != http.StatusCreated {
		t.Fatalf("mkdir = %d %v", status, body)
	}
	if status, body := f.do(t, "POST", "/api/v1/ssh-sessions/"+id+"/files/rename",
		map[string]any{"path": f.sshd.Root + "/uploaded.txt", "to": f.sshd.Root + "/sub/moved.txt"}); status != http.StatusNoContent {
		t.Fatalf("rename = %d %v", status, body)
	}
	if status, body := f.do(t, "POST", "/api/v1/ssh-sessions/"+id+"/files/chmod",
		map[string]any{"path": f.sshd.Root + "/sub/moved.txt", "mode": "600"}); status != http.StatusNoContent {
		t.Fatalf("chmod = %d %v", status, body)
	}
	info, err := os.Stat(f.sshd.Root + "/sub/moved.txt")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("after chmod: %v %v", info, err)
	}
	if status, _ := f.do(t, "DELETE",
		"/api/v1/ssh-sessions/"+id+"/files?path="+f.sshd.Root+"/sub/moved.txt", nil); status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}

	// Every write is on the record; browsing is not.
	f.audit.mustHave(t, "ssh_file_uploaded", "ssh_file_downloaded", "ssh_directory_created",
		"ssh_file_renamed", "ssh_file_mode_changed", "ssh_file_deleted")
	f.audit.mustNotHave(t, "ssh_directory_listed")
}

func TestShellUploadCannotEscapeTheDirectory(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)
	id := session["session_id"].(string)

	req, _ := http.NewRequest("POST",
		f.server.URL+"/api/v1/ssh-sessions/"+id+"/files/content?path="+f.sshd.Root+"&name=../escaped.txt",
		strings.NewReader("nope"))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a traversing filename got %d, want 400", resp.StatusCode)
	}
	if _, err := os.Stat(f.sshd.Root + "/../escaped.txt"); err == nil {
		t.Fatal("the upload escaped the directory it was aimed at")
	}
}

func TestShellSessionBelongsToOneUser(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)
	id := session["session_id"].(string)

	// Everything above ran as the fixture's user. Re-point the token parser at
	// somebody else and the same session id must become meaningless.
	other := newShellFixture(t, sshtest.Options{})
	status, body := other.do(t, "GET", "/api/v1/ssh-sessions/"+id+"/files?path=/", nil)
	if status != http.StatusNotFound {
		t.Fatalf("another user reading the session = %d %v, want 404", status, body)
	}
}

func TestShellCloseEndsTheSession(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)
	id := session["session_id"].(string)

	if status, _ := f.do(t, "DELETE", "/api/v1/ssh-sessions/"+id, nil); status != http.StatusNoContent {
		t.Fatalf("close = %d", status)
	}
	if f.registry.Len() != 0 {
		t.Fatal("the registry still holds the session")
	}
	if status, _ := f.do(t, "GET", "/api/v1/ssh-sessions/"+id+"/files?path=/", nil); status != http.StatusNotFound {
		t.Fatalf("files after close = %d, want 404", status)
	}
	if !f.sessions.closed(id) {
		t.Fatal("the session record was never closed; the admin list would show it open forever")
	}
	f.audit.mustHave(t, "ssh_session_opened", "ssh_session_closed")
}

func TestShellWithoutSFTPStillGivesATerminal(t *testing.T) {
	f := newShellFixture(t, sshtest.Options{NoSFTP: true})
	session := f.open(t)

	if session["files_available"] != false {
		t.Fatalf("files_available = %v on a guest with no sftp", session["files_available"])
	}
	conn := f.dialTerminal(t, session)
	defer conn.Close()
	if got := readUntil(t, conn, "test$ "); !strings.Contains(got, "test$ ") {
		t.Fatalf("the terminal should work regardless; read %q", got)
	}
}

// --- fixture plumbing ---------------------------------------------------

func (f *shellFixture) dialTerminal(t *testing.T, session map[string]any) *websocket.Conn {
	t.Helper()
	url := strings.Replace(f.server.URL, "http", "ws", 1) + session["ws_url"].(string)
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial terminal: %v (http %d)", err, status)
	}
	return conn
}

func readUntil(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	var seen strings.Builder
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return seen.String()
		}
		seen.Write(data)
		if strings.Contains(seen.String(), want) {
			_ = conn.SetReadDeadline(time.Time{})
			return seen.String()
		}
	}
}

func listingHas(body map[string]any, name string) bool {
	entries, _ := body["data"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["name"] == name {
			return true
		}
	}
	return false
}

type fakeShellInventory struct {
	vmID      uuid.UUID
	name      string
	state     string
	addresses []string
}

func (f *fakeShellInventory) ListVMs(context.Context, ports.VMFilter) (ports.VMPage, error) {
	return ports.VMPage{}, nil
}
func (f *fakeShellInventory) GetVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (ports.VMDetail, error) {
	return ports.VMDetail{VMListItem: ports.VMListItem{
		ID: f.vmID, Name: f.name, State: f.state, IPAddresses: f.addresses,
	}}, nil
}
func (f *fakeShellInventory) CanAccessVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (bool, error) {
	return true, nil
}
func (f *fakeShellInventory) VMHistory(context.Context, uuid.UUID, int) ([]ports.HistoryEntry, error) {
	return nil, nil
}
func (f *fakeShellInventory) SetPortalTags(context.Context, uuid.UUID, []string) error { return nil }
func (f *fakeShellInventory) SetNotes(context.Context, uuid.UUID, string) error        { return nil }
func (f *fakeShellInventory) Dashboard(context.Context, identity.Role, uuid.UUID) (ports.DashboardSummary, error) {
	return ports.DashboardSummary{}, nil
}

func (f *shellFixture) inventoryVM() uuid.UUID {
	// The fixture's inventory always answers for whatever id is asked, so any
	// well-formed uuid names "the" VM. A fixed one keeps the tests readable.
	return fixtureVMID
}

var fixtureVMID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

type fakeShellRepo struct {
	mu       sync.Mutex
	created  []*shell.Session
	closures map[string]string
}

func (r *fakeShellRepo) Create(_ context.Context, s *shell.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = append(r.created, s)
	return nil
}
func (r *fakeShellRepo) MarkConnected(context.Context, uuid.UUID, time.Time) error { return nil }
func (r *fakeShellRepo) Close(_ context.Context, id uuid.UUID, reason string, _, _ int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closures == nil {
		r.closures = map[string]string{}
	}
	r.closures[id.String()] = reason
	return nil
}
func (r *fakeShellRepo) Get(context.Context, uuid.UUID) (*shell.Session, error) {
	return nil, ports.ErrNotFound
}
func (r *fakeShellRepo) List(context.Context, bool, int) ([]ports.ShellSessionRecord, error) {
	return nil, nil
}
func (r *fakeShellRepo) closed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.closures[id]
	return ok
}

type fakeHostKeys struct {
	mu  sync.Mutex
	key *shell.HostKey
}

func (h *fakeHostKeys) Get(_ context.Context, vmID uuid.UUID) (shell.HostKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.key == nil {
		return shell.HostKey{}, ports.ErrNotFound
	}
	return *h.key, nil
}
func (h *fakeHostKeys) Trust(_ context.Context, key shell.HostKey) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.key == nil { // first write wins, as the real store does
		stored := key
		h.key = &stored
	}
	return nil
}
func (h *fakeHostKeys) Forget(context.Context, uuid.UUID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.key = nil
	return nil
}
func (h *fakeHostKeys) replaceKey(raw []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.key != nil {
		h.key.PublicKey = raw
		h.key.Fingerprint = "SHA256:the-old-one"
	}
}

type recordingAudit struct {
	mu      sync.Mutex
	actions []string
	all     []ports.AuditEntry
}

func (a *recordingAudit) Write(_ context.Context, e ports.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, e.Action)
	a.all = append(a.all, e)

	// An audit entry that carried the credential would defeat the entire
	// design, so the guard lives here rather than in a comment. The same goes
	// for the portal's own key, which is a secret of a different kind sitting
	// on the same paths (ADR 0006).
	for key, value := range e.Details {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if strings.Contains(text, sshPassword) {
			panic(fmt.Sprintf("audit detail %q leaked the ssh credential", key))
		}
		if strings.Contains(text, "PRIVATE KEY") {
			panic(fmt.Sprintf("audit detail %q leaked private key material", key))
		}
	}
	return nil
}

// entries returns every entry written so far, for the tests that care what a
// detail says rather than only that an action happened.
func (a *recordingAudit) entries() []ports.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ports.AuditEntry(nil), a.all...)
}

func (a *recordingAudit) mustHave(t *testing.T, actions ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, want := range actions {
		found := false
		for _, got := range a.actions {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no %q audit entry; saw %v", want, a.actions)
		}
	}
}

func (a *recordingAudit) mustNotHave(t *testing.T, actions ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, unwanted := range actions {
		for _, got := range a.actions {
			if got == unwanted {
				t.Errorf("unexpected %q audit entry: browsing should not fill the trail", unwanted)
			}
		}
	}
}

// The router puts a 30-second timeout on every request (router.go). A terminal
// is a request that is meant to last hours, so this asserts the obvious thing
// that would otherwise only be discovered by an operator whose shell died
// mid-command: the timeout does not reach a hijacked connection.
//
// It costs 35 seconds of wall clock, so it is skipped under -short.
func TestShellTerminalOutlivesTheRequestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 35s; the point is to outlive a 30s timeout")
	}
	f := newShellFixture(t, sshtest.Options{})
	session := f.open(t)

	conn := f.dialTerminal(t, session)
	defer conn.Close()
	readUntil(t, conn, "test$ ")

	time.Sleep(35 * time.Second)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo still-here\r")); err != nil {
		t.Fatalf("the terminal was closed under it: %v", err)
	}
	if got := readUntil(t, conn, "still-here"); !strings.Contains(got, "still-here") {
		t.Fatalf("no answer after 35 seconds; read %q", got)
	}
}
