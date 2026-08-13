package inventory

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	p := &Platform{IsEnabled: true, Health: HealthHealthy}

	for i := 1; i < BreakerFailureThreshold; i++ {
		if opened := p.RecordSyncFailure(now, "timeout"); opened {
			t.Fatalf("breaker opened after %d failures, want %d", i, BreakerFailureThreshold)
		}
		if !p.ShouldSync(now) {
			t.Errorf("sync suppressed after only %d failures", i)
		}
	}

	if opened := p.RecordSyncFailure(now, "timeout"); !opened {
		t.Fatalf("breaker did not open on failure %d", BreakerFailureThreshold)
	}
	if p.ShouldSync(now) {
		t.Error("ShouldSync = true with the breaker open; a dead platform would be hammered")
	}
	// The cooldown must expire on its own so a recovered platform resumes
	// without operator intervention.
	if !p.ShouldSync(now.Add(BreakerCooldown + time.Second)) {
		t.Error("breaker never reopens; a recovered platform would stay dark")
	}
}

func TestSuccessClosesBreakerAndReportsRecovery(t *testing.T) {
	p := &Platform{IsEnabled: true}
	for i := 0; i < BreakerFailureThreshold; i++ {
		p.RecordSyncFailure(now, "down")
	}

	if recovered := p.RecordSyncSuccess(now); !recovered {
		t.Error("recovery from a failing platform was not reported")
	}
	if p.ConsecutiveFailures != 0 || p.BreakerOpen(now) {
		t.Errorf("breaker state survived success: failures=%d openUntil=%v", p.ConsecutiveFailures, p.BreakerOpenUntil)
	}
	if p.Health != HealthHealthy {
		t.Errorf("health = %q after success, want healthy", p.Health)
	}

	// A healthy platform succeeding again is not a "recovery" worth notifying.
	if recovered := p.RecordSyncSuccess(now); recovered {
		t.Error("a routine success was reported as a recovery; operators would be spammed")
	}
}

func TestShouldSyncRespectsDisabledAndDeleted(t *testing.T) {
	tests := []struct {
		name     string
		platform Platform
		want     bool
	}{
		{"enabled", Platform{IsEnabled: true}, true},
		{"disabled", Platform{IsEnabled: false}, false},
		{"deleted", Platform{IsEnabled: true, DeletedAt: now}, false},
		{"breaker open", Platform{IsEnabled: true, BreakerOpenUntil: now.Add(time.Minute)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.platform.ShouldSync(now); got != tt.want {
				t.Errorf("ShouldSync() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMarkMissingRequiresThreeStrikes(t *testing.T) {
	state, count := SyncActive, 0

	for i := 1; i < MissingThreshold; i++ {
		var deleted bool
		state, count, deleted = MarkMissing(state, count, now)
		if deleted {
			t.Fatalf("asset deleted after %d misses, want %d", i, MissingThreshold)
		}
		if state != SyncMissing {
			t.Errorf("state = %q after %d misses, want missing", state, i)
		}
	}

	state, count, deleted := MarkMissing(state, count, now)
	if !deleted {
		t.Fatalf("asset not deleted after %d misses", MissingThreshold)
	}
	if state != SyncDeleted {
		t.Errorf("state = %q, want deleted", state)
	}
	if count != MissingThreshold {
		t.Errorf("missing count = %d, want %d", count, MissingThreshold)
	}
}

func TestPlatformValidation(t *testing.T) {
	valid := Platform{Name: "pve-dc1", Type: "proxmox", EndpointURL: "https://10.0.30.111:8006"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid platform rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Platform)
	}{
		{"no name", func(p *Platform) { p.Name = "" }},
		{"no type", func(p *Platform) { p.Type = "" }},
		{"no endpoint", func(p *Platform) { p.EndpointURL = "" }},
		{"bad TLS mode", func(p *Platform) { p.TLSMode = "trustme" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid
			tt.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Error("invalid platform accepted")
			}
		})
	}
}

func TestDefaultSyncIntervals(t *testing.T) {
	got := DefaultSyncIntervals()
	if got.Inventory != DefaultInventoryInterval || got.Metrics != DefaultMetricsInterval || got.Health != DefaultHealthInterval {
		t.Errorf("defaults = %+v", got)
	}
	if got.Health > got.Inventory {
		t.Error("health probes should run at least as often as inventory: they drive the breaker")
	}
}
