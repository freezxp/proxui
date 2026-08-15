package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// EdgeProviderRepository persists edge providers and their sealed credentials.
type EdgeProviderRepository struct{ db *Pool }

// NewEdgeProviderRepository builds the repository.
func NewEdgeProviderRepository(db *Pool) *EdgeProviderRepository {
	return &EdgeProviderRepository{db: db}
}

const edgeProviderColumns = `id, name, kind, account_id, coalesce(tunnel_id,''), tunnel_name,
	allowed_zone_ids, is_enabled, health, health_detail, last_seen_at,
	consecutive_failures, breaker_open_until, created_at, updated_at, deleted_at`

// Create stores a provider and its credential in one transaction.
//
// Together, because a provider without its credential is unusable and a
// credential without its provider is an unopenable secret nobody can point at
// — the same reason platforms do it this way.
func (r *EdgeProviderRepository) Create(ctx context.Context, p *publish.Provider, cred ports.SealedCredential) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create edge provider: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	zones, err := json.Marshal(nonNilZones(p.AllowedZoneIDs))
	if err != nil {
		return fmt.Errorf("encode allowed zones: %w", err)
	}

	{
		_, err = tx.Exec(ctx, `
			INSERT INTO edge_providers (id, name, kind, account_id, tunnel_id, tunnel_name,
				allowed_zone_ids, is_enabled, health, health_detail, created_at, updated_at)
			VALUES ($1,$2,$3,$4,nullif($5,''),$6,$7,$8,$9,$10,$11,$11)`,
			p.ID, p.Name, p.Kind, p.AccountID, p.TunnelID, p.TunnelName,
			zones, p.IsEnabled, string(p.Health), p.HealthDetail, p.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ports.ErrConflict
			}
			return fmt.Errorf("insert edge provider: %w", err)
		}
	}
	if err := insertEdgeCredential(ctx, tx, p.ID, cred); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create edge provider: %w", err)
	}
	return nil
}

func insertEdgeCredential(ctx context.Context, tx pgx.Tx, providerID uuid.UUID, cred ports.SealedCredential) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO edge_credentials (id, provider_id, ciphertext, nonce,
			dek_wrapped, dek_nonce, key_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider_id) DO UPDATE SET
			ciphertext = excluded.ciphertext, nonce = excluded.nonce,
			dek_wrapped = excluded.dek_wrapped, dek_nonce = excluded.dek_nonce,
			key_version = excluded.key_version, updated_at = now()`,
		uuid.New(), providerID, cred.Sealed.Ciphertext, cred.Sealed.Nonce,
		cred.Sealed.DEKWrapped, cred.Sealed.DEKNonce, cred.Sealed.KeyVersion)
	if err != nil {
		return fmt.Errorf("store edge credential: %w", err)
	}
	return nil
}

// List returns live providers, oldest first.
func (r *EdgeProviderRepository) List(ctx context.Context) ([]*publish.Provider, error) {
	rows, err := r.db.Query(ctx, `SELECT `+edgeProviderColumns+`
		FROM edge_providers WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list edge providers: %w", err)
	}
	defer rows.Close()

	var out []*publish.Provider
	for rows.Next() {
		p, err := scanEdgeProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one live provider.
func (r *EdgeProviderRepository) Get(ctx context.Context, id uuid.UUID) (*publish.Provider, error) {
	row := r.db.QueryRow(ctx, `SELECT `+edgeProviderColumns+`
		FROM edge_providers WHERE id = $1 AND deleted_at IS NULL`, id)
	p, err := scanEdgeProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return p, err
}

// Update writes the mutable fields. The credential is not among them: it is
// replaced through ReplaceCredential so that an ordinary edit cannot blank a
// secret by omitting it.
func (r *EdgeProviderRepository) Update(ctx context.Context, p *publish.Provider) error {
	zones, err := json.Marshal(nonNilZones(p.AllowedZoneIDs))
	if err != nil {
		return fmt.Errorf("encode allowed zones: %w", err)
	}

	tag, err := r.db.Exec(ctx, `
		UPDATE edge_providers SET name = $2, account_id = $3, tunnel_id = nullif($4,''),
			tunnel_name = $5, allowed_zone_ids = $6, is_enabled = $7, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		p.ID, p.Name, p.AccountID, p.TunnelID, p.TunnelName, zones, p.IsEnabled)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ports.ErrConflict
		}
		return fmt.Errorf("update edge provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// RecordHealth stores the outcome of a call.
//
// Kept apart from Update because health is written by background machinery on
// a cadence, while the rest is written by an administrator; sharing one
// statement would have a sync overwrite an edit made a moment earlier.
func (r *EdgeProviderRepository) RecordHealth(ctx context.Context, id uuid.UUID,
	health publish.Health, detail string, failures int, breakerUntil time.Time) error {
	var until *time.Time
	if !breakerUntil.IsZero() {
		until = &breakerUntil
	}
	var seen *time.Time
	if health == publish.HealthHealthy {
		now := time.Now().UTC()
		seen = &now
	}

	_, err := r.db.Exec(ctx, `
		UPDATE edge_providers SET health = $2, health_detail = $3,
			consecutive_failures = $4, breaker_open_until = $5,
			last_seen_at = coalesce($6, last_seen_at), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		id, string(health), detail, failures, until, seen)
	if err != nil {
		return fmt.Errorf("record edge provider health: %w", err)
	}
	return nil
}

// Delete soft-deletes a provider.
//
// Soft, so the audit trail keeps pointing at something real. The name index is
// scoped to live rows, so the name becomes available again immediately — the
// lesson from migration 00007, where a deleted platform reserved its name
// forever and the conflict had no visible cause.
func (r *EdgeProviderRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE edge_providers SET deleted_at = now(), is_enabled = false, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete edge provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Credential opens the provider's sealed token for one call.
func (r *EdgeProviderRepository) Credential(ctx context.Context, providerID uuid.UUID,
	vault *crypto.Vault) (ports.PlainCredential, error) {
	var sealed crypto.SealedSecret
	err := r.db.QueryRow(ctx, `
		SELECT ciphertext, nonce, dek_wrapped, dek_nonce, key_version
		FROM edge_credentials WHERE provider_id = $1`, providerID).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce, &sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.PlainCredential{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.PlainCredential{}, fmt.Errorf("load edge credential: %w", err)
	}

	token, err := vault.Open(sealed)
	if err != nil {
		return ports.PlainCredential{}, err
	}
	return ports.PlainCredential{Secret: token}, nil
}

// ReplaceCredential swaps the stored token.
func (r *EdgeProviderRepository) ReplaceCredential(ctx context.Context, providerID uuid.UUID,
	cred ports.SealedCredential) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace edge credential: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertEdgeCredential(ctx, tx, providerID, cred); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit replace edge credential: %w", err)
	}
	return nil
}

// SaveSnapshot records a routing table before it is changed (PUB-34).
func (r *EdgeProviderRepository) SaveSnapshot(ctx context.Context, providerID uuid.UUID,
	tunnelID string, version int, ingress []byte, takenBy *uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO edge_config_snapshots (provider_id, tunnel_id, version, ingress, taken_by)
		VALUES ($1,$2,$3,$4,$5)`, providerID, tunnelID, version, ingress, takenBy)
	if err != nil {
		return fmt.Errorf("save edge snapshot: %w", err)
	}
	return nil
}

// LatestSnapshot returns the most recent snapshot for a tunnel.
func (r *EdgeProviderRepository) LatestSnapshot(ctx context.Context, providerID uuid.UUID,
	tunnelID string) (ports.EdgeSnapshot, error) {
	var s ports.EdgeSnapshot
	err := r.db.QueryRow(ctx, `
		SELECT id, version, ingress, taken_at FROM edge_config_snapshots
		WHERE provider_id = $1 AND tunnel_id = $2
		ORDER BY taken_at DESC LIMIT 1`, providerID, tunnelID).
		Scan(&s.ID, &s.Version, &s.Ingress, &s.TakenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.EdgeSnapshot{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.EdgeSnapshot{}, fmt.Errorf("load edge snapshot: %w", err)
	}
	return s, nil
}

func scanEdgeProvider(row scanner) (*publish.Provider, error) {
	var (
		p         publish.Provider
		tunnelID  string
		zones     []byte
		health    string
		lastSeen  *time.Time
		breaker   *time.Time
		deletedAt *time.Time
	)
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.AccountID, &tunnelID, &p.TunnelName,
		&zones, &p.IsEnabled, &health, &p.HealthDetail, &lastSeen,
		&p.ConsecutiveFailures, &breaker, &p.CreatedAt, &p.UpdatedAt, &deletedAt)
	if err != nil {
		return nil, err
	}

	p.TunnelID = tunnelID
	p.Health = publish.Health(health)
	if len(zones) > 0 {
		if err := json.Unmarshal(zones, &p.AllowedZoneIDs); err != nil {
			return nil, fmt.Errorf("decode allowed zones: %w", err)
		}
	}
	if lastSeen != nil {
		p.LastSeenAt = *lastSeen
	}
	if breaker != nil {
		p.BreakerOpenUntil = *breaker
	}
	if deletedAt != nil {
		p.DeletedAt = *deletedAt
	}
	return &p, nil
}

// nonNilZones keeps an empty list encoding as `[]` rather than `null`, so the
// column's meaning — no zone may be written — is the same whether it was set
// explicitly or left alone.
func nonNilZones(zones []string) []string {
	if zones == nil {
		return []string{}
	}
	return zones
}
