package notify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/notify"
)

func TestSeverityOrdering(t *testing.T) {
	cases := []struct {
		have, minimum notify.Severity
		want          bool
	}{
		{notify.SeverityCritical, notify.SeverityWarning, true},
		{notify.SeverityWarning, notify.SeverityWarning, true},
		{notify.SeverityInfo, notify.SeverityWarning, false},
		{notify.SeverityInfo, notify.SeverityInfo, true},
		{notify.SeverityCritical, notify.SeverityInfo, true},
		// An unrecognized severity ranks as info: deliverable by a rule that
		// wants everything, and filtered out by one that wants only criticals.
		{notify.Severity("weird"), notify.SeverityInfo, true},
		{notify.Severity("weird"), notify.SeverityCritical, false},
	}
	for _, tc := range cases {
		if got := tc.have.AtLeast(tc.minimum); got != tc.want {
			t.Errorf("%q.AtLeast(%q) = %v, want %v", tc.have, tc.minimum, got, tc.want)
		}
	}
}

func baseEvent() notify.Event {
	return notify.Event{
		OutboxID: 1, Category: notify.CategorySyncFailure, Type: "sync.failed",
		Severity: notify.SeverityCritical, OccurredAt: time.Now(),
		Payload: map[string]any{"platform_name": "pve-home", "error": "connection refused"},
	}
}

func TestRuleMatching(t *testing.T) {
	platform := uuid.New()
	other := uuid.New()
	event := baseEvent()
	event.PlatformID = &platform

	cases := []struct {
		name string
		rule notify.Rule
		want bool
	}{
		{"matching category and severity", notify.Rule{
			Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityWarning, IsEnabled: true,
		}, true},
		{"disabled rule never fires", notify.Rule{
			Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityInfo,
		}, false},
		{"different category", notify.Rule{
			Category: notify.CategoryVMStateChange, MinSeverity: notify.SeverityInfo, IsEnabled: true,
		}, false},
		{"severity below the minimum", notify.Rule{
			Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityCritical, IsEnabled: true,
		}, true},
		{"scoped to this platform", notify.Rule{
			Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityInfo,
			PlatformID: &platform, IsEnabled: true,
		}, true},
		{"scoped to another platform", notify.Rule{
			Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityInfo,
			PlatformID: &other, IsEnabled: true,
		}, false},
	}
	for _, tc := range cases {
		if got := tc.rule.Matches(event); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An event with no platform must not match a rule scoped to one. Treating a
// missing platform as "any" would widen the scope, which is the wrong
// direction to be wrong in.
func TestPlatformScopedRuleIgnoresUnattributedEvents(t *testing.T) {
	platform := uuid.New()
	rule := notify.Rule{
		Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityInfo,
		PlatformID: &platform, IsEnabled: true,
	}
	if rule.Matches(baseEvent()) {
		t.Error("an event with no platform matched a platform-scoped rule")
	}
}

func TestRenderNamesWhatHappened(t *testing.T) {
	msg := notify.Render(baseEvent())
	if !strings.Contains(msg.Subject, "pve-home") {
		t.Errorf("subject %q does not name the platform", msg.Subject)
	}
	if !strings.Contains(msg.Body, "connection refused") {
		t.Errorf("body %q does not carry the reason", msg.Body)
	}
	// The subject has to survive a mail client's list view, where a prefix
	// like "ProxUI notification" would push the real content out of sight.
	if strings.HasPrefix(msg.Subject, "ProxUI") {
		t.Errorf("subject %q leads with the portal's name rather than the event", msg.Subject)
	}
}

func TestRenderStateChange(t *testing.T) {
	event := notify.Event{
		Category: notify.CategoryVMStateChange, Type: "vm.state_changed",
		Severity: notify.SeverityInfo,
		Payload:  map[string]any{"name": "devops", "old_state": "running", "new_state": "stopped"},
	}
	msg := notify.Render(event)
	if !strings.Contains(msg.Subject, "devops") || !strings.Contains(msg.Subject, "stopped") {
		t.Errorf("subject %q should name the VM and its new state", msg.Subject)
	}
	if !strings.Contains(msg.Body, "running") {
		t.Errorf("body %q should say what it changed from", msg.Body)
	}
}

// Two identical events must render identically, or a downstream system that
// deduplicates on message content will fail to.
func TestRenderIsDeterministic(t *testing.T) {
	event := notify.Event{
		Category: notify.CategorySyncFailure, Type: "sync.failed",
		Payload: map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "platform_name": "x"},
	}
	first := notify.Render(event).Body
	for i := 0; i < 20; i++ {
		if got := notify.Render(event).Body; got != first {
			t.Fatalf("render is not stable across calls:\n%q\n%q", first, got)
		}
	}
}

func TestKindAndCategoryValidation(t *testing.T) {
	for _, kind := range []notify.Kind{notify.KindEmail, notify.KindSlack, notify.KindWebhook} {
		if !kind.Valid() {
			t.Errorf("%q should be a valid kind", kind)
		}
	}
	if notify.Kind("carrier-pigeon").Valid() {
		t.Error("an unknown kind was accepted")
	}
	if !notify.ValidCategory(notify.CategorySecurity) {
		t.Error("security should be a valid category")
	}
	if notify.ValidCategory("gossip") {
		t.Error("an unknown category was accepted")
	}
}

// Alerts render their own wording, including the threshold and duration.
// Falling back to "performance_alert: alert.firing" would throw that away and
// deliver a message nobody can act on.
func TestRenderPrefersASelfDescribingEvent(t *testing.T) {
	event := notify.Event{
		Category: notify.CategoryPerformanceAlert, Type: "alert.firing",
		Severity: notify.SeverityWarning,
		Payload: map[string]any{
			"subject": "CPU burn on devops: CPU 97.5%",
			"body":    "CPU on devops has been above 90% for at least 10 minutes.",
			"vm_name": "devops",
		},
	}
	msg := notify.Render(event)
	if msg.Subject != "CPU burn on devops: CPU 97.5%" {
		t.Errorf("subject = %q, want the alert's own wording", msg.Subject)
	}
	if !strings.Contains(msg.Body, "10 minutes") {
		t.Errorf("body %q lost the alert's detail", msg.Body)
	}
	// The generic fallback must not reappear underneath it.
	if strings.Contains(msg.Subject, "alert.firing") {
		t.Errorf("subject %q leaked the event type", msg.Subject)
	}
}
