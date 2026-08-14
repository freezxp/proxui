package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/console"
)

// ConsoleDeps bundles what the console endpoints need.
type ConsoleDeps struct {
	Open     *command.OpenConsole
	Sessions ports.ConsoleRepository
	// Bridge serves the WebSocket upgrade; it is an interface so the router
	// can be built in tests without a Redis or a platform.
	Bridge ConsoleBridge
}

// ConsoleBridge serves a console WebSocket for a redeemed ticket.
type ConsoleBridge interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request, ticketID string)
}

type openConsoleRequest struct {
	Kind string `json:"kind"`
}

func (s *Server) handleOpenConsole(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var req openConsoleRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
	}

	out, err := s.console.Open.Handle(r.Context(), command.OpenConsoleInput{
		Actor: s.actor(r), Role: p.Role, VMID: id, Kind: console.Kind(req.Kind),
	})
	if err != nil {
		s.writeConsoleError(w, r, err)
		return
	}

	// The response carries an opaque ticket and nothing else: the browser
	// never learns the node address, the upstream ticket or the platform
	// credential (docs/06-sequence-diagrams.md §6.2).
	WriteJSON(w, http.StatusCreated, map[string]any{
		"session_id": out.SessionID.String(),
		"ws_url":     "/ws/console/" + out.TicketID.String(),
		"expires_in": int(out.ExpiresIn.Seconds()),
	})
}

// handleConsoleWS upgrades to the console bridge. Authentication happened when
// the ticket was issued: a browser cannot set headers on a WebSocket, so the
// single-use ticket in the path is the credential.
func (s *Server) handleConsoleWS(w http.ResponseWriter, r *http.Request) {
	if s.console.Bridge == nil {
		http.Error(w, "console proxying is not enabled", http.StatusServiceUnavailable)
		return
	}
	ticketID := chi.URLParam(r, "ticketID")
	if ticketID == "" {
		http.Error(w, "missing console ticket", http.StatusBadRequest)
		return
	}
	s.console.Bridge.ServeHTTP(w, r, ticketID)
}

func (s *Server) handleListConsoleSessions(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") == "true"
	sessions, err := s.console.Sessions.List(r.Context(), activeOnly, 100)
	if err != nil {
		s.serverError(w, r, err, "Could not list console sessions.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": sessions})
}

func (s *Server) writeConsoleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, console.ErrNotPermitted), errors.Is(err, ports.ErrNotFound):
		// Same response for "no such VM" and "not yours": the difference would
		// confirm the VM exists.
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, console.ErrNotRunning):
		WriteProblem(w, r, http.StatusConflict, "console.not_running",
			"This VM is not running, so it has no console.")
	case errors.Is(err, console.ErrUnsupported), errors.Is(err, connector.ErrNotSupported):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "console.unsupported",
			"This platform does not offer that console type.")
	case errors.Is(err, connector.ErrUnreachable), errors.Is(err, connector.ErrAuth):
		WriteProblem(w, r, http.StatusBadGateway, "platform.unreachable",
			"The platform could not be reached to open a console.")
	default:
		s.serverError(w, r, err, "Could not open a console.")
	}
}
