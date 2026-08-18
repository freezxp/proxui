package shellreg

import (
	"context"
	"sync"
	"time"

	"github.com/freezxp/proxui/internal/domain/shell"
)

// TicketStore holds one-time terminal tickets in the memory of one process.
//
// The console's equivalent lives in Redis, which is right for it: a console
// ticket names a VM, and any api instance can act on it. An SSH ticket names a
// connection that exists only here, so a ticket another process could read
// would be a ticket it could not honour - and Redis in this deployment runs
// with `appendonly yes`, which would additionally write it to disk. A ticket is
// not a secret worth persisting for sixty seconds (SSH-03, SSH-05).
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]shell.Ticket
	clock   func() time.Time
}

// NewTicketStore builds an empty store.
func NewTicketStore(clock func() time.Time) *TicketStore {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &TicketStore{tickets: map[string]shell.Ticket{}, clock: clock}
}

// Issue stores a ticket until it is redeemed or expires.
func (s *TicketStore) Issue(_ context.Context, t shell.Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tickets[t.ID.String()] = t
	return nil
}

// Redeem consumes a ticket, returning it exactly once. Deleting on read is the
// whole point: a replayed id finds nothing.
func (s *TicketStore) Redeem(_ context.Context, id string) (shell.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tickets[id]
	delete(s.tickets, id)
	if !ok || t.Expired(s.clock()) {
		return shell.Ticket{}, shell.ErrTicketNotFound
	}
	return t, nil
}

// sweepLocked drops expired tickets. There is no janitor goroutine because
// there is nothing to reclaim but a map entry, and issuing is the only moment
// the map can grow.
func (s *TicketStore) sweepLocked() {
	now := s.clock()
	for id, t := range s.tickets {
		if t.Expired(now) {
			delete(s.tickets, id)
		}
	}
}
