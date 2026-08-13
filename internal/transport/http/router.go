package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/domain/identity"
)

// UserLoader loads the current user for /auth/me.
type UserLoader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*identity.User, error)
}

// AuthDeps bundles everything the authentication endpoints need. Interfaces,
// not concrete types, so handler tests need no database.
type AuthDeps struct {
	Login    LoginHandler
	Refresh  RefreshHandler
	Logout   LogoutHandler
	Users    UserLoader
	Tokens   TokenParser
	Sessions SessionChecker
}

// ServerConfig is the constructor input for Server.
type ServerConfig struct {
	Log       zerolog.Logger
	Version   string
	Readiness *Readiness
	Auth      AuthDeps
	Admin     AdminDeps
	Platforms PlatformDeps
	Metrics   MetricsDeps

	// SecureCookies marks the refresh cookie Secure. It is off only for local
	// HTTP development; production terminates TLS at the reverse proxy.
	SecureCookies bool

	// Clock is injected so cookie expiry is testable.
	Clock func() time.Time
}

// Server owns the HTTP surface. Handlers hang off it so dependencies stay
// explicit (constructor injection, no globals).
type Server struct {
	log           zerolog.Logger
	version       string
	readiness     *Readiness
	auth          AuthDeps
	admin         AdminDeps
	platforms     PlatformDeps
	metrics       MetricsDeps
	secureCookies bool
	nowFn         func() time.Time
}

// NewServer builds the API server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Readiness == nil {
		cfg.Readiness = &Readiness{}
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Server{
		log:           cfg.Log,
		version:       cfg.Version,
		readiness:     cfg.Readiness,
		auth:          cfg.Auth,
		admin:         cfg.Admin,
		platforms:     cfg.Platforms,
		metrics:       cfg.Metrics,
		secureCookies: cfg.SecureCookies,
		nowFn:         cfg.Clock,
	}
}

func (s *Server) clock() time.Time { return s.nowFn() }

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
		r.Route("/auth", func(r chi.Router) {
			// Unauthenticated: these establish a session.
			r.Post("/login", s.handleLogin)
			r.Post("/refresh", s.handleRefresh)
			r.Post("/logout", s.handleLogout)

			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth())
				r.Get("/me", s.handleMe)
				r.Post("/logout-all", s.handleLogoutAll)
			})
		})

		// Everything below requires a session. Role gates come from the
		// permission map (permissions.go), which the boot check verifies
		// covers every wired route.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth())

			r.Route("/users", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListUsers)
				r.Post("/", s.handleCreateUser)
				r.Get("/{userID}", s.handleGetUser)
				r.Put("/{userID}", s.handleUpdateUser)
				r.Post("/{userID}/password", s.handleResetPassword)
				r.Put("/{userID}/groups", s.handleSetUserGroups)
			})

			r.Route("/user-groups", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListUserGroups)
				r.Post("/", s.handleCreateUserGroup)
				r.Delete("/{groupID}", s.handleDeleteUserGroup)
			})

			r.Route("/vm-groups", func(r chi.Router) {
				// Every role may see which VM groups exist: the inventory
				// filters need them. Only admins may change them.
				r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor)).
					Get("/", s.handleListVMGroups)
				r.With(RequireRole(identity.RoleAdmin)).Post("/", s.handleCreateVMGroup)
				r.With(RequireRole(identity.RoleAdmin)).Delete("/{groupID}", s.handleDeleteVMGroup)
			})

			r.Route("/platforms", func(r chi.Router) {
				// Every role may see which platforms exist and whether they
				// are healthy; only admins may configure or trigger them.
				r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor)).
					Get("/", s.handleListPlatforms)

				r.Group(func(r chi.Router) {
					r.Use(RequireRole(identity.RoleAdmin))
					r.Post("/", s.handleCreatePlatform)
					r.Post("/test", s.handleTestPlatform)
					r.Get("/{platformID}", s.handleGetPlatform)
					r.Put("/{platformID}", s.handleUpdatePlatform)
					r.Delete("/{platformID}", s.handleDeletePlatform)
					r.Post("/{platformID}/sync", s.handleSyncPlatform)
					r.Get("/{platformID}/sync-runs", s.handleListSyncRuns)
				})
			})

			r.With(RequireRole(identity.RoleAdmin)).Get("/connectors", s.handleListConnectors)

			// Metrics are readable by every role: seeing performance is the
			// point of the portal. Per-VM scoping arrives with the inventory
			// query layer.
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor)).
				Get("/vms/{vmID}/metrics", s.handleVMMetrics)

			r.Route("/grants", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListGrants)
				r.Post("/", s.handleCreateGrant)
				r.Delete("/{grantID}", s.handleDeleteGrant)
			})
		})

		r.NotFound(s.handleNotFound)
	})

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not supported for that resource.")
	})

	return r
}

func (s *Server) requireAuth() func(http.Handler) http.Handler {
	return RequireAuth(s.auth.Tokens, s.auth.Sessions)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
}
