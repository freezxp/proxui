# 03 — Functional Requirement Specification (FRS)

Requirement IDs are stable and referenced from tests and sprint tasks. Priority: **M**ust / **S**hould / **C**ould.

## 3.1 Authentication & session (AUTH)

| ID | Pri | Requirement |
|---|---|---|
| AUTH-01 | M | Users authenticate with username + password; passwords hashed with argon2id (see security doc for parameters). Google (OpenID Connect) was added later — see [ADR 0003](adr/0003-self-registration-and-google-sign-in.md). |
| AUTH-02 | M | Successful login issues a short-lived JWT access token (15 min) and a rotating refresh token (7 days, httpOnly secure cookie). |
| AUTH-03 | M | Refresh tokens are single-use; reuse of a rotated token revokes the whole session family and raises a security event. |
| AUTH-04 | S | Users may enroll TOTP (RFC 6238); when enrolled, login requires the 6-digit code. Enrolment is not live until a code confirms it. A code is accepted once — the step it matched is recorded, so it cannot be replayed inside its window. A challenge allows five attempts and expires in 5 minutes; the password step alone issues no session. Disabling requires the account password; admin can reset a user's TOTP, audited against both accounts. |
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
| PLAT-01 | M | Admin can register a platform: type (`proxmox`), name, API base URL, API token, TLS options (CA bundle or pinned fingerprint; `insecure` allowed but warned and audited). The configured URL is the address the portal prefers, not the only one it will use: further cluster members are discovered and failed over to when it is unreachable ([ADR 0009](adr/0009-a-platform-is-reached-through-any-cluster-member.md)). |
| PLAT-02 | M | "Test connection" validates reachability, token validity, permission sufficiency, and reports the platform version — before saving. |
| PLAT-03 | M | Credentials are envelope-encrypted at rest (AES-256-GCM, master key outside DB) and never returned by any API. |
| PLAT-04 | M | Platforms can be enabled/disabled; disabled platforms stop syncing but retain data (marked stale). |
| PLAT-05 | M | Admin can trigger an immediate full sync and see per-platform sync status, last success, recent errors, and the other addresses the platform is reachable at. |
| PLAT-06 | S | Per-platform sync interval configurable (default inventory 60 s, metrics 60 s, health 30 s). |

## 3.3a Provisioning (PROV) — see [ADR 0010](adr/0010-the-portal-can-create-and-destroy-guests.md)

| ID | Pri | Requirement |
|---|---|---|
| PROV-01 | M | Provisioning privileges are optional per platform. A token without them syncs unchanged and cannot provision; "Test connection" reports provisioning as available or names the privileges missing, rather than failing. |
| PROV-02 | M | Admin can list a platform's cloud-init templates, separately from the VM inventory, which continues to exclude templates unless `include_templates` is set. |
| PROV-03 | M | Admin can provision a guest from a template: name, target node, storage, cores, memory, disk size, network bridge, and IP configuration (static or DHCP). |
| PROV-04 | M | cloud-init receives a user name and SSH public keys only. No guest password is accepted, transmitted, or stored by any part of the portal ([ADR 0005](adr/0005-ssh-credentials-are-never-stored.md)). |
| PROV-05 | M | A provisioning request is a durable record advancing through clone → configure → resize → start. It survives a portal restart, and a request submitted twice clones once. |
| PROV-06 | M | A step that fails leaves the partially created guest in place, records which step failed, and does not attempt cleanup. |
| PROV-07 | M | Admin can destroy a guest. The request must carry the guest's name and the server must match it; templates are refused. |
| PROV-08 | M | Provisioning and destruction are admin-only, and both the intent and the outcome of each are written to the audit log under the `security` category. |
| PROV-09 | M | Admin can build a cloud-init template from a published cloud image without touching a node: the platform downloads the image, imports it as a disk, attaches a cloud-init drive, and converts the result. |
| PROV-10 | M | The portal ships a small catalogue of images with the URL, the distribution's checksum-file link and the default login user; any other URL can be entered by hand. No digests are bundled — a stale one is worse than none. |
| PROV-11 | M | A build requires a checksum and its algorithm. Building without verification is possible, must be stated explicitly, and writes a `template.build.unverified` audit entry naming the requester and the image. |
| PROV-12 | M | An image already present on the storage is not downloaded again. Template-building privileges are reported apart from provisioning ones, because cloning from a template needs strictly less than building one. |
| PROV-13 | M | A guest that becomes a template leaves the VM inventory as a conversion, recorded as such, rather than being reported `missing` until the mark-and-sweep deletes it. A platform whose inventory includes templates keeps the row. |

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

## 3.6a SSH terminal and file transfer (SSH)

Design: [29-ssh-terminal.md](29-ssh-terminal.md) · Credential decisions: [ADR 0005](adr/0005-ssh-credentials-are-never-stored.md) (guest credentials are never stored) and [ADR 0006](adr/0006-portal-owned-ssh-key.md) (the portal owns one key)

| ID | Pri | Requirement |
|---|---|---|
| SSH-01 | M | SSH button on VM detail (and list row) for Operator/Admin opens an xterm.js terminal in the portal, scoped per VM by the same grants as the console. |
| SSH-02 | M | The portal connects from the server to an address the platform reported for that VM; the browser never reaches the guest. A host that is not in that list is refused — the portal is not an SSH proxy to the rest of the network. |
| SSH-03 | M | Credentials (username + password, or private key and optional passphrase) are supplied per session and held only in the memory of the process serving it: never in Postgres, Redis, a log line, or any API response. |
| SSH-04 | M | Host keys are pinned per VM on first use, after the operator confirms the fingerprint. A changed key is refused outright; clearing a pin is an admin-only, audited action on a separate endpoint. |
| SSH-05 | M | The terminal WebSocket is authorized by a one-time ticket bound to user + VM + session, expiring unused in 60 s. One terminal per session. |
| SSH-06 | M | Sessions are recorded: user, VM, SSH username, address, start, end, duration, close reason, bytes. Admin can list and force-close. Every ending is recorded, including swept and shutdown ones. |
| SSH-07 | M | Idle sessions close after 30 min; hard cap 8 h — enforced server-side. A session with no terminal attached closes after 2 min of inactivity ([ADR 0008](adr/0008-detached-ssh-sessions-are-reclaimed-early.md)). |
| SSH-08 | M | Copy and paste both ways, including on a plain-HTTP origin where the asynchronous clipboard API does not exist. |
| SSH-09 | S | SFTP file browser over the same connection: list, download, upload (drag-and-drop, streamed, with progress), mkdir, rename, delete, chmod. A guest without SFTP still gets a terminal. |
| SSH-10 | M | Every file endpoint resolves the session for the calling user; another user's session id yields the same 404 as one that does not exist. Writes and transfers are audited with path and byte count; listing is not. |
| SSH-11 | S | The portal holds one Ed25519 key pair of its own. The private half is sealed with the same envelope encryption as a platform credential and is never returned by any endpoint; the public half is readable by an operator, because pasting it into cloud-init is a supported way to install it. Generating, rotating and deleting it are admin-only and audited. |
| SSH-12 | S | An operator can install the public half into `authorized_keys` for the account an open session is signed in as — over that session, with that account's permissions. The write appends and never truncates: keys already in the file survive it. `~/.ssh` is created at 0700 and `authorized_keys` set to 0600, because sshd ignores a file anyone else can write. Installing twice is a no-op. |
| SSH-13 | S | A connect can ask for the portal key instead of a typed credential. The request carries a boolean, never key material; the private half is read from the vault after the caller has been shown to be allowed on that VM. Every session open and every denial records which method was used. |
| SSH-14 | S | Removing the key from an account is available to the operator over the same session, takes out only the portal's own line, and forgets the install record either way. Rotation invalidates every install at once; those left behind are listed as stale rather than shown as working. |

### Node hardware sensors

Proxmox publishes no temperature anywhere in its API, so these are read from
the node itself over SSH with the portal's own key. See
[ADR 0007](adr/0007-the-portal-reads-node-sensors-over-ssh.md) for why that is
allowed to relax SSH-02, and what it is not allowed to become.

| ID | Pri | Requirement |
|---|---|---|
| SENSOR-01 | S | The portal reads each node's hardware sensors by running one fixed command, `sensors -j`, over SSH as the portal's own key. No node credential is ever stored, and a node connection cannot become a terminal, a file browser or a forwarded port. |
| SENSOR-02 | S | The node's address comes from the platform's own cluster membership, never from a request. A node the platform did not name is not reachable. |
| SENSOR-03 | S | A node's host key is pinned the first time it is met and a change is refused from then on. The fingerprint is shown on the host page; clearing a pin is admin-only and audited. |
| SENSOR-04 | S | Readings are stored per sensor — chip, label, and the high/critical thresholds the chip declares — rather than reduced to one number per node. |
| SENSOR-05 | S | Alert rules can watch a node's temperature, either in degrees or as the headroom left to the chip's own critical point. Nodes are not in VM groups, so a node rule covers the estate. |

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
| ADM-04 | S | Admin can delete a user outright, confirmed by typing the username. The account, its sessions, group memberships and second factor go; the audit trail keeps what the account did, under the name it had. Two deletions are refused: the caller's own account, and the last administrator who can sign in. Deactivation (ADM-01, AUTH-06) remains the answer for somebody who has merely left. |
