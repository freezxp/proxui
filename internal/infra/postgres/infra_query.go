package postgres

import (
	"context"
	"fmt"

	"github.com/freezxp/proxui/internal/app/ports"
)

// ListHosts returns the nodes behind the estate, newest sync state included so
// a node that vanished is visible rather than silently absent.
func (q *InventoryQuery) ListHosts(ctx context.Context) ([]ports.HostRow, error) {
	rows, err := q.db.Query(ctx, `
		SELECT h.id, h.name, p.name, h.status, coalesce(h.cpu_cores,0), coalesce(h.memory_bytes,0),
		       h.version, coalesce(h.uptime_s,0), h.sync_state::text,
		       (SELECT count(*) FROM vms v WHERE v.host_id = h.id AND v.deleted_at IS NULL)
		  FROM hosts h
		  JOIN platforms p ON p.id = h.platform_id
		 WHERE h.deleted_at IS NULL AND p.deleted_at IS NULL
		 ORDER BY p.name, h.name`)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()

	out := []ports.HostRow{}
	for rows.Next() {
		var h ports.HostRow
		if err := rows.Scan(&h.ID, &h.Name, &h.PlatformName, &h.Status, &h.CPUCores,
			&h.MemoryBytes, &h.Version, &h.UptimeS, &h.SyncState, &h.VMCount); err != nil {
			return nil, fmt.Errorf("scan host: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListStoragePools returns capacity per pool.
func (q *InventoryQuery) ListStoragePools(ctx context.Context) ([]ports.StorageRow, error) {
	rows, err := q.db.Query(ctx, `
		SELECT s.id, s.name, p.name, coalesce(h.name,''), s.storage_type,
		       coalesce(s.total_bytes,0), coalesce(s.used_bytes,0), s.is_shared, s.sync_state::text
		  FROM storage_pools s
		  JOIN platforms p ON p.id = s.platform_id
		  LEFT JOIN hosts h ON h.id = s.host_id
		 WHERE s.deleted_at IS NULL AND p.deleted_at IS NULL
		 ORDER BY p.name, s.name`)
	if err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}
	defer rows.Close()

	out := []ports.StorageRow{}
	for rows.Next() {
		var s ports.StorageRow
		if err := rows.Scan(&s.ID, &s.Name, &s.PlatformName, &s.HostName, &s.StorageType,
			&s.TotalBytes, &s.UsedBytes, &s.IsShared, &s.SyncState); err != nil {
			return nil, fmt.Errorf("scan storage: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListNetworks returns the interfaces the estate is wired with.
func (q *InventoryQuery) ListNetworks(ctx context.Context) ([]ports.NetworkRow, error) {
	rows, err := q.db.Query(ctx, `
		SELECT n.id, n.name, p.name, coalesce(h.name,''), n.net_type, n.cidr,
		       n.vlan_tag, n.sync_state::text
		  FROM networks n
		  JOIN platforms p ON p.id = n.platform_id
		  LEFT JOIN hosts h ON h.id = n.host_id
		 WHERE n.deleted_at IS NULL AND p.deleted_at IS NULL
		 ORDER BY p.name, h.name, n.name`)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	defer rows.Close()

	out := []ports.NetworkRow{}
	for rows.Next() {
		var n ports.NetworkRow
		if err := rows.Scan(&n.ID, &n.Name, &n.PlatformName, &n.HostName, &n.NetType,
			&n.CIDR, &n.VLANTag, &n.SyncState); err != nil {
			return nil, fmt.Errorf("scan network: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
