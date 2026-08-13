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
	// SecureCookies marks the refresh cookie Secure; disable only for local
	// HTTP development.
	SecureCookies bool

	// First-run administrator (ADM-03). Ignored once any account exists.
	AdminUsername    string
	AdminEmail       string
	AdminDisplayName string
	AdminPassword    string
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
		SecureCookies:    envBool("PROXUI_SECURE_COOKIES", true),
		AdminUsername:    env("PROXUI_ADMIN_USERNAME", ""),
		AdminEmail:       env("PROXUI_ADMIN_EMAIL", ""),
		AdminDisplayName: env("PROXUI_ADMIN_DISPLAY_NAME", ""),
		AdminPassword:    env("PROXUI_ADMIN_PASSWORD", ""),
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
