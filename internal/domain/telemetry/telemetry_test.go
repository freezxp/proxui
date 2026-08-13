package telemetry

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestSelectResolutionByWindow(t *testing.T) {
	tests := []struct {
		name   string
		window time.Duration
		want   Resolution
	}{
		{"one hour uses raw samples", time.Hour, ResolutionRaw},
		{"six hours still raw", 6 * time.Hour, ResolutionRaw},
		{"24 hours", 24 * time.Hour, Resolution5m},
		{"7 days", 7 * 24 * time.Hour, Resolution30m},
		{"30 days", 30 * 24 * time.Hour, Resolution3h},
		{"one year", 365 * 24 * time.Hour, Resolution3h},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelectResolution(now.Add(-tt.window), now, now)
			if got != tt.want {
				t.Errorf("SelectResolution(%v) = %q, want %q", tt.window, got, tt.want)
			}
		})
	}
}

// Every choice must stay inside the point budget, or a chart request could
// return an unbounded number of rows.
func TestSelectResolutionRespectsPointBudget(t *testing.T) {
	windows := []time.Duration{
		time.Hour, 6 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour,
		7 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour,
		365 * 24 * time.Hour,
	}
	for _, w := range windows {
		r := SelectResolution(now.Add(-w), now, now)
		points := int(w / Bucket(r))
		if points > MaxPoints {
			t.Errorf("window %v chose %q giving %d points, over the %d cap", w, r, points, MaxPoints)
		}
	}
}

// Asking for data older than a resolution's retention would return an empty
// chart, so the selector must move to one that still holds it.
func TestSelectResolutionSkipsExpiredData(t *testing.T) {
	// A one-hour window from three months ago: raw and 5m are long gone.
	from := now.Add(-90 * 24 * time.Hour)
	got := SelectResolution(from, from.Add(time.Hour), now)

	if got == ResolutionRaw || got == Resolution5m {
		t.Errorf("SelectResolution chose %q for data past its retention", got)
	}
}

func TestRateFromCounters(t *testing.T) {
	got, ok := Rate(CounterReading{
		Previous: 1000, PreviousTime: now,
		Current: 7000, CurrentTime: now.Add(time.Minute),
	})
	if !ok {
		t.Fatal("a normal counter pair produced no rate")
	}
	if got != 100 {
		t.Errorf("rate = %v, want 100 per second", got)
	}
}

// These are the cases that produce fictional spikes if handled naively.
func TestRateRejectsMeaninglessReadings(t *testing.T) {
	tests := []struct {
		name    string
		reading CounterReading
	}{
		{"no previous reading", CounterReading{Current: 500, CurrentTime: now}},
		{"counter reset by a reboot", CounterReading{
			Previous: 1e9, PreviousTime: now, Current: 42, CurrentTime: now.Add(time.Minute)}},
		{"zero elapsed time", CounterReading{
			Previous: 10, PreviousTime: now, Current: 20, CurrentTime: now}},
		{"clock went backwards", CounterReading{
			Previous: 10, PreviousTime: now, Current: 20, CurrentTime: now.Add(-time.Minute)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := Rate(tt.reading); ok {
				t.Error("produced a rate from a reading that cannot yield one")
			}
		})
	}
}

func TestRateWithinGapRejectsStaleReadings(t *testing.T) {
	fresh := CounterReading{
		Previous: 0, PreviousTime: now,
		Current: 60000, CurrentTime: now.Add(time.Minute),
	}
	if _, ok := RateWithinGap(fresh); !ok {
		t.Error("a one-minute gap was rejected")
	}

	stale := CounterReading{
		Previous: 0, PreviousTime: now,
		Current: 60000, CurrentTime: now.Add(MaxCounterGap + time.Minute),
	}
	if _, ok := RateWithinGap(stale); ok {
		t.Error("averaged a rate across a gap long enough to hide everything inside it")
	}
}

func TestBucketAndRetentionAreDefinedForEveryResolution(t *testing.T) {
	for _, r := range []Resolution{ResolutionRaw, Resolution5m, Resolution30m, Resolution3h} {
		if Bucket(r) == 0 {
			t.Errorf("resolution %q has no bucket width", r)
		}
		if Retention(r) == 0 {
			t.Errorf("resolution %q has no retention", r)
		}
	}
	// Coarser resolutions must be kept longer, or the selector's fallback
	// order would be nonsense.
	if !(Retention(ResolutionRaw) < Retention(Resolution5m) &&
		Retention(Resolution5m) < Retention(Resolution30m) &&
		Retention(Resolution30m) < Retention(Resolution3h)) {
		t.Error("retention does not increase with coarser resolution")
	}
}
