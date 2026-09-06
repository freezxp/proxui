package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// EventStreamer serves the live event socket.
type EventStreamer interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request, userID uuid.UUID, role identity.Role)
	Subscribers() int
}

// UserLoader loads the current user for /auth/me.
type UserLoader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*identity.User, error)
}

// AuthDeps bundles everything the authentication endpoints need. Interfaces,
// not concrete types, so handler tests need no database.
type AuthDeps struct {
	Login          LoginHandler
	Refresh        RefreshHandler
	Logout         LogoutHandler
	ChangePassword PasswordChanger
	// MFA is the second factor (AUTH-04): completing a challenged sign-in,
	// and the caller's own enrolment.
	MFA      MFAHandler
	Users    UserLoader
	Tokens   TokenParser
	Sessions SessionChecker
}

// PasswordChanger lets a signed-in user replace their own password.
type PasswordChanger interface {
	Handle(ctx context.Context, in command.ChangePasswordInput) error
}

// ServerConfig is the constructor input for Server.
type ServerConfig struct {
	Log        zerolog.Logger
	Version    string
	Readiness  *Readiness
	Auth       AuthDeps
	Admin      AdminDeps
	Platforms  PlatformDeps
	Provision  ProvisionDeps
	Deploy     DeployDeps
	Personal   PersonalDeps
	Metrics    MetricsDeps
	Inventory  InventoryDeps
	Console    ConsoleDeps
	Shell      ShellDeps
	Power      PowerDeps
	Edge       EdgeDeps
	Publishing PublishDeps

	// Sensors reads node hardware readings; nil disables those endpoints.
	Sensors SensorDeps

	// Events streams live updates; nil disables the endpoint.
	Events EventStreamer
	// Limiter enforces request rate limits; nil disables them.
	Limiter Limiter
	// SPA serves the built frontend; nil leaves the API bare, which is what
	// the dev setup wants while Vite serves the UI.
	SPA http.Handler

	// SecureCookies marks the refresh cookie Secure. It is off only for local
	// HTTP development; production terminates TLS at the reverse proxy.
	SecureCookies bool

	// StreamTickets authenticates the live event WebSocket, which a browser
	// cannot send a header on.
	StreamTickets StreamTicketStore

	// Registration carries self-registration and external sign-in.
	Registration RegistrationDeps

	// Notify carries the notification channels, rules and dispatcher.
	Notify NotifyDeps

	// Alerts carries the threshold rules and their current state.
	Alerts AlertDeps

	// Settings carries the runtime configuration store.
	Settings SettingsDeps

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
	registration  RegistrationDeps
	admin         AdminDeps
	platforms     PlatformDeps
	provision     ProvisionDeps
	deploy        DeployDeps
	personal      PersonalDeps
	metrics       MetricsDeps
	inventory     InventoryDeps
	console       ConsoleDeps
	shell         ShellDeps
	power         PowerDeps
	edge          EdgeDeps
	publishing    PublishDeps
	notify        NotifyDeps
	alerts        AlertDeps
	settings      SettingsDeps
	sensors       SensorDeps
	events        EventStreamer
	streamTickets StreamTicketStore
	limiter       Limiter
	spa           http.Handler
	startedAt     time.Time
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
		registration:  cfg.Registration,
		admin:         cfg.Admin,
		platforms:     cfg.Platforms,
		deploy:        cfg.Deploy,
		provision:     cfg.Provision,
		personal:      cfg.Personal,
		metrics:       cfg.Metrics,
		inventory:     cfg.Inventory,
		console:       cfg.Console,
		shell:         cfg.Shell,
		power:         cfg.Power,
		edge:          cfg.Edge,
		publishing:    cfg.Publishing,
		notify:        cfg.Notify,
		alerts:        cfg.Alerts,
		settings:      cfg.Settings,
		sensors:       cfg.Sensors,
		events:        cfg.Events,
		streamTickets: cfg.StreamTickets,
		limiter:       cfg.Limiter,
		spa:           cfg.SPA,
		startedAt:     time.Now(),
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
	r.Use(SecurityHeaders)

	// The console WebSocket authenticates with its single-use ticket rather
	// than a bearer token: a browser cannot set headers on a WebSocket.
	r.Get("/ws/console/{ticketID}", s.handleConsoleWS)
	// The live event stream, for the same reason: a browser cannot put an
	// Authorization header on a WebSocket, so the ticket is the credential.
	r.Get("/ws/events/{ticketID}", s.handleEventsWS)
	// The SSH terminal, for the same reason again. Its ticket additionally
	// names the live connection to attach to, which exists only in the memory
	// of this process (SSH-05).
	r.Get("/ws/ssh/{ticketID}", s.handleShellWS)

	r.Get("/healthz", s.handleLive)
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		// The request deadline belongs here rather than on the root router.
		// chi's Timeout cancels the request context and then writes a 504 once
		// the handler returns, which is right for a JSON call and wrong for
		// everything above: a WebSocket is long-lived by design, so a root
		// deadline would cut the live event stream at 30 seconds (its loop
		// selects on r.Context().Done()) and log a 504 for every console and
		// terminal that outlived one. /readyz brings its own shorter deadline.
		r.Use(middleware.Timeout(30 * time.Second))

		// Branding before authentication: the sign-in page has to render the
		// portal's own name and logo, and cannot do that after sign-in only.
		r.Get("/branding", s.handleBranding)
		// Which ways in this portal offers, so the sign-in page can show them
		// rather than guess.
		r.Get("/auth/methods", s.handleAuthMethods)

		r.Route("/auth", func(r chi.Router) {
			// Registration is rate limited as strictly as login: it is the
			// other endpoint reachable without an account.
			r.With(s.rateLimit("register", loginLimit, loginWindow)).
				Post("/register", s.handleRegister)
			r.Get("/google/start", s.handleGoogleStart)
			r.Get("/google/callback", s.handleGoogleCallback)

			// Unauthenticated: these establish a session.
			// Login is the one endpoint reachable without an account, so it
			// carries the strictest limit.
			r.With(s.loginRateLimit()).Post("/login", s.handleLogin)
			// The second half of a login, and reachable without a session for
			// the same reason login is. Rate limited identically: it is a
			// six-digit secret, and the limit is most of what stops guessing.
			r.With(s.loginRateLimit()).Post("/mfa", s.handleVerifyMFA)
			r.Post("/refresh", s.handleRefresh)
			r.Post("/logout", s.handleLogout)

			r.Group(func(r chi.Router) {
				r.Use(s.requireAuth())
				r.Get("/me", s.handleMe)
				// Every role, because the account being changed is the
				// caller's own and the current password must be supplied.
				r.Post("/password", s.handleChangePassword)
				r.Post("/logout-all", s.handleLogoutAll)
				// Enrolling, confirming and removing the caller's own factor.
				// Every role, because the account being changed is their own.
				r.Post("/me/totp", s.handleBeginTOTP)
				r.Post("/me/totp/confirm", s.handleConfirmTOTP)
				r.Delete("/me/totp", s.handleDisableTOTP)
			})
		})

		// Everything below requires a session. Role gates come from the
		// permission map (permissions.go), which the boot check verifies
		// covers every wired route.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth())
			r.Use(s.rateLimit("api", apiLimit, apiWindow))

			r.Route("/users", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListUsers)
				r.Post("/", s.handleCreateUser)
				r.Get("/{userID}", s.handleGetUser)
				r.Put("/{userID}", s.handleUpdateUser)
				r.Post("/{userID}/password", s.handleResetPassword)
				// Deletion is the account that should not exist; deactivation
				// (PUT above) stays the answer for the one that simply left.
				r.Delete("/{userID}", s.handleDeleteUser)
				// The lost-phone path: clearing somebody else's second factor
				// (AUTH-04). Audited against both accounts.
				r.Delete("/{userID}/totp", s.handleResetUserTOTP)
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
				r.With(RequireRole(identity.RoleAdmin)).Get("/{groupID}/members", s.handleListVMGroupMembers)
				r.With(RequireRole(identity.RoleAdmin)).Put("/{groupID}/members", s.handleSetVMGroupMembers)
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
					// Provisioning (ADR 0010). Admin-only, like the rest of
					// this group: creating and destroying guests is the one
					// thing the platform token could not do before.
					r.Get("/{platformID}/templates", s.handleListTemplates)
					r.Post("/{platformID}/provision", s.handleProvision)
					// Building the image everything else is cloned from, so an
					// empty template list is a button rather than an
					// instruction to go and run four commands elsewhere.
					r.Post("/{platformID}/templates", s.handleBuildTemplate)
					// What the nodes themselves need, and installing what the
					// portal can (ADR 0011). Admin-only for the same reason
					// the rest of this group is, and more so: installing a
					// package changes software on a hypervisor.
					r.Get("/{platformID}/readiness", s.handleReadiness)
					r.Post("/{platformID}/nodes/{node}/install", s.handleInstallPrerequisite)
					// Installing an application into a container. The largest
					// thing the portal does to a node, so it sits with the
					// rest of the admin-only platform group (ADR 0012).
					r.Post("/{platformID}/container-deployments", s.handleDeployContainerApp)
				})
			})

			// Edge providers: admin only, throughout. Publishing decides what
			// the outside world can reach, which is a different power from an
			// operator's grant over a VM (ADR 0004, PUB-40).
			r.Route("/edge-providers", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListEdgeProviders)
				r.Post("/", s.handleCreateEdgeProvider)
				r.Post("/test", s.handleTestEdgeCredential)
				r.Post("/{providerID}/verify", s.handleVerifyEdgeProvider)
				r.Get("/{providerID}/tunnels", s.handleListEdgeTunnels)
				r.Get("/{providerID}/ingress", s.handleGetEdgeIngress)
				r.Post("/{providerID}/snapshot", s.handleSnapshotEdgeIngress)
				r.Post("/{providerID}/preview", s.handlePreviewEdgeIngress)
				r.Get("/{providerID}/apps", s.handleListPublishedApps)
				r.Post("/{providerID}/apps", s.handlePublishApp)
				r.Delete("/{providerID}", s.handleDeleteEdgeProvider)
			})

			r.With(RequireRole(identity.RoleAdmin)).
				Delete("/published-apps/{appID}", s.handleUnpublishApp)

			// Container apps: the shipped catalogue and what has been deployed
			// from it. Not published apps, which are Cloudflare hostnames
			// (ADR 0012).
			r.Group(func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/container-apps", s.handleListContainerApps)
				r.Get("/container-deployments", s.handleListDeployments)
				r.Get("/container-deployments/{deploymentID}", s.handleGetDeployment)
			})

			r.With(RequireRole(identity.RoleAdmin)).Get("/connectors", s.handleListConnectors)

			// Inventory is readable by every role; which VMs appear is decided
			// per query by the caller's grants, not by the role gate.
			r.Group(func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor))
				r.Get("/dashboard", s.handleDashboard)
				r.Get("/vms", s.handleListVMs)
				r.Get("/vms/{vmID}", s.handleGetVM)
				r.Get("/vms/{vmID}/metrics", s.handleVMMetrics)
				r.Get("/vms/{vmID}/history", s.handleVMHistory)

			})

			// Consoles: operators and admins only, and scoped per VM inside the
			// command rather than by the role gate alone.
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator),
				s.rateLimit("console", consoleLimit, consoleWindow)).
				Post("/vms/{vmID}/console", s.handleOpenConsole)
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator)).
				Post("/vms/{vmID}/power", s.handlePower)
			r.With(RequireRole(identity.RoleAdmin)).
				Get("/console-sessions", s.handleListConsoleSessions)

			// Destroying a guest is irreversible and admin-only. The name
			// confirmation that guards it is checked in the command, not here.
			r.With(RequireRole(identity.RoleAdmin)).
				Delete("/vms/{vmID}", s.handleDestroyVM)
			r.With(RequireRole(identity.RoleAdmin)).
				Get("/image-catalogue", s.handleImageCatalogue)

			// Favourites and folders: any authenticated role. Arranging your
			// own view is not a privilege — and what you may arrange is
			// checked per VM inside the command, not by the role gate.
			r.Put("/vms/{vmID}/favourite", s.handleSetFavourite)
			r.Delete("/vms/{vmID}/favourite", s.handleClearFavourite)
			r.Put("/vms/{vmID}/folder", s.handleFileVM)
			r.Route("/folders", func(r chi.Router) {
				r.Get("/", s.handleListFolders)
				r.Post("/", s.handleCreateFolder)
				r.Patch("/{folderID}", s.handleUpdateFolder)
				r.Delete("/{folderID}", s.handleDeleteFolder)
				r.Put("/{folderID}/vms", s.handleFileVMs)
			})
			r.Route("/provision-requests", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListProvisionRequests)
				r.Get("/{requestID}", s.handleGetProvisionRequest)
			})

			// SSH terminals: the same gate as the console, and scoped per VM
			// inside the command. Opening one costs a live connection to a
			// guest, so it carries the console's rate limit too (SSH-01).
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator),
				s.rateLimit("ssh", consoleLimit, consoleWindow)).
				Post("/vms/{vmID}/ssh", s.handleOpenShell)
			// Forgetting a pinned host key is an administrator's decision,
			// deliberately away from the connect form (SSH-04).
			r.With(RequireRole(identity.RoleAdmin)).
				Delete("/vms/{vmID}/ssh-host-key", s.handleForgetHostKey)

			r.Route("/ssh-sessions", func(r chi.Router) {
				// Every route below resolves the session through the registry,
				// which refuses one that is not the caller's. The role gate is
				// the outer fence; ownership is the real check (SSH-10).
				r.With(RequireRole(identity.RoleAdmin)).Get("/", s.handleListShellSessions)

				r.Group(func(r chi.Router) {
					r.Use(RequireRole(identity.RoleAdmin, identity.RoleOperator))
					r.Delete("/{sessionID}", s.handleCloseShell)
					r.Get("/{sessionID}/files", s.handleListShellFiles)
					r.Delete("/{sessionID}/files", s.handleDeleteShellFile)
					r.Get("/{sessionID}/files/content", s.handleDownloadShellFile)
					r.Post("/{sessionID}/files/content", s.handleUploadShellFile)
					r.Post("/{sessionID}/files/mkdir", s.handleShellMkdir)
					r.Post("/{sessionID}/files/rename", s.handleShellRename)
					r.Post("/{sessionID}/files/chmod", s.handleShellChmod)
					// Installing the portal's key runs over the caller's own
					// session, with that guest account's permissions (SSH-12).
					r.Post("/{sessionID}/portal-key", s.handleInstallPortalKey)
					r.Delete("/{sessionID}/portal-key", s.handleRemovePortalKey)
				})
			})

			// The portal's own SSH key (SSH-11..SSH-14, ADR 0006). Reading the
			// public half is an operator's business; holding the pair is an
			// administrator's.
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator)).
				Get("/ssh-key", s.handleGetPortalKey)
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator)).
				Get("/vms/{vmID}/ssh-key", s.handleVMKeyInstalls)
			r.With(RequireRole(identity.RoleAdmin)).
				Post("/ssh-key", s.handleGeneratePortalKey)
			r.With(RequireRole(identity.RoleAdmin)).
				Delete("/ssh-key", s.handleDeletePortalKey)
			r.With(RequireRole(identity.RoleAdmin)).
				Get("/ssh-key/installs", s.handleListKeyInstalls)

			// Portal-owned annotations: operators may edit what they can see.
			r.Group(func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin, identity.RoleOperator))
				r.Put("/vms/{vmID}/tags", s.handleSetVMTags)
				r.Put("/vms/{vmID}/notes", s.handleSetVMNotes)
			})

			r.With(RequireRole(identity.RoleAdmin)).Get("/system/info", s.handleSystemInfo)

			// Live updates are scoped per subscriber inside the hub, so every
			// role may subscribe and each sees only their own estate.
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor)).
				Post("/events/ticket", s.handleEventTicket)

			r.Route("/audit-logs", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin, identity.RoleAuditor))
				r.Get("/", s.handleSearchAudit)
				r.Get("/export", s.handleExportAudit)
				r.Get("/categories", s.handleAuditCategories)
			})

			r.Route("/notification-channels", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListChannels)
				r.Post("/", s.handleCreateChannel)
				r.Put("/{channelID}", s.handleUpdateChannel)
				r.Delete("/{channelID}", s.handleDeleteChannel)
				r.Post("/{channelID}/test", s.handleTestChannel)
			})

			r.Route("/notification-rules", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListRules)
				r.Post("/", s.handleCreateRule)
				r.Delete("/{ruleID}", s.handleDeleteRule)
			})

			r.With(RequireRole(identity.RoleAdmin)).
				Get("/notification-deliveries", s.handleListDeliveries)

			r.Route("/settings", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListSettings)
				r.Put("/{key}", s.handleUpdateSetting)
			})

			r.Route("/alert-rules", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListAlertRules)
				r.Post("/", s.handleCreateAlertRule)
				r.Put("/{ruleID}", s.handleUpdateAlertRule)
				r.Delete("/{ruleID}", s.handleDeleteAlertRule)
			})

			// What is currently wrong is operational information, not
			// configuration: every role may read it.
			r.With(RequireRole(identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor)).
				Get("/alerts", s.handleListFiringAlerts)

			// Infrastructure describes the estate rather than the VMs in it.
			// An operator works on the machines they were granted; the nodes,
			// pools and bridges behind them are not theirs to survey.
			r.Group(func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin, identity.RoleReadOnly, identity.RoleAuditor))
				r.Get("/hosts", s.handleListHosts)
				// A node's own hardware, read from the node rather than from
				// the platform, which publishes no temperature at all
				// (ADR 0007).
				r.Get("/hosts/{hostID}/metrics", s.handleHostMetrics)
				r.Get("/hosts/{hostID}/sensors", s.handleHostSensors)
				r.Get("/hosts/{hostID}/sensors/series", s.handleHostSensorSeries)
				r.Get("/hosts/{hostID}/sensors/history", s.handleHostSensorHistory)
				r.Get("/storage", s.handleListStorage)
				r.Get("/networks", s.handleListNetworks)
			})

			// Clearing a node's pinned host key is the one write here, and it
			// is an administrator's: the pin is what refuses a node whose
			// identity changed.
			r.With(RequireRole(identity.RoleAdmin)).
				Delete("/hosts/{hostID}/host-key", s.handleForgetNodeKey)

			r.Route("/grants", func(r chi.Router) {
				r.Use(RequireRole(identity.RoleAdmin))
				r.Get("/", s.handleListGrants)
				r.Post("/", s.handleCreateGrant)
				r.Delete("/{grantID}", s.handleDeleteGrant)
			})
		})

		r.NotFound(s.handleNotFound)
	})

	// Anything not matched above is either a frontend route or a genuine 404.
	// API paths must never fall through to the SPA: an unknown endpoint should
	// answer with a problem document, not an HTML page.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if s.spa != nil && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ws/") {
			s.spa.ServeHTTP(w, r)
			return
		}
		s.handleNotFound(w, r)
	})
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
