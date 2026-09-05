// Command proxui is the single ProxUI binary. One artifact runs every role
// (api, worker, scheduler, or all) so deployment splits are a compose change,
// not a rebuild. See docs/05-system-architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	appalert "github.com/freezxp/proxui/internal/app/alert"
	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/imageprep"
	appnotify "github.com/freezxp/proxui/internal/app/notify"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/provisioner"
	"github.com/freezxp/proxui/internal/app/query"
	"github.com/freezxp/proxui/internal/app/sensor"
	appsetting "github.com/freezxp/proxui/internal/app/setting"
	"github.com/freezxp/proxui/internal/app/shellreg"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/edge"
	"github.com/freezxp/proxui/internal/edge/cloudflare"
	"github.com/freezxp/proxui/internal/infra/config"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/logging"
	infranotify "github.com/freezxp/proxui/internal/infra/notify"
	"github.com/freezxp/proxui/internal/infra/oauth"
	"github.com/freezxp/proxui/internal/infra/postgres"
	redisinfra "github.com/freezxp/proxui/internal/infra/redis"
	"github.com/freezxp/proxui/internal/infra/sensors"
	"github.com/freezxp/proxui/internal/infra/sshclient"
	"github.com/freezxp/proxui/internal/jobs"
	httpapi "github.com/freezxp/proxui/internal/transport/http"
	"github.com/freezxp/proxui/internal/transport/ws"
	"github.com/freezxp/proxui/web"
)

// limiterFunc adapts a rate-limiting function to the transport's interface,
// so the HTTP layer depends on a behaviour rather than on Redis.
type limiterFunc func(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error)

func (f limiterFunc) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error) {
	return f(ctx, bucket, limit, window)
}

// tokenIssuerName is the `iss` claim on access tokens.
const tokenIssuerName = "proxui"

// totpIssuerName labels the portal in an authenticator app. Deliberately not
// the configurable brand name: an app shows this beside the code, and an entry
// that renames itself when somebody edits a setting is one whose codes get
// typed into the wrong form.
const totpIssuerName = "ProxUI"

// Build metadata, injected via -ldflags at release time.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version":
			fmt.Printf("proxui %s (commit %s, built %s)\n", version, commit, buildTime)
			return
		}
	}

	role := flag.String("role", "", "process role: all|api|worker|scheduler (overrides PROXUI_ROLE)")
	flag.Parse()
	if *role != "" {
		_ = os.Setenv("PROXUI_ROLE", *role)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error:\n%v\n", err)
		os.Exit(2)
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat, version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error().Err(err).Msg("shutting down after fatal error")
		os.Exit(1)
	}
	log.Info().Msg("shutdown complete")
}

func run(ctx context.Context, cfg config.Config, log zerolog.Logger) error {
	log.Info().
		Str("role", string(cfg.Role)).
		Str("env", cfg.Environment).
		Str("commit", commit).
		Msg("starting proxui")

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := redisinfra.Connect(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer rdb.Close()

	readiness := &httpapi.Readiness{
		Checkers: map[string]httpapi.Checker{"database": pool, "redis": rdb},
		Timeout:  cfg.ReadinessTimeout,
	}

	// Dependency graph, wired by hand: explicit beats a DI framework for a
	// graph this size (docs/05-system-architecture.md §5.3).
	var (
		users      = postgres.NewUserRepository(pool)
		sessions   = postgres.NewSessionRepository(pool)
		audit      = postgres.NewAuditRepository(pool)
		accessRepo = postgres.NewAccessRepository(pool)
		hasher     = crypto.NewPasswordHasher()
		clock      = ports.SystemClock{}
	)

	masterKey, err := crypto.LoadMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	vault, err := crypto.NewVault(masterKey, 1)
	if err != nil {
		return err
	}

	var (
		platforms  = postgres.NewPlatformRepository(pool)
		assets     = postgres.NewAssetRepository(pool)
		syncRepo   = postgres.NewSyncRepository(pool)
		metrics    = postgres.NewMetricsRepository(pool)
		inventory  = postgres.NewInventoryQuery(pool)
		auditLog   = postgres.NewAuditQuery(pool)
		consoles   = postgres.NewConsoleRepository(pool)
		shells     = postgres.NewShellRepository(pool)
		hostKeys   = postgres.NewHostKeyStore(pool)
		provisions = postgres.NewProvisionRepository(pool)
		personal   = postgres.NewPersonalRepository(pool)

		edgeProviders = postgres.NewEdgeProviderRepository(pool)
		totpStore     = postgres.NewTOTPRepository(pool)
		tickets       = redisinfra.NewTicketStore(rdb)
		mfaChallenges = redisinfra.NewMFAChallengeStore(rdb)
		limiter       = redisinfra.NewRateLimiter(rdb)
	)

	// Edge providers (ADR 0004). The factory is injected so the command layer
	// never imports a concrete provider, which is what keeps its tests free of
	// a network and lets a second provider be added without touching them.
	edgeDeps := command.EdgeDeps{
		Providers: edgeProviders,
		Vault:     vault,
		Factory: func(creds edge.Credentials) (command.EdgeProvider, error) {
			return cloudflare.New(creds, edge.Options{})
		},
		Audit: audit,
		Clock: clock,
	}

	// The read half of the edge port, shared by the safety commands. Opening
	// the sealed token happens here so no layer above has to know one exists.
	edgeReaderFactory := func(ctx context.Context, providerID uuid.UUID) (*cloudflare.Provider, error) {
		p, err := edgeProviders.Get(ctx, providerID)
		if err != nil {
			return nil, err
		}
		cred, err := edgeProviders.Credential(ctx, providerID, vault)
		if err != nil {
			return nil, err
		}
		return cloudflare.New(edge.Credentials{Token: cred.Secret, AccountID: p.AccountID}, edge.Options{})
	}

	publishedApps := postgres.NewPublishedAppRepository(pool)

	publishDeps := command.PublishDeps{
		Providers: edgeProviders,
		Apps:      publishedApps,
		Factory: func(ctx context.Context, providerID uuid.UUID) (command.EdgeWriter, error) {
			return edgeReaderFactory(ctx, providerID)
		},
		Audit: audit, Clock: clock,
		SelfHostname: cfg.PublicHostname,
	}

	edgeSafety := command.EdgeSafetyDeps{
		Providers: edgeProviders,
		Factory: func(ctx context.Context, providerID uuid.UUID) (command.EdgeIngressReader, error) {
			return edgeReaderFactory(ctx, providerID)
		},
		Audit: audit, Clock: clock,
		SelfHostname: cfg.PublicHostname,
	}

	reconciler := &appsync.Reconciler{
		Platforms: platforms, Assets: assets, Runs: syncRepo,
		Clock: clock, Log: log,
	}
	// Node hardware sensors. Proxmox publishes no temperature, so the reading
	// comes from the node itself over SSH with the portal's own key (ADR 0007).
	// Every part of it degrades to "collects nothing": no portal key, no key
	// installed on the node, or a connector that cannot name its nodes.
	sensorRepo := postgres.NewSensorRepository(pool)
	sensorCollector := &sensor.Collector{
		Hosts: sensorRepo, Store: sensorRepo, SSH: sensorRepo,
		Source: sensors.NewReader(sshclient.NewDialer()),
		Key:    postgres.NewPortalKeyRepository(pool, vault),
		Log:    log, Clock: clock.Now,
	}

	syncService := &appsync.Service{
		Platforms: platforms, Runs: syncRepo, Reconciler: reconciler,
		Metrics: &appsync.MetricsCollector{Metrics: metrics, Clock: clock, Log: log},
		Sensors: sensorCollector,
		Vault:   vault, Clock: clock, Log: log,
	}
	queue := jobs.NewClient(rdb.Client, log)

	// The provisioning driver is built for both roles: the API creates requests
	// and the worker advances them, and both reach the platform through the
	// same connector the sync engine uses (ADR 0010).
	provisionDriver := &provisioner.Driver{
		Requests: provisions, Platforms: platforms, Access: accessRepo,
		Platform: syncService, Queue: queue, Audit: audit, Clock: clock, Log: log,
		// Templates get a guest agent and a cleared identity before they are
		// sealed. Without it every clone reports no address, which is what
		// leaves the portal unable to offer it an SSH session (PROV-14).
		Images: &imageprep.Preparer{
			Hosts:  sensorRepo,
			SSH:    sensorRepo,
			Key:    postgres.NewPortalKeyRepository(pool, vault),
			Runner: sshclient.NewDialer(),
			Log:    log,
		},
	}
	defer queue.Close()

	signingKey, err := crypto.LoadOrCreateRSAKey(cfg.JWTKeyFile)
	if err != nil {
		return err
	}
	tokens := crypto.NewTokenIssuer(signingKey, tokenIssuerName, identity.AccessTokenTTL)

	// Second factor (AUTH-04). The command is shared between the sign-in half
	// and the enrolment half, and the admin reset below, so there is one place
	// that decides what a valid code is.
	mfa := &command.MFA{
		Users: users, TOTP: totpStore, Codec: crypto.TOTPService{Issuer: totpIssuerName},
		Challenges: mfaChallenges, Hasher: hasher,
		Issue: &command.IssueSession{
			Users: users, Sessions: sessions, Tokens: tokens, Audit: audit, Clock: clock,
		},
		Vault: vault, Audit: audit, Clock: clock,
	}

	authDeps := httpapi.AuthDeps{
		Login: &command.Login{
			Users: users, Sessions: sessions, Hasher: hasher, Tokens: tokens,
			Audit: audit, Clock: clock, Challenges: mfaChallenges,
		},
		Refresh: &command.Refresh{Users: users, Sessions: sessions, Tokens: tokens, Audit: audit, Clock: clock},
		ChangePassword: &command.ChangePassword{
			Users: users, Sessions: sessions, Hasher: hasher, Audit: audit, Clock: clock,
		},
		Logout:   &command.Logout{Sessions: sessions, Audit: audit, Clock: clock},
		MFA:      mfa,
		Users:    users,
		Tokens:   tokens,
		Sessions: sessions,
	}

	// Built as a variable so that the shell registry, which only exists in an
	// API process, can be handed to it below: deleting an account has to close
	// the SSH sessions it left open, and only this process knows about them.
	deleteUser := &command.DeleteUser{Users: users, Access: accessRepo, Audit: audit, Clock: clock}

	adminDeps := httpapi.AdminDeps{
		CreateUser:    &command.CreateUser{Users: users, Access: accessRepo, Hasher: hasher, Audit: audit, Clock: clock},
		UpdateUser:    &command.UpdateUser{Users: users, Sessions: sessions, Audit: audit, Clock: clock},
		ResetPassword: &command.ResetPassword{Users: users, Sessions: sessions, Hasher: hasher, Audit: audit, Clock: clock},
		DeleteUser:    deleteUser,
		SetUserGroups: &command.SetUserGroups{Users: users, Access: accessRepo, Audit: audit, Clock: clock},
		ManageAccess:  &command.ManageAccess{Access: accessRepo, Audit: audit, Clock: clock},
		MFA:           mfa,
		Users:         users,
		Access:        accessRepo,
		Audit:         audit,
	}

	platformDeps := httpapi.PlatformDeps{
		Manage: &command.ManagePlatforms{
			Platforms: platforms, Vault: vault, Audit: audit, Clock: clock,
		},
		Platforms: platforms,
		Runs:      syncRepo,
		Sync:      queue,
	}

	// Migrations run on API startup under an advisory lock; worker-only
	// processes trust that an API instance has already applied them.
	if cfg.MigrateOnStart && cfg.Role.RunsAPI() {
		if err := postgres.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
			return err
		}
	}
	readiness.MigrationsApplied.Store(true)

	if cfg.Role.RunsAPI() && cfg.HasBootstrapAdmin() {
		bootstrap := &command.BootstrapAdmin{Users: users, Hasher: hasher, Audit: audit, Clock: clock}
		created, err := bootstrap.Handle(ctx, command.BootstrapAdminInput{
			Username:    cfg.AdminUsername,
			Email:       cfg.AdminEmail,
			DisplayName: cfg.AdminDisplayName,
			Password:    cfg.AdminPassword,
		})
		if err != nil {
			return err
		}
		if created {
			log.Info().Str("username", cfg.AdminUsername).
				Msg("bootstrap administrator created; password change is required at first login")
		}
	}

	// Notifications: one dispatcher serves both the relay (event-driven
	// delivery) and the API (test sends), so a channel proven by a test is
	// the same code path an incident will use.
	notifyRepo := postgres.NewNotifyRepository(pool)
	dispatcher := &appnotify.Dispatcher{
		Repo:    notifyRepo,
		Groups:  accessRepo,
		Senders: infranotify.DefaultRegistry(nil),
		Vault:   vault,
		Clock:   clock,
		Log:     log,
		Audit:   audit,
	}

	// Alerting: the evaluator runs in the scheduler role and publishes to the
	// same outbox everything else does, so an alert reaches channels by the
	// notification path built in sprint 16 rather than a parallel one.
	alertRepo := postgres.NewAlertRepository(pool)
	evaluator := &appalert.Evaluator{
		Repo: alertRepo, Metrics: metrics, VMs: inventory, Groups: accessRepo,
		Sensors: sensorRepo, Hosts: inventory,
		Events: syncRepo, Clock: clock, Log: log,
	}

	// Registration and external sign-in. The policy is read per request, so
	// switching registration off in Settings takes effect at once.
	settingsRepo := postgres.NewSettingsRepository(pool)
	registrationPolicy := &appsetting.Policy{
		Settings: settingsRepo, Vault: vault, Log: log,
		Fallback: oauth.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
		},
	}
	googleClient := oauth.New(registrationPolicy.GoogleConfig, nil)

	// Live state on page load (docs/10 §10.6). The read model is wrapped
	// rather than replaced: `vms` stays the reconciler's table, and this adds
	// an expiring overlay of the fields that go stale in seconds. Every
	// failure - slow platform, open breaker, Redis down - falls back to the
	// synced row, so the worst case is the behaviour the portal had before.
	liveVMs := appsync.NewLive(platforms, redisinfra.NewLiveStateStore(rdb),
		syncService.Connect, clock, log)
	liveInventory := &query.LiveInventory{
		Reader: inventory, Live: liveVMs,
		Enabled: registrationPolicy.LiveReadsEnabled,
	}

	provisionDeps := httpapi.ProvisionDeps{
		Provision: &command.Provision{
			Requests: provisions, Platforms: platforms, Sync: syncService,
			Queue: queue, Audit: audit, Clock: clock,
		},
		Destroy: &command.Destroy{
			Requests: provisions, Inventory: liveInventory, Platforms: platforms,
			Sync: syncService, Queue: queue, Audit: audit, Clock: clock,
		},
		Build: &command.BuildTemplate{
			Requests: provisions, Platforms: platforms, Sync: syncService,
			Queue: queue, Audit: audit, Clock: clock,
		},
		Requests:  provisions,
		Platforms: platforms,
		Sync:      syncService,
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	shutdown := make([]func(context.Context) error, 0, 2)

	// The event hub fans outbox events out to browsers, scoped per subscriber.
	eventHub := ws.NewEventHub(rdb.Client, inventory, log)

	if cfg.Role.RunsAPI() {
		eventHub.Start(ctx)

		// SSH sessions live in this process's memory, because the credential
		// that opened them was typed by an operator and deliberately never
		// stored (SSH-03). The registry is therefore created here rather than
		// wired from infra, and its sweep is what enforces the idle and
		// duration limits on sessions with no terminal attached (SSH-07).
		shellRegistry := shellreg.NewRegistry(clock.Now)
		// The portal's own SSH key (ADR 0006). The vault is bound in here so
		// that nothing above the repository has to hold a decryption argument
		// on its way to a dial.
		portalKeys := postgres.NewPortalKeyRepository(pool, vault)
		closeShell := &command.CloseShell{
			Sessions: shells, Registry: shellRegistry, Audit: audit, Clock: clock,
		}
		// A swept or shut-down session still has to be recorded: an audit trail
		// that only shows the endings someone asked for is not an audit trail.
		shellRegistry.OnEvict = func(l *shellreg.Live, reason string) {
			closeShell.Record(context.WithoutCancel(ctx), l, reason)
		}
		deleteUser.Shells = shellRegistry
		shellTickets := shellreg.NewTicketStore(clock.Now)
		go shellRegistry.Run(ctx.Done(), 30*time.Second)

		apiServer := httpapi.NewServer(httpapi.ServerConfig{
			Log:       log,
			Version:   version,
			Readiness: readiness,
			Auth:      authDeps,
			Admin:     adminDeps,
			Platforms: platformDeps,
			Provision: provisionDeps,
			Personal: httpapi.PersonalDeps{
				Personal: &command.Personal{
					Store: personal, Inventory: liveInventory, Clock: clock,
				},
			},
			Metrics: httpapi.MetricsDeps{Metrics: metrics},
			Inventory: httpapi.InventoryDeps{
				Inventory: liveInventory, Audit: auditLog, Metrics: metrics, Infra: inventory,
			},
			Alerts:   httpapi.AlertDeps{Alerts: alertRepo},
			Settings: httpapi.SettingsDeps{Settings: settingsRepo, Vault: vault},
			Registration: httpapi.RegistrationDeps{
				Register: &command.Register{
					Users: users, Policy: registrationPolicy, Hasher: hasher,
					Audit: audit, Clock: clock,
				},
				External: &command.SignInExternal{
					Users: users, Policy: registrationPolicy, Audit: audit, Clock: clock,
				},
				Sessions: &command.IssueSession{
					Users: users, Sessions: sessions, Tokens: tokens, Audit: audit, Clock: clock,
				},
				OAuth:    googleClient,
				Attempts: redisinfra.NewAttemptStore(rdb),
				Policy:   registrationPolicy,
			},
			Notify: httpapi.NotifyDeps{
				Repo: notifyRepo, Dispatcher: dispatcher, Vault: vault,
			},
			Edge: httpapi.EdgeDeps{
				Register: &command.RegisterEdgeProvider{EdgeDeps: edgeDeps},
				Test:     &command.TestEdgeCredential{EdgeDeps: edgeDeps},
				Verify:   &command.VerifyEdgeProvider{EdgeDeps: edgeDeps},
				Tunnels:  &command.ListEdgeTunnels{EdgeDeps: edgeDeps},
				Snapshot: &command.SnapshotEdgeIngress{EdgeSafetyDeps: edgeSafety},
				Preview:  &command.PreviewEdgeIngress{EdgeSafetyDeps: edgeSafety},
				Ingress: &query.EdgeIngress{
					Providers: edgeProviders,
					Apps:      publishedApps,
					Machines:  inventory,
					// Opening the sealed token stays here, in the composition
					// root, so the query layer has no reason to know a vault
					// exists. The layer test enforces that.
					Factory: func(ctx context.Context, providerID uuid.UUID) (query.IngressReader, error) {
						return edgeReaderFactory(ctx, providerID)
					},
				},
				Repo: edgeProviders,
			},
			Publishing: httpapi.PublishDeps{
				Publish:   &command.PublishApp{PublishDeps: publishDeps},
				Unpublish: &command.UnpublishApp{PublishDeps: publishDeps},
				Apps:      publishedApps,
			},
			Power: httpapi.PowerDeps{
				// The synced reader, deliberately: a power action reads the row
				// only to name the VM and record the state it was in, and then
				// invalidates the overlay anyway. Reading live here would open a
				// second connector to the same platform, inside the click the
				// operator is waiting on, to produce a value discarded two lines
				// later.
				Power: &command.Power{
					Inventory: inventory, Platforms: platforms, Sync: syncService,
					Live:  liveVMs,
					Audit: audit, Clock: clock,
				},
			},
			Sensors: httpapi.SensorDeps{
				Sensors: sensorRepo, Nodes: sensorRepo, Audit: audit,
			},
			Events:        eventHub,
			StreamTickets: httpapi.NewStreamTicketStore(rdb),
			Limiter:       limiterFunc(limiter.AllowRequest),
			SPA:           httpapi.NewSPA(web.Assets()),
			Console: httpapi.ConsoleDeps{
				Open: &command.OpenConsole{
					Inventory: liveInventory, Sessions: consoles, Tickets: tickets,
					Audit: audit, Clock: clock,
				},
				Sessions: consoles,
				Bridge: &ws.ConsoleBridge{
					Tickets:  tickets,
					Sessions: consoles,
					Resolver: &command.ConsoleEndpointResolver{
						Inventory: inventory, Platforms: platforms,
						Sync: syncService, Clock: clock,
					},
					Audit: audit, Clock: clock, Log: log,
				},
			},
			Shell: httpapi.ShellDeps{
				Open: &command.OpenShell{
					Inventory: liveInventory, Sessions: shells, HostKeys: hostKeys,
					Tickets: shellTickets, Registry: shellRegistry,
					Dialer: sshclient.NewDialer(), Audit: audit, Clock: clock,
					Keys: portalKeys,
				},
				Close: closeShell,
				Files: &command.ShellFiles{
					Registry: shellRegistry, Audit: audit, Clock: clock,
				},
				Keys: &command.PortalKey{
					Keys: portalKeys, KeyGen: sshclient.KeyGen{},
					Registry: shellRegistry, Inventory: inventory,
					Audit: audit, Clock: clock,
				},
				Sessions: shells,
				HostKeys: hostKeys,
				Bridge: &ws.ShellBridge{
					Tickets: shellTickets, Sessions: shells, Registry: shellRegistry,
					Closer: closeShell, Clock: clock, Log: log,
				},
			},
			SecureCookies: cfg.SecureCookies,
			Clock:         clock.Now,
		})
		handler := apiServer.Routes()

		// Deny by default: refuse to start if any route lacks a permission-map
		// entry, so an unprotected endpoint can never reach production.
		routes, ok := handler.(chi.Routes)
		if !ok {
			return errors.New("router does not expose chi.Routes; cannot verify permissions")
		}
		if err := httpapi.ValidatePermissions(routes); err != nil {
			return err
		}

		api := &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		shutdown = append(shutdown, api.Shutdown)
		serve(&wg, errCh, log, "api", cfg.HTTPAddr, api)

		metrics := &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           metricsMux(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		shutdown = append(shutdown, metrics.Shutdown)
		serve(&wg, errCh, log, "metrics", cfg.MetricsAddr, metrics)
	}

	// Job consumers and the periodic scheduler arrive with the sync engine
	// (sprint 6). Until then these roles idle so deployment wiring can be
	// exercised end to end.
	if cfg.Role.RunsWorker() {
		relay := &jobs.Relay{
			Store: syncRepo, Redis: rdb.Client, Log: log, Clock: clock,
			Notifier: dispatcher,
		}
		provisionHandler := &jobs.ProvisionHandler{Driver: provisionDriver, Client: queue, Log: log}
		worker := jobs.NewWorker(rdb.Client, &jobs.SyncHandler{Service: syncService, Log: log}, relay,
			provisionHandler, log)
		if err := worker.Start(); err != nil {
			return err
		}
		relay.Start(ctx)
		defer worker.Shutdown()

		// Requests that were mid-flight when this process last stopped are
		// still in the table; nothing else would ever look at them again
		// (ADR 0010).
		if err := provisionHandler.ResumeOpenRequests(ctx); err != nil {
			log.Warn().Err(err).Msg("could not resume open provisioning requests")
		}
	}

	if cfg.Role.RunsScheduler() {
		scheduler := jobs.NewScheduler(queue, platforms, log)
		scheduler.Start(ctx)
		defer scheduler.Stop()

		// Evaluated in the scheduler rather than as a queued job: it is cheap,
		// it must run on a steady cadence for sustained-duration rules to mean
		// anything, and a backlog of evaluations would be worse than a skipped
		// one (NOTIF-04).
		jobs.RunAlertEvaluator(ctx, evaluator, log)
	}

	select {
	case <-ctx.Done():
		log.Info().Msg("signal received, draining")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	for _, fn := range shutdown {
		if err := fn(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("graceful shutdown failed")
		}
	}
	wg.Wait()
	return nil
}

// serve starts srv in the background, reporting unexpected exits on errCh.
func serve(wg *sync.WaitGroup, errCh chan<- error, log zerolog.Logger, name, addr string, srv *http.Server) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("component", name).Str("addr", addr).Msg("listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s server: %w", name, err)
		}
	}()
}

func metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

// healthcheck backs the container HEALTHCHECK: it probes the local liveness
// endpoint and maps the result to an exit code.
func healthcheck() int {
	addr := os.Getenv("PROXUI_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+addr+"/healthz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthz returned %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
