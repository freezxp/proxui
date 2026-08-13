// Package sync holds the synchronization engine: it turns connector snapshots
// into persisted inventory, detecting what changed and what disappeared.
//
// The model is snapshot-based. Proxmox has no "changed since" API, but it
// returns the whole estate in one cheap call, so at ProxUI's scale a full
// snapshot per cycle costs less than any incremental bookkeeping would
// (docs/10-sync-engine.md §10.1).
package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// Stats counts what a run did, for the sync_runs record and the UI.
type Stats struct {
	Hosts    int `json:"hosts"`
	VMs      int `json:"vms"`
	Storage  int `json:"storage"`
	Networks int `json:"networks"`
	Added    int `json:"added"`
	Changed  int `json:"changed"`
	Missing  int `json:"missing"`
	Deleted  int `json:"deleted"`
	Errors   int `json:"errors"`
}

// Reconciler synchronizes one platform's inventory.
type Reconciler struct {
	Platforms ports.PlatformRepository
	Assets    AssetStore
	Runs      RunStore
	Clock     ports.Clock
	Log       zerolog.Logger
}

// Result reports the outcome of a run.
type Result struct {
	RunID  int64
	Status string
	Stats  Stats
}

// Reconcile runs one inventory synchronization for a platform.
//
// The sequence is: hosts first (VMs reference them), then VMs, then storage and
// networks, then sweep whatever the platform stopped reporting. Each stage is
// tolerant of the others failing, so one unreachable node downgrades the run to
// "partial" instead of losing the whole cluster's inventory.
func (r *Reconciler) Reconcile(ctx context.Context, platform *inventory.Platform, conn connector.Connector, trigger string) (Result, error) {
	now := r.Clock.Now()
	runID, err := r.Runs.StartRun(ctx, platform.ID, "inventory", trigger, now)
	if err != nil {
		return Result{}, err
	}

	var (
		stats  Stats
		result = Result{RunID: runID, Status: "success"}
	)

	// Fetch everything first: a snapshot taken across several calls should be
	// as close to a single instant as possible before any of it is persisted.
	snapshot, fetchErrs := r.fetch(ctx, conn)
	for _, fe := range fetchErrs {
		stats.Errors++
		if recErr := r.Runs.RecordError(ctx, runID, fe.scope, fe.err.Error(), nil); recErr != nil {
			r.Log.Error().Err(recErr).Msg("could not record sync error")
		}
	}

	// A failure to list VMs is fatal for the run: continuing would sweep every
	// VM as missing and, three runs later, delete the entire inventory.
	if snapshot.vmsFailed {
		err := errors.New("inventory listing failed; skipping reconciliation to avoid mass deletion")
		if len(fetchErrs) > 0 {
			err = fmt.Errorf("%s: %w", err, fetchErrs[0].err)
		}
		r.finish(ctx, runID, "failed", stats, err.Error(), now)
		return Result{RunID: runID, Status: "failed", Stats: stats}, err
	}

	tx, err := r.Runs.Begin(ctx)
	if err != nil {
		r.finish(ctx, runID, "failed", stats, err.Error(), now)
		return Result{RunID: runID, Status: "failed"}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	hostIDs, err := r.reconcileHosts(ctx, tx, platform.ID, snapshot.hosts, &stats, now)
	if err != nil {
		r.finish(ctx, runID, "failed", stats, err.Error(), now)
		return Result{RunID: runID, Status: "failed"}, err
	}

	if err := r.reconcileVMs(ctx, tx, platform, snapshot.vms, hostIDs, runID, &stats, now); err != nil {
		r.finish(ctx, runID, "failed", stats, err.Error(), now)
		return Result{RunID: runID, Status: "failed"}, err
	}

	for _, rec := range snapshot.storage {
		if err := r.Assets.UpsertStorage(ctx, tx, platform.ID, hostIDs[rec.HostID], rec, now); err != nil {
			return r.abort(ctx, runID, stats, now, err)
		}
		stats.Storage++
	}
	for _, rec := range snapshot.networks {
		if err := r.Assets.UpsertNetwork(ctx, tx, platform.ID, hostIDs[rec.HostID], rec, now); err != nil {
			return r.abort(ctx, runID, stats, now, err)
		}
		stats.Networks++
	}

	if err := tx.Commit(ctx); err != nil {
		r.finish(ctx, runID, "failed", stats, err.Error(), now)
		return Result{RunID: runID, Status: "failed"}, fmt.Errorf("commit sync: %w", err)
	}

	if stats.Errors > 0 {
		result.Status = "partial"
	}
	result.Stats = stats
	r.finish(ctx, runID, result.Status, stats, "", r.Clock.Now())
	return result, nil
}

type snapshot struct {
	hosts     []connector.HostRecord
	vms       []connector.VMRecord
	storage   []connector.StorageRecord
	networks  []connector.NetworkRecord
	vmsFailed bool
}

type scopedError struct {
	scope string
	err   error
}

// fetch collects every asset kind the connector supports, tolerating partial
// failure so one unsupported or broken collector does not lose the rest.
func (r *Reconciler) fetch(ctx context.Context, conn connector.Connector) (snapshot, []scopedError) {
	var (
		snap snapshot
		errs []scopedError
	)

	if c, ok := conn.(connector.HostCollector); ok {
		hosts, err := c.ListHosts(ctx)
		if err != nil {
			errs = append(errs, scopedError{"hosts", err})
		} else {
			snap.hosts = hosts
		}
	}

	if c, ok := conn.(connector.VirtualMachineCollector); ok {
		vms, err := c.ListVMs(ctx)
		if err != nil {
			errs = append(errs, scopedError{"vms", err})
			snap.vmsFailed = true
		} else {
			snap.vms = vms
		}
	} else {
		snap.vmsFailed = true
		errs = append(errs, scopedError{"vms", connector.Errorf(connector.ErrNotSupported, "list_vms", "connector does not collect VMs")})
	}

	if c, ok := conn.(connector.StorageCollector); ok {
		pools, err := c.ListStoragePools(ctx)
		if err != nil {
			errs = append(errs, scopedError{"storage", err})
		} else {
			snap.storage = pools
		}
	}

	if c, ok := conn.(connector.NetworkCollector); ok {
		nets, err := c.ListNetworks(ctx)
		if err != nil {
			errs = append(errs, scopedError{"networks", err})
		} else {
			snap.networks = nets
		}
	}
	return snap, errs
}

func (r *Reconciler) reconcileHosts(ctx context.Context, tx Tx, platformID uuid.UUID,
	hosts []connector.HostRecord, stats *Stats, now time.Time) (map[string]*uuid.UUID, error) {

	ids := map[string]*uuid.UUID{}
	for _, rec := range hosts {
		id, created, err := r.Assets.UpsertHost(ctx, tx, platformID, rec, now)
		if err != nil {
			return nil, err
		}
		hostID := id
		ids[rec.ExternalID] = &hostID
		stats.Hosts++
		if created {
			stats.Added++
		}
	}
	return ids, nil
}

func (r *Reconciler) reconcileVMs(ctx context.Context, tx Tx, platform *inventory.Platform,
	vms []connector.VMRecord, hostIDs map[string]*uuid.UUID, runID int64, stats *Stats, now time.Time) error {

	index, err := r.Assets.LoadVMIndex(ctx, platform.ID)
	if err != nil {
		return err
	}

	seen := make([]string, 0, len(vms))
	for _, rec := range vms {
		seen = append(seen, rec.ExternalID)

		existing, found := index[rec.ExternalID]
		var prior *ports.StoredAsset
		if found {
			p := existing
			prior = &p
		}

		id, changes, err := r.Assets.UpsertVM(ctx, tx, platform.ID, hostIDs[rec.HostID], rec, prior, now)
		if err != nil {
			return err
		}
		stats.VMs++

		if len(changes) == 0 {
			continue
		}
		if err := r.Assets.RecordHistory(ctx, tx, "vm", id, platform.ID, runID, changes, now); err != nil {
			return err
		}

		created := !found
		if created {
			stats.Added++
		} else {
			stats.Changed++
		}
		if err := r.emitVMChangeEvents(ctx, tx, platform, rec, id, changes, created, now); err != nil {
			return err
		}
	}

	swept, err := r.Assets.SweepMissingVMs(ctx, tx, platform.ID, seen, now)
	if err != nil {
		return err
	}
	for _, asset := range swept {
		field := inventory.FieldMissing
		if asset.SyncState == inventory.SyncDeleted {
			field = inventory.FieldDeleted
			stats.Deleted++
		} else {
			stats.Missing++
		}

		changes := []inventory.FieldChange{{Field: field, Old: asset.Name}}
		if err := r.Assets.RecordHistory(ctx, tx, "vm", asset.ID, platform.ID, runID, changes, now); err != nil {
			return err
		}
		if asset.SyncState != inventory.SyncDeleted {
			// A missing asset is an anomaly the UI shows, not yet news worth
			// notifying about: it usually resolves on the next run.
			continue
		}
		event := ports.DomainEvent{
			OccurredAt: now,
			Category:   ports.EventCategoryVMStateChange,
			Type:       ports.EventVMDeleted,
			Severity:   ports.SeverityWarning,
			Payload: map[string]any{
				"vm_id": asset.ID.String(), "vm_name": asset.Name,
				"external_id": asset.ExternalID, "platform_id": platform.ID.String(),
				"platform_name": platform.Name,
			},
		}
		if err := r.Runs.PublishEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

// emitVMChangeEvents queues notifications for the changes operators care about.
// Only creation and power-state transitions are eventful; a resize is history,
// not an alert.
func (r *Reconciler) emitVMChangeEvents(ctx context.Context, tx Tx, platform *inventory.Platform,
	rec connector.VMRecord, vmID uuid.UUID, changes []inventory.FieldChange, created bool, now time.Time) error {

	base := map[string]any{
		"vm_id": vmID.String(), "vm_name": rec.Name, "external_id": rec.ExternalID,
		"platform_id": platform.ID.String(), "platform_name": platform.Name,
		"host": rec.HostID,
	}

	if created {
		payload := cloneMap(base)
		payload["state"] = rec.State
		return r.Runs.PublishEvent(ctx, tx, ports.DomainEvent{
			OccurredAt: now, Category: ports.EventCategoryVMStateChange,
			Type: ports.EventVMCreated, Severity: ports.SeverityInfo, Payload: payload,
		})
	}

	for _, c := range changes {
		if c.Field != "state" {
			continue
		}
		payload := cloneMap(base)
		payload["from"], payload["to"] = c.Old, c.New
		severity := ports.SeverityInfo
		if c.New == "stopped" || c.New == "unknown" {
			severity = ports.SeverityWarning
		}
		return r.Runs.PublishEvent(ctx, tx, ports.DomainEvent{
			OccurredAt: now, Category: ports.EventCategoryVMStateChange,
			Type: ports.EventVMStateChanged, Severity: severity, Payload: payload,
		})
	}
	return nil
}

func (r *Reconciler) abort(ctx context.Context, runID int64, stats Stats, now time.Time, err error) (Result, error) {
	r.finish(ctx, runID, "failed", stats, err.Error(), now)
	return Result{RunID: runID, Status: "failed", Stats: stats}, err
}

func (r *Reconciler) finish(ctx context.Context, runID int64, status string, stats Stats, runErr string, now time.Time) {
	payload := map[string]any{
		"hosts": stats.Hosts, "vms": stats.VMs, "storage": stats.Storage,
		"networks": stats.Networks, "added": stats.Added, "changed": stats.Changed,
		"missing": stats.Missing, "deleted": stats.Deleted, "errors": stats.Errors,
	}
	// Use a detached context: a cancelled run still needs its outcome recorded,
	// or the run would sit in "running" forever.
	if err := r.Runs.FinishRun(context.WithoutCancel(ctx), runID, status, payload, runErr, now); err != nil {
		r.Log.Error().Err(err).Int64("sync_run_id", runID).Msg("could not finalize sync run")
	}
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}
