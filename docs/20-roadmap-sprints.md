# 20 — Development Roadmap & Sprint Plan

Solo developer, timeline flexible → sprints are **scope buckets in strict dependency order**, nominally one focused week each. Backend first (stakeholder direction): the API is fully exercisable via tests + generated client before serious UI work starts. Each sprint ends with something demonstrably working.

## 20.1 Phase map

| Phase | Sprints | Outcome |
|---|---|---|
| P1 Foundations | 1–3 | Skeleton, auth, RBAC core — deployable empty shell |
| P2 Platform & sync | 4–7 | Proxmox synced inventory + metrics in DB |
| P3 Backend feature-complete | 8–10 | Full API: audit, console proxy, power — **backend MVP** |
| P4 Frontend | 11–15 | Usable portal for all roles — **product MVP** |
| P5 Alerting & ops | 16–18 | Notifications, alert rules, admin polish |
| P6 Hardening & release | 19–20 | Security pass, DR drill, docs, v1.0 |

## 20.2 Sprint breakdown

| # | Theme | Key deliverables (FRS refs) | Exit demo |
|---|---|---|---|
| 1 | Repo & skeleton | Monorepo layout, Makefile, CI green on empty project, Compose dev env, goose+sqlc+oapi wiring, config loading, healthz/readyz, structured logging, RFC7807 middleware | `make dev` boots; CI badge green |
| 2 | AuthN | users table, argon2id, login/refresh-rotation/logout, JWT middleware, lockout (AUTH-01..07), bootstrap admin (ADM-03), auth audit events | login via curl; reuse-detection test green |
| 3 | RBAC & groups | roles, user/vm groups, grants, the visibility query + cache, permission map + boot validation, generated RBAC matrix test (RBAC-01..07), user CRUD API (ADM-01) | matrix test proves every route × role |
| 4 | Connector framework | `internal/connector` interfaces/registry/records, conformance suite, **mock connector** with fixtures & failure injection | conformance green on mock |
| 5 | Proxmox connector I | pve client (token auth, TLS modes, limiter), TestConnection w/ permission report, ListVMs/Hosts/Storage/Networks, Health (PLAT-01..02) | real cluster listed via test binary |
| 6 | Sync engine | scheduler+Asynq, reconciler (hash, field diff, history, mark-and-sweep), sync_runs/errors, outbox+relay, circuit breaker (SYNC-01..04, 06), platform CRUD API (PLAT-03..06) | register platform → inventory in DB; pull cable → breaker+event |
| 7 | Metrics pipeline | Timescale hypertables, aggregates, compression/retention, rate computation, gap healing, RRD backfill (PERF-01..04), `/vms/{id}/metrics` query | 1y chart data via API after backfill |
| 8 | Audit & inventory API | audit write path everywhere, partitions, search/export API (AUD-01..05), `/vms` list/detail/history, dashboard query, tags/notes (INV-01..03) | full read API demo via generated client |
| 9 | Console proxy | vncproxy ticket flow, one-time sid, WS bridge, idle/max timeouts, session audit, admin force-close (CONS-01..05) | noVNC HTML test page → real VM console |
| 10 | Backend MVP wrap | power actions (US-09), `/ws/events` scoped broadcaster (INV-04), rate limiting, `/system/info`, load test vs NFR-P — **tag v0.5 backend MVP** | k6 report meets NFR-P1/P4 |
| 11 | Frontend foundation | Vite+TS+Tailwind+shadcn, auth flow UI, layout shell, theme (dark mode), generated TS client, TanStack Query + WS wiring | login → empty shell in both themes |
| 12 | Dashboard & inventory UI | dashboard page, VM list (virtualized, filters, live updates), URL-persisted filters | 500-VM mock fleet browsable, live |
| 13 | VM detail & charts | detail tabs (overview/history), ECharts perf tab with range picker & rollups | 1y chart renders < 800 ms |
| 14 | Console UI | noVNC integration, toolbar, error/timeout states, session UX (13 §Console) | click-to-console < 3 s |
| 15 | Admin UI I | platform mgmt (form from config schema, test connection, sync history), users/groups/grants screens | admin onboards platform+user entirely in UI — **tag v0.8 product MVP** |
| 16 | Notifications | channels (SMTP/Slack/webhook+HMAC), routing rules, deliveries+retries, test-send (NOTIF-01..03) | Slack message on pulled cable |
| 17 | Alert rules | evaluator job, alert_states, cooldown/recovery, alert UI, firing list (NOTIF-04..05) | CPU-burn VM fires & resolves alert |
| 18 | Admin UI II & audit UI | audit explorer + CSV export, settings screen (ADM-02), notification admin pages, hosts/storage/networks pages (INV-05) | auditor persona full walkthrough |
| 19 | Hardening & ops | ASVS L2 checklist pass, console pentest, ZAP, backup/restore scripts + **restore drill**, monitoring overlay + dashboards + runbooks, image signing | restore drill < 30 min documented |
| 20 | Release | UAT with real users, perf regression run, docs freeze, upgrade-path test, v1.0.0 tag + prod deploy | v1.0 live |

## 20.3 Milestones & de-risking order

Riskiest things land earliest practical: connector contract (S4) before real connector (S5); console proxy (S9) before any console UI (S14); load characteristics proven at S10, not discovered at S20. If timeline pressure appears, the designed cut lines are: hosts/storage/networks UI (S18 partial), serial console (already v1.x), alert rules (S17) — never security or backup work.

## 20.4 Post-v1 cadence

Monthly patch releases (deps, Proxmox point-release compat); feature work per [22-future-enhancements.md](22-future-enhancements.md).
