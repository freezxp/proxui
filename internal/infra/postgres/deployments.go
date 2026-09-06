package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/deployment"
)

// DeploymentRepository persists container deployments (ADR 0012).
type DeploymentRepository struct{ db *Pool }

// NewDeploymentRepository builds the repository.
func NewDeploymentRepository(db *Pool) *DeploymentRepository {
	return &DeploymentRepository{db: db}
}

const deploymentColumns = `id, platform_id, node, app_id, app_name, state, container_id,
	requested_by, requested_by_name, spec, log, exit_code, error, created_at, updated_at`

// Create stores a new deployment in its pending state.
func (r *DeploymentRepository) Create(ctx context.Context, d *deployment.Deployment) error {
	spec, err := json.Marshal(d.Spec)
	if err != nil {
		return fmt.Errorf("encode deployment spec: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO container_deployments (`+deploymentColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		d.ID, d.PlatformID, d.Node, d.AppID, d.AppName, string(d.State), d.CTID,
		d.RequestedBy, d.RequestedByName, spec, d.Log, d.ExitCode, d.Error,
		d.Created, d.Updated)
	if err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	return nil
}

// Save writes back a deployment the driver has advanced.
//
// Everything mutable goes together: the state, the container it found, the log
// and the exit status change as one move, and a partial write would leave a row
// claiming to have finished with nothing to show for it.
func (r *DeploymentRepository) Save(ctx context.Context, d *deployment.Deployment) error {
	spec, err := json.Marshal(d.Spec)
	if err != nil {
		return fmt.Errorf("encode deployment spec: %w", err)
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE container_deployments
		SET state=$2, container_id=$3, spec=$4, log=$5, exit_code=$6, error=$7, updated_at=now()
		WHERE id=$1`,
		d.ID, string(d.State), d.CTID, spec,
		deployment.TruncateLog(d.Log), d.ExitCode, d.Error)
	if err != nil {
		return fmt.Errorf("save deployment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Get loads one deployment.
func (r *DeploymentRepository) Get(ctx context.Context, id uuid.UUID) (*deployment.Deployment, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+deploymentColumns+` FROM container_deployments WHERE id=$1`, id)
	d, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return d, nil
}

// List returns recent deployments, newest first. A nil platform lists across
// every platform, which is what the admin view wants.
func (r *DeploymentRepository) List(ctx context.Context, platformID uuid.UUID, limit int) ([]*deployment.Deployment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+deploymentColumns+` FROM container_deployments
		WHERE ($1::uuid IS NULL OR platform_id = $1)
		ORDER BY created_at DESC
		LIMIT $2`, nullUUID(platformID), limit)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	return collectDeployments(rows)
}

// ListOpen returns deployments that have not finished, which is what makes a
// restart survivable: the work is in the table, not in a goroutine.
func (r *DeploymentRepository) ListOpen(ctx context.Context) ([]*deployment.Deployment, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+deploymentColumns+` FROM container_deployments
		WHERE state NOT IN ('ready','failed')
		ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("list open deployments: %w", err)
	}
	defer rows.Close()
	return collectDeployments(rows)
}

func collectDeployments(rows pgx.Rows) ([]*deployment.Deployment, error) {
	out := []*deployment.Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDeployment(s scanner) (*deployment.Deployment, error) {
	var (
		d        deployment.Deployment
		state    string
		spec     []byte
		actor    *uuid.UUID
		platform uuid.UUID
		exit     *int
		created  time.Time
		updated  time.Time
	)
	if err := s.Scan(&d.ID, &platform, &d.Node, &d.AppID, &d.AppName, &state, &d.CTID,
		&actor, &d.RequestedByName, &spec, &d.Log, &exit, &d.Error,
		&created, &updated); err != nil {
		return nil, err
	}
	d.PlatformID = platform
	d.State = deployment.State(state)
	d.RequestedBy = actor
	d.ExitCode = exit
	d.Created = created
	d.Updated = updated
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &d.Spec); err != nil {
			return nil, fmt.Errorf("decode deployment spec: %w", err)
		}
	}
	return &d, nil
}
