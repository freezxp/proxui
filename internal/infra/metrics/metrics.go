// Package metrics is the portal's own Prometheus instrumentation
// (docs/16-observability.md §16.2).
//
// These are deliberately few. Every metric here answers a question an operator
// asks during an incident — is it up, is it syncing, is it delivering, is
// someone attacking the login — and nothing is exported because it was easy to
// count.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTP: the RED signals. Route rather than raw path, so a thousand VM
	// identifiers do not become a thousand time series.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxui_http_requests_total",
		Help: "HTTP requests handled, by route, method and status class.",
	}, []string{"route", "method", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "proxui_http_request_duration_seconds",
		Help: "How long requests take, by route and method.",
		// Buckets straddle the 500 ms target in NFR-P1 so the alert on p95 has
		// resolution where it matters.
		Buckets: []float64{0.005, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"route", "method"})

	// Synchronization: whether the estate's picture is current.
	SyncRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxui_sync_runs_total",
		Help: "Synchronization runs, by platform, kind and outcome.",
	}, []string{"platform", "kind", "status"})

	SyncDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxui_sync_duration_seconds",
		Help:    "How long a synchronization run takes.",
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"platform", "kind"})

	// PlatformUp is the single most useful gauge here: 0 means the portal
	// cannot see a platform, whatever the reason.
	PlatformUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxui_platform_up",
		Help: "1 when the platform last responded, 0 when it did not.",
	}, []string{"platform"})

	SyncLastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxui_sync_last_success_timestamp_seconds",
		Help: "Unix time of the last successful run. Its age is the real signal.",
	}, []string{"platform", "kind"})

	Assets = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxui_assets",
		Help: "Assets known per platform and type. A sudden drop means a sync went wrong.",
	}, []string{"platform", "type"})

	MetricSamples = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxui_metrics_samples_written_total",
		Help: "Performance samples stored. A flatline means collection broke.",
	}, []string{"platform"})

	// Consoles: capacity, and the security-relevant fact that they happened.
	ConsoleSessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "proxui_console_sessions_active",
		Help: "Console sessions currently connected.",
	})

	ConsoleSessions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxui_console_sessions_total",
		Help: "Console sessions opened, by how they ended.",
	}, []string{"reason"})

	// Notifications: an alert nobody received is worse than no alert.
	NotificationDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxui_notification_deliveries_total",
		Help: "Notification deliveries attempted, by channel kind and outcome.",
	}, []string{"kind", "status"})

	// Security: a burst here is the shape of a credential-stuffing attempt.
	LoginFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "proxui_login_failures_total",
		Help: "Failed sign-in attempts.",
	})

	LoginSuccesses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "proxui_logins_total",
		Help: "Successful sign-ins.",
	})

	// Alerting: how much the portal is currently unhappy about.
	AlertsFiring = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "proxui_alerts_firing",
		Help: "Alert rule and VM pairs currently firing.",
	})
)

// ObserveHTTP records one request. Status is bucketed by class so a rare 503
// does not create a series that never repeats.
func ObserveHTTP(route, method string, status int, elapsed time.Duration) {
	HTTPRequests.WithLabelValues(route, method, statusClass(status)).Inc()
	HTTPDuration.WithLabelValues(route, method).Observe(elapsed.Seconds())
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// ObserveSync records the outcome of one synchronization run.
func ObserveSync(platform, kind, status string, elapsed time.Duration, at time.Time) {
	SyncRuns.WithLabelValues(platform, kind, status).Inc()
	SyncDuration.WithLabelValues(platform, kind).Observe(elapsed.Seconds())
	if status == "success" {
		SyncLastSuccess.WithLabelValues(platform, kind).Set(float64(at.Unix()))
	}
}

// SetPlatformUp records whether a platform answered.
func SetPlatformUp(platform string, up bool) {
	value := 0.0
	if up {
		value = 1
	}
	PlatformUp.WithLabelValues(platform).Set(value)
}

// ObserveAssets records how much of each type a platform reported.
func ObserveAssets(platform string, counts map[string]int) {
	for assetType, count := range counts {
		Assets.WithLabelValues(platform, assetType).Set(float64(count))
	}
}

// StatusLabel renders an HTTP status for a label without allocating per call
// sites that already have the integer.
func StatusLabel(status int) string { return strconv.Itoa(status) }
