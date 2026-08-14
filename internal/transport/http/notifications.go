package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	appnotify "github.com/freezxp/proxui/internal/app/notify"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/notify"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// NotifyDeps bundles what the notification endpoints need.
type NotifyDeps struct {
	Repo       ports.NotifyRepository
	Dispatcher *appnotify.Dispatcher
	Vault      *crypto.Vault
}

type channelRequest struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config"`
	Secret    string         `json:"secret"`
	IsEnabled *bool          `json:"is_enabled"`
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.notify.Repo.ListChannels(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list notification channels.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": channels})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	kind := notify.Kind(req.Kind)
	if !kind.Valid() {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
			"That is not a notification channel type this portal can deliver to.")
		return
	}
	if req.Name == "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", "A channel needs a name.")
		return
	}

	channel := &notify.Channel{
		ID: uuid.New(), Name: req.Name, Kind: kind, Config: req.Config,
		IsEnabled: req.IsEnabled == nil || *req.IsEnabled, CreatedAt: s.clock(),
	}
	if channel.Config == nil {
		channel.Config = map[string]any{}
	}

	sealed, err := s.sealChannelSecret(req.Secret)
	if err != nil {
		s.serverError(w, r, err, "Could not store the channel secret.")
		return
	}
	if err := s.notify.Repo.CreateChannel(r.Context(), channel, sealed); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	channel.HasSecret = sealed != nil

	s.auditNotify(r, "notification_channel_created", channel.ID.String(), channel.Name,
		map[string]any{"kind": string(channel.Kind)})
	WriteJSON(w, http.StatusCreated, channel)
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "channelID")
	if !ok {
		return
	}
	var req channelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	existing, err := s.notify.Repo.GetChannel(r.Context(), id)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Config != nil {
		existing.Config = req.Config
	}
	if req.IsEnabled != nil {
		existing.IsEnabled = *req.IsEnabled
	}

	// An empty secret means "keep the stored one": the API never returns it,
	// so requiring it on every edit would mean retyping what cannot be read.
	sealed, err := s.sealChannelSecret(req.Secret)
	if err != nil {
		s.serverError(w, r, err, "Could not store the channel secret.")
		return
	}
	if err := s.notify.Repo.UpdateChannel(r.Context(), &existing, sealed); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}

	s.auditNotify(r, "notification_channel_updated", id.String(), existing.Name,
		map[string]any{"secret_replaced": sealed != nil})
	WriteJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "channelID")
	if !ok {
		return
	}
	if err := s.notify.Repo.DeleteChannel(r.Context(), id, s.clock()); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	s.auditNotify(r, "notification_channel_deleted", id.String(), "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "channelID")
	if !ok {
		return
	}
	err := s.notify.Dispatcher.SendTest(r.Context(), id)
	s.auditNotify(r, "notification_channel_tested", id.String(), "",
		map[string]any{"delivered": err == nil})

	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			s.writeNotifyError(w, r, err)
			return
		}
		// A failed test is an answer, not a server fault: the reason the
		// platform gave is the only useful thing to show.
		WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"delivered": false, "error": err.Error(),
		})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"delivered": true})
}

type ruleRequest struct {
	Category    string  `json:"category"`
	MinSeverity string  `json:"min_severity"`
	PlatformID  *string `json:"platform_id"`
	VMGroupID   *string `json:"vm_group_id"`
	ChannelID   string  `json:"channel_id"`
	IsEnabled   *bool   `json:"is_enabled"`
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.notify.Repo.ListRules(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list notification rules.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if !notify.ValidCategory(req.Category) {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
			"That is not an event category the portal produces.")
		return
	}
	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "A rule needs a channel.")
		return
	}

	severity := notify.Severity(req.MinSeverity)
	if req.MinSeverity == "" {
		severity = notify.SeverityInfo
	}

	rule := &notify.Rule{
		ID: uuid.New(), Category: req.Category, MinSeverity: severity,
		PlatformID: optionalUUID(req.PlatformID), VMGroupID: optionalUUID(req.VMGroupID),
		ChannelID: channelID, IsEnabled: req.IsEnabled == nil || *req.IsEnabled,
		CreatedAt: s.clock(),
	}
	// Confirm the channel exists before storing a rule that points at it, and
	// carry its name back: a rule rendered without one reads as routing to
	// nowhere.
	channel, err := s.notify.Repo.GetChannel(r.Context(), channelID)
	if err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	rule.ChannelName = channel.Name

	if err := s.notify.Repo.CreateRule(r.Context(), rule); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	s.auditNotify(r, "notification_rule_created", rule.ID.String(), rule.Category,
		map[string]any{"channel_id": channelID.String(), "min_severity": string(severity)})
	WriteJSON(w, http.StatusCreated, rule)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "ruleID")
	if !ok {
		return
	}
	if err := s.notify.Repo.DeleteRule(r.Context(), id); err != nil {
		s.writeNotifyError(w, r, err)
		return
	}
	s.auditNotify(r, "notification_rule_deleted", id.String(), "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	deliveries, err := s.notify.Repo.ListDeliveries(r.Context(), atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.serverError(w, r, err, "Could not list notification deliveries.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": deliveries})
}

func (s *Server) sealChannelSecret(secret string) (*crypto.SealedSecret, error) {
	if secret == "" {
		return nil, nil
	}
	sealed, err := s.notify.Vault.Seal(secret)
	if err != nil {
		return nil, err
	}
	return &sealed, nil
}

func (s *Server) writeNotifyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", "A channel with that name already exists.")
	case errors.Is(err, notify.ErrUnknownKind), errors.Is(err, notify.ErrInvalidConfig):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
	default:
		s.serverError(w, r, err, "Could not complete the request.")
	}
}

func (s *Server) auditNotify(r *http.Request, action, targetID, targetName string, details map[string]any) {
	actor := s.actor(r)
	actorID := actor.UserID
	_ = s.admin.Audit.Write(r.Context(), ports.AuditEntry{
		Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: "notification", Action: action,
		TargetType: "notification_channel", TargetID: targetID, TargetName: targetName,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}

func optionalUUID(raw *string) *uuid.UUID {
	if raw == nil || *raw == "" {
		return nil
	}
	parsed, err := uuid.Parse(*raw)
	if err != nil {
		return nil
	}
	return &parsed
}
