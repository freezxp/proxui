# 16 — Logging & Monitoring Strategy

## 16.1 Logging

- **Format:** structured JSON to stdout (zerolog), one event per line — Docker captures it; ship anywhere later without app changes.
- **Standard fields:** `ts`, `level`, `msg`, `request_id`, `user_id`, `component` (http/ws/sync/notify/connector), `platform_id`, `duration_ms`, `err` (wrapped chain).
- **Levels:** `debug` (off in prod, per-component toggle via settings), `info` (state transitions: sync run summaries, session lifecycle, job outcomes), `warn` (retries, degraded, circuit-breaker events), `error` (failed operations needing attention). No `fatal` outside startup.
- **Redaction:** the logger sink filters exact values of loaded secrets + token-shaped patterns; connector HTTP logging logs method/path/status/duration, never bodies or auth headers.
- **Correlation:** `request_id` (middleware-generated, echoed in responses and RFC 7807 errors) links API log lines ↔ audit entries ↔ user bug reports. Jobs use `job_id` + `sync_run_id` the same way.
- **What is a log vs. an audit entry:** logs are for operators debugging the portal (unstructured retention ~14 d via Docker log rotation); audit is the product feature with 400 d retention and its own API. Security-relevant events always go to audit; they *may also* log.

## 16.2 Metrics (Prometheus exposition on internal `:9090/metrics`)

| Metric | Type | Labels | Alerts on |
|---|---|---|---|
| `proxui_http_requests_total` / `_duration_seconds` | counter/histogram | route, method, status | 5xx rate > 1%/5 min; p95 > 500 ms |
| `proxui_sync_runs_total` / `_duration_seconds` | counter/histogram | platform, kind, status | failures > 0 in 10 min |
| `proxui_platform_up` | gauge | platform | 0 for 5 min |
| `proxui_sync_last_success_timestamp` | gauge | platform, kind | age > 5 min |
| `proxui_assets` | gauge | platform, type, sync_state | sudden drop > 20% |
| `proxui_console_sessions_active` | gauge | — | capacity watch (>10) |
| `proxui_queue_depth` / `proxui_job_retries_total` | gauge/counter | queue, task | depth > 100; retry spike |
| `proxui_metrics_samples_written_total` | counter | platform | flatline = collection broken |
| `proxui_notification_deliveries_total` | counter | channel, status | failed streak |
| `proxui_login_failures_total` | counter | — | burst (security) |
| Go runtime + pgx pool + Redis stats | — | — | saturation |

**Monitoring stack:** intentionally external and optional — a small Prometheus + Grafana Compose overlay (`deploy/compose/monitoring.yml`) with two provisioned dashboards ("ProxUI service health", "Sync & platforms") and an Alertmanager route. The portal must not depend on its own monitoring stack to function. Sites with existing Prometheus just scrape the endpoint.

**Self-monitoring overlap:** the portal's own notifier already alerts on sync failures/platform down (product feature). Prometheus covers what the app can't self-report: the app being down, DB/Redis saturation, queue stalls. `/healthz` (liveness) and `/readyz` (DB+Redis+migrations) feed Compose healthchecks and any external uptime checker.

## 16.3 Tracing

Deferred. `request_id`/`job_id` correlation covers a single-process modular monolith; OpenTelemetry becomes worthwhile only if roles are split across nodes. The middleware seam (context-carried IDs) is where OTel spans would attach — noted as a future enhancement, not built now.

## 16.4 Dashboards & runbooks

Each Grafana dashboard panel links to a runbook section in `docs/runbooks/` (created during sprint 19): *platform unreachable*, *queue backlog*, *DB disk growth*, *restore from backup*, *master key rotation*, *force-closing console sessions*. Runbooks are part of the definition of done for operational features.
