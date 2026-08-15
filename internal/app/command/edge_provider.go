package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// AuditCategoryEdge labels edge-configuration changes in the audit trail.
// Separate from platform management: these change what the outside world can
// reach, which is a different question to answer at audit time.
const AuditCategoryEdge = "edge"

// EdgeProviderFactory builds a provider from a credential. Injected so the
// command layer neither imports a concrete provider nor knows how one is
// constructed — the layer rule, and what makes these commands testable with a
// fake.
type EdgeProviderFactory func(creds edge.Credentials) (EdgeProvider, error)

// EdgeProvider is the slice of the edge port these commands need. Narrower
// than edge.Provider on purpose: nothing here writes, so nothing here should
// be able to.
type EdgeProvider interface {
	Verify(ctx context.Context) (edge.Health, error)
	Tunnels(ctx context.Context) ([]edge.Tunnel, error)
	Close() error
}

// EdgeDeps are what every edge command needs.
type EdgeDeps struct {
	Providers ports.EdgeProviderRepository
	// Vault seals the token for storage and opens it for one call. Held as
	// the concrete type to match how platform credentials are handled;
	// abstracting it here alone would leave two idioms for one job.
	Vault   *crypto.Vault
	Factory EdgeProviderFactory
	Audit   ports.AuditWriter
	Clock   ports.Clock
}

// TestEdgeCredentialInput checks a candidate credential before it is stored.
type TestEdgeCredentialInput struct {
	Actor     Actor
	AccountID string
	Token     string
}

// TestEdgeCredential verifies a credential without saving anything.
//
// It exists as its own command because the administrator needs the answer
// before committing: a token is shown once by Cloudflare, and finding out it
// was wrong after saving means going back for another one.
type TestEdgeCredential struct{ EdgeDeps }

// Handle runs the verification.
func (h *TestEdgeCredential) Handle(ctx context.Context, in TestEdgeCredentialInput) (edge.Health, error) {
	provider, err := h.Factory(edge.Credentials{
		Token:     strings.TrimSpace(in.Token),
		AccountID: strings.TrimSpace(in.AccountID),
	})
	if err != nil {
		return edge.Health{}, err
	}
	defer func() { _ = provider.Close() }()

	health, err := provider.Verify(ctx)

	// Audited whether or not it succeeded, and without the token: a failed
	// credential test is exactly the event worth having a record of, and the
	// account id is an identifier rather than a secret.
	h.audit(ctx, in.Actor, "edge_credential_tested", uuid.Nil, in.AccountID,
		outcomeOf(err), map[string]any{
			"reachable":     health.Reachable,
			"authenticated": health.Authenticated,
			"tunnels":       len(health.Tunnels),
			"missing_scopes": func() []string {
				names := make([]string, 0, len(health.MissingScopes))
				for _, g := range health.MissingScopes {
					names = append(names, g.Scope)
				}
				return names
			}(),
		})
	return health, err
}

// RegisterEdgeProviderInput is an administrator adding a provider.
type RegisterEdgeProviderInput struct {
	Actor          Actor
	Name           string
	AccountID      string
	Token          string
	TunnelID       string
	TunnelName     string
	AllowedZoneIDs []string
}

// RegisterEdgeProvider stores a provider and its sealed credential.
type RegisterEdgeProvider struct{ EdgeDeps }

// Handle validates, verifies and stores.
func (h *RegisterEdgeProvider) Handle(ctx context.Context, in RegisterEdgeProviderInput) (*publish.Provider, error) {
	now := h.Clock.Now()
	p := &publish.Provider{
		ID:   uuid.New(),
		Name: strings.TrimSpace(in.Name),
		Kind: publish.KindCloudflareTunnel,

		AccountID:      strings.TrimSpace(in.AccountID),
		TunnelID:       strings.TrimSpace(in.TunnelID),
		TunnelName:     strings.TrimSpace(in.TunnelName),
		AllowedZoneIDs: in.AllowedZoneIDs,

		IsEnabled: true,
		Health:    publish.HealthUnknown,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Token) == "" {
		return nil, fmt.Errorf("%w: an API token is required", publish.ErrInvalidProvider)
	}

	// Verified before it is stored. A provider whose credential does not work
	// is worse than no provider: it looks configured, and every later failure
	// has to be traced back to a credential nobody checked.
	provider, err := h.Factory(edge.Credentials{Token: in.Token, AccountID: p.AccountID})
	if err != nil {
		return nil, err
	}
	health, verifyErr := provider.Verify(ctx)
	_ = provider.Close()
	if verifyErr != nil {
		h.audit(ctx, in.Actor, "edge_provider_created", p.ID, p.Name,
			ports.OutcomeFailure, map[string]any{"error": verifyErr.Error()})
		return nil, verifyErr
	}

	// A chosen tunnel must be one the API can actually configure. Storing a
	// locally-managed tunnel would produce a provider that accepts every write
	// and changes nothing.
	if p.TunnelID != "" {
		if err := checkTunnelManageable(health.Tunnels, p.TunnelID); err != nil {
			return nil, err
		}
	}

	sealedSecret, err := h.Vault.Seal(in.Token)
	if err != nil {
		return nil, fmt.Errorf("seal edge credential: %w", err)
	}
	sealed := ports.SealedCredential{Kind: "api_token", Sealed: sealedSecret}

	if err := h.Providers.Create(ctx, p, sealed); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}

	h.audit(ctx, in.Actor, "edge_provider_created", p.ID, p.Name, ports.OutcomeSuccess,
		map[string]any{
			"account_id":  p.AccountID,
			"tunnel_id":   p.TunnelID,
			"tunnel_name": p.TunnelName,
			"zones":       len(p.AllowedZoneIDs),
		})
	return p, nil
}

// checkTunnelManageable refuses a tunnel the API cannot configure.
func checkTunnelManageable(tunnels []edge.Tunnel, tunnelID string) error {
	for _, t := range tunnels {
		if t.ID != tunnelID {
			continue
		}
		if !t.Manageable() {
			return edge.Errorf(edge.ErrNotManageable, "select_tunnel",
				"tunnel %q is locally managed, so its configuration lives in a file on its host and the API cannot change it", t.Name)
		}
		return nil
	}
	return edge.Errorf(edge.ErrInvalidConfig, "select_tunnel",
		"no tunnel with id %q is visible to this credential", tunnelID)
}

// ListEdgeTunnels reports the tunnels a stored provider can see.
//
// Read live rather than from the database: which tunnels exist, and whether
// each is connected, is Cloudflare's to answer and changes without the portal
// being told.
type ListEdgeTunnels struct{ EdgeDeps }

// Handle lists the tunnels.
func (h *ListEdgeTunnels) Handle(ctx context.Context, providerID uuid.UUID) ([]edge.Tunnel, error) {
	provider, _, err := h.connect(ctx, providerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = provider.Close() }()
	return provider.Tunnels(ctx)
}

// VerifyEdgeProvider re-runs the connection test against a stored provider and
// records the result as its health.
type VerifyEdgeProvider struct{ EdgeDeps }

// Handle verifies and records.
func (h *VerifyEdgeProvider) Handle(ctx context.Context, providerID uuid.UUID) (edge.Health, error) {
	provider, stored, err := h.connect(ctx, providerID)
	if err != nil {
		return edge.Health{}, err
	}
	defer func() { _ = provider.Close() }()

	health, verifyErr := provider.Verify(ctx)
	h.recordHealth(ctx, stored, health, verifyErr)
	return health, verifyErr
}

// recordHealth translates a verification into stored health and breaker state.
func (h *VerifyEdgeProvider) recordHealth(ctx context.Context, p *publish.Provider,
	health edge.Health, err error) {
	state, detail, failures := publish.HealthHealthy, "", 0

	switch {
	case err != nil:
		state, detail = publish.HealthUnreachable, err.Error()
		if !health.Reachable {
			detail = "could not reach Cloudflare: " + detail
		}
		failures = p.ConsecutiveFailures + 1
	case len(health.MissingScopes) > 0:
		// Reachable and working, but not for everything. Degraded rather than
		// unreachable: calling it unreachable would send someone to debug a
		// network when the answer is a permission.
		names := make([]string, 0, len(health.MissingScopes))
		for _, g := range health.MissingScopes {
			names = append(names, g.Scope)
		}
		state = publish.HealthDegraded
		detail = "missing permissions: " + strings.Join(names, ", ")
	}

	var breakerUntil time.Time
	if failures >= 3 {
		// Same shape as the platform breaker: back off rather than hammering
		// a control plane that is already unhappy.
		breakerUntil = h.Clock.Now().Add(time.Duration(failures) * time.Minute)
	}
	_ = h.Providers.RecordHealth(ctx, p.ID, state, detail, failures, breakerUntil)
}

// connect loads a stored provider and opens its credential for one call.
func (h *EdgeDeps) connect(ctx context.Context, providerID uuid.UUID) (EdgeProvider, *publish.Provider, error) {
	stored, err := h.Providers.Get(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	if !stored.IsEnabled {
		return nil, nil, fmt.Errorf("%w: the provider is disabled", publish.ErrInvalidProvider)
	}
	if stored.BreakerOpen(h.Clock.Now()) {
		return nil, nil, edge.Errorf(edge.ErrUnreachable, "connect",
			"calls to %s are suspended until %s after repeated failures",
			stored.Name, stored.BreakerOpenUntil.Format(time.RFC3339))
	}

	cred, err := h.Providers.Credential(ctx, providerID, h.Vault)
	if err != nil {
		return nil, nil, err
	}
	provider, err := h.Factory(edge.Credentials{Token: cred.Secret, AccountID: stored.AccountID})
	if err != nil {
		return nil, nil, err
	}
	return provider, stored, nil
}

func (h *EdgeDeps) audit(ctx context.Context, actor Actor, action string,
	targetID uuid.UUID, targetName, outcome string, details map[string]any) {
	target := ""
	if targetID != uuid.Nil {
		target = targetID.String()
	}
	actorID := actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: AuditCategoryEdge, Action: action,
		TargetType: "edge_provider", TargetID: target, TargetName: targetName,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: outcome, Details: details,
	})
}

func outcomeOf(err error) string {
	if err != nil {
		return ports.OutcomeFailure
	}
	return ports.OutcomeSuccess
}
