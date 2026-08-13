package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// Server owns the HTTP surface. Handlers hang off it so dependencies stay
// explicit (constructor injection, no globals).
type Server struct {
	log       zerolog.Logger
	version   string
	readiness *Readiness
}

// NewServer builds the API server. readiness may be nil for tests that only
// exercise liveness.
func NewServer(log zerolog.Logger, version string, readiness *Readiness) *Server {
	if readiness == nil {
		readiness = &Readiness{}
	}
	return &Server{log: log, version: version, readiness: readiness}
}

// Routes returns the fully wired handler.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(echoRequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(s.log))
	r.Use(recoverPanic(s.log))
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", s.handleLive)
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		// Feature routes are mounted here from sprint 2 onward. Every route
		// added must also carry a permission-map entry (see docs/03-frs.md).
		r.NotFound(s.handleNotFound)
	})

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not supported for that resource.")
	})

	return r
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
}
