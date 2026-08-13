package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter is a fixed-window counter in Redis.
//
// A sliding window would be more precise, but a fixed window costs one round
// trip and is accurate enough for the job: keeping a stolen password from being
// brute-forced and one client from monopolising the API. Redis holds the state
// so the limit is shared across API instances rather than per-process
// (docs/08-api-specification.md §8.10).
type RateLimiter struct{ client *redis.Client }

// NewRateLimiter builds the limiter.
func NewRateLimiter(c *Client) *RateLimiter { return &RateLimiter{client: c.Client} }

// Decision is the outcome of a rate-limit check.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// AllowRequest adapts Allow to the transport layer's Limiter interface.
func (r *RateLimiter) AllowRequest(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error) {
	d, err := r.Allow(ctx, bucket, limit, window)
	return d.Allowed, d.RetryAfter, err
}

// Allow counts a request against a bucket and reports whether it may proceed.
//
// INCR then EXPIRE on first use: the key carries its own lifetime, so an
// abandoned bucket disappears without a cleanup job.
func (r *RateLimiter) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (Decision, error) {
	key := "proxui:ratelimit:" + bucket

	pipe := r.client.TxPipeline()
	count := pipe.Incr(ctx, key)
	// ExpireNX sets the lifetime only on the first request of a window, so a
	// burst cannot keep pushing the window forward and evade the limit.
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		// A limiter that fails closed would take the API down with Redis.
		// Availability wins here: the other controls (lockout, auth) still
		// stand, and the failure is logged by the caller.
		return Decision{Allowed: true}, fmt.Errorf("rate limit check: %w", err)
	}

	used := int(count.Val())
	if used > limit {
		ttl, err := r.client.TTL(ctx, key).Result()
		if err != nil || ttl <= 0 {
			ttl = window
		}
		return Decision{Allowed: false, Remaining: 0, RetryAfter: ttl}, nil
	}
	return Decision{Allowed: true, Remaining: limit - used}, nil
}
