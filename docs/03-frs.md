# 03 — Functional Requirement Specification (FRS)

Requirement IDs are stable and referenced from tests and sprint tasks. Priority: **M**ust / **S**hould / **C**ould.

## 3.1 Authentication & session (AUTH)

| ID | Pri | Requirement |
|---|---|---|
| AUTH-01 | M | Users authenticate with username + password; passwords hashed with argon2id (see security doc for parameters). Google (OpenID Connect) was added later — see [ADR 0003](adr/0003-self-registration-and-google-sign-in.md). |
| AUTH-02 | M | Successful login issues a short-lived JWT access token (15 min) and a rotating refresh token (7 days, httpOnly secure cookie). |
| AUTH-03 | M | Refresh tokens are single-use; reuse of a rotated token revokes the whole session family and raises a security event. |
| AUTH-04 | S | Users may enroll TOTP (RFC 6238); when enrolled, login requires the 6-digit code. Admin can reset a user's TOTP. |
| AUTH-05 | M | 5 failed logins within 15 min locks the account for 15 min; lockout and failures are audited and raise security events. |
| AUTH-06 | M | Admin can deactivate a user; deactivation immediately revokes all sessions. |
| AUTH-07 | M | Logout revokes the refresh session; "logout everywhere" revokes all of the user's sessions. |
| AUTH-08 | S | Forced password change on first login and after admin reset; password policy: min 12 chars, checked against a breached-password list bundled offline. |

## 3.2 RBAC & scoping (RBAC)

| ID | Pri | Requirement |
|---|---|---|
| RBAC-01 | M | Exactly four roles exist: `admin`, `operator`, `readonly`, `auditor`. A user has exactly one role. |
| RBAC-02 | M | Permission matrix (enforced server-side on every endpoint): see table below. |
| RBAC-03 | M | Non-admin visibility is scoped by grants: user ∈ user_group, VM ∈ vm_group, grant links user_group→vm_group. Admins see everything. |
| RBAC-04 | M | A VM may belong to multiple VM groups; a user may belong to multiple user groups; effective access is the union. |
| RBAC-05 | M | VMs not in any granted group are invisible to that user in all APIs (list, detail, metrics, console) — filtered in queries, not in UI. |
| RBAC-06 | S | Auto-grouping rule: an admin may map a Proxmox pool or tag to a VM group so new VMs are grouped automatically on sync. |
| RBAC-07 | M | Every authorization denial returns 403 with a machine-readable code and is audit-logged. |

**Permission matrix (RBAC-02):**

| Capability | Admin | Operator | Read-Only | Auditor |
|---|---|---|---|---|
| View dashboard/inventory/performance (scoped) | ✅ (all) | ✅ | ✅ | ✅ (all, read-only) |
| Open VM console | ✅ | ✅ | ❌ | ❌ |
| VM power actions | ✅ | ✅ | ❌ | ❌ |
| Portal tags/notes on VMs | ✅ | ✅ | ❌ | ❌ |
| View audit logs | ✅ | ❌ | ❌ | ✅ |
| Manage platforms/credentials/sync | ✅ | ❌ | ❌ | ❌ |
| Manage users/groups/grants | ✅ | ❌ | ❌ | ❌ |
| Manage notifications/alert rules/settings | ✅ | ❌ | ❌ | ❌ |

## 3.3 Platform management (PLAT)

| ID | Pri | Requirement |
|---|---|---|
| PLAT-01 | M | Admin can register a platform: type (`proxmox`), name, API base URL, API token, TLS options (CA bundle or pinned fingerprint; `insecure` allowed but warned and audited). |
| PLAT-02 | M | "Test connection" validates reachability, token validity, permission sufficiency, and reports the platform version — before saving. |
| PLAT-03 | M | Credentials are envelope-encrypted at rest (AES-256-GCM, master key outside DB) and never returned by any API. |
| PLAT-04 | M | Platforms can be enabled/disabled; disabled platforms stop syncing but retain data (marked stale). |
| PLAT-05 | M | Admin can trigger an immediate full sync and see per-platform sync status, last success, and recent errors. |
| PLAT-06 | S | Per-platform sync interval configurable (default inventory 60 s, metrics 60 s, health 30 s). |

## 3.4 Synchronization (SYNC) — summary; full design in [10-sync-engine.md](10-sync-engine.md)

| ID | Pri | Requirement |
|---|---|---|
| SYNC-01 | M | Scheduler enqueues per-platform sync jobs on the configured cadence; a singleton lock prevents overlapping runs per platform+type. |
| SYNC-02 | M | Inventory sync upserts VMs, hosts, storage, networks; field-level change detection records what changed in `asset_state_history`. |
| SYNC-03 | M | Assets missing from a run are marked `missing`; missing for 3 consecutive runs → `deleted` (soft delete, retained 90 days). |
| SYNC-04 | M | Transient failures retry with exponential backoff + jitter (max 5); a platform failing repeatedly opens a circuit breaker (skip runs, probe every 5 min) and raises a `sync_failure` event. |
| SYNC-05 | M | Metrics sync writes per-VM and per-node samples to TimescaleDB; rollups and retention are automatic (continuous aggregates). |
| SYNC-06 | M | Detected changes publish domain events (Redis) consumed by the notifier and pushed to browsers over `/ws/events`. |

## 3.5 Inventory & dashboard (INV)

| ID | Pri | Requirement |
|---|---|---|
| INV-01 | M | VM list: paginated, sortable, filterable by name (substring), state, platform, node, VM group, tag; p95 < 500 ms at 500 VMs. |
| INV-02 | M | VM detail: identity (platform, node, VMID), state, resources (vCPU, RAM, disks, NICs), uptime, groups, portal tags/notes, recent state history. |
| INV-03 | M | Dashboard aggregates: total/running/stopped VMs, per-platform health, top-5 CPU and memory consumers, last 20 events — all permission-scoped. |
| INV-04 | S | Live updates: state changes push over WebSocket; list/detail update without reload. |
| INV-05 | C | Host, storage, network list & detail pages (v1.x). |

## 3.6 Console (CONS)

| ID | Pri | Requirement |
|---|---|---|
| CONS-01 | M | Console button on VM detail (and list row) for Operator/Admin creates a console session and opens noVNC in a new tab/panel. |
| CONS-02 | M | Backend obtains a Proxmox VNC ticket, then bridges browser WebSocket ↔ Proxmox `vncwebsocket`; browser never contacts Proxmox. |
| CONS-03 | M | Console session tokens are one-time, bound to user + VM, expire unused in 60 s. |
| CONS-04 | M | Sessions are recorded in audit: user, VM, start, end, duration, close reason. Admin can list and force-close active sessions. |
| CONS-05 | M | Idle console sessions close after 30 min (configurable); hard cap 8 h. |
| CONS-06 | C | Serial console via xterm.js (`termproxy`) for VMs with serial devices; LXC support. |

## 3.7 Performance (PERF)

| ID | Pri | Requirement |
|---|---|---|
| PERF-01 | M | Per-VM metrics: CPU %, memory used/total, disk read/write B/s, net rx/tx B/s, disk usage; per-node: CPU, memory, load, rootfs. |
| PERF-02 | M | Collection every 60 s; charts for 1h (raw), 24h (raw/1-min), 7d (5-min), 30d (30-min), 1y (3-h) via continuous aggregates. |
| PERF-03 | M | Retention: raw 48 h, 5-min 30 d, 30-min 6 mo, 3-h 400 d — enforced by Timescale retention policies. |
| PERF-04 | S | On platform registration, backfill up to 1 year of history from Proxmox RRD (`rrddata`) so charts are useful immediately. |

## 3.8 Audit (AUD)

| ID | Pri | Requirement |
|---|---|---|
| AUD-01 | M | Audited events: login success/failure/lockout/logout, token refresh anomalies, user/group/grant changes, platform & credential changes, settings changes, sync run summaries & errors, console sessions, power actions, notification config changes, API errors (5xx and upstream). |
| AUD-02 | M | Each entry: timestamp, actor (user or `system`), action, category, target type/id, source IP, user agent, outcome, structured details (JSONB), correlation/request ID. |
| AUD-03 | M | Audit log is append-only at the application layer (no update/delete endpoints; DB role for the app lacks UPDATE/DELETE on the table). |
| AUD-04 | M | Auditor/Admin can filter by time, actor, category, action, target; export CSV of the current filter (≤ 100k rows). |
| AUD-05 | S | Retention 400 days; monthly partitions dropped past retention. |

## 3.9 Notifications (NOTIF)

| ID | Pri | Requirement |
|---|---|---|
| NOTIF-01 | M | Channels: SMTP email, Slack (incoming webhook), generic webhook (JSON POST, HMAC-SHA256 signature header). Admin can create several of each and send a test message. |
| NOTIF-02 | M | Event categories: `sync_failure`, `vm_state_change`, `performance_alert`, `security`. Routing rules map category (+ optional severity / platform / VM group) → channels. |
| NOTIF-03 | M | Delivery is asynchronous (queue) with 3 retries; failures are visible in a delivery log and themselves audit-logged. |
| NOTIF-04 | M | Alert rules: metric (cpu/mem/disk), operator, threshold, sustained duration, scope (all or VM groups), severity. Evaluated every minute against rollups; fires once per breach with recovery notification; per-rule cooldown (default 30 min). |
| NOTIF-05 | S | Deduplication: identical event within cooldown window is suppressed with a counter. |

## 3.10 Settings & user management (ADM)

| ID | Pri | Requirement |
|---|---|---|
| ADM-01 | M | User CRUD (create with temp password, edit role/groups, deactivate). Self-service registration was added later and reverses the original "no self-registration" — see [ADR 0003](adr/0003-self-registration-and-google-sign-in.md); it ships disabled, and a self-registered account is read-only with no grants. |
| ADM-02 | M | System settings (sync defaults, session lifetimes, console idle timeout, retention overrides) editable in UI, stored in `settings`, change-audited, applied without restart. |
| ADM-03 | M | First-run bootstrap: initial admin created from environment variables/secret on first start only. |
