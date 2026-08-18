package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// ShellRepository records SSH sessions (SSH-06).
type ShellRepository struct{ db *Pool }

// NewShellRepository builds the repository.
func NewShellRepository(db *Pool) *ShellRepository { return &ShellRepository{db: db} }

// Create records a session at the moment the connection is established. A
// failed authentication never gets here - it has no session - but it is
// audited, so the trail shows the attempt either way.
func (r *ShellRepository) Create(ctx context.Context, s *shell.Session) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO ssh_sessions (id, user_id, vm_id, ssh_user, address, client_ip, user_agent, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		s.ID, s.UserID, s.VMID, s.SSHUser, s.Address,
		nullIP(s.ClientIP), nullString(s.UserAgent), s.StartedAt)
	if err != nil {
		return fmt.Errorf("create ssh session: %w", err)
	}
	return nil
}

// MarkConnected records that a terminal actually attached. A session can exist
// without one: the file browser alone is a legitimate use.
func (r *ShellRepository) MarkConnected(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE ssh_sessions SET connected_at=$2 WHERE id=$1 AND connected_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("mark ssh session connected: %w", err)
	}
	return nil
}

// Close finalizes a session with its outcome and traffic counters.
func (r *ShellRepository) Close(ctx context.Context, id uuid.UUID, reason string, tx, rx int64, at time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE ssh_sessions
		SET ended_at=$2, close_reason=$3, bytes_tx=$4, bytes_rx=$5
		WHERE id=$1 AND ended_at IS NULL`, id, at, reason, tx, rx)
	if err != nil {
		return fmt.Errorf("close ssh session: %w", err)
	}
	return nil
}

// Get loads one session.
func (r *ShellRepository) Get(ctx context.Context, id uuid.UUID) (*shell.Session, error) {
	var (
		s                shell.Session
		connected, ended *time.Time
	)
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id, vm_id, ssh_user, address, coalesce(host(client_ip),''),
		       coalesce(user_agent,''), started_at, connected_at, ended_at,
		       close_reason, bytes_tx, bytes_rx
		FROM ssh_sessions WHERE id = $1`, id).
		Scan(&s.ID, &s.UserID, &s.VMID, &s.SSHUser, &s.Address, &s.ClientIP,
			&s.UserAgent, &s.StartedAt, &connected, &ended,
			&s.CloseReason, &s.BytesTx, &s.BytesRx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ssh session: %w", err)
	}
	s.ConnectedAt = derefTime(connected)
	s.EndedAt = derefTime(ended)
	return &s, nil
}

// List returns recent sessions, optionally only the open ones.
//
// "Open" here means the database thinks so. A process that was killed leaves
// rows with no end time, which is why the administrator's view is fed by this
// query joined against the live registry rather than by either alone.
func (r *ShellRepository) List(ctx context.Context, activeOnly bool, limit int) ([]ports.ShellSessionRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT s.id, s.user_id, coalesce(u.username,'(deleted)'), s.vm_id, coalesce(v.name,'(deleted)'),
		       s.ssh_user, s.address, coalesce(host(s.client_ip),''), s.started_at, s.connected_at,
		       s.ended_at, s.close_reason, s.bytes_tx, s.bytes_rx
		FROM ssh_sessions s
		LEFT JOIN users u ON u.id = s.user_id
		LEFT JOIN vms v ON v.id = s.vm_id
		WHERE ($1 = false OR s.ended_at IS NULL)
		ORDER BY s.started_at DESC LIMIT $2`, activeOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list ssh sessions: %w", err)
	}
	defer rows.Close()

	out := []ports.ShellSessionRecord{}
	for rows.Next() {
		var (
			rec              ports.ShellSessionRecord
			connected, ended *time.Time
		)
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.Username, &rec.VMID, &rec.VMName,
			&rec.SSHUser, &rec.Address, &rec.ClientIP, &rec.StartedAt, &connected, &ended,
			&rec.CloseReason, &rec.BytesTx, &rec.BytesRx); err != nil {
			return nil, fmt.Errorf("scan ssh session: %w", err)
		}
		rec.ConnectedAt = derefTime(connected)
		rec.EndedAt = derefTime(ended)
		rec.Active = ended == nil
		out = append(out, rec)
	}
	return out, rows.Err()
}

// HostKeyStore pins one host key per VM (SSH-04).
type HostKeyStore struct{ db *Pool }

// NewHostKeyStore builds the store.
func NewHostKeyStore(db *Pool) *HostKeyStore { return &HostKeyStore{db: db} }

// Get returns the pinned key, or ErrNotFound for a VM never connected to.
func (s *HostKeyStore) Get(ctx context.Context, vmID uuid.UUID) (shell.HostKey, error) {
	var (
		key       shell.HostKey
		trustedBy *uuid.UUID
	)
	err := s.db.QueryRow(ctx, `
		SELECT vm_id, address, algorithm, fingerprint, public_key, first_seen_at, trusted_by
		FROM ssh_known_hosts WHERE vm_id = $1`, vmID).
		Scan(&key.VMID, &key.Address, &key.Algorithm, &key.Fingerprint,
			&key.PublicKey, &key.FirstSeenAt, &trustedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return shell.HostKey{}, ports.ErrNotFound
	}
	if err != nil {
		return shell.HostKey{}, fmt.Errorf("get host key: %w", err)
	}
	if trustedBy != nil {
		key.TrustedBy = *trustedBy
	}
	return key, nil
}

// Trust pins a key.
//
// ON CONFLICT DO NOTHING rather than an upsert, deliberately: an existing pin
// must never be quietly overwritten, because overwriting is exactly what an
// attacker who has just been detected would want. Replacing a pin goes through
// Forget, which is an explicit act with an audit entry behind it.
func (s *HostKeyStore) Trust(ctx context.Context, key shell.HostKey) error {
	var trustedBy *uuid.UUID
	if key.TrustedBy != uuid.Nil {
		id := key.TrustedBy
		trustedBy = &id
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO ssh_known_hosts (vm_id, address, algorithm, fingerprint, public_key, first_seen_at, trusted_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (vm_id) DO NOTHING`,
		key.VMID, key.Address, key.Algorithm, key.Fingerprint, key.PublicKey,
		key.FirstSeenAt, trustedBy)
	if err != nil {
		return fmt.Errorf("trust host key: %w", err)
	}
	return nil
}

// Forget drops the pin so the next connection starts the trust decision again.
func (s *HostKeyStore) Forget(ctx context.Context, vmID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM ssh_known_hosts WHERE vm_id = $1`, vmID)
	if err != nil {
		return fmt.Errorf("forget host key: %w", err)
	}
	return nil
}
