// Package config loads runtime configuration from the environment.
//
// Precedence is env vars -> compiled defaults. Runtime-tunable settings live in
// the database `settings` table (added in a later sprint); only bootstrap values
// that must exist before the database is reachable belong here.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role selects which parts of the system this process runs.
type Role string

const (
	RoleAll       Role = "all"
	RoleAPI       Role = "api"
	RoleWorker    Role = "worker"
	RoleScheduler Role = "scheduler"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleAll, RoleAPI, RoleWorker, RoleScheduler:
		return true
	}
	return false
}

// RunsAPI reports whether this role serves HTTP traffic.
func (r Role) RunsAPI() bool { return r == RoleAll || r == RoleAPI }

// RunsWorker reports whether this role consumes background jobs.
func (r Role) RunsWorker() bool { return r == RoleAll || r == RoleWorker }

// RunsScheduler reports whether this role enqueues periodic jobs.
func (r Role) RunsScheduler() bool { return r == RoleAll || r == RoleScheduler }

// Config is the fully resolved process configuration.
type Config struct {
	Role             Role
	Environment      string // dev | staging | prod
	HTTPAddr         string
	MetricsAddr      string
	DatabaseURL      string
	RedisURL         string
	LogLevel         string
	LogFormat        string // json | console
	MigrateOnStart   bool
	ShutdownTimeout  time.Duration
	ReadinessTimeout time.Duration

	// JWTKeyFile holds the RSA signing key for access tokens. It is generated
	// on first start if absent.
	JWTKeyFile string
	// MasterKeyFile holds the key that seals every platform credential. Losing
	// it means re-entering platform tokens; leaking it means they are readable.
	MasterKeyFile string
	// SecureCookies requires the Secure flag on the refresh cookie even when
	// the request did not arrive over TLS. It is a floor, not the whole rule:
	// a request that did arrive over TLS gets the flag regardless, so setting
	// this false cannot weaken the HTTPS path. Set it false only for a portal
	// that is also signed into over a plain-HTTP address, where a Secure
	// cookie would never be sent back and nobody could sign in at all.
	SecureCookies bool

	// PublicHostname is the name this portal is served at from outside — say
	// "vm.example.com". It is not used for routing; it is used to recognise
	// the portal's own rule in an edge provider's table so that no change can
	// delete or shadow it (docs/28, PUB-33).
	//
	// Configured rather than taken from the request because the protection has
	// to hold for an administrator working from the LAN address, who would
	// otherwise present a Host header that matches nothing in the table and
	// silently disable the guard. Empty means the portal is not published
	// through any managed provider, and the guard stays off.
	PublicHostname string

	// First-run administrator (ADM-03). Ignored once any account exists.
	AdminUsername    string
	AdminEmail       string
	AdminDisplayName string
	AdminPassword    string

	// Google sign-in. Deployment configuration rather than a setting: a
	// client secret in the settings table would sit there in plain text, and
	// this belongs with the master key and the database URL.
	GoogleClientID     string
	GoogleClientSecret string
	// GoogleRedirectURL pins the redirect URI to one value. Leave it unset and
	// each sign-in returns to the address the browser reached the portal at,
	// which is what a portal answering to several names needs; every one of
	// them has to be registered with Google either way. Set it for the
	// deployment behind a proxy whose public address the portal never sees.
	GoogleRedirectURL string
}

// HasBootstrapAdmin reports whether first-run admin credentials were supplied.
func (c Config) HasBootstrapAdmin() bool {
	return c.AdminUsername != "" && c.AdminPassword != "" && c.AdminEmail != ""
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	cfg := Config{
		Role:             Role(env("PROXUI_ROLE", string(RoleAll))),
		Environment:      env("PROXUI_ENV", "dev"),
		HTTPAddr:         env("PROXUI_HTTP_ADDR", ":8080"),
		MetricsAddr:      env("PROXUI_METRICS_ADDR", ":9090"),
		DatabaseURL:      env("PROXUI_DATABASE_URL", ""),
		RedisURL:         env("PROXUI_REDIS_URL", ""),
		LogLevel:         env("PROXUI_LOG_LEVEL", "info"),
		LogFormat:        env("PROXUI_LOG_FORMAT", "json"),
		MigrateOnStart:   envBool("PROXUI_MIGRATE_ON_START", true),
		ShutdownTimeout:  envDuration("PROXUI_SHUTDOWN_TIMEOUT", 15*time.Second),
		ReadinessTimeout: envDuration("PROXUI_READINESS_TIMEOUT", 2*time.Second),
		JWTKeyFile:       env("PROXUI_JWT_KEY_FILE", "secrets/jwt-signing-key.pem"),
		MasterKeyFile:    env("PROXUI_MASTER_KEY_FILE", "secrets/master.key"),
		SecureCookies:    envBool("PROXUI_SECURE_COOKIES", true),
		PublicHostname:   strings.TrimSpace(os.Getenv("PROXUI_PUBLIC_HOSTNAME")),
		AdminUsername:    env("PROXUI_ADMIN_USERNAME", ""),
		AdminEmail:       env("PROXUI_ADMIN_EMAIL", ""),
		AdminDisplayName: env("PROXUI_ADMIN_DISPLAY_NAME", ""),
		AdminPassword:    env("PROXUI_ADMIN_PASSWORD", ""),

		GoogleClientID:     env("PROXUI_GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env("PROXUI_GOOGLE_CLIENT_SECRET", ""),
		GoogleRedirectURL:  env("PROXUI_GOOGLE_REDIRECT_URL", ""),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports configuration that would prevent the process from running.
func (c Config) Validate() error {
	var errs []error
	if !c.Role.Valid() {
		errs = append(errs, fmt.Errorf("PROXUI_ROLE %q is not one of all|api|worker|scheduler", c.Role))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("PROXUI_DATABASE_URL is required"))
	}
	if c.RedisURL == "" {
		errs = append(errs, errors.New("PROXUI_REDIS_URL is required"))
	}
	if c.LogFormat != "json" && c.LogFormat != "console" {
		errs = append(errs, fmt.Errorf("PROXUI_LOG_FORMAT %q is not json|console", c.LogFormat))
	}
	return errors.Join(errs...)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
