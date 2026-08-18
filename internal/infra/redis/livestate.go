package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/freezxp/proxui/internal/app/ports"
)

// LiveStateStore caches on-demand reads of a platform's guest state.
//
// Redis rather than Postgres, and that is a deliberate boundary rather than a
// convenience: `vms` is the reconciler's table, and it decides what changed by
// diffing it. A live read that wrote there would leave nothing to diff and
// would silently end the vm.state_changed events that history and
// notifications depend on. What lives here is a view with a TTL, shared across
// replicas so that one operator's page load spares everyone else's.
type LiveStateStore struct{ client *redis.Client }

// NewLiveStateStore builds the store.
func NewLiveStateStore(c *Client) *LiveStateStore { return &LiveStateStore{client: c.Client} }

var _ ports.LiveStateStore = (*LiveStateStore)(nil)

func liveKey(platformID uuid.UUID) string { return "proxui:live:vms:" + platformID.String() }

// Put stores a snapshot for ttl.
func (s *LiveStateStore) Put(ctx context.Context, snap ports.LiveSnapshot, ttl time.Duration) error {
	payload, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode live snapshot: %w", err)
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if err := s.client.Set(ctx, liveKey(snap.PlatformID), payload, ttl).Err(); err != nil {
		return fmt.Errorf("store live snapshot: %w", err)
	}
	return nil
}

// Forget drops a platform's snapshot across every replica.
func (s *LiveStateStore) Forget(ctx context.Context, platformID uuid.UUID) error {
	if err := s.client.Del(ctx, liveKey(platformID)).Err(); err != nil {
		return fmt.Errorf("drop live snapshot: %w", err)
	}
	return nil
}

// Get returns the cached snapshot, or ErrNotFound once it has expired.
func (s *LiveStateStore) Get(ctx context.Context, platformID uuid.UUID) (ports.LiveSnapshot, error) {
	raw, err := s.client.Get(ctx, liveKey(platformID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ports.LiveSnapshot{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.LiveSnapshot{}, fmt.Errorf("read live snapshot: %w", err)
	}
	var snap ports.LiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ports.LiveSnapshot{}, fmt.Errorf("decode live snapshot: %w", err)
	}
	return snap, nil
}
