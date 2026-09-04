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
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete platform: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE platforms SET deleted_at=$2, is_enabled=false WHERE id=$1 AND deleted_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("delete platform: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}

	// The platform's inventory goes with it. Soft-deleting only the platform
	// row leaves its VMs, hosts, storage and networks visible everywhere the
	// portal looks, attributed to a platform the administrator can no longer
	// see. Every one of these tables is synced data the platform owned, so
	// none of it outlives its source.
	for _, table := range []string{"vms", "hosts", "storage_pools", "networks"} {
		if _, err := tx.Exec(ctx,
			`UPDATE `+table+` SET deleted_at=$2 WHERE platform_id=$1 AND deleted_at IS NULL`,
			id, at); err != nil {
			return fmt.Errorf("delete platform %s: %w", table, err)
		}
	}
	return tx.Commit(ctx)
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

// Endpoints lists the addresses a platform answers on, configured first
// (ADR 0009). The configured endpoint is not stored here — it lives in
// platforms.endpoint_url and the sync engine puts it at the head of the list —
// so an empty result is the normal state of a platform nothing has discovered
// yet, not an error.
func (r *PlatformRepository) Endpoints(ctx context.Context, platformID uuid.UUID) ([]ports.PlatformEndpoint, error) {
	rows, err := r.db.Query(ctx, `
		SELECT address, fingerprint, source, refreshed_at
		FROM platform_endpoints
		WHERE platform_id = $1
		ORDER BY source = 'configured' DESC, address`, platformID)
	if err != nil {
		return nil, fmt.Errorf("list platform endpoints: %w", err)
	}
	defer rows.Close()

	var out []ports.PlatformEndpoint
	for rows.Next() {
		var ep ports.PlatformEndpoint
		if err := rows.Scan(&ep.Address, &ep.Fingerprint, &ep.Source, &ep.RefreshedAt); err != nil {
			return nil, fmt.Errorf("scan platform endpoint: %w", err)
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// ReplaceEndpoints rewrites a platform's discovered addresses.
//
// Wholesale replacement rather than an upsert, because the list is a statement
// about the cluster as it is now: a member that has been removed, renamed or
// taken offline must leave the list, or the portal keeps a dead address it will
// spend a timeout on during every future failover. Replacing nothing with
// nothing is left alone so a failed discovery cannot empty the list.
func (r *PlatformRepository) ReplaceEndpoints(ctx context.Context, platformID uuid.UUID, eps []ports.PlatformEndpoint, at time.Time) error {
	if len(eps) == 0 {
		return nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace endpoints: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM platform_endpoints WHERE platform_id = $1`, platformID); err != nil {
		return fmt.Errorf("clear platform endpoints: %w", err)
	}
	for _, ep := range eps {
		source := ep.Source
		if source == "" {
			source = "discovered"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO platform_endpoints (platform_id, address, fingerprint, source, refreshed_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (platform_id, address) DO UPDATE
			SET fingerprint = EXCLUDED.fingerprint,
			    source      = EXCLUDED.source,
			    refreshed_at = EXCLUDED.refreshed_at`,
			platformID, ep.Address, ep.Fingerprint, source, at); err != nil {
			return fmt.Errorf("store platform endpoint: %w", err)
		}
	}
	return tx.Commit(ctx)
}
