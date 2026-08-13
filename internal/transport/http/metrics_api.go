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

	now := s.clock()
	from, to := now.Add(-time.Hour), now

	if rangeName := r.URL.Query().Get("range"); rangeName != "" {
		window, known := namedRanges[rangeName]
		if !known {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Unknown range.", map[string]string{"range": "one of 1h, 6h, 24h, 7d, 30d, 1y"})
			return
		}
		from = now.Add(-window)
	}
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Invalid from timestamp.", map[string]string{"from": "must be RFC3339"})
			return
		}
		from = parsed
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"Invalid to timestamp.", map[string]string{"to": "must be RFC3339"})
			return
		}
		to = parsed
	}
	if !from.Before(to) {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"The range is empty.", map[string]string{"from": "must be before to"})
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
