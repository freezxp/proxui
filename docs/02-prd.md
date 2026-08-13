# 02 — Product Requirements Document (PRD)

## 1. Vision

One portal, all VMs: a permission-scoped window onto the organization's virtualization estate, with zero direct hypervisor access for end users.

## 2. Users & personas

| Persona | Portal role(s) | Needs |
|---|---|---|
| **Cloud Administrator** | Admin | Manage platforms, users, groups, credentials; full visibility; break-glass console access |
| **Infrastructure Engineer** | Operator | Console access and power actions on the VM groups they own; performance history for capacity/troubleshooting |
| **Operations Team (NOC/helpdesk)** | Operator or Read-Only | Watch dashboards, spot down/degraded VMs, open consoles for triage (Operator) or escalate (Read-Only) |
| **Auditor / Security** | Auditor | Read-only audit trail: who logged in, who opened which console, what changed |

## 3. User stories (prioritized)

### Must have (MVP)
- **US-01** As any user, I log in with username/password (optionally TOTP) and see only VMs my groups grant me.
- **US-02** As any user, I see a dashboard: VM count by state, platform health, top consumers, recent events.
- **US-03** As any user, I browse/search/filter the VM inventory (name, state, platform, node, group, tags) and open a VM detail page.
- **US-04** As an Operator/Admin, I click **Console** on a VM and get an interactive noVNC session in the browser within 3 seconds.
- **US-05** As any user, I view VM performance: live CPU/mem/disk/net plus historical charts (1h/24h/7d/30d/1y).
- **US-06** As an Admin, I register a Proxmox cluster (endpoint + API token), test the connection, and see its inventory appear automatically.
- **US-07** As an Admin, I create users, assign roles, create user groups and VM groups, and grant group→group access.
- **US-08** As an Auditor, I search the audit log by user, action, resource, and time range, and export it as CSV.

### Should have
- **US-09** As an Operator, I start/stop/shutdown/reboot a VM I have access to (each action audited).
- **US-10** As an Admin, I configure notification channels (Email/Slack/Webhook) and routing per event category.
- **US-11** As an Admin, I define performance alert rules (e.g., CPU > 90% for 10 min) scoped to VM groups.
- **US-12** As any user, the VM list and detail pages update live (state changes push to the browser without refresh).

### Could have (v1.x)
- **US-13** Serial/xterm.js console for VMs with serial terminals; LXC container consoles.
- **US-14** As an Operator, I add portal-side tags/notes to VMs (never synced back to the platform).
- **US-15** Host, storage, and network inventory pages (data is already synced; UI exposure).

## 4. Functional scope by module

| Module | In v1 | Notes |
|---|---|---|
| Dashboard | ✅ | Fleet summary, health, top-N, recent events |
| Inventory (VMs) | ✅ | List, search, filter, detail |
| Console | ✅ | noVNC via backend proxy; serial console v1.x |
| Performance | ✅ | Live + 1y history, per-VM charts |
| Power actions | ✅ (should) | start/stop/shutdown/reboot only |
| Hosts/Storage/Network | Synced ✅, UI v1.x | Collectors run from day one so history exists |
| RBAC | ✅ | 4 fixed roles + VM-group scoping |
| Audit | ✅ | Login, config change, console session, sync, API errors |
| Notifications | ✅ | Email, Slack, Webhook; 4 event categories |
| Platform mgmt | ✅ | Proxmox connector; framework for more |

## 5. Explicit non-goals (v1)

- No VM provisioning, cloning, snapshots, migration, or configuration editing.
- No multi-tenancy, billing, or public SaaS features (per stakeholder decision).
- No agent installed inside guest VMs; all data comes from platform APIs.
- No LDAP/SSO in v1 (built-in auth chosen); architecture keeps an OIDC seam for later.

## 6. Constraints & assumptions

- Portal server can reach every Proxmox cluster's API (TCP 8006) and node VNC websockets; users only need to reach the portal.
- Proxmox VE 8.x with API tokens available; token granted `PVEAuditor` minimum, plus `VM.Console` (and `VM.PowerMgmt` for power actions).
- Scale ceiling for design: 3 clusters, ~30 nodes, 500 VMs, 50 users, 10 concurrent console sessions.
- Browsers: current Chrome/Edge/Firefox; no IE/legacy support.

## 7. Success metrics

| Metric | Target |
|---|---|
| Time from Proxmox change → visible in portal | ≤ 2 min |
| Console open time (click → interactive) | ≤ 3 s LAN |
| Dashboard/API p95 latency | ≤ 500 ms |
| Sync failure detection → notification | ≤ 5 min |
| Portal-attributable Proxmox credential distribution | 1 API token per cluster (vs. N users × M clusters today) |
