package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/setting"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// SettingsStore persists administrator overrides.
type SettingsStore interface {
	All(ctx context.Context) (map[string]json.RawMessage, error)
	Set(ctx context.Context, key string, value any, by uuid.UUID, at time.Time) error
	SetSecret(ctx context.Context, key, value string, vault *crypto.Vault, by uuid.UUID, at time.Time) error
	Reset(ctx context.Context, key string) error
}

// SettingsDeps bundles the settings endpoints' dependencies.
type SettingsDeps struct {
	Settings SettingsStore
	// Vault seals secret settings, with the same key that protects platform
	// credentials.
	Vault *crypto.Vault
}

func (s *Server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := s.settings.Settings.All(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not read the settings.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": setting.Resolve(stored)})
}

// handleBranding serves the portal's name, logo and sign-in notice without
// authentication.
//
// The sign-in page has to render before anyone has signed in, so a branded
// portal that only reveals its name afterwards is not branded. Nothing here is
// sensitive: it is the text and picture every visitor is meant to see.
func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	stored, err := s.settings.Settings.All(r.Context())
	if err != nil {
		// Branding must never be the reason nobody can sign in; fall back to
		// the built-in defaults.
		s.log.Warn().Err(err).Msg("could not read branding; serving defaults")
		stored = map[string]json.RawMessage{}
	}
	// Cached briefly: it changes rarely and is fetched on every page load.
	w.Header().Set("Cache-Control", "public, max-age=60")
	WriteJSON(w, http.StatusOK, setting.PublicValues(stored))
}

type settingUpdate struct {
	// Either, depending on the setting's kind. Both absent means "reset".
	Value *int    `json:"value"`
	Text  *string `json:"text"`
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

	if req.Value == nil && req.Text == nil {
		if err := s.settings.Settings.Reset(r.Context(), key); err != nil {
			s.serverError(w, r, err, "Could not reset the setting.")
			return
		}
		reset := any(def.Default)
		switch {
		case def.Kind.Secret():
			reset = "removed"
		case !def.Kind.Numeric():
			reset = def.DefaultText
		}
		s.auditSetting(r, key, map[string]any{"reset_to_default": reset})
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var (
		stored  any
		details map[string]any
	)
	switch {
	case def.Kind.Numeric():
		if req.Value == nil {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
				"This setting takes a number.")
			return
		}
		if err := def.ValidateNumber(*req.Value); err != nil {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
			return
		}
		stored, details = *req.Value, map[string]any{"value": *req.Value}
	case def.Kind.Secret():
		if req.Text == nil || *req.Text == "" {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
				"A secret cannot be set to nothing. Use reset to remove it.")
			return
		}
		if err := def.ValidateText(*req.Text); err != nil {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
			return
		}
		if err := s.settings.Settings.SetSecret(r.Context(), key, *req.Text,
			s.settings.Vault, actorID, s.clock()); err != nil {
			s.serverError(w, r, err, "Could not store the setting.")
			return
		}
		// The value never appears in the trail; that it changed, and by whom,
		// is what the trail is for.
		s.auditSetting(r, key, map[string]any{"secret_replaced": true})
		w.WriteHeader(http.StatusNoContent)
		return

	default:
		if req.Text == nil {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
				"This setting takes text.")
			return
		}
		if err := def.ValidateText(*req.Text); err != nil {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
			return
		}
		stored = *req.Text
		// A logo is up to 128 KB of base64; recording it in the audit detail
		// would bury the trail. Its size is the useful fact.
		if def.Kind == setting.KindImage {
			details = map[string]any{"bytes": len(*req.Text)}
		} else {
			details = map[string]any{"value": *req.Text}
		}
	}

	if err := s.settings.Settings.Set(r.Context(), key, stored, actorID, s.clock()); err != nil {
		s.serverError(w, r, err, "Could not store the setting.")
		return
	}
	s.auditSetting(r, key, details)
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
