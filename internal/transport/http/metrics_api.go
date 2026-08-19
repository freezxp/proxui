package httpapi

import (
	"net/http"
	"time"

	"github.com/freezxp/proxui/internal/app/ports"
)

// MetricsDeps bundles what the metrics endpoints need.
type MetricsDeps struct {
	Metrics ports.MetricsRepository
}

// namedRanges are the shorthand windows the UI offers. Anything else is
// expressed as explicit from/to timestamps.
var namedRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
}

// handleVMMetrics returns a VM's time series. The response names the resolution
// it came from, so a chart can say whether it is showing minutes or hours
// rather than implying a precision it does not have.
func (s *Server) handleVMMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}

	// Metrics are as sensitive as the VM they describe: a role gate alone
	// would let any operator read load for machines nobody granted them.
	p, _ := PrincipalFrom(r.Context())
	if s.inventory.Inventory != nil {
		allowed, err := s.inventory.Inventory.CanAccessVM(r.Context(), id, p.Role, p.UserID)
		if err != nil {
			s.serverError(w, r, err, "Could not verify access.")
			return
		}
		if !allowed {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
			return
		}
	}

	now := s.clock()
	from, to, ok := s.windowParams(w, r)
	if !ok {
		return
	}

	series, err := s.metrics.Metrics.VMSeries(r.Context(), id, from, to, now)
	if err != nil {
		s.serverError(w, r, err, "Could not read metrics.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"vm_id":  id.String(),
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"series": series,
	})
}

// handleHostMetrics returns a node's time series. Nodes are estate-level, so
// the gate is the same role set as the rest of the infrastructure views —
// unlike a VM's metrics, there is no per-grant scoping to apply.
func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "hostID")
	if !ok {
		return
	}

	now := s.clock()
	from, to, ok := s.windowParams(w, r)
	if !ok {
		return
	}

	series, err := s.metrics.Metrics.HostSeries(r.Context(), id, from, to, now)
	if err != nil {
		s.serverError(w, r, err, "Could not read metrics.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"host_id": id.String(),
		"from":    from.Format(time.RFC3339),
		"to":      to.Format(time.RFC3339),
		"series":  series,
	})
}

// windowParams reads the chart window every series endpoint accepts: a named
// range, or explicit from/to, defaulting to the last hour. It answers the
// caller with the problem document and reports false when the window is not
// usable, so handlers can return without repeating the validation.
func (s *Server) windowParams(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	now := s.clock()
	from, to := now.Add(-time.Hour), now

	if rangeName := r.URL.Query().Get("range"); rangeName != "" {
		window, known := namedRanges[rangeName]
		if !known {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Unknown range.", map[string]string{"range": "one of 1h, 6h, 24h, 7d, 30d, 1y"})
			return from, to, false
		}
		from = now.Add(-window)
	}
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Invalid from timestamp.", map[string]string{"from": "must be RFC3339"})
			return from, to, false
		}
		from = parsed
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Invalid to timestamp.", map[string]string{"to": "must be RFC3339"})
			return from, to, false
		}
		to = parsed
	}
	if !from.Before(to) {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"The range is empty.", map[string]string{"from": "must be before to"})
		return from, to, false
	}
	return from, to, true
}
