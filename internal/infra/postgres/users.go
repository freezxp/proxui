package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// UserRepository is the Postgres implementation of ports.UserRepository.
type UserRepository struct{ db *Pool }

// NewUserRepository builds the repository.
func NewUserRepository(db *Pool) *UserRepository { return &UserRepository{db: db} }

const userColumns = `id, username, email, display_name, password_hash, role, is_active,
	must_change_password, totp_enabled, failed_login_count, last_failed_at,
	locked_until, last_login_at, created_at, updated_at`

// Create inserts a new user.
func (r *UserRepository) Create(ctx context.Context, u *identity.User) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO users (id, username, email, display_name, password_hash, role,
			is_active, must_change_password, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`,
		u.ID, u.Username, u.Email, u.DisplayName, u.PasswordHash, string(u.Role),
		u.IsActive, u.MustChangePassword, u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s already exists", ports.ErrConflict, pgErr.ConstraintName)
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetByID loads a user by primary key.
func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*identity.User, error) {
	return r.scanOne(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
}

// GetByUsername loads a user by username (case-insensitive via citext).
func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*identity.User, error) {
	return r.scanOne(ctx, `SELECT `+userColumns+` FROM users WHERE username = $1`, username)
}

// Update persists mutable user state, including authentication counters.
func (r *UserRepository) Update(ctx context.Context, u *identity.User) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE users SET
			email = $2, display_name = $3, password_hash = $4, role = $5,
			is_active = $6, must_change_password = $7, totp_enabled = $8,
			failed_login_count = $9, last_failed_at = $10, locked_until = $11,
			last_login_at = $12, updated_at = now()
		WHERE id = $1`,
		u.ID, u.Email, u.DisplayName, u.PasswordHash, string(u.Role),
		u.IsActive, u.MustChangePassword, u.TOTPEnabled,
		u.FailedLoginCount, nullTime(u.LastFailedAt), nullTime(u.LockedUntil), nullTime(u.LastLoginAt))
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// CountAll returns the number of accounts, used by first-run bootstrap.
func (r *UserRepository) CountAll(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// List returns users matching the filter, newest accounts last.
func (r *UserRepository) List(ctx context.Context, f ports.UserFilter) ([]*identity.User, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+userColumns+` FROM users
		WHERE ($1 = '' OR username ILIKE '%'||$1||'%' OR email ILIKE '%'||$1||'%' OR display_name ILIKE '%'||$1||'%')
		  AND ($2 = '' OR role::text = $2)
		  AND ($3::boolean IS NULL OR is_active = $3)
		ORDER BY username`, f.Query, f.Role, f.Active)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*identity.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanUser(s scanner) (*identity.User, error) {
	var (
		u                                  identity.User
		role                               string
		lastFailed, lockedUntil, lastLogin *time.Time
	)
	err := s.Scan(
		&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &u.IsActive,
		&u.MustChangePassword, &u.TOTPEnabled, &u.FailedLoginCount, &lastFailed,
		&lockedUntil, &lastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.Role = identity.Role(role)
	u.LastFailedAt = derefTime(lastFailed)
	u.LockedUntil = derefTime(lockedUntil)
	u.LastLoginAt = derefTime(lastLogin)
	return &u, nil
}

func (r *UserRepository) scanOne(ctx context.Context, query string, args ...any) (*identity.User, error) {
	var (
		u                                  identity.User
		role                               string
		lastFailed, lockedUntil, lastLogin *time.Time
	)
	err := r.db.QueryRow(ctx, query, args...).Scan(
		&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &u.IsActive,
		&u.MustChangePassword, &u.TOTPEnabled, &u.FailedLoginCount, &lastFailed,
		&lockedUntil, &lastLogin, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	u.Role = identity.Role(role)
	u.LastFailedAt = derefTime(lastFailed)
	u.LockedUntil = derefTime(lockedUntil)
	u.LastLoginAt = derefTime(lastLogin)
	return &u, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return (*t).UTC()
}
