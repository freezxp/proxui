package httpapi

import (
	"errors"
	"net/http"
	"runtime"
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
	case errors.Is(err, connector.ErrUnreachable), errors.Is(err, connector.ErrAuth):
		WriteProblem(w, r, http.StatusBadGateway, "platform.unreachable",
			"The platform could not be reached.")
	default:
		s.serverError(w, r, err, "The power action could not be completed.")
	}
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
