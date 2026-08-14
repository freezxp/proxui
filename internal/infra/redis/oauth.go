package redis

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/freezxp/proxui/internal/infra/oauth"
)

// AttemptStore holds in-flight external sign-ins.
//
// Server-side rather than in a cookie, because the PKCE verifier must never
// travel through the browser: if it did, whatever intercepted the code could
// also complete the exchange.
type AttemptStore struct{ client *Client }

// NewAttemptStore builds the store.
func NewAttemptStore(client *Client) *AttemptStore { return &AttemptStore{client: client} }

// Put records an attempt against its state value.
func (s *AttemptStore) Put(ctx context.Context, state string, attempt oauth.Attempt, ttl time.Duration) error {
	raw, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, attemptKey(state), raw, ttl).Err()
}

// Take consumes an attempt. An attempt is good for exactly one callback, so
// this reads and deletes together and a replayed callback finds nothing —
// the same GETDEL trick the console tickets use.
func (s *AttemptStore) Take(ctx context.Context, state string) (oauth.Attempt, error) {
	if state == "" {
		return oauth.Attempt{}, errors.New("redis: no sign-in state supplied")
	}
	raw, err := s.client.GetDel(ctx, attemptKey(state)).Bytes()
	if err != nil {
		return oauth.Attempt{}, err
	}

	var attempt oauth.Attempt
	if err := json.Unmarshal(raw, &attempt); err != nil {
		return oauth.Attempt{}, err
	}
	if attempt.State != state {
		return oauth.Attempt{}, errors.New("redis: sign-in state does not match")
	}
	return attempt, nil
}

func attemptKey(state string) string { return "proxui:oauth:attempt:" + state }

// SetTicket stores a short-lived one-time value.
func (c *Client) SetTicket(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.Set(ctx, key, value, ttl).Err()
}

// TakeTicket reads and deletes in one step, so a ticket presented twice finds
// nothing the second time.
func (c *Client) TakeTicket(ctx context.Context, key string) (string, error) {
	return c.GetDel(ctx, key).Result()
}
