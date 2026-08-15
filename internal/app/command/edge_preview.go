package command

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// EdgeIngressReader is the read half of the edge port. Declared here rather
// than reusing the writer so that nothing in this file could write even if
// someone wanted it to.
type EdgeIngressReader interface {
	Ingress(ctx context.Context, tunnelID string) (edge.Config, error)
	Close() error
}

// EdgeReaderFactory opens a reader for a stored provider, by id. The id rather
// than a credential, so unsealing the token stays in the composition root.
type EdgeReaderFactory func(ctx context.Context, providerID uuid.UUID) (EdgeIngressReader, error)

// EdgeSafetyDeps are what the safety commands need.
type EdgeSafetyDeps struct {
	Providers ports.EdgeProviderRepository
	Factory   EdgeReaderFactory
	Audit     ports.AuditWriter
	Clock     ports.Clock
	// SelfHostname is the name this portal is served at, from configuration.
	// Empty disables the self-protection, which is correct for a portal that
	// is not published through the provider being edited.
	SelfHostname string
}

// SnapshotEdgeIngress reads a routing table and stores it (PUB-34).
//
// This is the revert path, and it is a database write rather than a tunnel
// write — nothing it does can affect traffic. It exists before anything can
// change a tunnel because the moment a snapshot is needed is the moment nobody
// can reach the portal to start taking them.
type SnapshotEdgeIngress struct{ EdgeSafetyDeps }

// SnapshotResult describes what was captured.
type SnapshotResult struct {
	TunnelID string
	Version  int
	Rules    int
}

// Handle reads the live table and stores it.
func (h *SnapshotEdgeIngress) Handle(ctx context.Context, providerID uuid.UUID, actor Actor) (SnapshotResult, error) {
	provider, cfg, err := h.readLive(ctx, providerID)
	if err != nil {
		return SnapshotResult{}, err
	}

	// Stored as the raw rule array rather than a decoded type: its only job is
	// to be written back verbatim, and decoding into a shape the portal
	// understands would drop any field the portal does not — which is the one
	// thing a restore must never do.
	raw, err := json.Marshal(cfg.Rules)
	if err != nil {
		return SnapshotResult{}, fmt.Errorf("encode snapshot: %w", err)
	}

	var takenBy *uuid.UUID
	if actor.UserID != uuid.Nil {
		id := actor.UserID
		takenBy = &id
	}
	if err := h.Providers.SaveSnapshot(ctx, providerID, provider.TunnelID, cfg.Version, raw, takenBy); err != nil {
		return SnapshotResult{}, err
	}

	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryEdge, "edge_snapshot_taken",
		"edge_provider", providerID.String(), provider.Name,
		map[string]any{"tunnel_id": provider.TunnelID, "version": cfg.Version, "rules": len(cfg.Rules)})

	return SnapshotResult{TunnelID: provider.TunnelID, Version: cfg.Version, Rules: len(cfg.Rules)}, nil
}

// PreviewEdgeIngressInput is a proposed routing table.
type PreviewEdgeIngressInput struct {
	Actor Actor
	// Desired is the whole table the caller wants, in order. Whole rather than
	// a patch because the provider's API replaces the whole array, and a
	// preview that did not work the same way would be previewing something
	// other than what happens.
	Desired []publish.Rule
	// ReadVersion is the version the caller last saw, if any. Supplying it
	// turns the preview into a staleness check as well (PUB-31).
	ReadVersion int
}

// PreviewEdgeIngress says what a proposed change would do, and whether it
// would be refused. It writes nothing, anywhere.
type PreviewEdgeIngress struct{ EdgeSafetyDeps }

// PreviewResult is the diff plus the current state it was computed against.
type PreviewResult struct {
	Plan           publish.Plan
	CurrentVersion int
	Current        publish.Table
	// Stale is set when the table moved since the caller read it. The plan is
	// still returned: the caller needs to see both what it wanted and what
	// changed underneath it.
	Stale error
}

// Handle computes the diff.
func (h *PreviewEdgeIngress) Handle(ctx context.Context, providerID uuid.UUID,
	in PreviewEdgeIngressInput) (PreviewResult, error) {
	_, cfg, err := h.readLive(ctx, providerID)
	if err != nil {
		return PreviewResult{}, err
	}

	current := make(publish.Table, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		current = append(current, publish.Rule{Hostname: r.Hostname, Path: r.Path, Service: r.Service})
	}

	result := PreviewResult{
		CurrentVersion: cfg.Version,
		Current:        current,
		Plan:           publish.BuildPlan(current, in.Desired, h.SelfHostname),
		Stale:          publish.CheckFresh(in.ReadVersion, cfg.Version),
	}
	return result, nil
}

// readLive loads a provider and its current routing table.
func (h *EdgeSafetyDeps) readLive(ctx context.Context, providerID uuid.UUID) (*publish.Provider, edge.Config, error) {
	provider, err := h.Providers.Get(ctx, providerID)
	if err != nil {
		return nil, edge.Config{}, err
	}
	if provider.TunnelID == "" {
		return nil, edge.Config{}, publish.ErrNoTunnel
	}
	if !provider.IsEnabled {
		return nil, edge.Config{}, fmt.Errorf("%w: the provider is disabled", publish.ErrInvalidProvider)
	}

	reader, err := h.Factory(ctx, providerID)
	if err != nil {
		return nil, edge.Config{}, err
	}
	defer func() { _ = reader.Close() }()

	// Always read fresh. A cached routing table is the exact ingredient of a
	// write that deletes somebody else's rule (PUB-30).
	cfg, err := reader.Ingress(ctx, provider.TunnelID)
	if err != nil {
		return nil, edge.Config{}, err
	}
	return provider, cfg, nil
}
