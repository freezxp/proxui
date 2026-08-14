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
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connectors/mock"
	"github.com/freezxp/proxui/internal/domain/console"
	"github.com/freezxp/proxui/internal/transport/ws"
)

// The bridge is the portal's most sensitive path, so it is tested end to end:
// a real WebSocket client, the real bridge, and the mock platform's WebSocket
// console standing in for Proxmox. No hypervisor involved.

type memTickets struct {
	mu      sync.Mutex
	tickets map[string]console.Ticket
}

func newMemTickets() *memTickets {
	return &memTickets{tickets: map[string]console.Ticket{}}
}

func (m *memTickets) Issue(_ context.Context, t console.Ticket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tickets[t.ID.String()] = t
	return nil
}

// Redeem deletes as it reads, exactly as the Redis store's GETDEL does.
func (m *memTickets) Redeem(_ context.Context, id string) (console.Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok {
		return console.Ticket{}, console.ErrTicketNotFound
	}
	delete(m.tickets, id)
	return t, nil
}

type memSessions struct {
	mu        sync.Mutex
	sessions  map[uuid.UUID]*console.Session
	connected int
	closed    []string
}

func newMemSessions() *memSessions {
	return &memSessions{sessions: map[uuid.UUID]*console.Session{}}
}

func (m *memSessions) Create(_ context.Context, s *console.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *memSessions) MarkConnected(_ context.Context, id uuid.UUID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected++
	if s, ok := m.sessions[id]; ok {
		s.ConnectedAt = at
	}
	return nil
}

func (m *memSessions) Close(_ context.Context, id uuid.UUID, reason string, tx, rx int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = append(m.closed, reason)
	if s, ok := m.sessions[id]; ok {
		s.EndedAt, s.CloseReason, s.BytesTx, s.BytesRx = at, reason, tx, rx
	}
	return nil
}

func (m *memSessions) Get(_ context.Context, id uuid.UUID) (*console.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return s, nil
}

func (m *memSessions) List(context.Context, bool, int) ([]ports.ConsoleSessionRecord, error) {
	return nil, nil
}

func (m *memSessions) closeReasons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.closed))
	copy(out, m.closed)
	return out
}

type memAudit struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (m *memAudit) Write(_ context.Context, e ports.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) has(action string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.Action == action {
			return true
		}
	}
	return false
}

// mockResolver hands the bridge the mock platform's WebSocket console.
type mockResolver struct {
	conn connector.Connector
	err  error
}

func (r *mockResolver) Resolve(ctx context.Context, _ uuid.UUID, kind console.Kind) (connector.ConsoleEndpoint, func(), error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	endpoint, err := r.conn.(connector.ConsoleProvider).CreateConsoleSession(ctx,
		connector.VMRef{ExternalID: "100", Type: "qemu"}, connector.ConsoleKind(kind))
	if err != nil {
		return nil, nil, err
	}
	return endpoint, func() {}, nil
}

type harness struct {
	server   *httptest.Server
	tickets  *memTickets
	sessions *memSessions
	audit    *memAudit
}

func newHarness(t *testing.T, resolver ws.EndpointResolver) *harness {
	t.Helper()
	h := &harness{tickets: newMemTickets(), sessions: newMemSessions(), audit: &memAudit{}}

	bridge := &ws.ConsoleBridge{
		Tickets: h.tickets, Sessions: h.sessions, Resolver: resolver,
		Audit: h.audit, Clock: ports.SystemClock{}, Log: zerolog.New(io.Discard),
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridge.ServeHTTP(w, r, strings.TrimPrefix(r.URL.Path, "/ws/console/"))
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) issue(t *testing.T) console.Ticket {
	t.Helper()
	ticket := console.NewTicket(uuid.New(), uuid.New(), uuid.New(), console.KindVNC, time.Now())
	if err := h.tickets.Issue(context.Background(), ticket); err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	if err := h.sessions.Create(context.Background(), &console.Session{
		ID: ticket.SessionID, UserID: ticket.UserID, VMID: ticket.VMID,
		Kind: console.KindVNC, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return ticket
}

func (h *harness) dial(t *testing.T, ticketID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws/console/" + ticketID
	return websocket.DefaultDialer.Dial(url, nil)
}

func mockConnector(t *testing.T) connector.Connector {
	t.Helper()
	c, err := mock.New(connector.Config{Endpoint: "mock://local"}, connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("mock connector: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The whole point of the bridge: bytes reach the platform and come back, with
// the browser never learning where the platform is.
func TestBridgeRelaysBothDirections(t *testing.T) {
	h := newHarness(t, &mockResolver{conn: mockConnector(t)})
	ticket := h.issue(t)

	client, _, err := h.dial(t, ticket.ID.String())
	if err != nil {
		t.Fatalf("dial console: %v", err)
	}
	defer client.Close()

	// An RFB handshake is just bytes to the bridge; that is the design.
	for _, payload := range [][]byte{[]byte("RFB 003.008\n"), {0x00, 0x01, 0x02, 0xff}} {
		if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, got, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(payload) {
			t.Errorf("round trip returned %q, want %q", got, payload)
		}
	}

	client.Close()
	waitFor(t, func() bool { return len(h.sessions.closeReasons()) > 0 })

	if h.sessions.connected == 0 {
		t.Error("the session was never marked connected")
	}
	if !h.audit.has("console_session_closed") {
		t.Error("closing a console was not audited")
	}

	session, _ := h.sessions.Get(context.Background(), ticket.SessionID)
	if session.BytesTx == 0 || session.BytesRx == 0 {
		t.Errorf("traffic counters = tx %d rx %d, want both non-zero", session.BytesTx, session.BytesRx)
	}
}

// A ticket is single use: this is what makes a leaked ticket worthless the
// moment the legitimate client has connected.
func TestTicketCannotBeReused(t *testing.T) {
	h := newHarness(t, &mockResolver{conn: mockConnector(t)})
	ticket := h.issue(t)

	first, _, err := h.dial(t, ticket.ID.String())
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()

	_, resp, err := h.dial(t, ticket.ID.String())
	if err == nil {
		t.Fatal("a spent ticket opened a second console")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay response = %v, want 401", resp)
	}
}

func TestUnknownTicketIsRejectedBeforeUpgrade(t *testing.T) {
	h := newHarness(t, &mockResolver{conn: mockConnector(t)})

	_, resp, err := h.dial(t, uuid.NewString())
	if err == nil {
		t.Fatal("an unknown ticket was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("response = %v, want 401 before any upgrade", resp)
	}
}

func TestExpiredTicketIsRejected(t *testing.T) {
	h := newHarness(t, &mockResolver{conn: mockConnector(t)})

	stale := console.Ticket{
		ID: uuid.New(), SessionID: uuid.New(), UserID: uuid.New(), VMID: uuid.New(),
		Kind: console.KindVNC, IssuedAt: time.Now().Add(-2 * console.TicketTTL),
		ExpiresAt: time.Now().Add(-console.TicketTTL),
	}
	if err := h.tickets.Issue(context.Background(), stale); err != nil {
		t.Fatalf("issue: %v", err)
	}

	_, resp, err := h.dial(t, stale.ID.String())
	if err == nil {
		t.Fatal("an expired ticket was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("response = %v, want 401", resp)
	}
}

// When the platform cannot be reached the browser must be told clearly, and the
// session must still be closed out rather than left open forever.
func TestUnreachablePlatformClosesCleanly(t *testing.T) {
	h := newHarness(t, &mockResolver{err: connector.Errorf(connector.ErrUnreachable, "console", "node down")})
	ticket := h.issue(t)

	client, _, err := h.dial(t, ticket.ID.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := client.ReadMessage()
	if readErr == nil {
		t.Fatal("expected the console to close")
	}
	if closeErr, ok := readErr.(*websocket.CloseError); ok {
		if closeErr.Code != ws.CloseUnavailable {
			t.Errorf("close code = %d, want %d so the UI can explain it", closeErr.Code, ws.CloseUnavailable)
		}
	}

	waitFor(t, func() bool { return len(h.sessions.closeReasons()) > 0 })
	reasons := h.sessions.closeReasons()
	if reasons[0] != console.ReasonUpstream {
		t.Errorf("close reason = %q, want %q", reasons[0], console.ReasonUpstream)
	}
}

// A console request that never connects still has to appear in the record; an
// audit trail that only logs successes is not an audit trail.
func TestFailedConnectionIsStillRecorded(t *testing.T) {
	h := newHarness(t, &mockResolver{err: connector.Errorf(connector.ErrAuth, "console", "token rejected")})
	ticket := h.issue(t)

	client, _, err := h.dial(t, ticket.ID.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	waitFor(t, func() bool { return h.audit.has("console_session_closed") })

	session, err := h.sessions.Get(context.Background(), ticket.SessionID)
	if err != nil {
		t.Fatalf("session was not recorded: %v", err)
	}
	if session.EndedAt.IsZero() {
		t.Error("a failed console session was left open")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met within the timeout")
}

// A browser that requests a subprotocol and does not get it back closes the
// connection immediately, which reaches the user as a console that never
// finishes connecting. The handshake here is what a browser actually sends.
func TestConsoleNegotiatesTheBinarySubprotocol(t *testing.T) {
	h := newHarness(t, &mockResolver{conn: mockConnector(t)})
	ticket := h.issue(t)

	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws/console/" + ticket.ID.String()
	dialer := websocket.Dialer{Subprotocols: []string{"binary"}}
	conn, resp, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "binary" {
		t.Errorf("server answered with subprotocol %q; a browser would fail the connection", got)
	}
	if got := conn.Subprotocol(); got != "binary" {
		t.Errorf("negotiated subprotocol = %q, want binary", got)
	}
}
