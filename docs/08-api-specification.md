# 08 — API Specification

Base path **`/api/v1`**. JSON everywhere. OpenAPI 3.1 is the source of truth, generated into server stubs and a typed TS client via `oapi-codegen` / `openapi-typescript` — this document is the human-readable contract.

## 8.1 Conventions

- **Auth:** `Authorization: Bearer <access JWT>` on every endpoint except `POST /auth/login`, `POST /auth/mfa`, `POST /auth/refresh`, `/healthz`, `/readyz`. Refresh token travels only in an httpOnly cookie scoped to `/api/v1/auth`.
- **Roles column** below = minimum role. `admin` implies all; scoped = result filtered by VM-group grants (Admin/Auditor see all).
- **Pagination:** `?page=1&per_page=50` (max 200) → response envelope `{data: [...], meta: {page, per_page, total}}`.
- **Filtering/sorting:** documented per endpoint; `sort=name` / `sort=-created_at`.
- **Errors (RFC 7807):**

```json
{ "type": "https://proxui.dev/errors/forbidden", "title": "Forbidden",
  "status": 403, "code": "rbac.console_denied",
  "detail": "Role 'readonly' cannot open consoles", "request_id": "req_9f2c…" }
```

- **Common status codes:** 200 OK, 201 Created, 202 Accepted (async), 204 No Content, 400 malformed, 401 unauthenticated, 403 forbidden, 404 not found (or out of scope — indistinguishable by design), 409 conflict, 422 validation, 429 rate limited (`Retry-After`), 500/502 upstream.

## 8.2 Authentication

| Method & URI | Roles | Description |
|---|---|---|
| POST `/auth/login` | — | Body `{username, password}` → `200 {access_token, expires_in}` + refresh cookie, or `200 {mfa_required: true, mfa_token, expires_in}` and **no cookie and no token** when a second factor is enrolled (AUTH-04). 401 invalid, 423 locked, 429 throttled |
| POST `/auth/mfa` | — | `{mfa_token, code}` → same success shape as login. `401 auth.invalid_code` (try again), `401 auth.mfa_challenge_expired` (the challenge is spent or timed out — start from the password). Five wrong codes end the challenge |
| POST `/auth/refresh` | cookie | Rotates refresh cookie → `{access_token, expires_in}`. 401 on invalid/reused (family revoked) |
| POST `/auth/logout` | any | Revokes current session family → 204 |
| POST `/auth/logout-all` | any | Revokes all own sessions → 204 |
| GET `/auth/me` | any | `{id, username, display_name, role, groups[], totp_enabled, must_change_password}` |
| PUT `/auth/me/password` | any | `{current_password, new_password}` → 204. 422 policy violation |
| POST `/auth/me/totp` | any | Begin enrollment → `{secret, otpauth_url, digits, period}` (QR rendered client-side). The factor is **not** live until confirmed; calling again replaces an unconfirmed enrollment. `409 auth.totp_already_enabled` over a working one |
| POST `/auth/me/totp/confirm` | any | `{code}` → 204; TOTP now required. The confirming code is spent, so it cannot double as the first sign-in code |
| DELETE `/auth/me/totp` | any | `{password}` → 204. The password is required: without it an unlocked screen would be enough to strip the factor |

**Example — login:**

```http
POST /api/v1/auth/login
{"username": "jsmith", "password": "…"}

200 OK
Set-Cookie: proxui_rt=…; HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth
{"access_token": "eyJhbGciOiJSUzI1NiIs…", "token_type": "Bearer", "expires_in": 900}
```

## 8.3 Dashboard & inventory

| Method & URI | Roles | Description |
|---|---|---|
| GET `/dashboard` | any (scoped) | `{vm_counts: {total, running, stopped, other}, platform_health: [...], top_cpu: [...], top_mem: [...], recent_events: [...], active_alerts: n}` |
| GET `/vms` | any (scoped) | Filters: `q` (name substring), `state`, `platform_id`, `host_id`, `group_id`, `tag`, `sync_state`. Sort: `name`, `state`, `cpu`, `-last_seen_at` |
| GET `/vms/{id}` | any (scoped) | Full detail incl. groups, platform/portal tags, notes, host, live snapshot |
| GET `/vms/{id}/metrics` | any (scoped) | `?range=1h\|24h\|7d\|30d\|1y` or `?from&to&step`. Server picks the right rollup. → `{series: {cpu_pct: [[ts,v]…], mem_used_bytes: …}}` |
| GET `/vms/{id}/history` | any (scoped) | Paginated `asset_state_history` for this VM |
| PUT `/vms/{id}/tags` | operator (scoped) | `{portal_tags: [...]}` → 200. Platform tags immutable here |
| PUT `/vms/{id}/notes` | operator (scoped) | `{notes}` → 200 |
| POST `/vms/{id}/power` | operator (scoped) | `{action: start\|stop\|shutdown\|reboot}` → 202 `{task_id}`. Audited. 409 if invalid for current state |
| GET `/hosts`, `/hosts/{id}`, `/hosts/{id}/metrics` | any (scoped¹) | Node inventory & metrics. ¹Hosts visible if the user can see ≥1 VM on them; admins all |
| GET `/hosts/{id}/sensors` | admin, read-only, auditor | Node hardware read from the node itself, not from the platform (ADR 0007). → `{at, readings: [{chip, label, kind, value, high, crit}…], summary, node}`. A node that has never answered returns 200 with no readings and the reason in `node.last_error` — most nodes start there |
| GET `/hosts/{id}/sensors/series` | admin, read-only, auditor | One sensor's history. `?chip=&label=` name it; `?range=` or `?from&to` as for VM metrics |
| GET `/hosts/{id}/sensors/history` | admin, read-only, auditor | Every temperature sensor's series for one node, aligned on a shared x-axis for one overlaid chart. `?range=` or `?from&to`. → `{data: [{chip, label, crit, points: [{t, v, max}…]}…]}` |
| DELETE `/hosts/{id}/host-key` | admin | Forget a node's pinned SSH host key so the next poll meets it afresh. Audited. 404 if nothing is pinned |
| GET `/storage`, `/storage/{id}` | any (scoped¹) | Storage pools with capacity |
| GET `/networks`, `/networks/{id}` | any (scoped¹) | Bridges/bonds/VLANs |

**Example — VM list item:**

```json
{ "id": "0198c1…", "name": "web-01", "platform": {"id": "…", "name": "pve-dc1", "datacenter": "dc1"},
  "host": {"id": "…", "name": "pve-node2"}, "external_id": "104", "vm_type": "qemu",
  "state": "running", "cpu_cores": 4, "memory_bytes": 8589934592, "uptime_s": 432000,
  "cpu_pct": 12.4, "mem_pct": 61.0, "groups": ["prod-web"], "portal_tags": ["tier:frontend"],
  "sync_state": "active", "last_seen_at": "2026-08-13T09:41:00Z" }
```

### 8.3a A user's own view ([INV-06…INV-09](03-frs.md))

Any authenticated role. These are private lists, not permissions: what a caller may organise is checked per VM inside the command, so the role gate has nothing to say about it.

| Method & URI | Roles | Description |
|---|---|---|
| PUT/DELETE `/vms/{id}/favourite` | any | Star or unstar, idempotent either way. Favourites sort above everything in `/vms`, server-side, because the list is paginated |
| PUT `/vms/{id}/folder` | any | `{folder_id}` or `null` to unfile. Re-filing moves rather than duplicating: a VM is in one folder |
| GET/POST `/folders` | any | The caller's own folders with VM counts; `{name}` → 201. A duplicate name is 409 — two identically named folders are indistinguishable in the picker they exist for |
| PATCH/DELETE `/folders/{id}` | any | Rename or reorder; deleting frees its VMs rather than removing them |
| PUT `/folders/{id}/vms` | any | `{vm_ids}` — file several at once, all or none |

`/vms` gains `folder_id=`, `folder=unfiled` and `favourite=1`, and `sort=folder` groups by folder with unfiled last. The Folders view is built entirely from these plus `GET /folders`: selecting a node sets one of the filters, so the contents pane is the ordinary list with an ordinary filter applied. A VM or folder that is not the caller's is 404 rather than 403: saying it exists is itself a disclosure (RBAC-05).

## 8.4 Console

| Method & URI | Roles | Description |
|---|---|---|
| POST `/vms/{id}/console` | operator (scoped) | `{kind: "vnc"}` → `201 {session_id, ws_url, expires_in: 60}`. 502 if platform unreachable |
| WS `/ws/console/{session_id}` | ticketed | One-time upgrade; binary RFB bridge to Proxmox. Close codes: 4000 idle, 4001 max duration, 4002 admin-forced, 4003 upstream lost |
| GET `/console-sessions?active=true` | admin | List sessions (active or historical, filterable by user/vm/time) |
| DELETE `/console-sessions/{id}` | admin | Force-close a live session → 204. Audited |

## 8.4a SSH terminal and files

Design: [29-ssh-terminal.md](29-ssh-terminal.md). Unlike the console, the
connection is opened and authenticated during the POST — the credential exists
only for that request — so a wrong password is a 401 on JSON rather than a
WebSocket that closes for unexplained reasons.

| Method & URI | Roles | Description |
|---|---|---|
| POST `/vms/{id}/ssh` | operator (scoped) | `{username, password?, private_key?, passphrase?, use_portal_key?, host?, port?, accept_host_key?}` → `201 {session_id, ws_url, expires_in: 60, address, ssh_user, host_key: {algorithm, fingerprint}, home, files_available, files_detail?}`. The credential is never echoed back |
| WS `/ws/ssh/{ticket}` | ticketed | One-time upgrade, one terminal per session. Binary frames are terminal bytes; text frames are control — only `{"type":"resize","cols":N,"rows":N}`. Close codes: 4000 idle, 4001 max duration, 4002 admin-forced, 4003 upstream lost, 4005 session gone |
| DELETE `/ssh-sessions/{id}` | operator (own), admin (any) | Close a session → 204 |
| GET `/ssh-sessions?active=true` | admin | List sessions, active or historical |
| DELETE `/vms/{id}/ssh-host-key` | admin | Forget the pinned host key so a rebuilt guest can be trusted again → 204. Audited |
| GET `/ssh-sessions/{id}/files?path=` | operator (own) | `200 {path, parent, data: [{name, path, size, mode, mode_bits, is_dir, is_link, target?, owner, group, mod_time}]}`. Empty `path` opens the account's home directory as the guest reports it |
| GET `/ssh-sessions/{id}/files/content?path=` | operator (own) | Streams the file as `application/octet-stream` with `Content-Disposition: attachment` — never rendered on the portal's origin |
| POST `/ssh-sessions/{id}/files/content?path=&name=` | operator (own) | Body is the file itself, streamed to the guest → `201 {path, bytes}`. `name` must be a plain entry name; max 2 GiB |
| POST `/ssh-sessions/{id}/files/mkdir` | operator (own) | `{path, name}` → `201 {path}` |
| POST `/ssh-sessions/{id}/files/rename` | operator (own) | `{path, to}` → 204. Both absolute, so a rename is also a move |
| POST `/ssh-sessions/{id}/files/chmod` | operator (own) | `{path, mode}` where mode is octal, e.g. `"644"` → 204 |
| DELETE `/ssh-sessions/{id}/files?path=` | operator (own) | Deletes a file or an empty directory → 204. Not recursive, by design |

`use_portal_key: true` authenticates with the portal's own key instead of
anything typed; the request carries a boolean, never key material.

**Problem codes:** `ssh.host_key_unknown` (409, body `{address, algorithm,
fingerprint}` — show it and re-POST with `accept_host_key`),
`ssh.host_key_mismatch` (409, body `{address, expected, got, first_seen_at}` —
refused; accepting the new fingerprint does not override it),
`ssh.auth_failed` (401), `ssh.unreachable` (502), `ssh.not_running` (409),
`ssh.no_address` (422), `ssh.credential_required` (400), `ssh.bad_key` (400),
`ssh.no_sftp` (422 — the terminal still works), `ssh.session_gone` (404),
`ssh.too_many_sessions` (429), `ssh.bad_path` (400), `ssh.no_such_file` (404),
`ssh.permission_denied` (403), `ssh.already_exists` (409),
`ssh.no_portal_key` (409 — nothing generated yet),
`ssh.bad_authorized_keys` (422 — the file on the guest is not readable as text,
or is implausibly large).

A session id belongs to one user: another signed-in operator presenting it gets
the same 404 as one that never existed.

### 8.4b The portal's SSH key

Design: [ADR 0006](adr/0006-portal-owned-ssh-key.md). One key pair, held by the
portal. The private half appears in no response; the public half is not a
secret and is meant to be copied.

| Method & URI | Roles | Description |
|---|---|---|
| GET `/ssh-key` | operator | `200 {exists: false}` or `200 {exists: true, public_key, algorithm, fingerprint, created_at}`. "Not generated yet" is a normal state, not a 404 |
| POST `/ssh-key` | admin | Generate, or rotate if one exists → `201` with the same shape. Audited as `ssh_portal_key_created` or `ssh_portal_key_rotated`, the latter carrying the previous fingerprint and the number of installs it invalidated |
| DELETE `/ssh-key` | admin | Forget the pair → 204. Lines already on guests are left behind; the count is audited |
| GET `/ssh-key/installs` | admin | `200 {data: [{vm_id, vm_name, ssh_user, fingerprint, installed_at, installed_by, stale}]}` — every account the key was installed into. `stale` means it carries a key the portal no longer holds |
| GET `/vms/{id}/ssh-key` | operator (scoped) | `200 {data: [...], key_exists, fingerprint?}` — the same rows for one VM, which is what lets the connect form offer key auth only where it will work |
| POST `/ssh-sessions/{id}/portal-key` | operator (own) | Install the public half into `authorized_keys` for the account this session is signed in as → `201` with the install row. Appends; installing twice changes nothing |
| DELETE `/ssh-sessions/{id}/portal-key` | operator (own) | Take the portal's line back out → 204. Only its own line; the record is forgotten either way |

Install and removal run over a session the caller already authenticated, so the
authorization is one they already had, and the write carries that guest
account's permissions rather than any privilege of the portal's.

## 8.5 Platform management

| Method & URI | Roles | Description |
|---|---|---|
| GET `/platforms` | any² | ²Non-admins get id/name/type/datacenter/health only (needed for filters); admins get full config |
| POST `/platforms` | admin | `{name, type, endpoint_url, datacenter, tls_mode, tls_ca_pem?, tls_fingerprint?, credential: {kind, token_id, secret}, sync_intervals?}` → 201. Secret write-only forever after |
| POST `/platforms/test` | admin | Same body → `200 {reachable, authorized, version, nodes, missing_permissions[]}` or 422 with the precise failure. Nothing persisted |
| GET `/platforms/{id}` | admin | Full detail + health + last sync summary. Credential never returned |
| PUT `/platforms/{id}` | admin | Update config; `credential` present ⇒ rotate secret |
| DELETE `/platforms/{id}` | admin | Soft delete (assets marked deleted). `?purge=true` hard-deletes after confirmation header `X-Confirm: {name}` |
| POST `/platforms/{id}/sync` | admin | `{kind: inventory\|metrics\|backfill}` → 202 `{sync_run_id}` |
| GET `/platforms/{id}/sync-runs` | admin | Paginated run history with stats |
| GET `/sync-runs/{id}` | admin | Run detail + errors |
| GET `/connectors` | admin | Registered connector types + capabilities → `[{type: "proxmox", version, capabilities: ["vm","host","storage","network","console","power","metrics_backfill"]}]` |

## 8.5a Provisioning ([ADR 0010](adr/0010-the-portal-can-create-and-destroy-guests.md))

Admin-only, every one of them. The platform token can now create and destroy guests, so the role gate is doing work the credential's own limits used to do for free.

| Method & URI | Roles | Description |
|---|---|---|
| GET `/platforms/{id}/templates` | admin | Cloud-init templates on the platform → `[{external_id, name, type, node, disk_bytes, has_cloud_init}]`. Read live: templates are excluded from the synced inventory, so there is nothing stored to read. Empty for a platform whose connector cannot provision |
| POST `/platforms/{id}/provision` | admin | `{template_id, name, node, storage?, full_clone?, vm_group_id?, ci_user?, ssh_keys[]?, ip_config?, nameserver?, search_domain?, cores?, memory_mb?, bridge?, vlan?, disk_name?, disk_grow_gb?, start_after_create?, start_on_boot?}` → 202 `{request_id, state}`. **No password field exists**: cloud-init takes a user and SSH keys, and a guest credential is never carried by the portal (PROV-04) |
| DELETE `/vms/{id}` | admin | `{confirm_name}` must equal the guest's name, matched server-side → 202 `{request_id, state}`. Templates are refused with 409 `template_protected` |
| GET `/provision-requests` | admin | Recent requests, newest first; `?platform_id=&limit=` |
| GET `/provision-requests/{id}` | admin | One request: `{state, step, vmid, error, …}`. Poll this — provisioning is four platform operations and the guest does not exist when the 202 returns |
| GET `/image-catalogue` | admin | The shipped cloud images → `[{id, name, url, checksum_url, checksum_algo, login_user}]`. No digests: a bundled one goes stale with the next point release, so the checksum *file* is linked instead |
| POST `/platforms/{id}/templates` | admin | `{name, node, image_url, image_storage, disk_storage, checksum?, checksum_algo?, skip_checksum?, cores?, memory_mb?, bridge?}` → 202 `{request_id, state}`. A checksum and algorithm are required unless `skip_checksum` is true, which is audited (PROV-11) |

States run `pending → cloning → configuring → resizing → starting → ready` for a creation, `pending → deleting → deleted` for a destruction, and `pending → downloading → creating → importing → converting → ready` for a template build; `failed` from any of them. A build whose image is already on the storage skips `downloading`. Resizing and starting are skipped when nothing was asked for. A failed request keeps its `vmid`: the partially created guest is left in place rather than cleaned up automatically (PROV-06).

A platform whose token lacks the provisioning privileges answers 409 `platform.not_capable`, which is a configuration an administrator chose rather than a fault. `POST /platforms/test` reports the same thing in advance as `provisioning_available` plus the names of anything missing.

## 8.6 Groups, grants, users

| Method & URI | Roles | Description |
|---|---|---|
| GET/POST `/vm-groups`, GET/PUT/DELETE `/vm-groups/{id}` | admin (GET: any, scoped to own) | CRUD; body incl. optional `auto_rule` |
| PUT `/vm-groups/{id}/members` | admin | `{add: [vm_id…], remove: [vm_id…]}` → 200 |
| GET/POST `/user-groups`, GET/PUT/DELETE `/user-groups/{id}` | admin | CRUD |
| PUT `/user-groups/{id}/members` | admin | `{add: [user_id…], remove: […]}` |
| GET/POST `/grants`, DELETE `/grants/{id}` | admin | Link user_group ↔ vm_group |
| GET `/users` | admin | Filter `q`, `role`, `active` |
| POST `/users` | admin | `{username, email, display_name, role, groups[], temp_password}` → 201; `must_change_password` set |
| GET/PUT `/users/{id}` | admin | Update role/groups/active/display; deactivate revokes sessions |
| POST `/users/{id}/reset-password` | admin | → `{temp_password}` (shown once); revokes sessions |
| DELETE `/users/{id}` | admin | Delete the account → 204. Sessions, memberships and TOTP cascade; audit entries keep the actor's name. 409 `user.self_delete` for your own account, 409 `user.last_admin` for the last active admin |
| DELETE `/users/{id}/totp` | admin | Reset MFA → 204. The lost-phone path; audited against both accounts |

## 8.7 Audit

| Method & URI | Roles | Description |
|---|---|---|
| GET `/audit-logs` | auditor | Filters: `from`, `to`, `actor`, `category`, `action`, `target_type`, `target_id`, `outcome`, `q` (detail search). Sort `-ts` fixed |
| GET `/audit-logs/export` | auditor | Same filters → `text/csv` stream (≤100k rows, else 422 asking to narrow) |
| GET `/audit-logs/categories` | auditor | Enumerations for filter UI |

## 8.8 Notifications & alerts

| Method & URI | Roles | Description |
|---|---|---|
| GET/POST `/notification-channels`, GET/PUT/DELETE `…/{id}` | admin | CRUD; secrets write-only |
| POST `/notification-channels/{id}/test` | admin | Sends test message → 200/502 with upstream detail |
| GET/POST `/notification-rules`, PUT/DELETE `…/{id}` | admin | Category→channel routing |
| GET `/notification-deliveries` | admin | Delivery log, filter by status/channel/time |
| GET/POST `/alert-rules`, GET/PUT/DELETE `…/{id}` | admin | Threshold rules (FRS NOTIF-04) |
| GET `/alerts?state=firing` | any (scoped) | Current alert states on visible VMs |

## 8.9 Settings & system

| Method & URI | Roles | Description |
|---|---|---|
| GET `/settings` | admin | All keys + defaults + descriptions |
| PUT `/settings` | admin | `{key: value, …}` partial update; validated; audited per key |
| GET `/system/info` | admin | Version, build, uptime, queue depth, DB/Redis health |
| POST `/events/ticket` | any but newuser | Mints a single-use, 30-second ticket for the stream below |
| WS `/ws/events/{ticketID}` | ticket (scoped) | Server-push: `vm.state_changed`, `sync.status`, `alert.fired/resolved` for visible VMs only |
| GET `/healthz` | — | Liveness (process up) |
| GET `/readyz` | — | Readiness (DB + Redis reachable, migrations current) |
| GET `/metrics` | network-restricted | Prometheus exposition (bound to internal listener) |

## 8.10 Rate limits

| Bucket | Limit |
|---|---|
| `POST /auth/login`, `/auth/mfa` | 5/min per IP + per username |
| `POST /vms/{id}/console` | 10/min per user |
| All authenticated APIs | 100/min per user (burst 20) |
| `GET /audit-logs/export` | 2 concurrent per user |

Exceeding → `429` + `Retry-After`; repeated abuse raises a `security` event.
