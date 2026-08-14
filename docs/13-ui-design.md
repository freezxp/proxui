# 13 — UI Design

## 13.1 Design system

- **Stack:** Tailwind CSS + shadcn/ui (Radix primitives) — accessible components, built-in dark mode via `class` strategy.
- **Theme:** light/dark/system toggle in the top bar, persisted per user. Semantic tokens (`bg-surface`, `text-muted`, state colors) so both themes stay consistent. State colors: running=green, stopped=gray, paused=amber, missing/stale=orange (striped badge), deleted=red, unknown=slate.
- **Responsive:** desktop-first (ops tool) but fully responsive. Breakpoints: ≥1280 full layout with left nav; 768–1279 collapsible nav, tables drop secondary columns; <768 nav becomes bottom sheet, tables become card lists. Console page always full-viewport.
- **Layout shell:** left sidebar (nav, filtered by role), top bar (global VM search ⌘K, platform-health indicator, theme toggle, user menu), content area. Persistent toast area for WS-pushed events.
- **Navigation by role:** Dashboard, Inventory (all) · Hosts, Storage, Networks (admin/readonly/auditor — an operator works on granted VMs, not the estate behind them) · Audit (admin/auditor) · Platforms, Users & Groups, Notifications, Settings (admin). Hidden ≠ protected: server enforces RBAC regardless.

## 13.2 Pages

### Dashboard (`/`)
- Stat row: Total / Running / Stopped / Other VMs (scoped), active alerts count.
- Platform health cards: name, DC, health dot, last sync age, VM count; click → platform detail (admin) or filtered inventory. Omitted entirely for operators, and the per-platform VM count follows the reader's scope.
- Top-5 CPU and Memory consumers (sparkline + current %) → VM detail.
- Recent events feed (last 20, scoped, live via WS): state changes, sync issues, fired alerts.
- Empty state (no grants): "No VMs are visible to your account — contact an administrator."

### Inventory (`/vms`)
- Toolbar: search (name substring, debounced), filters (state, platform, host, group, tag, sync_state), column picker, refresh indicator ("live · synced 12 s ago").
- Virtualized table: state badge, name, platform/DC, host, vCPU, RAM (used/total bar), disk, uptime, groups (chips), CPU% sparkline (last 15 min), row actions: Detail · Console (if permitted) · Power ▾.
- Row click → detail. Rows update in place via WS. URL-persisted filters (shareable links).

### VM Detail (`/vms/:id`) — tabs
1. **Overview:** identity card (name, VMID, platform, node, type, OS, IPs w/ copy, uptime, groups, platform tags read-only, portal tags/notes editable for operators), live gauges (CPU, mem, disk), recent state history (last 10 changes).
2. **Performance:** range picker (1h · 24h · 7d · 30d · 1y · custom); charts: CPU %, memory, disk I/O, network I/O, disk usage; hover crosshair sync across charts; min/avg/max legend; PNG/CSV export.
3. **Console:** big "Open Console" button (permission-aware with tooltip when denied) → new tab `/console/:vmId`; below: this VM's console session history (admin sees all users).
4. **History:** paginated field-change timeline from `asset_state_history` (who=sync/user, when, field, old→new).

### Console (`/console/:vmId`)
- Full-viewport noVNC canvas; slim toolbar: VM name + state badge, connection quality, Ctrl-Alt-Del, clipboard paste, fullscreen, disconnect.
- States: connecting spinner → connected; idle-timeout warning at 28 min with "stay connected"; clear error panels (VM stopped · platform unreachable · session expired) each with a retry path.
- Serial console (v1.x) renders xterm.js in the same shell.

### Platform Management (`/platforms`, admin)
- List: name, type, DC, endpoint, health, VM/host counts, last sync, enabled toggle.
- Add/Edit drawer: type select (from `/connectors`), schema-driven config fields, TLS mode with CA/fingerprint input, credential fields (write-only, "replace" affordance on edit), **Test connection** button showing the full report (reachable / authorized / version / missing permissions) before Save is enabled.
- Detail: health timeline, sync-run history table (status, duration, added/changed/deleted counts) with per-run error drill-down, "Sync now" / "Backfill history" buttons, danger zone (disable, delete w/ typed-name confirm).

### Hosts (`/hosts`), Storage (`/storage`), Networks (`/networks`)
- Hosts list: name, platform, status, CPU/mem bars, VM count, version → detail with node charts + VM list.
- Storage: name, type, shared?, capacity bar (warn ≥80%, crit ≥90%), platform/node.
- Networks: iface, type, VLAN, CIDR, node. (UI in v1.x; data synced from v1.)

### Audit Logs (`/audit`, admin+auditor)
- Filter bar: time range (presets + custom), actor, category, action, outcome, target, free-text.
- Table: time, actor, action (human-readable sentence), target link, IP, outcome badge; expandable row → full JSON details + request_id.
- Export CSV of current filter. Live tail toggle.

### User Management (`/admin/users`, `/admin/groups`, admin)
- Users: table (name, username, role chip, groups, MFA?, active?, last login), create dialog (temp password shown once with copy), edit (role, groups, active), reset password / reset MFA actions, per-user session list with revoke.
- Groups: two panes — user groups & VM groups; VM group editor: manual member picker (search VMs) + auto-rule builder (platform + pool/tag); grants matrix: user groups × VM groups with checkboxes.

### Notifications (`/admin/notifications`, admin)
- Channels tab: cards per channel (kind icon, target, enabled, last delivery status), add/edit forms per kind, "Send test".
- Routing tab: rules table (category, severity≥, scope, channel).
- Alert rules tab: rule builder (metric, op, threshold, sustained minutes, VM-group scope, severity, cooldown) + current firing list.
- Deliveries tab: log with status, attempts, error detail, redelivery button.

### Settings (`/admin/settings`, admin)
- Grouped forms: Sync defaults · Sessions & security (token lifetimes, lockout, console idle/max) · Retention (metrics, audit, history) · Branding (portal name, login banner). Each field shows default + "modified" indicator; saves audited per key.

### Auth pages
- Login (username/password → optional TOTP step), forced password change, "session expired" interstitial that preserves the return URL. No self-registration, no public password reset (admin-driven).

## 13.3 UX rules

- Every action button is permission-aware: hidden when the role can never do it, disabled-with-tooltip when temporarily unavailable (e.g., console on a stopped VM).
- Destructive actions require typed-name confirmation (platform delete) or explicit confirm dialog (power stop/reboot, force-close session).
- All timestamps render in the user's locale with UTC on hover; relative ("2 m ago") in tables.
- Stale data is always labeled: assets from an unreachable platform show a "stale — last seen X" badge rather than silently lying.
- Errors surface the `request_id` so users can quote it to admins (who can grep logs/audit by it).
