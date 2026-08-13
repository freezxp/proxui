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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/config"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/logging"
	"github.com/freezxp/proxui/internal/infra/postgres"
	redisinfra "github.com/freezxp/proxui/internal/infra/redis"
	httpapi "github.com/freezxp/proxui/internal/transport/http"
)

// tokenIssuerName is the `iss` claim on access tokens.
const tokenIssuerName = "proxui"

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

	signingKey, err := crypto.LoadOrCreateRSAKey(cfg.JWTKeyFile)
	if err != nil {
		return err
	}
	tokens := crypto.NewTokenIssuer(signingKey, tokenIssuerName, identity.AccessTokenTTL)

	authDeps := httpapi.AuthDeps{
		Login:    &command.Login{Users: users, Sessions: sessions, Hasher: hasher, Tokens: tokens, Audit: audit, Clock: clock},
		Refresh:  &command.Refresh{Users: users, Sessions: sessions, Tokens: tokens, Audit: audit, Clock: clock},
		Logout:   &command.Logout{Sessions: sessions, Audit: audit, Clock: clock},
		Users:    users,
		Tokens:   tokens,
		Sessions: sessions,
	}

	adminDeps := httpapi.AdminDeps{
		CreateUser:    &command.CreateUser{Users: users, Access: accessRepo, Hasher: hasher, Audit: audit, Clock: clock},
		UpdateUser:    &command.UpdateUser{Users: users, Sessions: sessions, Audit: audit, Clock: clock},
		ResetPassword: &command.ResetPassword{Users: users, Sessions: sessions, Hasher: hasher, Audit: audit, Clock: clock},
		SetUserGroups: &command.SetUserGroups{Users: users, Access: accessRepo, Audit: audit, Clock: clock},
		ManageAccess:  &command.ManageAccess{Access: accessRepo, Audit: audit, Clock: clock},
		Users:         users,
		Access:        accessRepo,
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

	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	shutdown := make([]func(context.Context) error, 0, 2)

	if cfg.Role.RunsAPI() {
		apiServer := httpapi.NewServer(httpapi.ServerConfig{
			Log:           log,
			Version:       version,
			Readiness:     readiness,
			Auth:          authDeps,
			Admin:         adminDeps,
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
		log.Warn().Str("component", "worker").Msg("no job handlers registered yet")
	}
	if cfg.Role.RunsScheduler() {
		log.Warn().Str("component", "scheduler").Msg("no periodic jobs registered yet")
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
