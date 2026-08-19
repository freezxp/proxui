package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// SensorDeps bundles what the node sensor endpoints need. Nil disables them,
// which is what a deployment without the collector wired up gets.
type SensorDeps struct {
	Sensors ports.HostSensorStore
	Nodes   ports.NodeSSHStore
	Audit   ports.AuditWriter
}

// nodeSensorsResponse is one node's hardware as the host page shows it.
type nodeSensorsResponse struct {
	HostID   uuid.UUID               `json:"host_id"`
	At       time.Time               `json:"at,omitempty"`
	Readings []telemetry.Reading     `json:"readings"`
	Summary  telemetry.SensorSummary `json:"summary"`
	// Node is how the portal reaches this node and how the last poll went.
	// Present even when no reading has ever arrived: for a node whose key is
	// not installed yet, this is the entire answer.
	Node *ports.NodeSSH `json:"node,omitempty"`
}

// handleHostSensors returns a node's current readings.
//
// A node that has never answered is not an error. Most nodes start that way —
// the portal's key has to be installed on them first — so the response is a
// successful empty one carrying the reason, rather than a 404 that reads like
// the host does not exist.
func (s *Server) handleHostSensors(w http.ResponseWriter, r *http.Request) {
	if s.sensors.Sensors == nil {
		WriteProblem(w, r, http.StatusNotFound, "sensors.unavailable",
			"This portal is not collecting node sensors.")
		return
	}
	hostID, ok := s.hostIDParam(w, r)
	if !ok {
		return
	}

	latest, err := s.sensors.Sensors.Latest(r.Context(), hostID)
	if err != nil {
		s.serverError(w, r, err, "Could not read this node's sensors.")
		return
	}

	out := nodeSensorsResponse{
		HostID:   hostID,
		At:       latest.At,
		Readings: latest.Readings,
		Summary:  telemetry.Summarize(latest.Readings),
	}
	if out.Readings == nil {
		out.Readings = []telemetry.Reading{}
	}
	if s.sensors.Nodes != nil {
		if node, err := s.sensors.Nodes.Get(r.Context(), hostID); err == nil {
			out.Node = &node
		} else if !errors.Is(err, ports.ErrNotFound) {
			s.serverError(w, r, err, "Could not read this node's connection state.")
			return
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// handleHostSensorSeries returns one sensor's history for a chart.
func (s *Server) handleHostSensorSeries(w http.ResponseWriter, r *http.Request) {
	if s.sensors.Sensors == nil {
		WriteProblem(w, r, http.StatusNotFound, "sensors.unavailable",
			"This portal is not collecting node sensors.")
		return
	}
	hostID, ok := s.hostIDParam(w, r)
	if !ok {
		return
	}
	chip, label := r.URL.Query().Get("chip"), r.URL.Query().Get("label")
	if chip == "" || label == "" {
		WriteProblem(w, r, http.StatusBadRequest, "sensors.no_series",
			"Name the sensor with chip and label.")
		return
	}

	from, to, ok := s.windowParams(w, r)
	if !ok {
		return
	}
	res := telemetry.SelectResolution(from, to, s.clock())

	points, err := s.sensors.Sensors.Series(r.Context(), hostID, chip, label, from, to, res)
	if err != nil {
		s.serverError(w, r, err, "Could not read this sensor's history.")
		return
	}
	if points == nil {
		points = []ports.SensorPoint{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"data": points,
		"meta": map[string]any{
			"chip": chip, "label": label, "resolution": string(res),
			"from": from, "to": to,
		},
	})
}

// handleForgetNodeKey drops a node's pinned host key.
//
// The pin is what refuses a node whose identity changed, so clearing it is
// deliberately an administrator's action and deliberately audited: a rebuilt
// node and a substituted one look identical from here, and the record of who
// decided which is the only thing that tells them apart afterwards.
func (s *Server) handleForgetNodeKey(w http.ResponseWriter, r *http.Request) {
	if s.sensors.Nodes == nil {
		WriteProblem(w, r, http.StatusNotFound, "sensors.unavailable",
			"This portal is not collecting node sensors.")
		return
	}
	hostID, ok := s.hostIDParam(w, r)
	if !ok {
		return
	}

	before, err := s.sensors.Nodes.Get(r.Context(), hostID)
	if errors.Is(err, ports.ErrNotFound) {
		WriteProblem(w, r, http.StatusNotFound, "not_found",
			"This node has no pinned host key.")
		return
	}
	if err != nil {
		s.serverError(w, r, err, "Could not read this node's pinned key.")
		return
	}
	if err := s.sensors.Nodes.Forget(r.Context(), hostID); err != nil {
		s.serverError(w, r, err, "Could not clear this node's pinned key.")
		return
	}

	if s.sensors.Audit != nil {
		actor := s.actor(r)
		actorID := actor.UserID
		_ = s.sensors.Audit.Write(r.Context(), ports.AuditEntry{
			Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
			Category: "infrastructure", Action: "node.host_key.forgotten",
			TargetType: "host", TargetID: hostID.String(), TargetName: before.Address,
			SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
			Outcome: ports.OutcomeSuccess,
			Details: map[string]any{"fingerprint": before.Fingerprint},
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// hostIDParam reads and validates the host in the path.
func (s *Server) hostIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return uuid.Nil, false
	}
	return hostID, true
}
