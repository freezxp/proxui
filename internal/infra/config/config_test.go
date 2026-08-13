package config

import (
	"testing"
	"time"
)

func TestLoadAppliesDefaults(t *testing.T) {
	t.Setenv("PROXUI_DATABASE_URL", "postgres://x/y")
	t.Setenv("PROXUI_REDIS_URL", "redis://localhost:6379/0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Role != RoleAll {
		t.Errorf("Role = %q, want %q", cfg.Role, RoleAll)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if !cfg.MigrateOnStart {
		t.Error("MigrateOnStart = false, want true")
	}
}

func TestLoadOverridesFromEnv(t *testing.T) {
	t.Setenv("PROXUI_DATABASE_URL", "postgres://x/y")
	t.Setenv("PROXUI_REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("PROXUI_ROLE", "worker")
	t.Setenv("PROXUI_HTTP_ADDR", ":9999")
	t.Setenv("PROXUI_MIGRATE_ON_START", "false")
	t.Setenv("PROXUI_SHUTDOWN_TIMEOUT", "42s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Role != RoleWorker {
		t.Errorf("Role = %q, want worker", cfg.Role)
	}
	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want :9999", cfg.HTTPAddr)
	}
	if cfg.MigrateOnStart {
		t.Error("MigrateOnStart = true, want false")
	}
	if cfg.ShutdownTimeout != 42*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 42s", cfg.ShutdownTimeout)
	}
}

func TestLoadRejectsMissingAndInvalidValues(t *testing.T) {
	t.Setenv("PROXUI_DATABASE_URL", "")
	t.Setenv("PROXUI_REDIS_URL", "")
	t.Setenv("PROXUI_ROLE", "nonsense")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want validation errors")
	}
}

func TestRoleDispatch(t *testing.T) {
	tests := []struct {
		role                         Role
		wantAPI, wantWorker, wantSch bool
	}{
		{RoleAll, true, true, true},
		{RoleAPI, true, false, false},
		{RoleWorker, false, true, false},
		{RoleScheduler, false, false, true},
	}
	for _, tt := range tests {
		if got := tt.role.RunsAPI(); got != tt.wantAPI {
			t.Errorf("%s.RunsAPI() = %v, want %v", tt.role, got, tt.wantAPI)
		}
		if got := tt.role.RunsWorker(); got != tt.wantWorker {
			t.Errorf("%s.RunsWorker() = %v, want %v", tt.role, got, tt.wantWorker)
		}
		if got := tt.role.RunsScheduler(); got != tt.wantSch {
			t.Errorf("%s.RunsScheduler() = %v, want %v", tt.role, got, tt.wantSch)
		}
	}
}
