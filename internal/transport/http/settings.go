package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/setting"
)

// SettingsStore persists administrator overrides.
type SettingsStore interface {
	All(ctx context.Context) (map[string]int, error)
	Set(ctx context.Context, key string, value int, by uuid.UUID, at time.Time) error
	Reset(ctx context.Context, key string) error
}

// SettingsDeps bundles the settings endpoints' dependencies.
type SettingsDeps struct {
	Settings SettingsStore
}

func (s *Server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := s.settings.Settings.All(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not read the settings.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": setting.Resolve(stored)})
}

type settingUpdate struct {
	Value *int `json:"value"`
}

// handleUpdateSetting stores one value, or clears it back to the default when
// no value is supplied. Each change is audited on its own, so "who raised the
// console timeout" has an answer.
func (s *Server) handleUpdateSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	def, ok := setting.Lookup(key)
	if !ok {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "That is not a setting this portal reads.")
		return
	}

	var req settingUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	actor := s.actor(r)
	actorID := actor.UserID

	if req.Value == nil {
		if err := s.settings.Settings.Reset(r.Context(), key); err != nil {
			s.serverError(w, r, err, "Could not reset the setting.")
			return
		}
		s.auditSetting(r, key, map[string]any{"reset_to_default": def.Default})
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := def.Validate(*req.Value); err != nil {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
		return
	}
	if err := s.settings.Settings.Set(r.Context(), key, *req.Value, actorID, s.clock()); err != nil {
		s.serverError(w, r, err, "Could not store the setting.")
		return
	}
	s.auditSetting(r, key, map[string]any{"value": *req.Value})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditSetting(r *http.Request, key string, details map[string]any) {
	actor := s.actor(r)
	actorID := actor.UserID
	_ = s.admin.Audit.Write(r.Context(), ports.AuditEntry{
		Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: "settings", Action: "setting_changed",
		TargetType: "setting", TargetID: key, TargetName: key,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}
