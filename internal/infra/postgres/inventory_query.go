package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// InventoryQuery reads inventory for the API. It is deliberately separate from
// AssetRepository: that one serves the reconciler and writes normalized rows,
// this one serves the UI and returns denormalized read models (lightweight
// CQRS, docs/05-system-architecture.md §5.3).
type InventoryQuery struct{ db *Pool }

// NewInventoryQuery builds the query side.
func NewInventoryQuery(db *Pool) *InventoryQuery { return &InventoryQuery{db: db} }

// scopeClause is the single place VM visibility is decided.
//
// Admins and auditors see everything: one manages the estate, the other must be
// able to audit all of it. Everyone else sees only VMs reachable through a
// grant. Every VM-touching query in this file composes this fragment, so there
// is exactly one definition of "can see" to review (docs/07 §7.5, RBAC-05).
func scopeClause(role identity.Role, argIndex int) (string, bool) {
	if role == identity.RoleAdmin || role == identity.RoleAuditor {
		return "", false
	}
	return fmt.Sprintf(`v.id IN (
		SELECT vgm.vm_id FROM vm_group_members vgm
		JOIN access_grants ag ON ag.vm_group_id = vgm.vm_group_id
		JOIN user_group_members ugm ON ugm.user_group_id = ag.user_group_id
		WHERE ugm.user_id = $%d)`, argIndex), true
}

const vmListColumns = `
	v.id, v.external_id, v.name, v.vm_type, v.state::text, v.cpu_cores, v.memory_bytes,
	v.disk_bytes, v.uptime_s, v.ip_addresses, v.platform_tags, v.portal_tags,
	v.platform_pool, v.sync_state::text, v.last_seen_at,
	p.id, p.name, p.datacenter, coalesce(h.id::text,''), coalesce(h.name,'')`

// ListVMs returns a page of VMs the caller may see.
func (q *InventoryQuery) ListVMs(ctx context.Context, f ports.VMFilter) (ports.VMPage, error) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	// Deleted assets stay in the database for history but are not inventory.
	where = append(where, "v.sync_state <> 'deleted'")

	if f.Query != "" {
		add("v.name ILIKE '%%' || $%d || '%%'", f.Query)
	}
	if f.State != "" {
		add("v.state = $%d::vm_state", f.State)
	}
	if f.PlatformID != uuid.Nil {
		add("v.platform_id = $%d", f.PlatformID)
	}
	if f.HostID != uuid.Nil {
		add("v.host_id = $%d", f.HostID)
	}
	if f.GroupID != uuid.Nil {
		add("v.id IN (SELECT vm_id FROM vm_group_members WHERE vm_group_id = $%d)", f.GroupID)
	}
	if f.Tag != "" {
		add("($%d = ANY(v.portal_tags) OR $%d = ANY(v.platform_tags))", f.Tag)
		// The clause references the argument twice; rewrite the last entry.
		where[len(where)-1] = fmt.Sprintf(
			"($%d = ANY(v.portal_tags) OR $%d = ANY(v.platform_tags))", len(args), len(args))
	}
	if clause, scoped := scopeClause(f.Role, len(args)+1); scoped {
		args = append(args, f.UserID)
		where = append(where, clause)
	}

	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int
	countSQL := `SELECT count(*) FROM vms v
		JOIN platforms p ON p.id = v.platform_id
		LEFT JOIN hosts h ON h.id = v.host_id ` + whereSQL
	if err := q.db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return ports.VMPage{}, fmt.Errorf("count vms: %w", err)
	}

	limit, offset := f.Limit, f.Offset
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, offset)

	listSQL := fmt.Sprintf(`SELECT %s FROM vms v
		JOIN platforms p ON p.id = v.platform_id
		LEFT JOIN hosts h ON h.id = v.host_id
		%s ORDER BY %s LIMIT $%d OFFSET $%d`,
		vmListColumns, whereSQL, orderBy(f.Sort), len(args)-1, len(args))

	rows, err := q.db.Query(ctx, listSQL, args...)
	if err != nil {
		return ports.VMPage{}, fmt.Errorf("list vms: %w", err)
	}
	defer rows.Close()

	page := ports.VMPage{Total: total, Limit: limit, Offset: offset, Items: []ports.VMListItem{}}
	for rows.Next() {
		item, err := scanVMListItem(rows)
		if err != nil {
			return ports.VMPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

// orderBy maps the sort keys the API accepts onto columns. Anything unknown
// falls back to name, so a bad parameter cannot inject SQL or error the page.
func orderBy(sort string) string {
	desc := strings.HasPrefix(sort, "-")
	key := strings.TrimPrefix(sort, "-")

	column := map[string]string{
		"name":         "v.name",
		"state":        "v.state",
		"cpu":          "v.cpu_cores",
		"memory":       "v.memory_bytes",
		"uptime":       "v.uptime_s",
		"last_seen_at": "v.last_seen_at",
		"platform":     "p.name",
	}[key]
	if column == "" {
		return "v.name ASC"
	}
	if desc {
		return column + " DESC NULLS LAST"
	}
	return column + " ASC"
}

func scanVMListItem(s scanner) (ports.VMListItem, error) {
	var (
		item      ports.VMListItem
		ips       []byte
		lastSeen  time.Time
		cpu       *int
		mem, disk *int64
		uptime    *int64
		hostID    string
	)
	err := s.Scan(&item.ID, &item.ExternalID, &item.Name, &item.VMType, &item.State,
		&cpu, &mem, &disk, &uptime, &ips, &item.PlatformTags, &item.PortalTags,
		&item.Pool, &item.SyncState, &lastSeen,
		&item.PlatformID, &item.PlatformName, &item.Datacenter, &hostID, &item.HostName)
	if err != nil {
		return ports.VMListItem{}, fmt.Errorf("scan vm: %w", err)
	}
	item.CPUCores = derefInt(cpu)
	item.MemoryBytes = derefInt64(mem)
	item.DiskBytes = derefInt64(disk)
	item.UptimeS = derefInt64(uptime)
	item.LastSeenAt = lastSeen
	if hostID != "" {
		if id, err := uuid.Parse(hostID); err == nil {
			item.HostID = &id
		}
	}
	if len(ips) > 0 {
		_ = json.Unmarshal(ips, &item.IPAddresses)
	}
	return item, nil
}

// GetVM returns one VM, or ErrNotFound when it does not exist or the caller may
// not see it. The two cases are deliberately indistinguishable: telling an
// unauthorized caller that a VM exists is itself a disclosure (RBAC-05).
func (q *InventoryQuery) GetVM(ctx context.Context, id uuid.UUID, role identity.Role, userID uuid.UUID) (ports.VMDetail, error) {
	args := []any{id}
	where := "v.id = $1 AND v.sync_state <> 'deleted'"
	if clause, scoped := scopeClause(role, 2); scoped {
		args = append(args, userID)
		where += " AND " + clause
	}

	sql := fmt.Sprintf(`SELECT %s, v.notes, v.attrs, v.first_seen_at
		FROM vms v JOIN platforms p ON p.id = v.platform_id
		LEFT JOIN hosts h ON h.id = v.host_id WHERE %s`, vmListColumns, where)

	var (
		detail    ports.VMDetail
		ips       []byte
		attrs     []byte
		lastSeen  time.Time
		cpu       *int
		mem, disk *int64
		uptime    *int64
		hostID    string
	)
	err := q.db.QueryRow(ctx, sql, args...).Scan(
		&detail.ID, &detail.ExternalID, &detail.Name, &detail.VMType, &detail.State,
		&cpu, &mem, &disk, &uptime, &ips, &detail.PlatformTags, &detail.PortalTags,
		&detail.Pool, &detail.SyncState, &lastSeen,
		&detail.PlatformID, &detail.PlatformName, &detail.Datacenter, &hostID, &detail.HostName,
		&detail.Notes, &attrs, &detail.FirstSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.VMDetail{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.VMDetail{}, fmt.Errorf("get vm: %w", err)
	}

	detail.CPUCores = derefInt(cpu)
	detail.MemoryBytes = derefInt64(mem)
	detail.DiskBytes = derefInt64(disk)
	detail.UptimeS = derefInt64(uptime)
	detail.LastSeenAt = lastSeen
	if hostID != "" {
		if parsed, err := uuid.Parse(hostID); err == nil {
			detail.HostID = &parsed
		}
	}
	if len(ips) > 0 {
		_ = json.Unmarshal(ips, &detail.IPAddresses)
	}
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &detail.Attrs)
	}

	groups, err := q.vmGroups(ctx, id)
	if err != nil {
		return ports.VMDetail{}, err
	}
	detail.Groups = groups
	return detail, nil
}

func (q *InventoryQuery) vmGroups(ctx context.Context, vmID uuid.UUID) ([]string, error) {
	rows, err := q.db.Query(ctx, `
		SELECT g.name FROM vm_groups g
		JOIN vm_group_members m ON m.vm_group_id = g.id
		WHERE m.vm_id = $1 ORDER BY g.name`, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm groups: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan vm group: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CanAccessVM reports whether the caller may act on a VM. Commands use it
// before mutating, so authorization does not depend on a prior read.
func (q *InventoryQuery) CanAccessVM(ctx context.Context, id uuid.UUID, role identity.Role, userID uuid.UUID) (bool, error) {
	args := []any{id}
	where := "v.id = $1 AND v.sync_state <> 'deleted'"
	if clause, scoped := scopeClause(role, 2); scoped {
		args = append(args, userID)
		where += " AND " + clause
	}

	var exists bool
	err := q.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vms v WHERE "+where+")", args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check vm access: %w", err)
	}
	return exists, nil
}

// VMHistory returns a VM's field-level change history.
func (q *InventoryQuery) VMHistory(ctx context.Context, id uuid.UUID, limit int) ([]ports.HistoryEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.db.Query(ctx, `
		SELECT changed_at, field, coalesce(old_value,''), coalesce(new_value,'')
		FROM asset_state_history WHERE asset_id = $1
		ORDER BY changed_at DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("read vm history: %w", err)
	}
	defer rows.Close()

	out := []ports.HistoryEntry{}
	for rows.Next() {
		var e ports.HistoryEntry
		if err := rows.Scan(&e.ChangedAt, &e.Field, &e.OldValue, &e.NewValue); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetPortalTags replaces a VM's portal-owned tags. Sync never touches these.
func (q *InventoryQuery) SetPortalTags(ctx context.Context, id uuid.UUID, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	tag, err := q.db.Exec(ctx, `UPDATE vms SET portal_tags = $2 WHERE id = $1`, id, tags)
	if err != nil {
		return fmt.Errorf("set portal tags: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// SetNotes replaces a VM's portal-owned notes.
func (q *InventoryQuery) SetNotes(ctx context.Context, id uuid.UUID, notes string) error {
	tag, err := q.db.Exec(ctx, `UPDATE vms SET notes = $2 WHERE id = $1`, id, notes)
	if err != nil {
		return fmt.Errorf("set notes: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Dashboard returns the fleet summary, scoped to what the caller may see.
func (q *InventoryQuery) Dashboard(ctx context.Context, role identity.Role, userID uuid.UUID) (ports.DashboardSummary, error) {
	var (
		summary ports.DashboardSummary
		args    []any
		where   = "v.sync_state <> 'deleted'"
	)
	if clause, scoped := scopeClause(role, 1); scoped {
		args = append(args, userID)
		where += " AND " + clause
	}

	err := q.db.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE v.state = 'running'),
		       count(*) FILTER (WHERE v.state = 'stopped'),
		       count(*) FILTER (WHERE v.sync_state = 'missing')
		FROM vms v WHERE `+where, args...).
		Scan(&summary.TotalVMs, &summary.RunningVMs, &summary.StoppedVMs, &summary.MissingVMs)
	if err != nil {
		return ports.DashboardSummary{}, fmt.Errorf("dashboard counts: %w", err)
	}
	summary.OtherVMs = summary.TotalVMs - summary.RunningVMs - summary.StoppedVMs

	platforms, err := q.db.Query(ctx, `
		SELECT p.id, p.name, p.datacenter, p.health::text, coalesce(p.detected_version,''),
		       p.last_seen_at, p.breaker_open_until,
		       (SELECT count(*) FROM vms v WHERE v.platform_id = p.id AND v.sync_state <> 'deleted')
		FROM platforms p WHERE p.deleted_at IS NULL ORDER BY p.name`)
	if err != nil {
		return ports.DashboardSummary{}, fmt.Errorf("dashboard platforms: %w", err)
	}
	defer platforms.Close()

	summary.Platforms = []ports.PlatformHealth{}
	for platforms.Next() {
		var (
			ph           ports.PlatformHealth
			lastSeen     *time.Time
			breakerUntil *time.Time
		)
		if err := platforms.Scan(&ph.ID, &ph.Name, &ph.Datacenter, &ph.Health, &ph.Version,
			&lastSeen, &breakerUntil, &ph.VMCount); err != nil {
			return ports.DashboardSummary{}, fmt.Errorf("scan platform health: %w", err)
		}
		ph.LastSeenAt = derefTime(lastSeen)
		ph.BreakerOpen = breakerUntil != nil && breakerUntil.After(time.Now())
		summary.Platforms = append(summary.Platforms, ph)
	}
	if err := platforms.Err(); err != nil {
		return ports.DashboardSummary{}, err
	}

	summary.TopCPU, err = q.topConsumers(ctx, "cpu_pct", role, userID)
	if err != nil {
		return ports.DashboardSummary{}, err
	}
	summary.TopMemory, err = q.topConsumers(ctx, "mem_pct", role, userID)
	if err != nil {
		return ports.DashboardSummary{}, err
	}
	return summary, nil
}

// topConsumers reads the busiest VMs from the most recent sample of each.
// Only recent samples count: a VM that stopped reporting an hour ago is not
// "currently" busy.
func (q *InventoryQuery) topConsumers(ctx context.Context, metric string, role identity.Role, userID uuid.UUID) ([]ports.TopConsumer, error) {
	value := "latest.cpu_pct"
	if metric == "mem_pct" {
		value = "CASE WHEN latest.mem_total_bytes > 0 THEN latest.mem_used_bytes::float / latest.mem_total_bytes * 100 ELSE 0 END"
	}

	args := []any{time.Now().Add(-10 * time.Minute)}
	where := "v.sync_state <> 'deleted'"
	if clause, scoped := scopeClause(role, 2); scoped {
		args = append(args, userID)
		where += " AND " + clause
	}

	sql := fmt.Sprintf(`
		SELECT v.id, v.name, p.name, %s AS value
		FROM (SELECT DISTINCT ON (vm_id) vm_id, cpu_pct, mem_used_bytes, mem_total_bytes
		      FROM metrics_vm WHERE time >= $1 ORDER BY vm_id, time DESC) latest
		JOIN vms v ON v.id = latest.vm_id
		JOIN platforms p ON p.id = v.platform_id
		WHERE %s ORDER BY value DESC NULLS LAST LIMIT 5`, value, where)

	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("top consumers: %w", err)
	}
	defer rows.Close()

	out := []ports.TopConsumer{}
	for rows.Next() {
		var c ports.TopConsumer
		var v *float64
		if err := rows.Scan(&c.VMID, &c.Name, &c.PlatformName, &v); err != nil {
			return nil, fmt.Errorf("scan top consumer: %w", err)
		}
		c.Value = derefFloat(v)
		out = append(out, c)
	}
	return out, rows.Err()
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// AllVMNames maps every live VM to its name, unscoped by grants. The alert
// evaluator is not acting for a user: it evaluates the whole estate and the
// resulting notification is routed by rule, not by who can see the VM.
func (q *InventoryQuery) AllVMNames(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := q.db.Query(ctx, `SELECT id, name FROM vms WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list vm names: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]string{}
	for rows.Next() {
		var (
			id   uuid.UUID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan vm name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}
