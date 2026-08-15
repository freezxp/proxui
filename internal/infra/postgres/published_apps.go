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
)

// PublishedAppRepository persists published apps.
type PublishedAppRepository struct{ db *Pool }

// NewPublishedAppRepository builds the repository.
func NewPublishedAppRepository(db *Pool) *PublishedAppRepository {
	return &PublishedAppRepository{db: db}
}

const publishedAppColumns = `id, provider_id, hostname, path, service_url, vm_id,
	coalesce(vm_port,0), origin_request, coalesce(dns_zone_id,''), coalesce(dns_record_id,''),
	is_enabled, exposure_ack_by, exposure_ack_at, last_applied_at, last_error,
	created_at, updated_at, deleted_at`

// Create stores an app.
func (r *PublishedAppRepository) Create(ctx context.Context, app *publish.App) error {
	origin, err := json.Marshal(nonNilOrigin(app.OriginRequest))
	if err != nil {
		return fmt.Errorf("encode origin settings: %w", err)
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO published_apps (id, provider_id, hostname, path, service_url,
			vm_id, vm_port, origin_request, dns_zone_id, dns_record_id, is_enabled,
			exposure_ack_by, exposure_ack_at, last_applied_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,nullif($7,0),$8,nullif($9,''),nullif($10,''),$11,
			$12,$13,$14,$15,$15)`,
		app.ID, app.ProviderID, app.Hostname, app.Path, app.ServiceURL,
		app.VMID, app.VMPort, origin, app.DNSZoneID, app.DNSRecordID, app.IsEnabled,
		app.ExposureAckBy, nullTime(app.ExposureAckAt), nullTime(app.LastAppliedAt), app.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ports.ErrConflict
		}
		return fmt.Errorf("insert published app: %w", err)
	}
	return nil
}

// Get returns one live app.
func (r *PublishedAppRepository) Get(ctx context.Context, id uuid.UUID) (*publish.App, error) {
	row := r.db.QueryRow(ctx, `SELECT `+publishedAppColumns+`
		FROM published_apps WHERE id = $1 AND deleted_at IS NULL`, id)
	app, err := scanPublishedApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return app, err
}

// ListByProvider returns a provider's live apps.
func (r *PublishedAppRepository) ListByProvider(ctx context.Context, providerID uuid.UUID) ([]*publish.App, error) {
	rows, err := r.db.Query(ctx, `SELECT `+publishedAppColumns+`
		FROM published_apps WHERE provider_id = $1 AND deleted_at IS NULL
		ORDER BY hostname, path`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list published apps: %w", err)
	}
	defer rows.Close()

	var out []*publish.App
	for rows.Next() {
		app, err := scanPublishedApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// Update writes the mutable fields.
func (r *PublishedAppRepository) Update(ctx context.Context, app *publish.App) error {
	origin, err := json.Marshal(nonNilOrigin(app.OriginRequest))
	if err != nil {
		return fmt.Errorf("encode origin settings: %w", err)
	}

	tag, err := r.db.Exec(ctx, `
		UPDATE published_apps SET service_url = $2, vm_id = $3, vm_port = nullif($4,0),
			origin_request = $5, dns_zone_id = nullif($6,''), dns_record_id = nullif($7,''),
			is_enabled = $8, last_applied_at = $9, last_error = $10, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		app.ID, app.ServiceURL, app.VMID, app.VMPort, origin,
		app.DNSZoneID, app.DNSRecordID, app.IsEnabled,
		nullTime(app.LastAppliedAt), app.LastError)
	if err != nil {
		return fmt.Errorf("update published app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Delete soft-deletes an app, freeing its hostname for republication.
func (r *PublishedAppRepository) Delete(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE published_apps SET deleted_at = $2, is_enabled = false, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("delete published app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

func scanPublishedApp(row scanner) (*publish.App, error) {
	var (
		app       publish.App
		vmID      *uuid.UUID
		origin    []byte
		ackBy     *uuid.UUID
		ackAt     *time.Time
		applied   *time.Time
		deletedAt *time.Time
	)
	err := row.Scan(&app.ID, &app.ProviderID, &app.Hostname, &app.Path, &app.ServiceURL,
		&vmID, &app.VMPort, &origin, &app.DNSZoneID, &app.DNSRecordID,
		&app.IsEnabled, &ackBy, &ackAt, &applied, &app.LastError,
		&app.CreatedAt, &app.UpdatedAt, &deletedAt)
	if err != nil {
		return nil, err
	}

	app.VMID, app.ExposureAckBy = vmID, ackBy
	if len(origin) > 0 {
		if err := json.Unmarshal(origin, &app.OriginRequest); err != nil {
			return nil, fmt.Errorf("decode origin settings: %w", err)
		}
	}
	for dst, src := range map[*time.Time]*time.Time{
		&app.ExposureAckAt: ackAt, &app.LastAppliedAt: applied, &app.DeletedAt: deletedAt,
	} {
		if src != nil {
			*dst = *src
		}
	}
	return &app, nil
}

func nonNilOrigin(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
