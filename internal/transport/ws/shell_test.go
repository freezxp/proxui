package ws_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/transport/ws"
)

// These tests cover the link the unit tests either side of it cannot: that the
// terminal bridge really gives the session back when the socket goes, and that
// the sweep then sees a detached session and reclaims it (SSH-07, ADR 0008).
// The registry's own tests set the attached flag by hand; here the bridge sets
// it, over a real WebSocket.

type shellClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *shellClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *shellClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type shellTickets struct {
	mu      sync.Mutex
	tickets map[string]shell.Ticket
}

func (s *shellTickets) Issue(_ context.Context, t shell.Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickets == nil {
		s.tickets = map[string]shell.Ticket{}
	}
	s.tickets[t.ID.String()] = t
	return nil
}

func (s *shellTickets) Redeem(_ context.Context, id string) (shell.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[id]
	if !ok {
		return shell.Ticket{}, shell.ErrTicketNotFound
	}
	delete(s.tickets, id)
	return t, nil
}

type shellSessions struct {
	mu     sync.Mutex
	closed []string
}

func (s *shellSessions) Create(context.Context, *shell.Session) error              { return nil }
func (s *shellSessions) MarkConnected(context.Context, uuid.UUID, time.Time) error { return nil }
func (s *shellSessions) Get(context.Context, uuid.UUID) (*shell.Session, error) {
	return nil, ports.ErrNotFound
}
func (s *shellSessions) List(context.Context, bool, int) ([]ports.ShellSessionRecord, error) {
	return nil, nil
}
func (s *shellSessions) Close(_ context.Context, _ uuid.UUID, reason string, _, _ int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, reason)
	return nil
}

// fakeTerm is a shell on the guest that produces nothing and blocks until it
// is closed, which is what an idle login prompt does.
type fakeTerm struct {
	done chan struct{}
	once sync.Once
}

func (t *fakeTerm) Read([]byte) (int, error)    { <-t.done; return 0, io.EOF }
func (t *fakeTerm) Write(p []byte) (int, error) { return len(p), nil }
func (t *fakeTerm) Resize(int, int) error       { return nil }
func (t *fakeTerm) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

type fakeShellConn struct {
	mu     sync.Mutex
	closed bool
	term   *fakeTerm
}

func (c *fakeShellConn) Terminal(ports.TerminalSize) (ports.Terminal, error) { return c.term, nil }
func (c *fakeShellConn) Files() (ports.RemoteFS, error)                      { return nil, nil }
func (c *fakeShellConn) HostKey() shell.HostKey                              { return shell.HostKey{} }
func (c *fakeShellConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeShellConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type shellHarness struct {
	server   *httptest.Server
	clock    *shellClock
	registry *shellreg.Registry
	tickets  *shellTickets
	live     *shellreg.Live
	conn     *fakeShellConn
	reasons  chan string
}

func newShellHarness(t *testing.T) *shellHarness {
	t.Helper()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	h := &shellHarness{
		clock:   &shellClock{now: now},
		tickets: &shellTickets{},
		conn:    &fakeShellConn{term: &fakeTerm{done: make(chan struct{})}},
		reasons: make(chan string, 4),
	}
	h.registry = shellreg.NewRegistry(h.clock.Now)
	h.registry.OnEvict = func(_ *shellreg.Live, reason string) { h.reasons <- reason }

	h.live = shellreg.NewLive(uuid.New(), uuid.New(), uuid.New(), "vm", "root",
		"10.0.0.1:22", h.conn, now)
	if err := h.registry.Add(h.live); err != nil {
		t.Fatalf("add: %v", err)
	}

	bridge := &ws.ShellBridge{
		Tickets: h.tickets, Sessions: &shellSessions{}, Registry: h.registry,
		Clock: h.clock, Log: zerolog.New(io.Discard),
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridge.ServeHTTP(w, r, strings.TrimPrefix(r.URL.Path, "/ws/shell/"))
	}))
	t.Cleanup(func() {
		h.server.Close()
		h.conn.term.Close()
	})
	return h
}

// dial attaches a terminal the way the browser does, ticket and all.
func (h *shellHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ticket := shell.NewTicket(h.live.SessionID, h.live.UserID, h.live.VMID, h.clock.Now())
	if err := h.tickets.Issue(context.Background(), ticket); err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws/shell/" + ticket.ID.String()
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client
}

// awaitAttached waits for the bridge's own goroutine to reach the state the
// test is about, since the socket closing and the server noticing are not the
// same instant.
func (h *shellHarness) awaitAttached(t *testing.T, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.live.Attached() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session attached = %v, want %v", h.live.Attached(), want)
}

// The defect ADR 0008 is about: a tab that goes away without a goodbye must
// not hold one of the operator's eight slots for half an hour.
func TestShellBridgeReleasesTheSessionWhenTheSocketGoes(t *testing.T) {
	h := newShellHarness(t)

	client := h.dial(t)
	h.awaitAttached(t, true)

	// The tab is gone. Nothing was said on the way out - this is the backstop,
	// not the browser's own release.
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	h.awaitAttached(t, false)

	// Still inside the grace: a socket that dropped under a page left open
	// gets its couple of minutes to come back.
	h.clock.advance(shell.DetachedGrace - time.Second)
	h.registry.Sweep()
	if h.registry.Len() != 1 {
		t.Fatal("a detached session was reclaimed before its grace was up")
	}

	h.clock.advance(time.Second)
	h.registry.Sweep()
	if h.registry.Len() != 0 {
		t.Fatal("an abandoned session survived the sweep; it holds a guest login nobody can reach")
	}
	if !h.conn.isClosed() {
		t.Fatal("the reclaimed session's SSH connection was left open")
	}
	select {
	case reason := <-h.reasons:
		if reason != shell.ReasonAbandoned {
			t.Fatalf("close reason = %q, want %q", reason, shell.ReasonAbandoned)
		}
	default:
		t.Fatal("nothing was reported to OnEvict, so no record or audit entry would be written")
	}
}

// The other half of the same decision: an operator sitting in front of a quiet
// terminal keeps the long limit. If the flag were not really wired through the
// bridge, this would close them out after two minutes of not typing.
func TestShellBridgeKeepsAQuietAttachedTerminal(t *testing.T) {
	h := newShellHarness(t)

	client := h.dial(t)
	defer client.Close()
	h.awaitAttached(t, true)

	h.clock.advance(shell.DetachedGrace * 2)
	h.registry.Sweep()
	if h.registry.Len() != 1 {
		t.Fatal("an attached terminal was closed on the detached limit")
	}

	h.clock.advance(shell.IdleTimeout)
	h.registry.Sweep()
	if h.registry.Len() != 0 {
		t.Fatal("an attached terminal outlived the idle limit")
	}
}
