package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// capturingStreamer records the context the router handed the event socket.
type capturingStreamer struct {
	ctx context.Context
}

func (c *capturingStreamer) ServeHTTP(_ http.ResponseWriter, r *http.Request, _ uuid.UUID, _ identity.Role) {
	c.ctx = r.Context()
}

func (c *capturingStreamer) Subscribers() int { return 0 }

// staticTickets redeems any ticket, so the test reaches the streamer.
type staticTickets struct{}

func (staticTickets) Issue(context.Context, string, StreamTicket, time.Duration) error { return nil }

func (staticTickets) Redeem(context.Context, string) (StreamTicket, error) {
	return StreamTicket{UserID: uuid.New(), Role: identity.RoleAdmin}, nil
}

// The live event stream runs until the browser goes away: its loop selects on
// r.Context().Done(), so a request deadline on the root router would end every
// stream at the deadline and log a 504 for it. The same deadline would mark
// every console and terminal that outlived it as a failed request.
func TestWebSocketRoutesCarryNoRequestDeadline(t *testing.T) {
	streamer := &capturingStreamer{}
	srv := NewServer(ServerConfig{
		Log:           zerolog.New(io.Discard),
		Version:       "test",
		Readiness:     &Readiness{},
		Events:        streamer,
		StreamTickets: staticTickets{},
	})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/events/"+uuid.NewString(), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if streamer.ctx == nil {
		t.Fatal("the event streamer was never reached")
	}
	if deadline, ok := streamer.ctx.Deadline(); ok {
		t.Fatalf("the event socket got a request deadline of %s; long-lived sockets must have none",
			time.Until(deadline).Round(time.Second))
	}
}

// The deadline the WebSocket routes must not have is still on the JSON API,
// where a request that never finishes should not hold a connection forever.
func TestAPIRoutesKeepTheRequestDeadline(t *testing.T) {
	store := &deadlineSettings{}
	srv := NewServer(ServerConfig{
		Log:       zerolog.New(io.Discard),
		Version:   "test",
		Readiness: &Readiness{},
		Settings:  SettingsDeps{Settings: store},
	})

	// /api/v1/branding is the API route reachable without a session.
	rec := do(t, srv.Routes(), http.MethodGet, "/api/v1/branding")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if !store.ok {
		t.Fatal("an API request carried no deadline; the 30-second timeout is gone")
	}
	if left := time.Until(store.deadline); left > 30*time.Second || left < 25*time.Second {
		t.Fatalf("deadline in %s, want about 30s", left.Round(time.Second))
	}
}

// deadlineSettings reports the deadline the router put on an API request.
type deadlineSettings struct {
	deadline time.Time
	ok       bool
}

func (d *deadlineSettings) All(ctx context.Context) (map[string]json.RawMessage, error) {
	d.deadline, d.ok = ctx.Deadline()
	return map[string]json.RawMessage{}, nil
}

func (d *deadlineSettings) Set(context.Context, string, any, uuid.UUID, time.Time) error {
	return nil
}

func (d *deadlineSettings) SetSecret(context.Context, string, string, *crypto.Vault, uuid.UUID, time.Time) error {
	return nil
}

func (d *deadlineSettings) Reset(context.Context, string) error { return nil }
