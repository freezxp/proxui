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
	"github.com/freezxp/proxui/internal/domain/publish"
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
	p.id, p.name, p.datacenter, coalesce(h.id::text,''), coalesce(h.name,''),
	fav.vm_id IS NOT NULL, coalesce(fld.id::text,''), coalesce(fld.name,'')`

// personalJoins attach the viewer's own favourites and filing.
//
// LEFT JOINs on the caller's rows only, so a VM nobody starred still appears and
// two people looking at the same machine each see their own answer. The user id
// is passed separately from scopeClause's, which is only added for a role that
// is scoped at all — an administrator sees every VM and still has favourites.
const personalJoins = `
	LEFT JOIN vm_favourites     fav ON fav.vm_id = v.id AND fav.user_id = $%d
	LEFT JOIN vm_folder_members fm  ON fm.vm_id  = v.id AND fm.user_id = $%d
	LEFT JOIN vm_folders        fld ON fld.id    = fm.folder_id`

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
	where = append(where, "v.sync_state <> 'deleted'", "v.deleted_at IS NULL", "p.deleted_at IS NULL")

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

	// The viewer's own id, for the favourite and folder joins. Separate from
	// the scope clause's copy, which is only added for a role that is scoped:
	// an administrator sees every VM and still has favourites of their own.
	args = append(args, f.UserID)
	viewer := len(args)
	joins := fmt.Sprintf(personalJoins, viewer, viewer)

	if f.FolderID != uuid.Nil {
		add("fm.folder_id = $%d", f.FolderID)
	}
	if f.Unfiled {
		where = append(where, "fm.vm_id IS NULL")
	}
	if f.FavouritesOnly {
		// The join is already there for the ordering, so this is a clause and
		// nothing else.
		where = append(where, "fav.vm_id IS NOT NULL")
	}

	whereSQL := "WHERE " + strings.Join(where, " AND ")

	// The count carries the same joins as the listing. It has to: a folder
	// filter narrows on them, and a total that ignored the filter would page a
	// short list as though it were long.
	var total int
	countSQL := `SELECT count(*) FROM vms v
		JOIN platforms p ON p.id = v.platform_id
		LEFT JOIN hosts h ON h.id = v.host_id ` + joins + " " + whereSQL
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
		%s
		%s ORDER BY %s LIMIT $%d OFFSET $%d`,
		vmListColumns, joins, whereSQL, orderBy(f.Sort), len(args)-1, len(args))

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

	// Favourites sort above everything, whatever column the table is sorted by,
	// so the pinned block holds while the rest of the list reorders underneath.
	//
	// It has to happen here rather than in the browser: the list is paginated
	// server-side, and sorting the fifty rows already fetched would float a
	// favourite to the top of page 4 and leave page 1 exactly as it was.
	const favouritesFirst = "(fav.vm_id IS NOT NULL) DESC, "

	// Grouping is a sort rather than a tree, which is what lets it survive
	// paging: rows arrive in folder order and the table draws a heading
	// whenever the folder changes. Unfiled VMs sort last — they are where
	// everything starts, not a folder somebody made.
	if key == "folder" {
		direction := "ASC"
		if desc {
			direction = "DESC"
		}
		return favouritesFirst + "fld.position " + direction + " NULLS LAST, fld.name " +
			direction + " NULLS LAST, v.name ASC"
	}

	if column == "" {
		return favouritesFirst + "v.name ASC"
	}
	if desc {
		return favouritesFirst + column + " DESC NULLS LAST"
	}
	return favouritesFirst + column + " ASC"
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
		folderID  string
	)
	err := s.Scan(&item.ID, &item.ExternalID, &item.Name, &item.VMType, &item.State,
		&cpu, &mem, &disk, &uptime, &ips, &item.PlatformTags, &item.PortalTags,
		&item.Pool, &item.SyncState, &lastSeen,
		&item.PlatformID, &item.PlatformName, &item.Datacenter, &hostID, &item.HostName,
		&item.IsFavourite, &folderID, &item.FolderName)
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
	if folderID != "" {
		if id, err := uuid.Parse(folderID); err == nil {
			item.FolderID = &id
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

	// The viewer's own favourite and folder, same as the listing — a detail
	// page that could not draw the star it was opened from would be odd.
	args = append(args, userID)
	viewer := len(args)
	sql := fmt.Sprintf(`SELECT %s, v.notes, v.attrs, v.first_seen_at
		FROM vms v JOIN platforms p ON p.id = v.platform_id
		LEFT JOIN hosts h ON h.id = v.host_id
		%s
		WHERE %s`, vmListColumns, fmt.Sprintf(personalJoins, viewer, viewer), where)

	var (
		detail    ports.VMDetail
		ips       []byte
		attrs     []byte
		lastSeen  time.Time
		cpu       *int
		mem, disk *int64
		uptime    *int64
		hostID    string
		folderID  string
	)
	err := q.db.QueryRow(ctx, sql, args...).Scan(
		&detail.ID, &detail.ExternalID, &detail.Name, &detail.VMType, &detail.State,
		&cpu, &mem, &disk, &uptime, &ips, &detail.PlatformTags, &detail.PortalTags,
		&detail.Pool, &detail.SyncState, &lastSeen,
		&detail.PlatformID, &detail.PlatformName, &detail.Datacenter, &hostID, &detail.HostName,
		&detail.IsFavourite, &folderID, &detail.FolderName,
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
	if folderID != "" {
		if parsed, err := uuid.Parse(folderID); err == nil {
			detail.FolderID = &parsed
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

	// Platform health is estate-level information: names, datacenters,
	// versions and whether a cluster is reachable. An operator works on the
	// VMs they were granted and has no business knowing the shape of the
	// estate behind them, so they get no platform block at all.
	summary.Platforms = []ports.PlatformHealth{}
	if role == identity.RoleOperator {
		return q.finishDashboard(ctx, summary, role, userID)
	}

	// The per-platform count follows the caller's scope for the same reason
	// the VM list does: a scoped reader must not learn how many machines exist
	// beyond their grants by reading a total.
	countScope := "v.sync_state <> 'deleted'"
	countArgs := []any{}
	if clause, scoped := scopeClause(role, 1); scoped {
		countArgs = append(countArgs, userID)
		countScope += " AND " + clause
	}

	platforms, err := q.db.Query(ctx, `
		SELECT p.id, p.name, p.datacenter, p.health::text, coalesce(p.detected_version,''),
		       p.last_seen_at, p.breaker_open_until,
		       (SELECT count(*) FROM vms v
		         WHERE v.platform_id = p.id AND `+countScope+`)
		FROM platforms p WHERE p.deleted_at IS NULL ORDER BY p.name`, countArgs...)
	if err != nil {
		return ports.DashboardSummary{}, fmt.Errorf("dashboard platforms: %w", err)
	}
	defer platforms.Close()

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

	return q.finishDashboard(ctx, summary, role, userID)
}

// finishDashboard adds the parts every role sees, whatever their scope.
func (q *InventoryQuery) finishDashboard(ctx context.Context, summary ports.DashboardSummary, role identity.Role, userID uuid.UUID) (ports.DashboardSummary, error) {
	var err error
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

// AllHostNames maps every live node to its name, for the same reason and with
// the same scoping as AllVMNames: node alerts are evaluated over the estate.
func (q *InventoryQuery) AllHostNames(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := q.db.Query(ctx, `
		SELECT h.id, h.name FROM hosts h
		  JOIN platforms p ON p.id = h.platform_id
		 WHERE h.deleted_at IS NULL AND p.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list node names: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]string{}
	for rows.Next() {
		var (
			id   uuid.UUID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan node name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

// AllVMNames maps every live VM to its name, unscoped by grants. The alert
// evaluator is not acting for a user: it evaluates the whole estate and the
// resulting notification is routed by rule, not by who can see the VM.
func (q *InventoryQuery) AllVMNames(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := q.db.Query(ctx, `
		SELECT v.id, v.name FROM vms v
		  JOIN platforms p ON p.id = v.platform_id
		 WHERE v.deleted_at IS NULL AND p.deleted_at IS NULL`)
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

// MachineAddresses returns every live VM with its addresses.
//
// Unscoped by role on purpose: its only caller is the edge panel, which is
// admin-only, and an address-to-VM lookup that silently omitted machines would
// report a rule as pointing at nothing when it points at something the caller
// simply cannot see. That would be a worse answer than no answer.
//
// The whole set is loaded rather than queried per address because the design
// caps this at a few hundred VMs, and a join against a JSON array of addresses
// is more machinery than the problem deserves.
func (q *InventoryQuery) MachineAddresses(ctx context.Context) ([]publish.MachineRef, error) {
	rows, err := q.db.Query(ctx, `
		SELECT id, name, coalesce(ip_addresses, '[]'::jsonb), state, platform_id
		FROM vms WHERE sync_state <> 'deleted'`)
	if err != nil {
		return nil, fmt.Errorf("list machine addresses: %w", err)
	}
	defer rows.Close()

	var out []publish.MachineRef
	for rows.Next() {
		var (
			m     publish.MachineRef
			id    uuid.UUID
			plat  uuid.UUID
			addrs []byte
		)
		if err := rows.Scan(&id, &m.Name, &addrs, &m.State, &plat); err != nil {
			return nil, err
		}
		m.ID, m.PlatformID = id.String(), plat.String()
		if len(addrs) > 0 {
			if err := json.Unmarshal(addrs, &m.Addresses); err != nil {
				// A VM with unreadable addresses is still a VM; dropping the
				// whole listing over one bad row would be the wrong trade.
				m.Addresses = nil
			}
		}
		m.IsReachable = m.State == "running"
		out = append(out, m)
	}
	return out, rows.Err()
}
