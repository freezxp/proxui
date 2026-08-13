package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
)

// AccessRepository persists groups and grants.
type AccessRepository struct{ db *Pool }

// NewAccessRepository builds the repository.
func NewAccessRepository(db *Pool) *AccessRepository { return &AccessRepository{db: db} }

// --- user groups -------------------------------------------------------

// CreateUserGroup inserts a user group.
func (r *AccessRepository) CreateUserGroup(ctx context.Context, g *access.UserGroup) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO user_groups (id, name, description, created_at) VALUES ($1,$2,$3,$4)`,
		g.ID, g.Name, g.Description, g.CreatedAt)
	return wrapConflict(err, "create user group")
}

// ListUserGroups returns all user groups with their member counts.
func (r *AccessRepository) ListUserGroups(ctx context.Context) ([]access.UserGroup, error) {
	rows, err := r.db.Query(ctx, `
		SELECT g.id, g.name, g.description, g.created_at, count(m.user_id)
		FROM user_groups g
		LEFT JOIN user_group_members m ON m.user_group_id = g.id
		GROUP BY g.id ORDER BY g.name`)
	if err != nil {
		return nil, fmt.Errorf("list user groups: %w", err)
	}
	defer rows.Close()

	var out []access.UserGroup
	for rows.Next() {
		var g access.UserGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.MemberCount); err != nil {
			return nil, fmt.Errorf("scan user group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteUserGroup removes a user group; memberships and grants cascade.
func (r *AccessRepository) DeleteUserGroup(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM user_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// SetUserGroups replaces a user's group memberships in one transaction.
func (r *AccessRepository) SetUserGroups(ctx context.Context, userID uuid.UUID, groupIDs []uuid.UUID) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin set user groups: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM user_group_members WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear user groups: %w", err)
	}
	for _, gid := range groupIDs {
		_, err := tx.Exec(ctx,
			`INSERT INTO user_group_members (user_group_id, user_id) VALUES ($1,$2)`, gid, userID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return fmt.Errorf("%w: user group %s", ports.ErrNotFound, gid)
			}
			return fmt.Errorf("add user to group: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit set user groups: %w", err)
	}
	return nil
}

// UserGroupNames returns the group names a user belongs to.
func (r *AccessRepository) UserGroupNames(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT g.name FROM user_groups g
		JOIN user_group_members m ON m.user_group_id = g.id
		WHERE m.user_id = $1 ORDER BY g.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user group names: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan group name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// --- VM groups ---------------------------------------------------------

// CreateVMGroup inserts a VM group.
func (r *AccessRepository) CreateVMGroup(ctx context.Context, g *access.VMGroup) error {
	var rule any
	if len(g.AutoRule) > 0 {
		rule = g.AutoRule
	}
	_, err := r.db.Exec(ctx,
		`INSERT INTO vm_groups (id, name, description, auto_rule, created_at) VALUES ($1,$2,$3,$4,$5)`,
		g.ID, g.Name, g.Description, rule, g.CreatedAt)
	return wrapConflict(err, "create vm group")
}

// ListVMGroups returns all VM groups.
func (r *AccessRepository) ListVMGroups(ctx context.Context) ([]access.VMGroup, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, name, description, auto_rule, created_at FROM vm_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list vm groups: %w", err)
	}
	defer rows.Close()

	var out []access.VMGroup
	for rows.Next() {
		var g access.VMGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.AutoRule, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan vm group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteVMGroup removes a VM group; grants cascade.
func (r *AccessRepository) DeleteVMGroup(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM vm_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete vm group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// --- grants ------------------------------------------------------------

// CreateGrant links a user group to a VM group.
func (r *AccessRepository) CreateGrant(ctx context.Context, g *access.Grant) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO access_grants (id, user_group_id, vm_group_id, granted_by, created_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		g.ID, g.UserGroupID, g.VMGroupID, g.GrantedBy, g.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("%w: referenced group does not exist", ports.ErrNotFound)
		}
	}
	return wrapConflict(err, "create grant")
}

// ListGrants returns all grants with both group names resolved.
func (r *AccessRepository) ListGrants(ctx context.Context) ([]access.Grant, error) {
	rows, err := r.db.Query(ctx, `
		SELECT g.id, g.user_group_id, ug.name, g.vm_group_id, vg.name, g.granted_by, g.created_at
		FROM access_grants g
		JOIN user_groups ug ON ug.id = g.user_group_id
		JOIN vm_groups  vg ON vg.id = g.vm_group_id
		ORDER BY ug.name, vg.name`)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()

	var out []access.Grant
	for rows.Next() {
		var g access.Grant
		if err := rows.Scan(&g.ID, &g.UserGroupID, &g.UserGroupName, &g.VMGroupID, &g.VMGroupName, &g.GrantedBy, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteGrant revokes a grant.
func (r *AccessRepository) DeleteGrant(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM access_grants WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// VisibleVMGroupIDs returns the VM groups a user can reach through their group
// memberships. This is the one place visibility is computed; every VM-scoped
// query builds on it (docs/07 §7.5). Admins and auditors bypass it entirely.
func (r *AccessRepository) VisibleVMGroupIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT ag.vm_group_id
		FROM access_grants ag
		JOIN user_group_members ugm ON ugm.user_group_id = ag.user_group_id
		WHERE ugm.user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve visible vm groups: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan vm group id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func wrapConflict(err error, op string) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s already exists", ports.ErrConflict, pgErr.ConstraintName)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrNotFound
	}
	return fmt.Errorf("%s: %w", op, err)
}
