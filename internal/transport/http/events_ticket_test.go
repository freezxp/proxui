package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/domain/identity"
)

// fakeTicketRedis is the GETDEL contract the real store depends on: a read
// consumes the key. Getting this wrong in the fake would hide the one property
// the ticket exists for, so it is modelled rather than stubbed.
type fakeTicketRedis struct {
	mu     sync.Mutex
	values map[string]string
	ttls   map[string]time.Duration
}

func newFakeTicketRedis() *fakeTicketRedis {
	return &fakeTicketRedis{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakeTicketRedis) SetTicket(_ context.Context, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key], f.ttls[key] = value, ttl
	return nil
}

func (f *fakeTicketRedis) TakeTicket(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	if !ok {
		return "", errors.New("no such ticket")
	}
	delete(f.values, key)
	return v, nil
}

// A ticket names the person the stream will be scoped to, so it has to survive
// the round trip through Redis intact.
func TestStreamTicketCarriesWhoItWasIssuedTo(t *testing.T) {
	store := NewStreamTicketStore(newFakeTicketRedis())
	want := StreamTicket{UserID: uuid.New(), Role: identity.RoleOperator}

	if err := store.Issue(context.Background(), "ticket-1", want, streamTicketTTL); err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	got, err := store.Redeem(context.Background(), "ticket-1")
	if err != nil {
		t.Fatalf("Redeem() error = %v", err)
	}
	if got != want {
		t.Errorf("redeemed %+v, want %+v", got, want)
	}
}

// The whole point of a ticket in a URL is that it is worth nothing once used.
func TestStreamTicketIsSingleUse(t *testing.T) {
	store := NewStreamTicketStore(newFakeTicketRedis())
	_ = store.Issue(context.Background(), "ticket-1",
		StreamTicket{UserID: uuid.New(), Role: identity.RoleAdmin}, streamTicketTTL)

	if _, err := store.Redeem(context.Background(), "ticket-1"); err != nil {
		t.Fatalf("first redeem failed: %v", err)
	}
	if _, err := store.Redeem(context.Background(), "ticket-1"); err == nil {
		t.Error("a stream ticket was redeemed twice")
	}
	if _, err := store.Redeem(context.Background(), "never-issued"); err == nil {
		t.Error("a ticket nobody issued was accepted")
	}
	if _, err := store.Redeem(context.Background(), ""); err == nil {
		t.Error("an empty ticket id was accepted")
	}
}

// countingStreamer records whether the socket was ever handed over, and to whom.
type countingStreamer struct {
	served int
	userID uuid.UUID
	role   identity.Role
}

func (c *countingStreamer) ServeHTTP(w http.ResponseWriter, _ *http.Request, userID uuid.UUID, role identity.Role) {
	c.served++
	c.userID, c.role = userID, role
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func (c *countingStreamer) Subscribers() int { return c.served }

func newStreamServer(t *testing.T, streamer EventStreamer, store StreamTicketStore) *Server {
	t.Helper()
	rd := &Readiness{}
	rd.MigrationsApplied.Store(true)
	return NewServer(ServerConfig{
		Log: zerolog.New(io.Discard), Version: "test", Readiness: rd,
		Events: streamer, StreamTickets: store,
	})
}

// An unusable ticket must be refused before the upgrade, so the browser gets a
// plain 401 rather than a socket that closes for no stated reason.
func TestEventsSocketRefusesATicketItDidNotIssue(t *testing.T) {
	streamer := &countingStreamer{}
	store := NewStreamTicketStore(newFakeTicketRedis())
	routes := newStreamServer(t, streamer, store).Routes()

	rec := do(t, routes, http.MethodGet, "/ws/events/"+uuid.NewString())

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if streamer.served != 0 {
		t.Error("the stream was served to a request carrying no valid ticket")
	}
}

// The socket carries no Authorization header at all — the ticket is the only
// credential, and it is what tells the hub whose events these are.
func TestEventsSocketAuthenticatesByTicketAlone(t *testing.T) {
	streamer := &countingStreamer{}
	store := NewStreamTicketStore(newFakeTicketRedis())
	routes := newStreamServer(t, streamer, store).Routes()

	want := StreamTicket{UserID: uuid.New(), Role: identity.RoleReadOnly}
	if err := store.Issue(context.Background(), "abc", want, streamTicketTTL); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/events/abc", nil))

	if streamer.served != 1 {
		t.Fatalf("stream served %d times, want 1", streamer.served)
	}
	if streamer.userID != want.UserID || streamer.role != want.Role {
		t.Errorf("stream scoped to %v/%s, want %v/%s",
			streamer.userID, streamer.role, want.UserID, want.Role)
	}

	// And the same ticket a second time gets nowhere.
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/events/abc", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("replayed ticket status = %d, want 401", rec.Code)
	}
	if streamer.served != 1 {
		t.Error("a replayed ticket served the stream a second time")
	}
}
