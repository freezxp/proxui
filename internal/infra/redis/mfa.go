package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// MFAChallengeStore holds logins that have passed their password check and are
// waiting for a code (AUTH-04).
//
// Redis for the same reasons as console tickets, plus one of its own: the
// attempt counter has to be shared. A challenge held in one process's memory
// would let five guesses become five per replica, and would strand a
// half-finished login if the browser's next request landed elsewhere.
type MFAChallengeStore struct{ client *redis.Client }

// NewMFAChallengeStore builds the store.
func NewMFAChallengeStore(c *Client) *MFAChallengeStore {
	return &MFAChallengeStore{client: c.Client}
}

func mfaKey(id string) string { return "proxui:mfa:challenge:" + id }

// Issue stores a challenge with its own TTL.
func (s *MFAChallengeStore) Issue(ctx context.Context, c identity.MFAChallenge) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode mfa challenge: %w", err)
	}
	ttl := c.ExpiresAt.Sub(c.IssuedAt)
	if ttl <= 0 {
		ttl = identity.MFAChallengeTTL
	}
	if err := s.client.Set(ctx, mfaKey(c.ID.String()), payload, ttl).Err(); err != nil {
		return fmt.Errorf("store mfa challenge: %w", err)
	}
	return nil
}

// Get returns a live challenge.
func (s *MFAChallengeStore) Get(ctx context.Context, id string) (identity.MFAChallenge, error) {
	raw, err := s.client.Get(ctx, mfaKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return identity.MFAChallenge{}, ports.ErrNotFound
	}
	if err != nil {
		return identity.MFAChallenge{}, fmt.Errorf("read mfa challenge: %w", err)
	}
	var c identity.MFAChallenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return identity.MFAChallenge{}, fmt.Errorf("decode mfa challenge: %w", err)
	}
	return c, nil
}

// Fail records a wrong code and returns the challenge as it now stands.
//
// The remaining TTL is preserved rather than reset: a wrong guess must not buy
// more time on a challenge that was about to expire. Once the attempts are
// spent the challenge is deleted, so the next guess finds nothing and the
// operator starts again from the password.
func (s *MFAChallengeStore) Fail(ctx context.Context, id string) (identity.MFAChallenge, error) {
	key := mfaKey(id)
	ttl, err := s.client.TTL(ctx, key).Result()
	if err != nil {
		return identity.MFAChallenge{}, fmt.Errorf("read mfa challenge ttl: %w", err)
	}

	c, err := s.Get(ctx, id)
	if err != nil {
		return identity.MFAChallenge{}, err
	}
	c.Attempts++

	if c.Exhausted() || ttl <= 0 {
		if err := s.client.Del(ctx, key).Err(); err != nil {
			return c, fmt.Errorf("drop mfa challenge: %w", err)
		}
		return c, nil
	}

	payload, err := json.Marshal(c)
	if err != nil {
		return c, fmt.Errorf("encode mfa challenge: %w", err)
	}
	if err := s.client.Set(ctx, key, payload, ttl).Err(); err != nil {
		return c, fmt.Errorf("store mfa challenge: %w", err)
	}
	return c, nil
}

// Consume removes an answered challenge, so a correct code cannot be replayed
// with the same challenge id.
func (s *MFAChallengeStore) Consume(ctx context.Context, id string) error {
	if err := s.client.Del(ctx, mfaKey(id)).Err(); err != nil {
		return fmt.Errorf("consume mfa challenge: %w", err)
	}
	return nil
}
