package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// EdgeDeps bundles the edge-provider handlers.
type EdgeDeps struct {
	Register *command.RegisterEdgeProvider
	Test     *command.TestEdgeCredential
	Verify   *command.VerifyEdgeProvider
	Tunnels  *command.ListEdgeTunnels
	Repo     ports.EdgeProviderRepository
}

// edgeProviderResponse is a provider as the API returns it.
//
// There is no token field, and no shape one could be added to by accident: the
// response type is built from the domain value field by field rather than
// serialised from it, so a credential cannot arrive here by someone adding a
// struct tag somewhere else.
type edgeProviderResponse struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	AccountID      string   `json:"account_id"`
	TunnelID       string   `json:"tunnel_id"`
	TunnelName     string   `json:"tunnel_name"`
	AllowedZoneIDs []string `json:"allowed_zone_ids"`
	IsEnabled      bool     `json:"is_enabled"`
	Ready          bool     `json:"ready"`
	Health         string   `json:"health"`
	HealthDetail   string   `json:"health_detail"`
	LastSeenAt     *string  `json:"last_seen_at"`
	CreatedAt      string   `json:"created_at"`
}

func toEdgeProviderResponse(p *publish.Provider) edgeProviderResponse {
	out := edgeProviderResponse{
		ID: p.ID.String(), Name: p.Name, Kind: p.Kind,
		AccountID: p.AccountID, TunnelID: p.TunnelID, TunnelName: p.TunnelName,
		AllowedZoneIDs: p.AllowedZoneIDs, IsEnabled: p.IsEnabled, Ready: p.Ready(),
		Health: string(p.Health), HealthDetail: p.HealthDetail,
		CreatedAt: p.CreatedAt.Format(time.RFC3339),
	}
	if out.AllowedZoneIDs == nil {
		out.AllowedZoneIDs = []string{}
	}
	if !p.LastSeenAt.IsZero() {
		seen := p.LastSeenAt.Format(time.RFC3339)
		out.LastSeenAt = &seen
	}
	return out
}

type tunnelResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RemotelyManaged bool   `json:"remotely_managed"`
	Connections     int    `json:"connections"`
	Manageable      bool   `json:"manageable"`
	Active          bool   `json:"active"`
	// Reason explains an unmanageable tunnel, so the UI does not have to
	// reconstruct why a row is disabled.
	Reason string `json:"reason,omitempty"`
}

func toTunnelResponses(tunnels []edge.Tunnel) []tunnelResponse {
	out := make([]tunnelResponse, 0, len(tunnels))
	for _, t := range tunnels {
		r := tunnelResponse{
			ID: t.ID, Name: t.Name, RemotelyManaged: t.RemotelyManaged,
			Connections: t.Connections, Manageable: t.Manageable(), Active: t.Active(),
		}
		switch {
		case t.DeletedAt != nil:
			r.Reason = "This tunnel has been deleted."
		case !t.RemotelyManaged:
			r.Reason = "This tunnel is configured by a file on its own host, so the portal cannot change it. " +
				"Only tunnels created for remote management can be configured here."
		case t.Connections == 0:
			// Not a blocker, but the difference between "my rule is wrong" and
			// "nothing is running", which are otherwise identical symptoms.
			r.Reason = "No cloudflared instance is connected, so this tunnel is not serving traffic."
		}
		out = append(out, r)
	}
	return out
}

type healthResponseBody struct {
	Reachable     bool               `json:"reachable"`
	Authenticated bool               `json:"authenticated"`
	MissingScopes []scopeGapResponse `json:"missing_scopes"`
	Tunnels       []tunnelResponse   `json:"tunnels"`
	Warnings      []string           `json:"warnings"`
}

type scopeGapResponse struct {
	Scope  string `json:"scope"`
	Blocks string `json:"blocks"`
}

func toHealthResponse(h edge.Health) healthResponseBody {
	out := healthResponseBody{
		Reachable: h.Reachable, Authenticated: h.Authenticated,
		MissingScopes: make([]scopeGapResponse, 0, len(h.MissingScopes)),
		Tunnels:       toTunnelResponses(h.Tunnels),
		Warnings:      h.Warnings,
	}
	for _, g := range h.MissingScopes {
		out.MissingScopes = append(out.MissingScopes, scopeGapResponse{Scope: g.Scope, Blocks: g.Blocks})
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	return out
}

func (s *Server) handleListEdgeProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.edge.Repo.List(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list edge providers.")
		return
	}
	out := make([]edgeProviderResponse, 0, len(providers))
	for _, p := range providers {
		out = append(out, toEdgeProviderResponse(p))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

type testEdgeRequest struct {
	AccountID string `json:"account_id"`
	Token     string `json:"token"`
}

func (s *Server) handleTestEdgeCredential(w http.ResponseWriter, r *http.Request) {
	var req testEdgeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.AccountID == "" || req.Token == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"An account id and an API token are both required.",
			map[string]string{"account_id": "required", "token": "required"})
		return
	}

	health, err := s.edge.Test.Handle(r.Context(), command.TestEdgeCredentialInput{
		Actor: s.actor(r), AccountID: req.AccountID, Token: req.Token,
	})
	if err != nil {
		// A credential that cannot be verified is reported as a body rather
		// than only as an error, because what the test managed to reach before
		// failing is the useful part.
		s.writeEdgeError(w, r, err, toHealthResponse(health))
		return
	}
	WriteJSON(w, http.StatusOK, toHealthResponse(health))
}

type createEdgeProviderRequest struct {
	Name           string   `json:"name"`
	AccountID      string   `json:"account_id"`
	Token          string   `json:"token"`
	TunnelID       string   `json:"tunnel_id"`
	TunnelName     string   `json:"tunnel_name"`
	AllowedZoneIDs []string `json:"allowed_zone_ids"`
}

func (s *Server) handleCreateEdgeProvider(w http.ResponseWriter, r *http.Request) {
	var req createEdgeProviderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	p, err := s.edge.Register.Handle(r.Context(), command.RegisterEdgeProviderInput{
		Actor: s.actor(r), Name: req.Name, AccountID: req.AccountID, Token: req.Token,
		TunnelID: req.TunnelID, TunnelName: req.TunnelName, AllowedZoneIDs: req.AllowedZoneIDs,
	})
	if err != nil {
		s.writeEdgeError(w, r, err, healthResponseBody{})
		return
	}
	WriteJSON(w, http.StatusCreated, toEdgeProviderResponse(p))
}

func (s *Server) handleVerifyEdgeProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "providerID")
	if !ok {
		return
	}
	health, err := s.edge.Verify.Handle(r.Context(), id)
	if err != nil {
		s.writeEdgeError(w, r, err, toHealthResponse(health))
		return
	}
	WriteJSON(w, http.StatusOK, toHealthResponse(health))
}

func (s *Server) handleListEdgeTunnels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "providerID")
	if !ok {
		return
	}
	tunnels, err := s.edge.Tunnels.Handle(r.Context(), id)
	if err != nil {
		s.writeEdgeError(w, r, err, healthResponseBody{})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": toTunnelResponses(tunnels)})
}

func (s *Server) handleDeleteEdgeProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "providerID")
	if !ok {
		return
	}
	if err := s.edge.Repo.Delete(r.Context(), id); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
			return
		}
		s.serverError(w, r, err, "Could not delete the edge provider.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeEdgeError maps an edge failure onto a problem response.
//
// The distinction that matters throughout: an error naming the provider means
// Cloudflare answered and said no, and nothing in the portal will change it.
func (s *Server) writeEdgeError(w http.ResponseWriter, r *http.Request, err error, health healthResponseBody) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", "An edge provider with that name already exists.")
	case errors.Is(err, publish.ErrInvalidProvider), errors.Is(err, edge.ErrInvalidConfig):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
	case errors.Is(err, edge.ErrNotManageable):
		// Its own code because the fix is on Cloudflare's side and involves
		// recreating the tunnel, which no other error here implies.
		WriteProblemWithBody(w, r, http.StatusUnprocessableEntity, "edge.not_manageable", err.Error(), health)
	case errors.Is(err, edge.ErrAuth):
		WriteProblemWithBody(w, r, http.StatusBadGateway, "edge.auth_failed",
			"Cloudflare rejected the API token.", health)
	case errors.Is(err, edge.ErrPermission):
		WriteProblemWithBody(w, r, http.StatusBadGateway, "edge.permission_denied",
			"The API token lacks a permission this needs.", health)
	case errors.Is(err, edge.ErrRefused):
		WriteProblemWithBody(w, r, http.StatusConflict, "edge.refused", err.Error(), health)
	case errors.Is(err, edge.ErrThrottled):
		WriteProblem(w, r, http.StatusServiceUnavailable, "edge.throttled",
			"Cloudflare rate limited the request. Try again shortly.")
	case errors.Is(err, edge.ErrUnreachable):
		WriteProblemWithBody(w, r, http.StatusBadGateway, "edge.unreachable",
			"Cloudflare could not be reached.", health)
	default:
		s.serverError(w, r, err, "The request could not be completed.")
	}
}
