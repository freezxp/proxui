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
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// PlatformRepository persists platforms and their sealed credentials.
type PlatformRepository struct{ db *Pool }

// NewPlatformRepository builds the repository.
func NewPlatformRepository(db *Pool) *PlatformRepository { return &PlatformRepository{db: db} }

const platformColumns = `id, name, type, endpoint_url, datacenter, is_enabled, tls_mode,
	coalesce(tls_ca_pem,''), coalesce(tls_fingerprint,''), config, sync_intervals,
	health, health_detail, detected_version, last_seen_at,
	consecutive_failures, breaker_open_until, created_at, updated_at, deleted_at`

// Create stores a platform and its credential in one transaction: a platform
// without its credential would be unusable, and a credential without its
// platform would be an orphaned secret.
func (r *PlatformRepository) Create(ctx context.Context, p *inventory.Platform, cred ports.SealedCredential) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create platform: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	config, err := json.Marshal(p.Config)
	if err != nil {
		return fmt.Errorf("encode platform config: %w", err)
	}
	intervals, err := json.Marshal(p.SyncIntervals)
	if err != nil {
		return fmt.Errorf("encode sync intervals: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO platforms (id, name, type, endpoint_url, datacenter, is_enabled,
			tls_mode, tls_ca_pem, tls_fingerprint, config, sync_intervals, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
		p.ID, p.Name, p.Type, p.EndpointURL, p.Datacenter, p.IsEnabled,
		nullEmpty(p.TLSMode, "verify"), nullString(p.TLSCAPEM), nullString(p.TLSFingerprint),
		config, intervals, p.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: a platform named %q already exists", ports.ErrConflict, p.Name)
		}
		return fmt.Errorf("create platform: %w", err)
	}

	if err := insertCredential(ctx, tx, p.ID, cred); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create platform: %w", err)
	}
	return nil
}

func insertCredential(ctx context.Context, tx pgx.Tx, platformID uuid.UUID, cred ports.SealedCredential) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_credentials (id, platform_id, kind, token_id,
			ciphertext, nonce, dek_wrapped, dek_nonce, key_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (platform_id) DO UPDATE SET
			kind = EXCLUDED.kind, token_id = EXCLUDED.token_id,
			ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce,
			dek_wrapped = EXCLUDED.dek_wrapped, dek_nonce = EXCLUDED.dek_nonce,
			key_version = EXCLUDED.key_version, rotated_at = now()`,
		uuid.New(), platformID, cred.Kind, cred.TokenID,
		cred.Sealed.Ciphertext, cred.Sealed.Nonce, cred.Sealed.DEKWrapped,
		cred.Sealed.DEKNonce, cred.Sealed.KeyVersion)
	if err != nil {
		return fmt.Errorf("store credential: %w", err)
	}
	return nil
}

// Get loads a platform by id.
func (r *PlatformRepository) Get(ctx context.Context, id uuid.UUID) (*inventory.Platform, error) {
	return r.scanOne(ctx, `SELECT `+platformColumns+` FROM platforms WHERE id = $1 AND deleted_at IS NULL`, id)
}

// List returns platforms, optionally including disabled ones.
func (r *PlatformRepository) List(ctx context.Context, includeDisabled bool) ([]*inventory.Platform, error) {
	rows, err := r.db.Query(ctx, `SELECT `+platformColumns+`
		FROM platforms WHERE deleted_at IS NULL AND ($1 OR is_enabled) ORDER BY name`, includeDisabled)
	if err != nil {
		return nil, fmt.Errorf("list platforms: %w", err)
	}
	defer rows.Close()

	var out []*inventory.Platform
	for rows.Next() {
		p, err := scanPlatform(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update persists mutable platform configuration.
func (r *PlatformRepository) Update(ctx context.Context, p *inventory.Platform) error {
	config, err := json.Marshal(p.Config)
	if err != nil {
		return fmt.Errorf("encode platform config: %w", err)
	}
	intervals, err := json.Marshal(p.SyncIntervals)
	if err != nil {
		return fmt.Errorf("encode sync intervals: %w", err)
	}

	tag, err := r.db.Exec(ctx, `
		UPDATE platforms SET name=$2, endpoint_url=$3, datacenter=$4, is_enabled=$5,
			tls_mode=$6, tls_ca_pem=$7, tls_fingerprint=$8, config=$9, sync_intervals=$10,
			updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL`,
		p.ID, p.Name, p.EndpointURL, p.Datacenter, p.IsEnabled,
		nullEmpty(p.TLSMode, "verify"), nullString(p.TLSCAPEM), nullString(p.TLSFingerprint),
		config, intervals)
	if err != nil {
		return fmt.Errorf("update platform: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// UpdateHealth persists health and circuit-breaker state after a sync attempt.
func (r *PlatformRepository) UpdateHealth(ctx context.Context, p *inventory.Platform) error {
	_, err := r.db.Exec(ctx, `
		UPDATE platforms SET health=$2, health_detail=$3, detected_version=$4,
			last_seen_at=$5, consecutive_failures=$6, breaker_open_until=$7, updated_at=now()
		WHERE id=$1`,
		p.ID, string(p.Health), p.HealthDetail, p.DetectedVersion,
		nullTime(p.LastSeenAt), p.ConsecutiveFailures, nullTime(p.BreakerOpenUntil))
	if err != nil {
		return fmt.Errorf("update platform health: %w", err)
	}
	return nil
}

// SoftDelete marks a platform deleted, keeping its assets resolvable for
// history and audit until the janitor purges them.
func (r *PlatformRepository) SoftDelete(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE platforms SET deleted_at=$2, is_enabled=false WHERE id=$1 AND deleted_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("delete platform: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Credential loads and unseals a platform's secret. The plaintext is returned
// to a caller-scoped variable and never cached.
func (r *PlatformRepository) Credential(ctx context.Context, platformID uuid.UUID, vault *crypto.Vault) (ports.PlainCredential, error) {
	var (
		kind, tokenID string
		sealed        crypto.SealedSecret
	)
	err := r.db.QueryRow(ctx, `
		SELECT kind, token_id, ciphertext, nonce, dek_wrapped, dek_nonce, key_version
		FROM platform_credentials WHERE platform_id = $1`, platformID).
		Scan(&kind, &tokenID, &sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce, &sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.PlainCredential{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.PlainCredential{}, fmt.Errorf("load credential: %w", err)
	}

	secret, err := vault.Open(sealed)
	if err != nil {
		return ports.PlainCredential{}, err
	}
	return ports.PlainCredential{Kind: kind, TokenID: tokenID, Secret: secret}, nil
}

// ReplaceCredential rotates a platform's stored secret.
func (r *PlatformRepository) ReplaceCredential(ctx context.Context, platformID uuid.UUID, cred ports.SealedCredential) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rotate credential: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertCredential(ctx, tx, platformID, cred); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rotate credential: %w", err)
	}
	return nil
}

func (r *PlatformRepository) scanOne(ctx context.Context, query string, args ...any) (*inventory.Platform, error) {
	p, err := scanPlatform(r.db.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return p, err
}

func scanPlatform(s scanner) (*inventory.Platform, error) {
	var (
		p                             inventory.Platform
		health                        string
		config, intervals             []byte
		lastSeen, breakerUntil, delAt *time.Time
	)
	err := s.Scan(&p.ID, &p.Name, &p.Type, &p.EndpointURL, &p.Datacenter, &p.IsEnabled,
		&p.TLSMode, &p.TLSCAPEM, &p.TLSFingerprint, &config, &intervals,
		&health, &p.HealthDetail, &p.DetectedVersion, &lastSeen,
		&p.ConsecutiveFailures, &breakerUntil, &p.CreatedAt, &p.UpdatedAt, &delAt)
	if err != nil {
		return nil, err
	}

	p.Health = inventory.Health(health)
	p.LastSeenAt = derefTime(lastSeen)
	p.BreakerOpenUntil = derefTime(breakerUntil)
	p.DeletedAt = derefTime(delAt)
	if len(config) > 0 {
		if err := json.Unmarshal(config, &p.Config); err != nil {
			return nil, fmt.Errorf("decode platform config: %w", err)
		}
	}
	if len(intervals) > 0 {
		if err := json.Unmarshal(intervals, &p.SyncIntervals); err != nil {
			return nil, fmt.Errorf("decode sync intervals: %w", err)
		}
	}
	return &p, nil
}

func nullEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
