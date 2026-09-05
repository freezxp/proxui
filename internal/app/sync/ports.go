package sync

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// Tx is a database transaction the reconciler batches its writes into, so a
// crash mid-run leaves no half-applied snapshot.
type Tx interface {
	ports.Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// AssetStore persists synced assets. Implemented by infra/postgres.
type AssetStore interface {
	LoadVMIndex(ctx context.Context, platformID uuid.UUID) (map[string]ports.StoredAsset, error)
	UpsertVM(ctx context.Context, tx ports.Querier, platformID uuid.UUID, hostID *uuid.UUID,
		rec connector.VMRecord, existing *ports.StoredAsset, now time.Time) (uuid.UUID, []inventory.FieldChange, error)
	UpsertHost(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
		rec connector.HostRecord, now time.Time) (uuid.UUID, bool, error)
	UpsertStorage(ctx context.Context, tx ports.Querier, platformID uuid.UUID, hostID *uuid.UUID,
		rec connector.StorageRecord, now time.Time) error
	UpsertNetwork(ctx context.Context, tx ports.Querier, platformID uuid.UUID, hostID *uuid.UUID,
		rec connector.NetworkRecord, now time.Time) error
	// SweepMissingVMs advances the mark-and-sweep for VMs absent from a run.
	// templates names those that are absent because they became templates,
	// which is a conversion rather than a disappearance and is closed out at
	// once rather than counted missing (ADR 0010).
	SweepMissingVMs(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
		seen, templates []string, now time.Time) ([]ports.SweptAsset, error)
	RecordHistory(ctx context.Context, tx ports.Querier, assetType string, assetID, platformID uuid.UUID,
		syncRunID int64, changes []inventory.FieldChange, now time.Time) error
}

// RunStore records sync runs and queues the events they produce.
type RunStore interface {
	StartRun(ctx context.Context, platformID uuid.UUID, kind, trigger string, now time.Time) (int64, error)
	FinishRun(ctx context.Context, runID int64, status string, stats map[string]any, runErr string, now time.Time) error
	RecordError(ctx context.Context, runID int64, scope, message string, detail map[string]any) error
	PublishEvent(ctx context.Context, tx ports.Querier, e ports.DomainEvent) error
	Begin(ctx context.Context) (Tx, error)
}
