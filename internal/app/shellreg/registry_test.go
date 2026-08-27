package shellreg_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// fakeConn stands in for a live SSH connection. The registry never speaks the
// protocol; it only has to know whether the connection is still open.
type fakeConn struct {
	mu     sync.Mutex
	closed bool
	files  ports.RemoteFS
	err    error
}

func (c *fakeConn) Terminal(ports.TerminalSize) (ports.Terminal, error) { return nil, nil }
func (c *fakeConn) Files() (ports.RemoteFS, error)                      { return c.files, c.err }
func (c *fakeConn) HostKey() shell.HostKey                              { return shell.HostKey{} }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func live(user uuid.UUID, at time.Time) (*shellreg.Live, *fakeConn) {
	conn := &fakeConn{}
	return shellreg.NewLive(uuid.New(), user, uuid.New(), "vm", "root", "10.0.0.1:22", conn, at), conn
}

func TestRegistryRefusesSomeoneElsesSession(t *testing.T) {
	registry := shellreg.NewRegistry(time.Now)
	owner, intruder := uuid.New(), uuid.New()

	session, _ := live(owner, time.Now())
	if err := registry.Add(session); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := registry.Get(session.SessionID, owner); err != nil {
		t.Fatalf("the owner was refused their own session: %v", err)
	}
	// Deliberately the same error as "no such session": telling the intruder
	// that it exists would confirm that it does.
	if _, err := registry.Get(session.SessionID, intruder); !errors.Is(err, shell.ErrSessionNotFound) {
		t.Fatalf("another user got %v, want ErrSessionNotFound", err)
	}
	// uuid.Nil is an administrator acting on someone else's session.
	if _, err := registry.Get(session.SessionID, uuid.Nil); err != nil {
		t.Fatalf("an administrator was refused: %v", err)
	}
}

func TestRegistryCapsSessionsPerUser(t *testing.T) {
	registry := shellreg.NewRegistry(time.Now)
	user := uuid.New()

	for i := 0; i < shellreg.MaxPerUser; i++ {
		session, _ := live(user, time.Now())
		if err := registry.Add(session); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	extra, _ := live(user, time.Now())
	if err := registry.Add(extra); !errors.Is(err, shellreg.ErrTooMany) {
		t.Fatalf("add beyond the ceiling = %v, want ErrTooMany", err)
	}

	// Someone else is unaffected: the limit is per person, not global-by-proxy.
	other, _ := live(uuid.New(), time.Now())
	if err := registry.Add(other); err != nil {
		t.Fatalf("a different user was blocked by the first user's sessions: %v", err)
	}
}

func TestRegistrySweepsIdleSessions(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	registry := shellreg.NewRegistry(clock.Now)

	var (
		mu      sync.Mutex
		evicted []string
	)
	registry.OnEvict = func(_ *shellreg.Live, reason string) {
		mu.Lock()
		defer mu.Unlock()
		evicted = append(evicted, reason)
	}

	session, conn := live(uuid.New(), now)
	if err := registry.Add(session); err != nil {
		t.Fatalf("add: %v", err)
	}
	// With a terminal attached, so this exercises the long idle limit rather
	// than the short one a detached session gets.
	if !session.Attach() {
		t.Fatal("attach: the new session was already claimed")
	}

	// Not yet idle.
	clock.advance(shell.IdleTimeout - time.Minute)
	registry.Sweep()
	if registry.Len() != 1 {
		t.Fatal("a session in use was swept")
	}

	// Typing resets it — the whole point of Touch.
	session.Touch(clock.now)
	clock.advance(shell.IdleTimeout - time.Minute)
	registry.Sweep()
	if registry.Len() != 1 {
		t.Fatal("activity did not hold the idle timer off")
	}

	clock.advance(shell.IdleTimeout)
	registry.Sweep()
	if registry.Len() != 0 {
		t.Fatal("an idle session survived the sweep; a root shell would stay open")
	}
	if !conn.isClosed() {
		t.Fatal("the swept session's connection was never closed")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != shell.ReasonIdle {
		t.Fatalf("evictions = %v, want one idle_timeout so the record and audit are written", evicted)
	}
}

func TestRegistrySweepReclaimsAnAbandonedSession(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	registry := shellreg.NewRegistry(clock.Now)

	var reasons []string
	registry.OnEvict = func(_ *shellreg.Live, reason string) { reasons = append(reasons, reason) }

	session, conn := live(uuid.New(), now)
	if err := registry.Add(session); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !session.Attach() {
		t.Fatal("attach: the new session was already claimed")
	}

	// The browser went away. Whatever it managed to say on its way out, the
	// terminal socket is gone and the session is detached.
	session.Detach()

	clock.advance(shell.DetachedGrace - time.Second)
	registry.Sweep()
	if registry.Len() != 1 {
		t.Fatal("a session was reclaimed before its grace was up")
	}

	// The file browser is a legitimate way to use a session with no terminal,
	// so anything touching it holds it open.
	session.Touch(clock.now)
	clock.advance(shell.DetachedGrace - time.Second)
	registry.Sweep()
	if registry.Len() != 1 {
		t.Fatal("activity did not hold the detached limit off; the file browser would be cut off mid-transfer")
	}

	clock.advance(shell.DetachedGrace)
	registry.Sweep()
	if registry.Len() != 0 {
		t.Fatal("an abandoned session survived; it holds a guest login nobody can reach")
	}
	if !conn.isClosed() {
		t.Fatal("the reclaimed session's connection was never closed")
	}
	if len(reasons) != 1 || reasons[0] != shell.ReasonAbandoned {
		t.Fatalf("evictions = %v, want one abandoned so the record and audit are written", reasons)
	}
}

// A session that is opened and never attached is the case where the operator
// closed the tab between posting the credential and the terminal connecting.
// Nothing will ever attach to it - the ticket outlives it by seconds - so it
// must not wait out the long idle limit.
func TestRegistrySweepReclaimsASessionNoTerminalEverTook(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	registry := shellreg.NewRegistry(clock.Now)

	var reasons []string
	registry.OnEvict = func(_ *shellreg.Live, reason string) { reasons = append(reasons, reason) }

	session, _ := live(uuid.New(), now)
	if err := registry.Add(session); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Long enough for the browser to have opened a terminal several times over.
	clock.advance(shell.TicketTTL)
	registry.Sweep()
	if registry.Len() != 1 {
		t.Fatal("a session was reclaimed while its ticket was still being redeemed")
	}

	clock.advance(shell.DetachedGrace)
	registry.Sweep()
	if registry.Len() != 0 {
		t.Fatal("a session no terminal ever took survived")
	}
	if len(reasons) != 1 || reasons[0] != shell.ReasonAbandoned {
		t.Fatalf("evictions = %v, want abandoned", reasons)
	}
}

func TestRegistrySweepEnforcesTheHardCeiling(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	registry := shellreg.NewRegistry(clock.Now)

	var reasons []string
	registry.OnEvict = func(_ *shellreg.Live, reason string) { reasons = append(reasons, reason) }

	session, _ := live(uuid.New(), now)
	_ = registry.Add(session)

	// Busy the whole time: activity must not defeat the hard cap.
	for elapsed := time.Duration(0); elapsed < shell.MaxDuration; elapsed += time.Hour {
		clock.advance(time.Hour)
		session.Touch(clock.now)
		registry.Sweep()
	}
	if registry.Len() != 0 {
		t.Fatal("an eight-hour session survived; the hard cap is not enforced")
	}
	if len(reasons) != 1 || reasons[0] != shell.ReasonMaxDuration {
		t.Fatalf("evictions = %v, want max_duration", reasons)
	}
}

func TestRegistryCloseAllEndsEverything(t *testing.T) {
	registry := shellreg.NewRegistry(time.Now)
	var closed int
	registry.OnEvict = func(*shellreg.Live, string) { closed++ }

	for i := 0; i < 3; i++ {
		session, _ := live(uuid.New(), time.Now())
		_ = registry.Add(session)
	}

	// A shutdown that dropped connections without reporting them would leave
	// sessions that look open forever in the administrator's list.
	registry.CloseAll(shell.ReasonUpstream)
	if registry.Len() != 0 || closed != 3 {
		t.Fatalf("after CloseAll: %d open, %d reported", registry.Len(), closed)
	}
}

func TestRegistryCloseUserEndsOnlyTheirs(t *testing.T) {
	registry := shellreg.NewRegistry(time.Now)
	var reported int
	registry.OnEvict = func(*shellreg.Live, string) { reported++ }

	leaver := uuid.New()
	theirs, theirConn := live(leaver, time.Now())
	alsoTheirs, _ := live(leaver, time.Now())
	somebodyElse, otherConn := live(uuid.New(), time.Now())
	for _, l := range []*shellreg.Live{theirs, alsoTheirs, somebodyElse} {
		if err := registry.Add(l); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// A deleted account keeps its shell until something closes it: the
	// connection is already open and answers no further authorization.
	if n := registry.CloseUser(leaver, "account deleted"); n != 2 {
		t.Fatalf("CloseUser closed %d sessions, want 2", n)
	}
	if !theirConn.isClosed() {
		t.Error("the deleted account's connection is still open")
	}
	if otherConn.isClosed() {
		t.Error("somebody else's connection was closed too")
	}
	if registry.Len() != 1 || reported != 2 {
		t.Fatalf("after CloseUser: %d open, %d reported", registry.Len(), reported)
	}
}

func TestLiveAttachIsExclusive(t *testing.T) {
	session, _ := live(uuid.New(), time.Now())

	if !session.Attach() {
		t.Fatal("the first terminal could not attach")
	}
	// A second tab must not share one shell: two people's keystrokes
	// interleaved into one prompt reads as a haunting.
	if session.Attach() {
		t.Fatal("a second terminal attached to the same session")
	}
	session.Detach()
	if !session.Attach() {
		t.Fatal("a reconnect could not take the session back")
	}
}

func TestLiveOpensFilesOnce(t *testing.T) {
	conn := &fakeConn{files: nopFS{}}
	session := shellreg.NewLive(uuid.New(), uuid.New(), uuid.New(), "vm", "root", "a:22", conn, time.Now())

	first, err := session.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	second, _ := session.Files()
	if first != second {
		t.Fatal("a second call negotiated another sftp subsystem")
	}
}

func TestRemovedSessionIsGone(t *testing.T) {
	registry := shellreg.NewRegistry(time.Now)
	user := uuid.New()
	session, conn := live(user, time.Now())
	_ = registry.Add(session)

	if _, ok := registry.Remove(session.SessionID); !ok {
		t.Fatal("Remove reported nothing to remove")
	}
	if !conn.isClosed() {
		t.Fatal("Remove left the connection open")
	}
	if _, err := registry.Get(session.SessionID, user); !errors.Is(err, shell.ErrSessionNotFound) {
		t.Fatalf("a removed session is still reachable: %v", err)
	}
	if _, ok := registry.Remove(session.SessionID); ok {
		t.Fatal("a second Remove reported success")
	}
}

func TestTicketStoreRedeemsOnce(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	store := shellreg.NewTicketStore(clock.Now)
	ctx := context.Background()

	ticket := shell.NewTicket(uuid.New(), uuid.New(), uuid.New(), now)
	if err := store.Issue(ctx, ticket); err != nil {
		t.Fatalf("issue: %v", err)
	}

	if got, err := store.Redeem(ctx, ticket.ID.String()); err != nil || got.SessionID != ticket.SessionID {
		t.Fatalf("redeem = %v, %v", got, err)
	}
	// A replayed ticket must find nothing: that is what makes a stolen one
	// worthless the moment the real browser has used it.
	if _, err := store.Redeem(ctx, ticket.ID.String()); !errors.Is(err, shell.ErrTicketNotFound) {
		t.Fatalf("replay = %v, want ErrTicketNotFound", err)
	}
}

func TestTicketStoreExpires(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	store := shellreg.NewTicketStore(clock.Now)
	ctx := context.Background()

	ticket := shell.NewTicket(uuid.New(), uuid.New(), uuid.New(), now)
	_ = store.Issue(ctx, ticket)

	clock.advance(shell.TicketTTL)
	if _, err := store.Redeem(ctx, ticket.ID.String()); !errors.Is(err, shell.ErrTicketNotFound) {
		t.Fatalf("an expired ticket was accepted: %v", err)
	}
}

// --- helpers ------------------------------------------------------------

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type nopFS struct{}

func (nopFS) List(context.Context, string) ([]shell.FileEntry, error) { return nil, nil }
func (nopFS) Stat(context.Context, string) (shell.FileEntry, error)   { return shell.FileEntry{}, nil }
func (nopFS) Open(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}
func (nopFS) Create(context.Context, string) (io.WriteCloser, error) { return nil, nil }
func (nopFS) Append(context.Context, string) (io.WriteCloser, error) { return nil, nil }
func (nopFS) Mkdir(context.Context, string) error                    { return nil }
func (nopFS) Remove(context.Context, string) error                   { return nil }
func (nopFS) Rename(context.Context, string, string) error           { return nil }
func (nopFS) Chmod(context.Context, string, uint32) error            { return nil }
func (nopFS) Home(context.Context) (string, error)                   { return "/root", nil }
