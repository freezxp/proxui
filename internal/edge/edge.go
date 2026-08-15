// Package edge is the port every edge-configuration integration implements.
//
// It is a sibling to internal/connector, not a part of it. A connector answers
// questions about virtual machines; an edge provider publishes a service to
// the internet by editing a routing table and a DNS zone. They share a shape —
// typed error classes, small capability interfaces, a fake for tests — and no
// contract at all (ADR 0004).
//
// The whole package is deliberately small. There is one implementation today,
// and an interface designed against a single example is an interface shaped
// like that example wearing a general name. It is kept narrow so a second
// provider can reshape it honestly rather than inheriting Cloudflare's ideas.
package edge

import (
	"context"
	"time"
)

// Capability names an optional behaviour. The core asks for capabilities
// rather than type-switching on a provider name, so the UI can hide what a
// provider cannot do.
type Capability string

const (
	// CapabilityIngress is reading and writing hostname routing rules.
	CapabilityIngress Capability = "ingress"
	// CapabilityDNS is creating the record that points a name at the tunnel.
	// Separate because they are separate writes to separate systems, and a
	// provider might plausibly do one and not the other.
	CapabilityDNS Capability = "dns"
	// CapabilityAccess is reading which hostnames sit behind an identity
	// check. Read-only by decision — see docs/28 §28.7.
	CapabilityAccess Capability = "access"
)

// Credentials are what a provider authenticates with. Held in memory only for
// the life of a call; the stored form is sealed with the master key.
type Credentials struct {
	// Token is an API token. Never logged, never returned by any read.
	Token string
	// AccountID scopes account-level calls.
	AccountID string
}

// Options tune a provider's HTTP behaviour.
type Options struct {
	Timeout        time.Duration
	RequestsPerSec float64
}

// Tunnel is one connector endpoint on the provider.
type Tunnel struct {
	ID   string
	Name string
	// RemotelyManaged reports whether this tunnel's configuration can be
	// written through the API at all. Cloudflare calls it config_src: a
	// locally-managed tunnel reads a file on its host and ignores every write
	// we make. Surfacing it is not a nicety — it is the difference between
	// "this will work" and "this will silently do nothing".
	RemotelyManaged bool
	// Connections is how many cloudflared instances are currently connected.
	// Zero means a correct rule will still not serve traffic, which is
	// otherwise indistinguishable from a wrong rule.
	Connections int
	CreatedAt   time.Time
	DeletedAt   *time.Time
}

// Active reports whether the tunnel could serve traffic right now.
func (t Tunnel) Active() bool { return t.DeletedAt == nil && t.Connections > 0 }

// Manageable reports whether this portal can configure the tunnel.
func (t Tunnel) Manageable() bool { return t.DeletedAt == nil && t.RemotelyManaged }

// Rule is one hostname routing rule.
//
// Order is significant: rules match top to bottom, first match wins, and the
// last must be a catch-all with no hostname. Callers must treat a []Rule as a
// sequence, never as a set.
type Rule struct {
	Hostname string
	Path     string
	// Service is where matching traffic goes — "http://10.0.30.20:8080", or a
	// literal like "http_status:404" for the catch-all.
	Service string
	// Origin carries provider-specific per-rule settings, passed through
	// unread so that a rule this portal did not create survives a rewrite
	// byte-for-byte.
	Origin map[string]any
}

// IsCatchAll reports whether this rule matches everything left over. It is
// identified by having no hostname, which is the provider's own rule.
func (r Rule) IsCatchAll() bool { return r.Hostname == "" }

// Config is a tunnel's complete routing table, plus whatever the provider
// needs to detect that it changed underneath us.
type Config struct {
	Rules []Rule
	// Version is the provider's own revision marker, echoed back on write so
	// a concurrent edit can be refused rather than silently overwritten. Zero
	// means the provider does not offer one, and the caller must fall back to
	// comparing the rules it read.
	Version int
	// Origin is tunnel-wide default settings, preserved unread.
	Origin map[string]any
}

// Zone is a DNS zone the credential can work with.
type Zone struct {
	ID   string
	Name string
}

// DNSRecord is a record in a zone. Enough of one to tell a record the portal
// created from one somebody else made on the same name, which is what stops
// an unpublish deleting the wrong thing.
type DNSRecord struct {
	ID      string
	Name    string
	Type    string
	Content string
	Proxied bool
}

// Health is what a connection test found.
type Health struct {
	Reachable     bool
	Authenticated bool
	// MissingScopes names each absent permission alongside what it prevents,
	// so the report says what stops working rather than only what is absent.
	MissingScopes []ScopeGap
	Tunnels       []Tunnel
	Warnings      []string
}

// ScopeGap is one missing permission and its consequence.
type ScopeGap struct {
	Scope  string
	Blocks string
}

// Provider is the minimum every edge integration implements.
type Provider interface {
	// Capabilities reports what this provider can do.
	Capabilities() []Capability
	// Verify checks the credential and reports what it can reach. It must not
	// write anything.
	Verify(ctx context.Context) (Health, error)
	// Tunnels lists the connector endpoints on the account.
	Tunnels(ctx context.Context) ([]Tunnel, error)
	// Close releases any resources.
	Close() error
}

// IngressReader reads a tunnel's routing table. Separate from the writer
// because the read-only half of this feature ships first and is useful alone
// (docs/28 §28.11).
type IngressReader interface {
	Ingress(ctx context.Context, tunnelID string) (Config, error)
}

// IngressWriter replaces a tunnel's routing table.
//
// Replace, not amend: the underlying API has no "add one rule", so every write
// carries the whole table and a stale read deletes other people's routes. The
// method is named for what it does so no caller mistakes it for an append.
type IngressWriter interface {
	ReplaceIngress(ctx context.Context, tunnelID string, cfg Config) error
}

// Has reports whether a capability is present.
func Has(p Provider, c Capability) bool {
	for _, got := range p.Capabilities() {
		if got == c {
			return true
		}
	}
	return false
}
