package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/freezxp/proxui/internal/domain/console"
)

// TicketStore holds one-time console tickets.
//
// Redis rather than the database because a ticket lives for sixty seconds and
// must be redeemable exactly once: GETDEL does that atomically, so two browsers
// racing on the same id cannot both get a console. Expiry is the store's job,
// not a cleanup task's.
type TicketStore struct{ client *redis.Client }

// NewTicketStore builds the store.
func NewTicketStore(c *Client) *TicketStore { return &TicketStore{client: c.Client} }

func ticketKey(id string) string { return "proxui:console:ticket:" + id }

// Issue stores a ticket with its own TTL.
func (s *TicketStore) Issue(ctx context.Context, t console.Ticket) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode console ticket: %w", err)
	}
	ttl := t.ExpiresAt.Sub(t.IssuedAt)
	if ttl <= 0 {
		ttl = console.TicketTTL
	}
	if err := s.client.Set(ctx, ticketKey(t.ID.String()), payload, ttl).Err(); err != nil {
		return fmt.Errorf("store console ticket: %w", err)
	}
	return nil
}

// Redeem consumes a ticket, returning it exactly once.
//
// GETDEL is the whole point: reading and deleting in one round trip means a
// replayed ticket id finds nothing, so a stolen ticket is useless the moment
// the legitimate client has used it.
func (s *TicketStore) Redeem(ctx context.Context, id string) (console.Ticket, error) {
	raw, err := s.client.GetDel(ctx, ticketKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return console.Ticket{}, console.ErrTicketNotFound
	}
	if err != nil {
		return console.Ticket{}, fmt.Errorf("redeem console ticket: %w", err)
	}

	var ticket console.Ticket
	if err := json.Unmarshal(raw, &ticket); err != nil {
		return console.Ticket{}, fmt.Errorf("decode console ticket: %w", err)
	}
	return ticket, nil
}
