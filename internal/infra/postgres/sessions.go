package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// SessionRepository is the Postgres implementation of ports.SessionRepository.
type SessionRepository struct{ db *Pool }

// NewSessionRepository builds the repository.
func NewSessionRepository(db *Pool) *SessionRepository { return &SessionRepository{db: db} }

const sessionColumns = `id, user_id, family_id, refresh_token_hash, host(ip), user_agent,
	expires_at, rotated_at, revoked_at, created_at`

// Create stores a new session.
func (r *SessionRepository) Create(ctx context.Context, s *identity.Session) error {
	return insertSession(ctx, r.db, s)
}

// GetByTokenHash loads the session a refresh token belongs to.
func (r *SessionRepository) GetByTokenHash(ctx context.Context, hash []byte) (*identity.Session, error) {
	var (
		s                    identity.Session
		ip, userAgent        *string
		rotatedAt, revokedAt *time.Time
	)
	err := r.db.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE refresh_token_hash = $1`, hash).
		Scan(&s.ID, &s.UserID, &s.FamilyID, &s.TokenHash, &ip, &userAgent,
			&s.ExpiresAt, &rotatedAt, &revokedAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if ip != nil {
		s.IP = *ip
	}
	if userAgent != nil {
		s.UserAgent = *userAgent
	}
	s.RotatedAt = derefTime(rotatedAt)
	s.RevokedAt = derefTime(revokedAt)
	return &s, nil
}

// Rotate spends the old session and stores its successor atomically.
func (r *SessionRepository) Rotate(ctx context.Context, old, next *identity.Session) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The rotated_at guard makes rotation idempotent under concurrency: two
	// racing refreshes cannot both succeed, and the loser is treated as reuse.
	tag, err := tx.Exec(ctx, `UPDATE sessions SET rotated_at = $2 WHERE id = $1 AND rotated_at IS NULL`,
		old.ID, old.RotatedAt)
	if err != nil {
		return fmt.Errorf("mark session rotated: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return identity.ErrRefreshTokenReuse
	}
	if err := insertSessionTx(ctx, tx, next); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rotate: %w", err)
	}
	return nil
}

// RevokeFamily revokes every session in a rotation chain (reuse detection).
func (r *SessionRepository) RevokeFamily(ctx context.Context, familyID uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`, familyID, at)
	if err != nil {
		return fmt.Errorf("revoke session family: %w", err)
	}
	return nil
}

// RevokeAllForUser revokes every session a user holds (logout everywhere,
// deactivation, password reset).
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, at)
	if err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// IsSessionActive reports whether the session backing an access token is still
// usable. This is what makes deactivation take effect immediately instead of
// waiting for the access token to expire.
func (r *SessionRepository) IsSessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	var active bool
	err := r.db.QueryRow(ctx, `
		SELECT s.revoked_at IS NULL AND s.expires_at > now() AND u.is_active
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1`, sessionID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}
	return active, nil
}

func insertSession(ctx context.Context, db *Pool, s *identity.Session) error {
	_, err := db.Exec(ctx, insertSessionSQL, insertSessionArgs(s)...)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func insertSessionTx(ctx context.Context, tx pgx.Tx, s *identity.Session) error {
	_, err := tx.Exec(ctx, insertSessionSQL, insertSessionArgs(s)...)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

const insertSessionSQL = `
	INSERT INTO sessions (id, user_id, family_id, refresh_token_hash, ip, user_agent, expires_at, created_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

func insertSessionArgs(s *identity.Session) []any {
	return []any{s.ID, s.UserID, s.FamilyID, s.TokenHash, nullIP(s.IP), nullString(s.UserAgent), s.ExpiresAt, s.CreatedAt}
}

// nullIP normalizes a client address for the inet column, dropping anything
// unparseable rather than failing the write.
func nullIP(s string) *string {
	if s == "" {
		return nil
	}
	if addrPort, err := netip.ParseAddrPort(s); err == nil {
		host := addrPort.Addr().String()
		return &host
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		host := addr.String()
		return &host
	}
	return nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
