package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/alert"
)

// AlertStore is what the alert endpoints need from the repository.
type AlertStore interface {
	CreateRule(ctx context.Context, rule *alert.Rule) error
	ListRules(ctx context.Context) ([]alert.Rule, error)
	DeleteRule(ctx context.Context, id uuid.UUID, at time.Time) error
	SetRuleEnabled(ctx context.Context, id uuid.UUID, enabled bool) error
	FiringStatuses(ctx context.Context) ([]alert.Status, error)
}

// AlertDeps bundles the alert endpoints' dependencies.
type AlertDeps struct {
	Alerts AlertStore
}

type alertRuleRequest struct {
	Name string `json:"name"`
	// Subject is "vm" or "host"; empty means a VM, so every client written
	// before nodes had sensors keeps working.
	Subject   string  `json:"subject"`
	Metric    string  `json:"metric"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	DurationS int     `json:"duration_s"`
	VMGroupID *string `json:"vm_group_id"`
	Severity  string  `json:"severity"`
	CooldownS *int    `json:"cooldown_s"`
	IsEnabled *bool   `json:"is_enabled"`
}

func (s *Server) handleListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.alerts.Alerts.ListRules(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list alert rules.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (s *Server) handleCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	var req alertRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	op := alert.Operator(req.Op)
	if req.Op == "" {
		op = alert.OpGreater
	}
	severity := req.Severity
	if severity == "" {
		severity = ports.SeverityWarning
	}
	cooldown := 1800
	if req.CooldownS != nil {
		cooldown = *req.CooldownS
	}

	rule := &alert.Rule{
		ID: uuid.New(), Name: req.Name, Subject: alert.Subject(req.Subject),
		Metric: alert.Metric(req.Metric), Op: op,
		Threshold: req.Threshold, DurationS: req.DurationS,
		VMGroupID: optionalUUID(req.VMGroupID), Severity: severity, CooldownS: cooldown,
		IsEnabled: req.IsEnabled == nil || *req.IsEnabled, CreatedAt: s.clock(),
	}
	if err := rule.Validate(); err != nil {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
		return
	}
	if err := s.alerts.Alerts.CreateRule(r.Context(), rule); err != nil {
		s.writeAlertError(w, r, err)
		return
	}

	s.auditAlert(r, "alert_rule_created", rule.ID.String(), rule.Name, map[string]any{
		"subject": string(rule.SubjectOrDefault()),
		"metric":  string(rule.Metric), "op": string(rule.Op), "threshold": rule.Threshold,
		"duration_s": rule.DurationS, "severity": rule.Severity,
	})
	WriteJSON(w, http.StatusCreated, rule)
}

type alertRuleUpdate struct {
	IsEnabled *bool `json:"is_enabled"`
}

func (s *Server) handleUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "ruleID")
	if !ok {
		return
	}
	var req alertRuleUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.IsEnabled == nil {
		WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"Only is_enabled can be changed; delete and recreate a rule to alter its threshold.")
		return
	}
	if err := s.alerts.Alerts.SetRuleEnabled(r.Context(), id, *req.IsEnabled); err != nil {
		s.writeAlertError(w, r, err)
		return
	}
	s.auditAlert(r, "alert_rule_updated", id.String(), "", map[string]any{"is_enabled": *req.IsEnabled})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "ruleID")
	if !ok {
		return
	}
	if err := s.alerts.Alerts.DeleteRule(r.Context(), id, s.clock()); err != nil {
		s.writeAlertError(w, r, err)
		return
	}
	s.auditAlert(r, "alert_rule_deleted", id.String(), "", nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleListFiringAlerts answers "what is wrong right now", which is the
// question the dashboard and the on-call engineer both start from.
func (s *Server) handleListFiringAlerts(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.alerts.Alerts.FiringStatuses(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list firing alerts.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": statuses, "meta": map[string]any{"total": len(statuses)}})
}

func (s *Server) writeAlertError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", "An alert rule with that name already exists.")
	case errors.Is(err, alert.ErrInvalidMetric), errors.Is(err, alert.ErrInvalidOperator),
		errors.Is(err, alert.ErrInvalidThreshold), errors.Is(err, alert.ErrInvalidName),
		errors.Is(err, alert.ErrInvalidSubject):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
	default:
		s.serverError(w, r, err, "Could not complete the request.")
	}
}

func (s *Server) auditAlert(r *http.Request, action, targetID, targetName string, details map[string]any) {
	actor := s.actor(r)
	actorID := actor.UserID
	_ = s.admin.Audit.Write(r.Context(), ports.AuditEntry{
		Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: "alerting", Action: action,
		TargetType: "alert_rule", TargetID: targetID, TargetName: targetName,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}
