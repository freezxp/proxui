package httpapi

import (
	"context"
	"net/http"

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
