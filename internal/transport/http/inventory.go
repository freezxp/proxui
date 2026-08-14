package httpapi

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// InventoryDeps bundles the read models the inventory and audit endpoints use.
type InventoryDeps struct {
	// Infra serves hosts, storage and networks. They are unscoped by grants:
	// a node's name and capacity are not tenant data, and hiding them would
	// make a VM's location unexplainable.
	Infra     InfraReader
	Inventory ports.InventoryReader
	Audit     ports.AuditReader
	Metrics   ports.MetricsRepository
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}

	q := r.URL.Query()
	filter := ports.VMFilter{
		Role:   p.Role,
		UserID: p.UserID,
		Query:  q.Get("q"),
		State:  q.Get("state"),
		Tag:    q.Get("tag"),
		Sort:   q.Get("sort"),
		Limit:  atoiDefault(q.Get("per_page"), 50),
	}
	if state := filter.State; state != "" && !validVMState(state) {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Unknown state.",
			map[string]string{"state": "one of running, stopped, paused, suspended, unknown"})
		return
	}
	filter.PlatformID = parseUUIDParam(q.Get("platform_id"))
	filter.HostID = parseUUIDParam(q.Get("host_id"))
	filter.GroupID = parseUUIDParam(q.Get("group_id"))

	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	filter.Offset = (page - 1) * filter.Limit

	result, err := s.inventory.Inventory.ListVMs(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, err, "Could not list virtual machines.")
		return
	}

	// Live gauges come from the metrics store rather than the inventory row, so
	// the list shows current load without the reconciler having to write
	// volatile values into asset records on every cycle.
	s.attachLiveMetrics(r, result.Items)

	WriteJSON(w, http.StatusOK, map[string]any{
		"data": result.Items,
		"meta": map[string]any{
			"total": result.Total, "page": page, "per_page": result.Limit,
		},
	})
}

func (s *Server) attachLiveMetrics(r *http.Request, items []ports.VMListItem) {
	if s.inventory.Metrics == nil || len(items) == 0 {
		return
	}
	latest, err := s.inventory.Metrics.LatestVMMetrics(r.Context(), s.clock().Add(-10*time.Minute))
	if err != nil {
		// Missing gauges are a degraded list, not a failed request.
		s.log.Warn().Err(err).Msg("could not attach live metrics")
		return
	}
	for i := range items {
		point, ok := latest[items[i].ID]
		if !ok {
			continue
		}
		items[i].CPUPct = point.CPUPct
		if point.MemTotalBytes > 0 {
			items[i].MemPct = float64(point.MemUsedBytes) / float64(point.MemTotalBytes) * 100
		}
	}
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	detail, err := s.inventory.Inventory.GetVM(r.Context(), id, p.Role, p.UserID)
	if err != nil {
		s.writeInventoryError(w, r, err)
		return
	}

	items := []ports.VMListItem{detail.VMListItem}
	s.attachLiveMetrics(r, items)
	detail.VMListItem = items[0]

	WriteJSON(w, http.StatusOK, detail)
}

func (s *Server) handleVMHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	// Check access before reading history: history is as sensitive as the VM.
	allowed, err := s.inventory.Inventory.CanAccessVM(r.Context(), id, p.Role, p.UserID)
	if err != nil {
		s.serverError(w, r, err, "Could not read history.")
		return
	}
	if !allowed {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return
	}

	entries, err := s.inventory.Inventory.VMHistory(r.Context(), id, atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.serverError(w, r, err, "Could not read history.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": entries})
}

type tagsRequest struct {
	PortalTags []string `json:"portal_tags"`
}

type notesRequest struct {
	Notes string `json:"notes"`
}

func (s *Server) handleSetVMTags(w http.ResponseWriter, r *http.Request) {
	id, allowed := s.authorizeVMWrite(w, r)
	if !allowed {
		return
	}
	var req tagsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if err := s.inventory.Inventory.SetPortalTags(r.Context(), id, req.PortalTags); err != nil {
		s.writeInventoryError(w, r, err)
		return
	}
	s.auditVMChange(r, id, "vm_tags_changed", map[string]any{"portal_tags": req.PortalTags})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetVMNotes(w http.ResponseWriter, r *http.Request) {
	id, allowed := s.authorizeVMWrite(w, r)
	if !allowed {
		return
	}
	var req notesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(req.Notes) > 4096 {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Notes are too long.",
			map[string]string{"notes": "at most 4096 characters"})
		return
	}
	if err := s.inventory.Inventory.SetNotes(r.Context(), id, req.Notes); err != nil {
		s.writeInventoryError(w, r, err)
		return
	}
	s.auditVMChange(r, id, "vm_notes_changed", nil)
	w.WriteHeader(http.StatusNoContent)
}

// authorizeVMWrite resolves the VM id and confirms the caller may modify it.
// Scoping is enforced here rather than relying on the role gate alone: an
// operator may edit VMs they were granted, not every VM.
func (s *Server) authorizeVMWrite(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return uuid.Nil, false
	}
	p, _ := PrincipalFrom(r.Context())

	allowed, err := s.inventory.Inventory.CanAccessVM(r.Context(), id, p.Role, p.UserID)
	if err != nil {
		s.serverError(w, r, err, "Could not verify access.")
		return uuid.Nil, false
	}
	if !allowed {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) auditVMChange(r *http.Request, id uuid.UUID, action string, details map[string]any) {
	if s.admin.Audit == nil {
		return
	}
	actor := s.actor(r)
	actorID := actor.UserID
	_ = s.admin.Audit.Write(r.Context(), ports.AuditEntry{
		Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: "inventory", Action: action,
		TargetType: "vm", TargetID: id.String(),
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}
	summary, err := s.inventory.Inventory.Dashboard(r.Context(), p.Role, p.UserID)
	if err != nil {
		s.serverError(w, r, err, "Could not build the dashboard.")
		return
	}
	WriteJSON(w, http.StatusOK, summary)
}

// --- audit ------------------------------------------------------------

func (s *Server) auditFilter(r *http.Request) (ports.AuditFilter, error) {
	q := r.URL.Query()
	f := ports.AuditFilter{
		Category:   q.Get("category"),
		Action:     q.Get("action"),
		Outcome:    q.Get("outcome"),
		TargetType: q.Get("target_type"),
		TargetID:   q.Get("target_id"),
		Query:      q.Get("q"),
		ActorID:    parseUUIDParam(q.Get("actor")),
		Limit:      atoiDefault(q.Get("per_page"), 100),
	}
	if raw := q.Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, fmt.Errorf("from must be RFC3339")
		}
		f.From = parsed
	}
	if raw := q.Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, fmt.Errorf("to must be RFC3339")
		}
		f.To = parsed
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	f.Offset = (page - 1) * f.Limit
	return f, nil
}

func (s *Server) handleSearchAudit(w http.ResponseWriter, r *http.Request) {
	filter, err := s.auditFilter(r)
	if err != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
		return
	}

	result, err := s.inventory.Audit.Search(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, err, "Could not search the audit log.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"data": result.Items,
		"meta": map[string]any{
			"total": result.Total, "per_page": result.Limit, "offset": result.Offset,
		},
	})
}

// handleExportAudit streams the current filter as CSV. Rows are written as they
// are read so a large export never has to fit in memory, and the response
// starts arriving immediately.
func (s *Server) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	filter, err := s.auditFilter(r)
	if err != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
		return
	}
	filter.Limit = 100000
	filter.Offset = 0

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"proxui-audit-%s.csv\"", s.clock().Format("20060102-150405")))

	out := csv.NewWriter(w)
	_ = out.Write([]string{"timestamp", "actor", "category", "action", "target_type",
		"target_id", "target_name", "source_ip", "outcome", "request_id", "details"})

	err = s.inventory.Audit.Stream(r.Context(), filter, func(record ports.AuditRecord) error {
		details := ""
		if len(record.Details) > 0 {
			details = fmt.Sprintf("%v", record.Details)
		}
		return out.Write([]string{
			record.Time.Format(time.RFC3339), record.ActorName, record.Category,
			record.Action, record.TargetType, record.TargetID, record.TargetName,
			record.SourceIP, record.Outcome, record.RequestID, details,
		})
	})
	if err != nil {
		// The header and some rows are already sent, so the status cannot be
		// changed. Log it and end the stream: a truncated CSV is visible to the
		// user, whereas a silent success would not be.
		s.log.Error().Err(err).Msg("audit export failed mid-stream")
	}
	out.Flush()
}

func (s *Server) handleAuditCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := s.inventory.Audit.Categories(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list audit categories.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": categories})
}

func (s *Server) writeInventoryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ports.ErrNotFound) {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return
	}
	s.serverError(w, r, err, "The request could not be completed.")
}

func validVMState(state string) bool {
	switch state {
	case "running", "stopped", "paused", "suspended", "unknown":
		return true
	}
	return false
}

func parseUUIDParam(raw string) uuid.UUID {
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func atoiDefault(raw string, def int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

var _ = identity.RoleOperator
