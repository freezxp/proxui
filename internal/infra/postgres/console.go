package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/console"
)

// ConsoleRepository records console sessions.
type ConsoleRepository struct{ db *Pool }

// NewConsoleRepository builds the repository.
func NewConsoleRepository(db *Pool) *ConsoleRepository { return &ConsoleRepository{db: db} }

// Create records a session at the moment it is requested, before any bytes
// flow. If the bridge then crashes, the attempt is still on the record - an
// audit trail that only logs successful connections is not an audit trail.
func (r *ConsoleRepository) Create(ctx context.Context, s *console.Session) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO console_sessions (id, user_id, vm_id, kind, client_ip, user_agent, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		s.ID, s.UserID, s.VMID, string(s.Kind), nullIP(s.ClientIP), nullString(s.UserAgent), s.StartedAt)
	if err != nil {
		return fmt.Errorf("create console session: %w", err)
	}
	return nil
}

// MarkConnected records that the bridge reached the platform.
func (r *ConsoleRepository) MarkConnected(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE console_sessions SET connected_at=$2 WHERE id=$1`, id, at)
	if err != nil {
		return fmt.Errorf("mark console connected: %w", err)
	}
	return nil
}

// Close finalizes a session with its outcome and traffic counters.
func (r *ConsoleRepository) Close(ctx context.Context, id uuid.UUID, reason string, tx, rx int64, at time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE console_sessions
		SET ended_at=$2, close_reason=$3, bytes_tx=$4, bytes_rx=$5
		WHERE id=$1 AND ended_at IS NULL`, id, at, reason, tx, rx)
	if err != nil {
		return fmt.Errorf("close console session: %w", err)
	}
	return nil
}

// Get loads one session.
func (r *ConsoleRepository) Get(ctx context.Context, id uuid.UUID) (*console.Session, error) {
	s, err := scanConsoleSession(r.db.QueryRow(ctx, `
		SELECT id, user_id, vm_id, kind, coalesce(host(client_ip),''), coalesce(user_agent,''),
		       started_at, connected_at, ended_at, close_reason, bytes_tx, bytes_rx
		FROM console_sessions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return s, err
}

// List returns recent sessions, optionally only the open ones.
func (r *ConsoleRepository) List(ctx context.Context, activeOnly bool, limit int) ([]ports.ConsoleSessionRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT c.id, c.user_id, coalesce(u.username,'(deleted)'), c.vm_id, coalesce(v.name,'(deleted)'),
		       c.kind, coalesce(host(c.client_ip),''), c.started_at, c.connected_at, c.ended_at,
		       c.close_reason, c.bytes_tx, c.bytes_rx
		FROM console_sessions c
		LEFT JOIN users u ON u.id = c.user_id
		LEFT JOIN vms v ON v.id = c.vm_id
		WHERE ($1 = false OR c.ended_at IS NULL)
		ORDER BY c.started_at DESC LIMIT $2`, activeOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list console sessions: %w", err)
	}
	defer rows.Close()

	out := []ports.ConsoleSessionRecord{}
	for rows.Next() {
		var (
			rec              ports.ConsoleSessionRecord
			connected, ended *time.Time
		)
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.Username, &rec.VMID, &rec.VMName,
			&rec.Kind, &rec.ClientIP, &rec.StartedAt, &connected, &ended,
			&rec.CloseReason, &rec.BytesTx, &rec.BytesRx); err != nil {
			return nil, fmt.Errorf("scan console session: %w", err)
		}
		rec.ConnectedAt = derefTime(connected)
		rec.EndedAt = derefTime(ended)
		rec.Active = ended == nil
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanConsoleSession(s scanner) (*console.Session, error) {
	var (
		sess             console.Session
		kind             string
		connected, ended *time.Time
	)
	err := s.Scan(&sess.ID, &sess.UserID, &sess.VMID, &kind, &sess.ClientIP, &sess.UserAgent,
		&sess.StartedAt, &connected, &ended, &sess.CloseReason, &sess.BytesTx, &sess.BytesRx)
	if err != nil {
		return nil, err
	}
	sess.Kind = console.Kind(kind)
	sess.ConnectedAt = derefTime(connected)
	sess.EndedAt = derefTime(ended)
	return &sess, nil
}
