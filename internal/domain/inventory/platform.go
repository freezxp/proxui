// Package inventory is the Inventory bounded context: platforms and the assets
// synced from them, plus the rules governing how synced state changes.
package inventory

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sync defaults, in seconds. Health runs most often because it is cheapest and
// drives the circuit breaker.
const (
	DefaultInventoryInterval = 60
	DefaultMetricsInterval   = 60
	DefaultHealthInterval    = 30
)

// Circuit breaker policy (docs/10-sync-engine.md §10.5). After this many
// consecutive failures a platform stops being polled until a probe succeeds, so
// one dead cluster cannot generate a retry storm.
const (
	BreakerFailureThreshold = 3
	BreakerCooldown         = 5 * time.Minute
)

// Deleted-asset policy: an asset must be absent this many consecutive runs
// before it is treated as gone. One miss is an API hiccup; three is a deletion.
const MissingThreshold = 3

// Domain errors.
var (
	ErrInvalidPlatform = errors.New("inventory: invalid platform")
	ErrPlatformDeleted = errors.New("inventory: platform is deleted")
)

// Health mirrors the connector's health states.
type Health string

const (
	HealthUnknown     Health = "unknown"
	HealthHealthy     Health = "healthy"
	HealthDegraded    Health = "degraded"
	HealthUnreachable Health = "unreachable"
)

// SyncState tracks whether an asset is still present on its platform.
type SyncState string

const (
	SyncActive  SyncState = "active"
	SyncMissing SyncState = "missing"
	SyncDeleted SyncState = "deleted"
)

// Platform is a connected cluster or endpoint.
type Platform struct {
	ID              uuid.UUID
	Name            string
	Type            string
	EndpointURL     string
	Datacenter      string
	IsEnabled       bool
	TLSMode         string
	TLSCAPEM        string
	TLSFingerprint  string
	Config          map[string]any
	SyncIntervals   SyncIntervals
	Health          Health
	HealthDetail    string
	DetectedVersion string
	LastSeenAt      time.Time

	ConsecutiveFailures int
	BreakerOpenUntil    time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

// SyncIntervals holds the per-kind cadence in seconds.
type SyncIntervals struct {
	Inventory int `json:"inventory"`
	Metrics   int `json:"metrics"`
	Health    int `json:"health"`
}

// DefaultSyncIntervals returns the shipped cadence.
func DefaultSyncIntervals() SyncIntervals {
	return SyncIntervals{
		Inventory: DefaultInventoryInterval,
		Metrics:   DefaultMetricsInterval,
		Health:    DefaultHealthInterval,
	}
}

// Validate checks the invariants that must hold for any platform.
func (p *Platform) Validate() error {
	if p.Name == "" {
		return errors.New("inventory: platform name is required")
	}
	if p.Type == "" {
		return errors.New("inventory: platform type is required")
	}
	if p.EndpointURL == "" {
		return errors.New("inventory: platform endpoint is required")
	}
	switch p.TLSMode {
	case "", "verify", "custom_ca", "fingerprint", "insecure":
	default:
		return errors.New("inventory: unknown TLS mode " + p.TLSMode)
	}
	return nil
}

// IsDeleted reports whether the platform has been soft-deleted.
func (p *Platform) IsDeleted() bool { return !p.DeletedAt.IsZero() }

// BreakerOpen reports whether polling is currently suppressed.
func (p *Platform) BreakerOpen(now time.Time) bool {
	return !p.BreakerOpenUntil.IsZero() && now.Before(p.BreakerOpenUntil)
}

// ShouldSync reports whether a scheduled run should proceed. A disabled or
// deleted platform is skipped outright; an open breaker suppresses work until
// its cooldown elapses.
func (p *Platform) ShouldSync(now time.Time) bool {
	return p.IsEnabled && !p.IsDeleted() && !p.BreakerOpen(now)
}

// RecordSyncFailure advances the breaker. It reports whether this failure
// opened it, which the caller turns into a sync_failure event.
func (p *Platform) RecordSyncFailure(now time.Time, detail string) (opened bool) {
	p.ConsecutiveFailures++
	p.HealthDetail = detail
	if p.Health != HealthDegraded {
		p.Health = HealthUnreachable
	}
	if p.ConsecutiveFailures >= BreakerFailureThreshold && !p.BreakerOpen(now) {
		p.BreakerOpenUntil = now.Add(BreakerCooldown)
		return true
	}
	return false
}

// RecordSyncSuccess closes the breaker and clears failure state. It reports
// whether this recovered a previously failing platform, which becomes a
// recovery notification.
func (p *Platform) RecordSyncSuccess(now time.Time) (recovered bool) {
	recovered = p.ConsecutiveFailures >= BreakerFailureThreshold || p.Health == HealthUnreachable
	p.ConsecutiveFailures = 0
	p.BreakerOpenUntil = time.Time{}
	p.Health = HealthHealthy
	p.HealthDetail = ""
	p.LastSeenAt = now
	return recovered
}
