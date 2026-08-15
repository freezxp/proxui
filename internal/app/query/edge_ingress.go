package query

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// IngressReader is the slice of the edge port this query needs. Read-only by
// construction: there is no method here that can change anything.
type IngressReader interface {
	Ingress(ctx context.Context, tunnelID string) (edge.Config, error)
	Close() error
}

// IngressReaderFactory opens a reader for a stored provider, by id.
//
// It takes an id rather than a credential so that opening the sealed token
// stays in the composition root: this package then has no reason to know that
// credentials are encrypted, or that a vault exists. The layer test enforces
// the boundary, and caught the first attempt at this that did not.
type IngressReaderFactory func(ctx context.Context, providerID uuid.UUID) (IngressReader, error)

// MachineLister supplies the inventory side of the join.
type MachineLister interface {
	MachineAddresses(ctx context.Context) ([]publish.MachineRef, error)
}

// EdgeIngress reads a tunnel's routing table and says what each rule is.
//
// The join to the inventory is the reason this panel exists at all: Cloudflare
// can already show a list of hostnames and addresses, and cannot say that
// 10.0.13.10 is the VM called `amp`, or that a rule points at an address no
// machine holds any more.
type EdgeIngress struct {
	Providers ports.EdgeProviderRepository
	Machines  MachineLister
	Factory   IngressReaderFactory
}

// IngressView is a routing table as the API returns it.
type IngressView struct {
	ProviderID uuid.UUID
	TunnelID   string
	TunnelName string
	// Version is the provider's revision marker, carried so a later write can
	// detect that the table changed underneath it (PUB-31).
	Version int
	Rules   []publish.DescribeRule
	// Counts, so the caller does not have to walk the list to summarise it.
	PortalOwned int
	External    int
	Unmatched   int
}

// Handle reads and annotates.
//
// selfHostname is the name the portal is currently being reached at, so its
// own rule can be marked. Taken as an argument rather than configured because
// nothing tells the portal its public name — branding derives it the same way.
//
// Good enough to *display* the protected rule; not good enough to *enforce*
// its protection, because an administrator working from the LAN address would
// pass that instead and the tunnel rule would go unrecognised. The write path
// needs a configured hostname, which is a P3 problem (docs/28, PUB-33).
func (q *EdgeIngress) Handle(ctx context.Context, providerID uuid.UUID, selfHostname string) (IngressView, error) {
	provider, err := q.Providers.Get(ctx, providerID)
	if err != nil {
		return IngressView{}, err
	}
	if provider.TunnelID == "" {
		return IngressView{}, publish.ErrNoTunnel
	}
	if !provider.IsEnabled {
		return IngressView{}, fmt.Errorf("%w: the provider is disabled", publish.ErrInvalidProvider)
	}

	reader, err := q.Factory(ctx, providerID)
	if err != nil {
		return IngressView{}, err
	}
	defer func() { _ = reader.Close() }()

	cfg, err := reader.Ingress(ctx, provider.TunnelID)
	if err != nil {
		return IngressView{}, err
	}

	// The inventory side is best-effort: a routing table is still worth
	// showing when the database cannot answer, just without the join.
	machines, machineErr := q.Machines.MachineAddresses(ctx)
	if machineErr != nil {
		machines = nil
	}

	rules := make([]publish.Rule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		rules = append(rules, publish.Rule{Hostname: r.Hostname, Path: r.Path, Service: r.Service})
	}

	// Nothing is portal-owned yet: published_apps arrives with the write path,
	// so every rule here was put there by someone else and is shown read-only.
	described := publish.Describe(rules, machines, selfHostname, nil)

	view := IngressView{
		ProviderID: provider.ID, TunnelID: provider.TunnelID,
		TunnelName: provider.TunnelName, Version: cfg.Version, Rules: described,
	}
	for _, d := range described {
		switch d.Origin {
		case publish.OriginPortal:
			view.PortalOwned++
		case publish.OriginExternal:
			view.External++
		}
		if d.Unmatched {
			view.Unmatched++
		}
	}
	if machineErr != nil {
		return view, fmt.Errorf("ingress read but the inventory join failed: %w", machineErr)
	}
	return view, nil
}
