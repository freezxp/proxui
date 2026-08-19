package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
)

// InfraReader serves the read-only infrastructure views (INV-05).
type InfraReader interface {
	ListHosts(ctx context.Context) ([]ports.HostRow, error)
	ListStoragePools(ctx context.Context) ([]ports.StorageRow, error)
	ListNetworks(ctx context.Context) ([]ports.NetworkRow, error)
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inventory.Infra.ListHosts(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list hosts.")
		return
	}

	// The hottest reading per node, in one query rather than one per row. A
	// failure here costs the temperature column and nothing else: the list of
	// nodes is the answer, and their sensors are a decoration on it.
	if s.sensors.Sensors != nil && len(rows) > 0 {
		ids := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		summaries, err := s.sensors.Sensors.Summaries(r.Context(), ids)
		if err != nil {
			s.log.Warn().Err(err).Msg("could not read node sensors for the host list")
		} else {
			for i := range rows {
				if summary, ok := summaries[rows[i].ID]; ok && summary.Count > 0 {
					rows[i].Sensors = &summary
				}
			}
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{"data": rows, "meta": map[string]any{"total": len(rows)}})
}

func (s *Server) handleListStorage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inventory.Infra.ListStoragePools(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list storage.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": rows, "meta": map[string]any{"total": len(rows)}})
}

func (s *Server) handleListNetworks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.inventory.Infra.ListNetworks(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list networks.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": rows, "meta": map[string]any{"total": len(rows)}})
}
