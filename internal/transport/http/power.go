package httpapi

import (
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/connector"
)

// PowerDeps bundles the lifecycle-action dependencies.
type PowerDeps struct {
	Power *command.Power
}

type powerRequest struct {
	Action string `json:"action"`
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var req powerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	out, err := s.power.Power.Handle(r.Context(), command.PowerInput{
		Actor: s.actor(r), Role: p.Role, VMID: id,
		Action: connector.PowerAction(req.Action),
	})
	if err != nil {
		s.writePowerError(w, r, err)
		return
	}

	// 202: the platform accepted a task, the machine has not finished changing
	// state yet. The next sync reports what actually happened.
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"task_id": out.TaskID, "node": out.Node, "status": "accepted",
	})
}

func (s *Server) writePowerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, command.ErrPowerNotPermitted):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, connector.ErrNotSupported):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"Unknown or unsupported power action.",
			map[string]string{"action": "one of start, stop, shutdown, reboot"})
	case errors.Is(err, connector.ErrPermission):
		// The platform token lacks VM.PowerMgmt: an administrator must widen it
		// on the cluster, so say so rather than reporting a generic failure.
		WriteProblem(w, r, http.StatusBadGateway, "platform.permission_denied",
			"The platform credential is not allowed to perform power actions.")
	case errors.Is(err, connector.ErrRefused):
		// The platform answered and declined — a VM already in the state being
		// asked for, a config lock held by another task. Its own words are the
		// whole value here, so they are passed through rather than replaced
		// with a summary that would send an operator looking in the wrong
		// place. Nothing in a connector error is secret: credentials never
		// reach the detail string.
		WriteProblem(w, r, http.StatusConflict, "platform.refused", platformDetail(err))
	case errors.Is(err, connector.ErrUnreachable), errors.Is(err, connector.ErrAuth):
		WriteProblem(w, r, http.StatusBadGateway, "platform.unreachable",
			"The platform could not be reached.")
	default:
		s.serverError(w, r, err, "The power action could not be completed.")
	}
}

// platformDetail renders a connector error for an operator.
//
// The connector's Detail already carries the platform's own explanation; what
// is stripped is the class prefix and operation label, which are for logs
// rather than for someone looking at a dialog.
func platformDetail(err error) string {
	var cerr *connector.Error
	if !errors.As(err, &cerr) || strings.TrimSpace(cerr.Detail) == "" {
		return "The platform refused the request."
	}
	detail := strings.TrimSpace(cerr.Detail)
	if !strings.HasSuffix(detail, ".") {
		detail += "."
	}
	return strings.ToUpper(detail[:1]) + detail[1:]
}

// handleSystemInfo reports build and runtime facts for operators.
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"version":    s.version,
		"go_version": runtime.Version(),
		"uptime_s":   int(time.Since(s.startedAt).Seconds()),
		"goroutines": runtime.NumGoroutine(),
	}
	if s.events != nil {
		info["event_subscribers"] = s.events.Subscribers()
	}
	WriteJSON(w, http.StatusOK, info)
}
