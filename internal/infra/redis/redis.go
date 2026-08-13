// Package redis provides the shared Redis client (cache, queue transport,
// pub/sub) used across the application.
package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Client wraps the go-redis client with the small surface the app depends on.
type Client struct{ *redis.Client }

// Connect opens the Redis client and verifies it with a ping.
func Connect(ctx context.Context, url string) (*Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Client{c}, nil
}

// Ping implements the readiness Checker contract.
func (c *Client) Ping(ctx context.Context) error { return c.Client.Ping(ctx).Err() }
