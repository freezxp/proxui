package publish

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrInvalidProvider covers a provider that cannot work as configured.
	ErrInvalidProvider = errors.New("publish: invalid edge provider")
	// ErrZoneNotAllowed means a hostname falls outside the zones this
	// provider may write to.
	ErrZoneNotAllowed = errors.New("publish: that zone is not in the provider's allowed list")
	// ErrNoTunnel means the provider has no tunnel selected yet.
	ErrNoTunnel = errors.New("publish: no tunnel has been selected for this provider")
)

// Health mirrors the platform vocabulary so one set of words describes both.
type Health string

const (
	HealthUnknown     Health = "unknown"
	HealthHealthy     Health = "healthy"
	HealthDegraded    Health = "degraded"
	HealthUnreachable Health = "unreachable"
)

// Provider is a registered edge control plane.
//
// It holds no credential: the sealed secret lives beside it and is loaded only
// when a call is about to be made, so the type that gets listed, logged and
// serialised cannot carry a token by accident.
type Provider struct {
	ID   uuid.UUID
	Name string
	Kind string

	AccountID  string
	TunnelID   string
	TunnelName string

	// AllowedZoneIDs is the write boundary (PUB-04). Empty means nothing may
	// be written, which is the correct state for a provider that has been
	// registered but not yet scoped.
	AllowedZoneIDs []string

	IsEnabled bool

	Health       Health
	HealthDetail string
	LastSeenAt   time.Time

	ConsecutiveFailures int
	BreakerOpenUntil    time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

// KindCloudflareTunnel is the only implementation today.
const KindCloudflareTunnel = "cloudflare_tunnel"

// Validate checks what must hold for any provider.
func (p *Provider) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: a name is required", ErrInvalidProvider)
	}
	if len(p.Name) > 100 {
		return fmt.Errorf("%w: the name is longer than 100 characters", ErrInvalidProvider)
	}
	if p.Kind != KindCloudflareTunnel {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidProvider, p.Kind)
	}
	if strings.TrimSpace(p.AccountID) == "" {
		return fmt.Errorf("%w: an account id is required", ErrInvalidProvider)
	}
	return nil
}

// Ready reports whether the provider can be used for a publishing operation.
// Registered is not the same as usable: a provider with a working credential
// and no tunnel chosen is a legitimate intermediate state.
func (p *Provider) Ready() bool {
	return p.IsEnabled && p.DeletedAt.IsZero() && strings.TrimSpace(p.TunnelID) != ""
}

// AllowsZone reports whether the provider may write to a zone.
func (p *Provider) AllowsZone(zoneID string) bool {
	for _, id := range p.AllowedZoneIDs {
		if id == zoneID {
			return true
		}
	}
	return false
}

// BreakerOpen reports whether calls should be suppressed right now.
func (p *Provider) BreakerOpen(now time.Time) bool {
	return !p.BreakerOpenUntil.IsZero() && now.Before(p.BreakerOpenUntil)
}

// ZoneOf returns the registrable domain a hostname belongs to.
//
// A deliberately naive last-two-labels rule, and wrong for a public suffix
// with two labels — `app.example.co.uk` yields `co.uk`. It is used only to
// *suggest* a zone in the UI and to spot an obvious mismatch; the authoritative
// answer is the zone list the provider returns, matched by suffix. Embedding a
// public suffix list to do slightly better at a job that has a correct
// alternative would be the wrong trade.
func ZoneOf(hostname string) string {
	labels := strings.Split(strings.TrimSpace(strings.ToLower(hostname)), ".")
	if len(labels) < 2 {
		return ""
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// MatchZone picks the zone a hostname belongs to from the zones available.
//
// Longest suffix wins, so `intranet.my` beats `my` when both are present, and
// a hostname that matches nothing returns "" rather than guessing. This is the
// authoritative path; ZoneOf is only a hint.
func MatchZone(hostname string, zoneNames []string) string {
	h := strings.ToLower(strings.TrimSpace(hostname))
	best := ""
	for _, z := range zoneNames {
		zone := strings.ToLower(strings.TrimSpace(z))
		if zone == "" {
			continue
		}
		if h != zone && !strings.HasSuffix(h, "."+zone) {
			continue
		}
		if len(zone) > len(best) {
			best = zone
		}
	}
	return best
}
