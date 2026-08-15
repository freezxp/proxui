package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// EdgeWriter is the write half of the edge port.
//
// Kept as its own interface, separate from the reader, so that the packages
// which only read cannot be handed something that writes by accident.
type EdgeWriter interface {
	EdgeIngressReader
	ReplaceIngress(ctx context.Context, tunnelID string, cfg edge.Config) error
	Zones(ctx context.Context) ([]edge.Zone, error)
	FindDNSRecord(ctx context.Context, zoneID, name string) (edge.DNSRecord, bool, error)
	CreateTunnelDNS(ctx context.Context, zoneID, hostname, tunnelID string) (edge.DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
}

// EdgeWriterFactory opens a writer for a stored provider, by id.
type EdgeWriterFactory func(ctx context.Context, providerID uuid.UUID) (EdgeWriter, error)

// PublishDeps are what the write commands need.
type PublishDeps struct {
	Providers ports.EdgeProviderRepository
	Apps      ports.PublishedAppRepository
	Factory   EdgeWriterFactory
	Audit     ports.AuditWriter
	Clock     ports.Clock
	// SelfHostname is the portal's own name, from configuration. Empty
	// disables the self-protection, which is correct only when the portal is
	// not published through the provider being changed.
	SelfHostname string
}

// PublishAppInput is an administrator publishing a service.
type PublishAppInput struct {
	Actor      Actor
	ProviderID uuid.UUID

	Hostname string
	Path     string

	// Either a resolved target...
	ServiceURL string
	// ...or a machine from the inventory plus a port, which the caller
	// resolves to an address before getting here.
	VMID    *uuid.UUID
	VMPort  int
	Scheme  string
	Address string

	OriginRequest map[string]any

	// AcknowledgeExposure confirms the caller understands this hostname will
	// be reachable by anyone on the internet (PUB-43). Required, because it is
	// the most consequential thing this feature does.
	AcknowledgeExposure bool

	// ReadVersion is the version the caller last saw. Supplying it makes the
	// write refuse rather than clobber a concurrent change (PUB-31).
	ReadVersion int
}

// PublishApp creates a routing rule and the DNS record that reaches it.
type PublishApp struct{ PublishDeps }

// Handle publishes.
//
// The order is the safety property. Read fresh, check nothing moved, validate
// the whole resulting table, snapshot what is there, write the ingress rule,
// then write DNS — and if DNS fails, put the ingress back. Ingress before DNS
// because the half-done states are not equally bad: a rule with no DNS record
// is invisible and harmless, while a DNS record with no rule serves Cloudflare
// error 1033 to anyone who visits.
func (h *PublishApp) Handle(ctx context.Context, in PublishAppInput) (*publish.App, error) {
	if !in.AcknowledgeExposure {
		return nil, publish.ErrExposureNotAcknowledged
	}

	provider, err := h.Providers.Get(ctx, in.ProviderID)
	if err != nil {
		return nil, err
	}
	if !provider.Ready() {
		return nil, publish.ErrNoTunnel
	}

	now := h.Clock.Now()
	app := &publish.App{
		ID: uuid.New(), ProviderID: provider.ID,
		Hostname: strings.ToLower(strings.TrimSpace(in.Hostname)),
		Path:     strings.TrimSpace(in.Path),
		VMID:     in.VMID, VMPort: in.VMPort,
		OriginRequest: in.OriginRequest,
		IsEnabled:     true,
		CreatedAt:     now, UpdatedAt: now,
	}
	app.ServiceURL = strings.TrimSpace(in.ServiceURL)
	if app.ServiceURL == "" {
		app.ServiceURL = publish.TargetFor(in.Scheme, in.Address, in.VMPort)
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	if ackBy := in.Actor.UserID; ackBy != uuid.Nil {
		app.ExposureAckBy, app.ExposureAckAt = &ackBy, now
	}

	writer, err := h.Factory(ctx, in.ProviderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = writer.Close() }()

	// The zone must be one the administrator allowed. DNS:Edit reaches a whole
	// zone, so this list is the real write boundary and is checked before
	// anything is sent (PUB-04).
	zoneID, err := h.resolveZone(ctx, writer, provider, app.Hostname)
	if err != nil {
		return nil, err
	}

	current, cfg, err := readTable(ctx, writer, provider.TunnelID)
	if err != nil {
		return nil, err
	}
	if err := publish.CheckFresh(in.ReadVersion, cfg.Version); err != nil {
		return nil, err
	}

	desired := publish.ApplyTo(current, app)
	plan := publish.BuildPlan(current, desired, h.SelfHostname)
	if !plan.Safe() {
		return nil, plan.Refusal
	}
	if !plan.TouchesAnything() {
		return nil, fmt.Errorf("%w: that hostname already routes there", ports.ErrConflict)
	}

	// Snapshot before the write, never after. This is the revert path, and a
	// snapshot taken afterwards would record the mistake rather than the state
	// to go back to (PUB-34).
	if err := h.snapshot(ctx, provider, cfg, in.Actor); err != nil {
		return nil, err
	}

	if err := writer.ReplaceIngress(ctx, provider.TunnelID, toEdgeConfig(desired, cfg)); err != nil {
		h.auditApp(ctx, in.Actor, "app_published", app, ports.OutcomeFailure,
			map[string]any{"error": err.Error(), "stage": "ingress"})
		return nil, err
	}

	// From here the ingress rule is live. Anything that fails has to put it
	// back rather than leave the table half-changed.
	record, found, err := writer.FindDNSRecord(ctx, zoneID, app.Hostname)
	switch {
	case err != nil:
		return nil, h.rollback(ctx, writer, provider, cfg, app, in.Actor, err, "dns_lookup")
	case found && !strings.Contains(record.Content, "cfargotunnel.com"):
		// Somebody else's record on this name. Overwriting it is not this
		// feature's business, and doing so silently would be worse.
		return nil, h.rollback(ctx, writer, provider, cfg, app, in.Actor,
			fmt.Errorf("%w: %s already has a DNS record pointing at %s",
				ports.ErrConflict, app.Hostname, record.Content), "dns_conflict")
	case found:
		// Already points at a tunnel. Adopted rather than recreated, and not
		// recorded as ours, so unpublishing will leave it alone.
		app.DNSZoneID = zoneID
	default:
		created, err := writer.CreateTunnelDNS(ctx, zoneID, app.Hostname, provider.TunnelID)
		if err != nil {
			return nil, h.rollback(ctx, writer, provider, cfg, app, in.Actor, err, "dns_create")
		}
		app.DNSZoneID, app.DNSRecordID = zoneID, created.ID
	}

	app.LastAppliedAt = now
	if err := h.Apps.Create(ctx, app); err != nil {
		// The provider is now correct and the portal cannot record it. Rolling
		// back is the honest response: a rule the portal does not know about
		// is one it will delete on the next full write.
		return nil, h.rollback(ctx, writer, provider, cfg, app, in.Actor, err, "store")
	}

	h.auditApp(ctx, in.Actor, "app_published", app, ports.OutcomeSuccess, map[string]any{
		"service":       app.ServiceURL,
		"dns_record_id": app.DNSRecordID,
		"tunnel_id":     provider.TunnelID,
		"added":         plan.Added,
		"modified":      plan.Modified,
	})
	return app, nil
}

// UnpublishApp removes a rule and the DNS record the portal created for it.
type UnpublishApp struct{ PublishDeps }

// Handle unpublishes.
func (h *UnpublishApp) Handle(ctx context.Context, appID uuid.UUID, actor Actor, readVersion int) error {
	app, err := h.Apps.Get(ctx, appID)
	if err != nil {
		return err
	}
	provider, err := h.Providers.Get(ctx, app.ProviderID)
	if err != nil {
		return err
	}
	if !provider.Ready() {
		return publish.ErrNoTunnel
	}

	writer, err := h.Factory(ctx, provider.ID)
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()

	current, cfg, err := readTable(ctx, writer, provider.TunnelID)
	if err != nil {
		return err
	}
	if err := publish.CheckFresh(readVersion, cfg.Version); err != nil {
		return err
	}

	desired := publish.RemoveFrom(current, app)
	plan := publish.BuildPlan(current, desired, h.SelfHostname)
	if !plan.Safe() {
		return plan.Refusal
	}

	if err := h.snapshot(ctx, provider, cfg, actor); err != nil {
		return err
	}
	if err := writer.ReplaceIngress(ctx, provider.TunnelID, toEdgeConfig(desired, cfg)); err != nil {
		h.auditApp(ctx, actor, "app_unpublished", app, ports.OutcomeFailure,
			map[string]any{"error": err.Error(), "stage": "ingress"})
		return err
	}

	// Only the record the portal created. A CNAME somebody else made on the
	// same name is not ours to delete, and DNSRecordID is empty in that case
	// precisely so this cannot happen (PUB-23).
	if app.DNSRecordID != "" && app.DNSZoneID != "" {
		if err := writer.DeleteDNSRecord(ctx, app.DNSZoneID, app.DNSRecordID); err != nil {
			// The rule is gone, so nothing is being served; a leftover DNS
			// record resolves to a tunnel that will 1033. Reported rather than
			// rolled back: putting the rule back to tidy up DNS would republish
			// something the administrator asked to remove.
			h.auditApp(ctx, actor, "app_unpublished", app, ports.OutcomeFailure,
				map[string]any{"error": err.Error(), "stage": "dns_delete",
					"note": "the routing rule was removed; the DNS record remains"})
			return err
		}
	}

	if err := h.Apps.Delete(ctx, appID, h.Clock.Now()); err != nil {
		return err
	}
	h.auditApp(ctx, actor, "app_unpublished", app, ports.OutcomeSuccess,
		map[string]any{"removed": plan.Removed, "tunnel_id": provider.TunnelID})
	return nil
}

// --- shared machinery ----------------------------------------------------

// resolveZone finds the allowed zone a hostname belongs to.
func (h *PublishDeps) resolveZone(ctx context.Context, writer EdgeWriter,
	provider *publish.Provider, hostname string) (string, error) {
	zones, err := writer.Zones(ctx)
	if err != nil {
		return "", err
	}

	names := make([]string, 0, len(zones))
	byName := map[string]edge.Zone{}
	for _, z := range zones {
		names = append(names, z.Name)
		byName[strings.ToLower(z.Name)] = z
	}

	match := publish.MatchZone(hostname, names)
	if match == "" {
		return "", fmt.Errorf("%w: no zone this credential can see covers %s",
			publish.ErrZoneNotAllowed, hostname)
	}
	zone := byName[match]
	if !provider.AllowsZone(zone.ID) {
		return "", fmt.Errorf("%w: %s is in zone %s, which this provider is not allowed to write to",
			publish.ErrZoneNotAllowed, hostname, zone.Name)
	}
	return zone.ID, nil
}

// rollback puts the routing table back and returns the original error.
func (h *PublishDeps) rollback(ctx context.Context, writer EdgeWriter, provider *publish.Provider,
	before edge.Config, app *publish.App, actor Actor, cause error, stage string) error {
	details := map[string]any{"error": cause.Error(), "stage": stage}

	if err := writer.ReplaceIngress(ctx, provider.TunnelID, before); err != nil {
		// Both the change and the undo failed. This is the case the snapshot
		// and runbook 24.5 exist for, and it must be unmistakable in the log.
		details["rollback_error"] = err.Error()
		details["note"] = "the routing table could not be restored; see runbook 24.5"
		h.auditApp(ctx, actor, "app_published", app, ports.OutcomeFailure, details)
		return fmt.Errorf("%w (and the routing table could not be rolled back: %v)", cause, err)
	}

	details["rolled_back"] = true
	h.auditApp(ctx, actor, "app_published", app, ports.OutcomeFailure, details)
	return cause
}

func (h *PublishDeps) snapshot(ctx context.Context, provider *publish.Provider,
	cfg edge.Config, actor Actor) error {
	raw, err := json.Marshal(cfg.Rules)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	var by *uuid.UUID
	if actor.UserID != uuid.Nil {
		id := actor.UserID
		by = &id
	}
	return h.Providers.SaveSnapshot(ctx, provider.ID, provider.TunnelID, cfg.Version, raw, by)
}

func (h *PublishDeps) auditApp(ctx context.Context, actor Actor, action string,
	app *publish.App, outcome string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["hostname"] = app.Hostname
	if app.Path != "" {
		details["path"] = app.Path
	}
	actorID := actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: AuditCategoryEdge, Action: action,
		TargetType: "published_app", TargetID: app.ID.String(), TargetName: app.Hostname,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: outcome, Details: details,
	})
}

// readTable reads the live routing table. Always fresh: a cached one is the
// exact ingredient of a write that deletes somebody else's rule (PUB-30).
func readTable(ctx context.Context, reader EdgeIngressReader, tunnelID string) (publish.Table, edge.Config, error) {
	cfg, err := reader.Ingress(ctx, tunnelID)
	if err != nil {
		return nil, edge.Config{}, err
	}
	table := make(publish.Table, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		table = append(table, publish.Rule{Hostname: r.Hostname, Path: r.Path, Service: r.Service})
	}
	return table, cfg, nil
}

// toEdgeConfig turns a domain table back into a provider configuration,
// carrying across the per-rule settings the portal does not interpret.
func toEdgeConfig(table publish.Table, from edge.Config) edge.Config {
	origins := map[string]map[string]any{}
	for _, r := range from.Rules {
		origins[strings.ToLower(r.Hostname)+"\x00"+r.Path] = r.Origin
	}

	out := edge.Config{Version: from.Version, Origin: from.Origin}
	for _, r := range table {
		out.Rules = append(out.Rules, edge.Rule{
			Hostname: r.Hostname, Path: r.Path, Service: r.Service,
			Origin: origins[strings.ToLower(r.Hostname)+"\x00"+r.Path],
		})
	}
	return out
}
