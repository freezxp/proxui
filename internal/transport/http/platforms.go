package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// SyncEnqueuer triggers an immediate synchronization.
type SyncEnqueuer interface {
	EnqueueInventorySync(ctx context.Context, platformID uuid.UUID, trigger string) error
}

// SyncRunLister reads a platform's synchronization history.
type SyncRunLister interface {
	ListRuns(ctx context.Context, platformID uuid.UUID, limit int) ([]ports.SyncRunSummary, error)
}

// PlatformDeps bundles what the platform endpoints need.
type PlatformDeps struct {
	Manage    *command.ManagePlatforms
	Platforms ports.PlatformRepository
	Runs      SyncRunLister
	Sync      SyncEnqueuer
}

type platformRequest struct {
	Name           string                   `json:"name"`
	Type           string                   `json:"type"`
	EndpointURL    string                   `json:"endpoint_url"`
	Datacenter     string                   `json:"datacenter"`
	TLSMode        string                   `json:"tls_mode"`
	TLSCAPEM       string                   `json:"tls_ca_pem"`
	TLSFingerprint string                   `json:"tls_fingerprint"`
	Config         map[string]any           `json:"config"`
	CredentialKind string                   `json:"credential_kind"`
	TokenID        string                   `json:"token_id"`
	Secret         string                   `json:"secret"`
	IsEnabled      *bool                    `json:"is_enabled"`
	Intervals      *inventory.SyncIntervals `json:"sync_intervals"`
}

// platformResponse never carries the credential secret in any form.
type platformResponse struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	Type            string                  `json:"type"`
	EndpointURL     string                  `json:"endpoint_url"`
	Datacenter      string                  `json:"datacenter"`
	IsEnabled       bool                    `json:"is_enabled"`
	TLSMode         string                  `json:"tls_mode"`
	Health          string                  `json:"health"`
	HealthDetail    string                  `json:"health_detail,omitempty"`
	DetectedVersion string                  `json:"detected_version,omitempty"`
	LastSeenAt      *string                 `json:"last_seen_at,omitempty"`
	SyncIntervals   inventory.SyncIntervals `json:"sync_intervals"`
	BreakerOpen     bool                    `json:"breaker_open"`
	CreatedAt       string                  `json:"created_at"`
}

func toPlatformResponse(p *inventory.Platform, now func() time.Time) platformResponse {
	resp := platformResponse{
		ID: p.ID.String(), Name: p.Name, Type: p.Type, EndpointURL: p.EndpointURL,
		Datacenter: p.Datacenter, IsEnabled: p.IsEnabled, TLSMode: p.TLSMode,
		Health: string(p.Health), HealthDetail: p.HealthDetail,
		DetectedVersion: p.DetectedVersion, SyncIntervals: p.SyncIntervals,
		BreakerOpen: p.BreakerOpen(now()), CreatedAt: p.CreatedAt.Format(timeFormat),
	}
	if !p.LastSeenAt.IsZero() {
		seen := p.LastSeenAt.Format(timeFormat)
		resp.LastSeenAt = &seen
	}
	return resp
}

func (s *Server) handleListPlatforms(w http.ResponseWriter, r *http.Request) {
	platforms, err := s.platforms.Platforms.List(r.Context(), true)
	if err != nil {
		s.serverError(w, r, err, "Could not list platforms.")
		return
	}
	out := make([]platformResponse, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, toPlatformResponse(p, s.clock))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{"total": len(out)}})
}

func (s *Server) handleGetPlatform(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	p, err := s.platforms.Platforms.Get(r.Context(), id)
	if err != nil {
		s.writePlatformError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toPlatformResponse(p, s.clock))
}

func (s *Server) handleCreatePlatform(w http.ResponseWriter, r *http.Request) {
	var req platformRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, err := s.platforms.Manage.Create(r.Context(), s.platformInput(r, req))
	if err != nil {
		s.writePlatformError(w, r, err)
		return
	}

	// Register and sync: waiting a full cycle to see whether a new platform
	// works would make registration feel broken.
	if err := s.platforms.Sync.EnqueueInventorySync(r.Context(), p.ID, "registration"); err != nil {
		s.log.Error().Err(err).Msg("could not queue initial sync")
	}
	WriteJSON(w, http.StatusCreated, toPlatformResponse(p, s.clock))
}

func (s *Server) handleTestPlatform(w http.ResponseWriter, r *http.Request) {
	var req platformRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	report, err := s.platforms.Manage.TestConnection(r.Context(), s.platformInput(r, req))
	if err != nil {
		// A failed probe is a useful answer, not a server error: return the
		// report alongside the reason so the UI can show both.
		WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"reachable": report.Reachable, "authenticated": report.Authenticated,
			"error": err.Error(),
		})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"reachable": report.Reachable, "authenticated": report.Authenticated,
		"version": report.Version, "nodes": report.NodeCount,
		"missing_permissions": report.MissingPermissions, "warnings": report.Warnings,
	})
}

func (s *Server) handleUpdatePlatform(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	var req platformRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	p, err := s.platforms.Manage.Update(r.Context(), id, s.platformInput(r, req))
	if err != nil {
		s.writePlatformError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toPlatformResponse(p, s.clock))
}

func (s *Server) handleDeletePlatform(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	if err := s.platforms.Manage.Delete(r.Context(), s.actor(r), id); err != nil {
		s.writePlatformError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSyncPlatform(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	if _, err := s.platforms.Platforms.Get(r.Context(), id); err != nil {
		s.writePlatformError(w, r, err)
		return
	}
	actor := s.actor(r)
	if err := s.platforms.Sync.EnqueueInventorySync(r.Context(), id, "manual:"+actor.UserID.String()); err != nil {
		s.serverError(w, r, err, "Could not queue the synchronization.")
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"status": "queued"})
}

func (s *Server) handleListSyncRuns(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	runs, err := s.platforms.Runs.ListRuns(r.Context(), id, 50)
	if err != nil {
		s.serverError(w, r, err, "Could not list synchronization runs.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": runs})
}

// handleListConnectors exposes which platform types this build supports, so the
// UI can offer them without hardcoding a list that drifts from the registry.
func (s *Server) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	infos := connector.Registered()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, map[string]any{
			"type": info.Type, "display_name": info.DisplayName, "version": info.Version,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) platformInput(r *http.Request, req platformRequest) command.PlatformInput {
	return command.PlatformInput{
		Actor:          s.actor(r),
		Name:           req.Name,
		Type:           req.Type,
		EndpointURL:    req.EndpointURL,
		Datacenter:     req.Datacenter,
		TLSMode:        req.TLSMode,
		TLSCAPEM:       req.TLSCAPEM,
		TLSFingerprint: req.TLSFingerprint,
		Config:         req.Config,
		CredentialKind: req.CredentialKind,
		TokenID:        req.TokenID,
		Secret:         req.Secret,
		Intervals:      req.Intervals,
		IsEnabled:      req.IsEnabled,
	}
}

func (s *Server) writePlatformError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested platform does not exist.")
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", "A platform with that name already exists.")
	case errors.Is(err, inventory.ErrInvalidPlatform):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"type": "must match a registered connector"})
	case errors.Is(err, connector.ErrInvalidConfig):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
	case errors.Is(err, connector.ErrAuth), errors.Is(err, connector.ErrPermission):
		WriteProblem(w, r, http.StatusBadGateway, "platform.auth_failed", err.Error())
	case errors.Is(err, connector.ErrUnreachable):
		WriteProblem(w, r, http.StatusBadGateway, "platform.unreachable", err.Error())
	default:
		s.serverError(w, r, err, "The request could not be completed.")
	}
}
