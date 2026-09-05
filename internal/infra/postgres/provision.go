package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/provision"
)

// ProvisionRepository persists create-and-destroy requests (ADR 0010).
type ProvisionRepository struct{ db *Pool }

// NewProvisionRepository builds the repository.
func NewProvisionRepository(db *Pool) *ProvisionRepository { return &ProvisionRepository{db: db} }

const provisionColumns = `id, platform_id, kind, state, step, requested_by, requested_by_name,
	template_external_id, target_node, guest_name, vmid, vm_group_id,
	spec, task_id, error, created_at, updated_at`

// CreateRequest stores a new request in its pending state.
func (r *ProvisionRepository) CreateRequest(ctx context.Context, req *provision.Request) error {
	spec, err := json.Marshal(req.Spec)
	if err != nil {
		return fmt.Errorf("encode provision spec: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO provision_requests (`+provisionColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		req.ID, req.PlatformID, string(req.Kind), string(req.State), req.Step,
		req.RequestedBy, req.RequestedByName,
		req.TemplateExternalID, req.TargetNode, req.GuestName, req.VMID, req.VMGroupID,
		spec, req.TaskID, req.Error, req.Created, req.Updated)
	if err != nil {
		return fmt.Errorf("create provision request: %w", err)
	}
	return nil
}

// SaveRequest writes back a request the driver has advanced.
//
// Everything mutable is written together rather than field by field: the state,
// the step, the task handle and the identifier of the guest change as one move,
// and a partial write would leave a request whose state and task disagree.
func (r *ProvisionRepository) SaveRequest(ctx context.Context, req *provision.Request) error {
	spec, err := json.Marshal(req.Spec)
	if err != nil {
		return fmt.Errorf("encode provision spec: %w", err)
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE provision_requests
		SET state=$2, step=$3, vmid=$4, task_id=$5, error=$6, spec=$7,
		    vm_group_id=$8, target_node=$9, updated_at=now()
		WHERE id=$1`,
		req.ID, string(req.State), req.Step, req.VMID, req.TaskID, req.Error, spec,
		req.VMGroupID, req.TargetNode)
	if err != nil {
		return fmt.Errorf("save provision request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// GetRequest loads one request.
func (r *ProvisionRepository) GetRequest(ctx context.Context, id uuid.UUID) (*provision.Request, error) {
	row := r.db.QueryRow(ctx, `SELECT `+provisionColumns+` FROM provision_requests WHERE id=$1`, id)
	req, err := scanProvisionRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provision request: %w", err)
	}
	return req, nil
}

// ListRequests returns a platform's most recent requests, newest first. A nil
// platform id lists across every platform, which is what the admin view wants.
func (r *ProvisionRepository) ListRequests(ctx context.Context, platformID uuid.UUID, limit int) ([]*provision.Request, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+provisionColumns+` FROM provision_requests
		WHERE ($1::uuid IS NULL OR platform_id = $1)
		ORDER BY created_at DESC
		LIMIT $2`, nullUUID(platformID), limit)
	if err != nil {
		return nil, fmt.Errorf("list provision requests: %w", err)
	}
	defer rows.Close()
	return collectProvisionRequests(rows)
}

// ListOpenRequests returns requests that have not finished.
//
// This is what makes a restart survivable: the work is in the table, not in a
// goroutine, so whatever was mid-clone when the process stopped is still here
// to be picked up. The partial index behind it keeps the query cheap forever,
// since finished requests are the overwhelming majority.
func (r *ProvisionRepository) ListOpenRequests(ctx context.Context) ([]*provision.Request, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+provisionColumns+` FROM provision_requests
		WHERE state NOT IN ('ready','deleted','failed')
		ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("list open provision requests: %w", err)
	}
	defer rows.Close()
	return collectProvisionRequests(rows)
}

func collectProvisionRequests(rows pgx.Rows) ([]*provision.Request, error) {
	var out []*provision.Request
	for rows.Next() {
		req, err := scanProvisionRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan provision request: %w", err)
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func scanProvisionRequest(s scanner) (*provision.Request, error) {
	var (
		req      provision.Request
		kind     string
		state    string
		spec     []byte
		actor    *uuid.UUID
		vmGroup  *uuid.UUID
		platform uuid.UUID
	)
	if err := s.Scan(&req.ID, &platform, &kind, &state, &req.Step, &actor, &req.RequestedByName,
		&req.TemplateExternalID, &req.TargetNode, &req.GuestName, &req.VMID, &vmGroup,
		&spec, &req.TaskID, &req.Error, &req.Created, &req.Updated); err != nil {
		return nil, err
	}
	req.PlatformID = platform
	req.Kind = provision.Kind(kind)
	req.State = provision.State(state)
	req.RequestedBy = actor
	req.VMGroupID = vmGroup
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &req.Spec); err != nil {
			return nil, fmt.Errorf("decode provision spec: %w", err)
		}
	}
	return &req, nil
}

// nullUUID turns the zero value into SQL NULL, so one query serves both "this
// platform" and "every platform".
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// FindVMByExternalID resolves a platform-side identifier to the portal's own.
//
// A guest created seconds ago is not here yet — it arrives when a sync brings
// it in — so absence is reported as the nil UUID rather than an error. The
// caller is waiting for it, not failing on it. Soft-deleted rows are excluded:
// a VMID reused by the platform must not resolve to the guest that had it
// before.
func (r *ProvisionRepository) FindVMByExternalID(ctx context.Context, platformID uuid.UUID, externalID string) (uuid.UUID, error) {
	if externalID == "" {
		return uuid.Nil, nil
	}
	var id uuid.UUID
	err := r.db.QueryRow(ctx, `
		SELECT id FROM vms
		WHERE platform_id = $1 AND external_id = $2 AND deleted_at IS NULL`,
		platformID, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("find vm by external id: %w", err)
	}
	return id, nil
}
