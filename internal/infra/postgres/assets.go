package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// AssetRepository persists synced inventory. Its methods are written for the
// reconciler: upsert by natural key, compare fingerprints, and sweep whatever
// the platform stopped reporting.
type AssetRepository struct{ db *Pool }

// NewAssetRepository builds the repository.
func NewAssetRepository(db *Pool) *AssetRepository { return &AssetRepository{db: db} }

// LoadVMIndex returns every known VM for a platform, keyed by external id.
// The reconciler holds this in memory for one run: at the scale ProxUI targets
// this is a few hundred small rows, and it turns per-asset lookups into one
// query (docs/10-sync-engine.md §10.3).
//
// Deleted rows are included, and that is load-bearing rather than incidental.
// A soft delete leaves the row in place, so `(platform_id, external_id)` is
// still taken; an index that hid those rows told the reconciler to INSERT a VM
// that already existed, and the unique constraint refused it. One guest
// returning after three missed runs then failed the whole platform's sync, on
// every run, until somebody deleted the row by hand.
func (r *AssetRepository) LoadVMIndex(ctx context.Context, platformID uuid.UUID) (map[string]ports.StoredAsset, error) {
	rows, err := r.db.Query(ctx, `
		SELECT external_id, id, content_hash, sync_state, missing_count, name, state::text,
		       coalesce(host_id::text,''), cpu_cores, memory_bytes, disk_bytes
		FROM vms WHERE platform_id = $1`, platformID)
	if err != nil {
		return nil, fmt.Errorf("load vm index: %w", err)
	}
	defer rows.Close()

	index := map[string]ports.StoredAsset{}
	for rows.Next() {
		var (
			key          string
			a            ports.StoredAsset
			syncState    string
			cpu          *int
			memory, disk *int64
		)
		if err := rows.Scan(&key, &a.ID, &a.ContentHash, &syncState, &a.MissingCount,
			&a.Name, &a.State, &a.HostID, &cpu, &memory, &disk); err != nil {
			return nil, fmt.Errorf("scan vm index: %w", err)
		}
		a.SyncState = inventory.SyncState(syncState)
		a.Extra = map[string]string{
			"cpu_cores":    intPtrString(cpu),
			"memory_bytes": int64PtrString(memory),
			"disk_bytes":   int64PtrString(disk),
		}
		index[key] = a
	}
	return index, rows.Err()
}

// UpsertVM inserts or updates a VM from a connector record and reports the
// fields that changed. Portal-owned columns (portal_tags, notes) are absent
// from the UPDATE list on purpose: sync must never overwrite what an operator
// typed (docs/10-sync-engine.md §10.4).
func (r *AssetRepository) UpsertVM(ctx context.Context, tx ports.Querier, platformID uuid.UUID, hostID *uuid.UUID,
	rec connector.VMRecord, existing *ports.StoredAsset, now time.Time) (uuid.UUID, []inventory.FieldChange, error) {

	// Real platforms routinely return no tags and no addresses. A nil slice
	// would violate the NOT NULL columns, so it is normalized to empty here
	// rather than requiring every connector to remember.
	tags := nonNilStrings(rec.Tags)
	ips, err := json.Marshal(nonNilStrings(rec.IPAddresses))
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("encode ip addresses: %w", err)
	}
	attrs, err := json.Marshal(rec.Attrs)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("encode attrs: %w", err)
	}
	fingerprint := rec.Fingerprint()

	if existing == nil {
		id := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO vms (id, platform_id, host_id, external_id, name, vm_type, state,
				cpu_cores, memory_bytes, disk_bytes, uptime_s, ip_addresses, platform_tags,
				platform_pool, content_hash, sync_state, missing_count, attrs,
				first_seen_at, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'active',0,$16,$17,$17)`,
			id, platformID, hostID, rec.ExternalID, rec.Name, rec.Type, rec.State,
			rec.CPUCores, rec.MemoryBytes, rec.DiskBytes, rec.UptimeS, ips, tags,
			rec.Pool, fingerprint, attrs, now)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("insert vm: %w", err)
		}
		return id, []inventory.FieldChange{{Field: inventory.FieldCreated, New: rec.Name}}, nil
	}

	// Unchanged assets get a cheap touch: no diffing, no history, just proof
	// they were seen this run.
	if string(existing.ContentHash) == string(fingerprint) && existing.SyncState == inventory.SyncActive {
		_, err := tx.Exec(ctx, `UPDATE vms SET last_seen_at=$2, uptime_s=$3, missing_count=0 WHERE id=$1`,
			existing.ID, now, rec.UptimeS)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("touch vm: %w", err)
		}
		return existing.ID, nil, nil
	}

	changes := diffVM(*existing, rec, hostID)
	if existing.SyncState == inventory.SyncDeleted {
		// The UPDATE below revives the row anyway — sync_state back to active,
		// deleted_at cleared. Saying so explicitly matters because a guest that
		// comes back unchanged produces no other field change, and the history
		// tab would otherwise show it deleted and never heard from again.
		changes = append([]inventory.FieldChange{
			{Field: inventory.FieldRestored, New: rec.Name},
		}, changes...)
	}
	_, err = tx.Exec(ctx, `
		UPDATE vms SET host_id=$2, name=$3, vm_type=$4, state=$5, cpu_cores=$6,
			memory_bytes=$7, disk_bytes=$8, uptime_s=$9, ip_addresses=$10,
			platform_tags=$11, platform_pool=$12, content_hash=$13,
			sync_state='active', missing_count=0, attrs=$14, last_seen_at=$15,
			deleted_at=NULL
		WHERE id=$1`,
		existing.ID, hostID, rec.Name, rec.Type, rec.State, rec.CPUCores,
		rec.MemoryBytes, rec.DiskBytes, rec.UptimeS, ips, tags, rec.Pool,
		fingerprint, attrs, now)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("update vm: %w", err)
	}
	return existing.ID, changes, nil
}

// diffVM computes the field-level changes recorded in history. Only fields an
// operator would care about are compared; uptime is excluded because it always
// differs and would drown the history in noise.
func diffVM(existing ports.StoredAsset, rec connector.VMRecord, hostID *uuid.UUID) []inventory.FieldChange {
	var changes []inventory.FieldChange
	add := func(field, old, new string) {
		if old != new {
			changes = append(changes, inventory.FieldChange{Field: field, Old: old, New: new})
		}
	}

	add("name", existing.Name, rec.Name)
	add("state", existing.State, rec.State)
	add("cpu_cores", existing.Extra["cpu_cores"], fmt.Sprintf("%d", rec.CPUCores))
	add("memory_bytes", existing.Extra["memory_bytes"], fmt.Sprintf("%d", rec.MemoryBytes))
	add("disk_bytes", existing.Extra["disk_bytes"], fmt.Sprintf("%d", rec.DiskBytes))

	newHost := ""
	if hostID != nil {
		newHost = hostID.String()
	}
	add("host_id", existing.HostID, newHost)

	if existing.SyncState != inventory.SyncActive {
		changes = append(changes, inventory.FieldChange{
			Field: "sync_state", Old: string(existing.SyncState), New: string(inventory.SyncActive),
		})
	}
	return changes
}

// SweepMissingVMs marks every VM absent from this run, advancing the
// mark-and-sweep counters and soft-deleting those that have been gone long
// enough. It returns the VMs whose state changed so the caller can record
// history and emit events.
func (r *AssetRepository) SweepMissingVMs(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
	seen, templates []string, now time.Time) ([]ports.SweptAsset, error) {

	// A guest that became a template is absent from the listing for a reason
	// the platform can state, so it is closed out at once rather than counted
	// missing three times first. Deciding it here, in the same statement that
	// does the counting, is what keeps it immune to timing: it does not matter
	// whether the conversion happened before or after any particular run began
	// (ADR 0010).
	rows, err := tx.Query(ctx, `
		UPDATE vms SET
			missing_count = CASE WHEN external_id = ANY($5) THEN missing_count ELSE missing_count + 1 END,
			sync_state    = CASE
				WHEN external_id = ANY($5) THEN 'deleted'::sync_state
				WHEN missing_count + 1 >= $3 THEN 'deleted'::sync_state
				ELSE 'missing'::sync_state END,
			deleted_at    = CASE
				WHEN external_id = ANY($5) THEN $4::timestamptz
				WHEN missing_count + 1 >= $3 THEN $4::timestamptz
				ELSE NULL END
		WHERE platform_id = $1
		  AND sync_state <> 'deleted'
		  AND NOT (external_id = ANY($2))
		RETURNING id, external_id, name, sync_state::text, missing_count,
		          external_id = ANY($5) AS converted`,
		platformID, seen, inventory.MissingThreshold, now, nonNilStrings(templates))
	if err != nil {
		return nil, fmt.Errorf("sweep missing vms: %w", err)
	}
	defer rows.Close()

	var swept []ports.SweptAsset
	for rows.Next() {
		var a ports.SweptAsset
		var state string
		if err := rows.Scan(&a.ID, &a.ExternalID, &a.Name, &state, &a.MissingCount, &a.Converted); err != nil {
			return nil, fmt.Errorf("scan swept vm: %w", err)
		}
		a.SyncState = inventory.SyncState(state)
		swept = append(swept, a)
	}
	return swept, rows.Err()
}

// UpsertHost inserts or updates a host and returns its id.
func (r *AssetRepository) UpsertHost(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
	rec connector.HostRecord, now time.Time) (uuid.UUID, bool, error) {

	attrs, err := json.Marshal(rec.Attrs)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("encode attrs: %w", err)
	}

	var (
		id      uuid.UUID
		created bool
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO hosts (id, platform_id, external_id, name, status, cpu_cores,
			memory_bytes, version, uptime_s, content_hash, sync_state, missing_count,
			attrs, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',0,$11,$12,$12)
		ON CONFLICT (platform_id, external_id) DO UPDATE SET
			name=EXCLUDED.name, status=EXCLUDED.status, cpu_cores=EXCLUDED.cpu_cores,
			memory_bytes=EXCLUDED.memory_bytes, version=EXCLUDED.version,
			uptime_s=EXCLUDED.uptime_s, content_hash=EXCLUDED.content_hash,
			sync_state='active', missing_count=0, attrs=EXCLUDED.attrs,
			last_seen_at=EXCLUDED.last_seen_at, deleted_at=NULL
		RETURNING id, (xmax = 0)`,
		uuid.New(), platformID, rec.ExternalID, rec.Name, rec.Status, rec.CPUCores,
		rec.MemoryBytes, rec.Version, rec.UptimeS, rec.Fingerprint(), attrs, now).
		Scan(&id, &created)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("upsert host: %w", err)
	}
	return id, created, nil
}

// UpsertStorage inserts or updates a storage pool.
func (r *AssetRepository) UpsertStorage(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
	hostID *uuid.UUID, rec connector.StorageRecord, now time.Time) error {

	attrs, err := json.Marshal(rec.Attrs)
	if err != nil {
		return fmt.Errorf("encode attrs: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO storage_pools (id, platform_id, host_id, external_id, natural_key, name,
			storage_type, total_bytes, used_bytes, is_shared, content_hash, sync_state,
			missing_count, attrs, first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'active',0,$12,$13,$13)
		ON CONFLICT (platform_id, natural_key) DO UPDATE SET
			host_id=EXCLUDED.host_id, name=EXCLUDED.name, storage_type=EXCLUDED.storage_type,
			total_bytes=EXCLUDED.total_bytes, used_bytes=EXCLUDED.used_bytes,
			is_shared=EXCLUDED.is_shared, content_hash=EXCLUDED.content_hash,
			sync_state='active', missing_count=0, attrs=EXCLUDED.attrs,
			last_seen_at=EXCLUDED.last_seen_at, deleted_at=NULL`,
		uuid.New(), platformID, hostID, rec.ExternalID, rec.NaturalKey(), rec.Name,
		rec.StorageType, rec.TotalBytes, rec.UsedBytes, rec.IsShared, rec.Fingerprint(), attrs, now)
	if err != nil {
		return fmt.Errorf("upsert storage: %w", err)
	}
	return nil
}

// UpsertNetwork inserts or updates a network interface.
func (r *AssetRepository) UpsertNetwork(ctx context.Context, tx ports.Querier, platformID uuid.UUID,
	hostID *uuid.UUID, rec connector.NetworkRecord, now time.Time) error {

	attrs, err := json.Marshal(rec.Attrs)
	if err != nil {
		return fmt.Errorf("encode attrs: %w", err)
	}
	var vlan *int
	if rec.VLANTag > 0 {
		vlan = &rec.VLANTag
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO networks (id, platform_id, host_id, external_id, natural_key, name,
			net_type, cidr, vlan_tag, content_hash, sync_state, missing_count, attrs,
			first_seen_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',0,$11,$12,$12)
		ON CONFLICT (platform_id, natural_key) DO UPDATE SET
			host_id=EXCLUDED.host_id, name=EXCLUDED.name, net_type=EXCLUDED.net_type,
			cidr=EXCLUDED.cidr, vlan_tag=EXCLUDED.vlan_tag, content_hash=EXCLUDED.content_hash,
			sync_state='active', missing_count=0, attrs=EXCLUDED.attrs,
			last_seen_at=EXCLUDED.last_seen_at, deleted_at=NULL`,
		uuid.New(), platformID, hostID, rec.ExternalID, rec.NaturalKey(), rec.Name,
		rec.NetType, rec.CIDR, vlan, rec.Fingerprint(), attrs, now)
	if err != nil {
		return fmt.Errorf("upsert network: %w", err)
	}
	return nil
}

// RecordHistory appends field-level change rows.
func (r *AssetRepository) RecordHistory(ctx context.Context, tx ports.Querier, assetType string,
	assetID, platformID uuid.UUID, syncRunID int64, changes []inventory.FieldChange, now time.Time) error {

	for _, c := range changes {
		_, err := tx.Exec(ctx, `
			INSERT INTO asset_state_history (changed_at, asset_type, asset_id, platform_id,
				sync_run_id, field, old_value, new_value)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			now, assetType, assetID, platformID, syncRunID, c.Field,
			nullString(c.Old), nullString(c.New))
		if err != nil {
			return fmt.Errorf("record history: %w", err)
		}
	}
	return nil
}

// nonNilStrings returns an empty slice for nil, so JSON encodes [] rather than
// null and array columns receive a value.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func intPtrString(v *int) string {
	if v == nil {
		return "0"
	}
	return fmt.Sprintf("%d", *v)
}

func int64PtrString(v *int64) string {
	if v == nil {
		return "0"
	}
	return fmt.Sprintf("%d", *v)
}
