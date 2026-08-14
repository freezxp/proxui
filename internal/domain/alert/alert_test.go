package alert_test

import (
	"strings"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/domain/alert"
)

func cpuRule() alert.Rule {
	return alert.Rule{
		Name: "CPU burn", Metric: alert.MetricCPUPct, Op: alert.OpGreater,
		Threshold: 90, DurationS: 600, CooldownS: 1800, Severity: "warning", IsEnabled: true,
	}
}

// A spike must not become an alert. This is the whole reason for the pending
// state, and the easiest behaviour to lose in a refactor.
func TestBriefSpikeNeverFires(t *testing.T) {
	rule := cpuRule()
	start := time.Now()

	first := alert.Evaluate(rule, alert.Status{}, 95, start)
	if first.State != alert.StatePending {
		t.Fatalf("first breach = %q, want pending", first.State)
	}
	if first.Notify {
		t.Error("a single breaching sample notified immediately")
	}

	// One minute later the metric is fine again: back to ok, silently.
	previous := alert.Status{State: first.State, Since: first.Since}
	recovered := alert.Evaluate(rule, previous, 10, start.Add(time.Minute))
	if recovered.State != alert.StateOK {
		t.Errorf("state after the spike passed = %q, want ok", recovered.State)
	}
	if recovered.Notify || recovered.Recovered {
		t.Error("a spike that never fired sent a recovery message")
	}
}

func TestSustainedBreachFiresOnceTheDurationElapses(t *testing.T) {
	rule := cpuRule()
	start := time.Now()

	decision := alert.Evaluate(rule, alert.Status{}, 95, start)
	previous := alert.Status{State: decision.State, Since: decision.Since}

	// Nine minutes in: still pending, still silent.
	nine := alert.Evaluate(rule, previous, 96, start.Add(9*time.Minute))
	if nine.State != alert.StatePending || nine.Notify {
		t.Errorf("at 9 minutes = %q notify=%v, want pending and silent", nine.State, nine.Notify)
	}

	// Ten minutes in: fires, and says so.
	ten := alert.Evaluate(rule, previous, 97, start.Add(10*time.Minute))
	if ten.State != alert.StateFiring {
		t.Fatalf("at 10 minutes = %q, want firing", ten.State)
	}
	if !ten.Fired || !ten.Notify {
		t.Error("crossing into firing did not notify")
	}
	// Since must point at when the breach began, not when it fired: an
	// operator asking "how long has this been bad" wants the former.
	if !ten.Since.Equal(start) {
		t.Errorf("since = %v, want the start of the breach %v", ten.Since, start)
	}
}

// A rule with no sustained duration is a "tell me immediately" rule.
func TestZeroDurationFiresOnFirstBreach(t *testing.T) {
	rule := cpuRule()
	rule.DurationS = 0

	decision := alert.Evaluate(rule, alert.Status{}, 95, time.Now())
	if decision.State != alert.StateFiring || !decision.Notify {
		t.Errorf("zero-duration rule = %q notify=%v, want firing and notifying", decision.State, decision.Notify)
	}
}

// While an alert stays firing it must not repeat every minute, but it must not
// go silent forever either (NOTIF-05).
func TestFiringRepeatsOnlyAfterTheCooldown(t *testing.T) {
	rule := cpuRule()
	start := time.Now()
	notified := start
	firing := alert.Status{State: alert.StateFiring, Since: start, LastNotifiedAt: &notified}

	quiet := alert.Evaluate(rule, firing, 99, start.Add(20*time.Minute))
	if quiet.State != alert.StateFiring {
		t.Fatalf("state = %q, want it still firing", quiet.State)
	}
	if quiet.Notify {
		t.Error("a firing alert repeated inside its cooldown")
	}

	loud := alert.Evaluate(rule, firing, 99, start.Add(31*time.Minute))
	if !loud.Notify {
		t.Error("a firing alert stayed silent past its cooldown")
	}
	if loud.Fired {
		t.Error("a repeat was reported as a fresh firing")
	}
}

// A cooldown of zero means "never repeat", which is a legitimate choice for a
// noisy rule and must not be read as "repeat every evaluation".
func TestZeroCooldownNeverRepeats(t *testing.T) {
	rule := cpuRule()
	rule.CooldownS = 0
	start := time.Now()
	notified := start
	firing := alert.Status{State: alert.StateFiring, Since: start, LastNotifiedAt: &notified}

	for _, after := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
		if alert.Evaluate(rule, firing, 99, start.Add(after)).Notify {
			t.Errorf("a zero-cooldown rule repeated after %s", after)
		}
	}
}

func TestRecoveryNotifiesOnce(t *testing.T) {
	rule := cpuRule()
	start := time.Now()
	firing := alert.Status{State: alert.StateFiring, Since: start}

	recovered := alert.Evaluate(rule, firing, 12, start.Add(time.Hour))
	if recovered.State != alert.StateOK {
		t.Fatalf("state = %q, want ok", recovered.State)
	}
	if !recovered.Recovered || !recovered.Notify {
		t.Error("recovery from a firing alert was silent")
	}

	// Already recovered: nothing more to say.
	ok := alert.Status{State: alert.StateOK, Since: recovered.Since}
	again := alert.Evaluate(rule, ok, 12, start.Add(2*time.Hour))
	if again.Notify || again.Recovered {
		t.Error("an alert that was already ok announced recovery again")
	}
}

func TestLessThanOperator(t *testing.T) {
	rule := cpuRule()
	rule.Op = alert.OpLess
	rule.Threshold = 5
	rule.DurationS = 0

	if !alert.Evaluate(rule, alert.Status{}, 1, time.Now()).Fired {
		t.Error("a below-threshold rule did not fire under its threshold")
	}
	if alert.Evaluate(rule, alert.Status{}, 50, time.Now()).Fired {
		t.Error("a below-threshold rule fired above its threshold")
	}
}

func TestValidationRejectsRulesThatCanNeverFire(t *testing.T) {
	cases := []struct {
		name string
		rule alert.Rule
	}{
		{"no name", alert.Rule{Metric: alert.MetricCPUPct, Op: alert.OpGreater, Threshold: 90}},
		{"unknown metric", alert.Rule{Name: "x", Metric: "vibes", Op: alert.OpGreater, Threshold: 1}},
		{"unknown operator", alert.Rule{Name: "x", Metric: alert.MetricCPUPct, Op: "~", Threshold: 1}},
		{"percentage over 100", alert.Rule{Name: "x", Metric: alert.MetricCPUPct, Op: alert.OpGreater, Threshold: 150}},
		{"negative threshold", alert.Rule{Name: "x", Metric: alert.MetricNetRxBps, Op: alert.OpGreater, Threshold: -1}},
	}
	for _, tc := range cases {
		if err := tc.rule.Validate(); err == nil {
			t.Errorf("%s: was accepted", tc.name)
		}
	}
	if err := cpuRule().Validate(); err != nil {
		t.Errorf("a reasonable rule was rejected: %v", err)
	}
}

func TestDescribeSaysWhatAndForHowLong(t *testing.T) {
	rule := cpuRule()

	subject, body := alert.Describe(rule, "devops", 97.5, false)
	if !strings.Contains(subject, "devops") {
		t.Errorf("subject %q does not name the VM", subject)
	}
	for _, want := range []string{"above", "90", "10 minutes", "97.5"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q is missing %q", body, want)
		}
	}

	subject, body = alert.Describe(rule, "devops", 12, true)
	if !strings.HasPrefix(subject, "Resolved") {
		t.Errorf("recovery subject %q does not read as a resolution", subject)
	}
	if !strings.Contains(body, "no longer") || !strings.Contains(body, "12") {
		t.Errorf("recovery body %q does not say it recovered or where it now sits", body)
	}
}

// A message is read by a person under pressure. Full float precision and
// "for at least 0 seconds" both make one harder to act on.
func TestDescribeIsReadable(t *testing.T) {
	rule := cpuRule()

	_, body := alert.Describe(rule, "devops", 4.634626865386963, false)
	if strings.Contains(body, "4.634626865386963") {
		t.Errorf("body %q carries raw float precision", body)
	}
	if !strings.Contains(body, "4.6%") {
		t.Errorf("body %q does not round the value", body)
	}

	instant := cpuRule()
	instant.DurationS = 0
	_, body = alert.Describe(instant, "devops", 95, false)
	if strings.Contains(body, "0 seconds") {
		t.Errorf("body %q reports a zero sustained duration", body)
	}

	rate := alert.Rule{
		Name: "busy disk", Metric: alert.MetricDiskReadBps, Op: alert.OpGreater,
		Threshold: 1048576, DurationS: 60,
	}
	_, body = alert.Describe(rate, "devops", 5242880, false)
	// A byte rate in units beats a nine-digit number.
	if !strings.Contains(body, "MiB/s") {
		t.Errorf("body %q does not scale a byte rate into readable units", body)
	}
}
