package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/identity"
)

// StreamTicketStore holds one-time tickets for the live event stream.
//
// The same problem the console solved, and the same solution: a browser cannot
// put an Authorization header on a WebSocket, so the credential has to be
// something the URL can carry safely. A single-use ticket, redeemed and
// deleted on connect, is worth far less than the access token it stands in
// for — which is why the token itself is never put in a URL, where it would
// land in history and every proxy log.
type StreamTicketStore interface {
	Issue(ctx context.Context, id string, t StreamTicket, ttl time.Duration) error
	Redeem(ctx context.Context, id string) (StreamTicket, error)
}

// StreamTicket is who a redeemed ticket belongs to. The role travels with it
// so the hub can scope the stream without a second lookup, and because the
// role at the moment of issue is the one the ticket was authorized under.
type StreamTicket struct {
	UserID uuid.UUID     `json:"user_id"`
	Role   identity.Role `json:"role"`
}

// streamTicketTTL is deliberately short. A ticket is used within a second of
// being issued; anything longer is only a window for it to be stolen from a
// log or a shoulder.
const streamTicketTTL = 30 * time.Second

func (s *Server) handleEventTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}
	if s.events == nil || s.streamTickets == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "events.unavailable",
			"Live updates are not enabled.")
		return
	}

	id := uuid.NewString()
	if err := s.streamTickets.Issue(r.Context(), id,
		StreamTicket{UserID: p.UserID, Role: p.Role}, streamTicketTTL); err != nil {
		s.serverError(w, r, err, "Could not start the live stream.")
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"ws_url":     "/ws/events/" + id,
		"expires_in": int(streamTicketTTL.Seconds()),
	})
}

// handleEventsWS upgrades the live stream, authenticated by its ticket.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	if s.events == nil || s.streamTickets == nil {
		http.Error(w, "live events are not enabled", http.StatusServiceUnavailable)
		return
	}
	ticket, err := s.streamTickets.Redeem(r.Context(), chi.URLParam(r, "ticketID"))
	if err != nil {
		// Unknown, expired or already used: all the same answer, and all
		// before the upgrade so the client gets a plain HTTP error.
		http.Error(w, "the stream ticket is not valid", http.StatusUnauthorized)
		return
	}
	s.events.ServeHTTP(w, r, ticket.UserID, ticket.Role)
}

// redisStreamTickets stores stream tickets in Redis.
type redisStreamTickets struct{ redis RedisTicketOps }

// RedisTicketOps is the slice of Redis the ticket store needs.
type RedisTicketOps interface {
	SetTicket(ctx context.Context, key, value string, ttl time.Duration) error
	TakeTicket(ctx context.Context, key string) (string, error)
}

// NewStreamTicketStore adapts Redis to the ticket store.
func NewStreamTicketStore(ops RedisTicketOps) StreamTicketStore {
	return &redisStreamTickets{redis: ops}
}

func (r *redisStreamTickets) Issue(ctx context.Context, id string, t StreamTicket, ttl time.Duration) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return r.redis.SetTicket(ctx, streamTicketKey(id), string(raw), ttl)
}

func (r *redisStreamTickets) Redeem(ctx context.Context, id string) (StreamTicket, error) {
	if id == "" {
		return StreamTicket{}, errors.New("no ticket")
	}
	raw, err := r.redis.TakeTicket(ctx, streamTicketKey(id))
	if err != nil {
		return StreamTicket{}, err
	}
	var t StreamTicket
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return StreamTicket{}, err
	}
	return t, nil
}

func streamTicketKey(id string) string { return "proxui:events:ticket:" + id }
