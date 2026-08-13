# 01 — Executive Summary

## The problem

Administrators and operations staff currently need direct access to each Proxmox cluster's native UI to see VM status, open consoles, and check performance. That means distributing platform credentials broadly, no unified view across clusters/datacenters, no per-team scoping of which VMs a user may see, and no central audit trail of who accessed what.

## The product

**ProxUI** is a self-hosted web portal that provides:

1. **A single dashboard** listing VMs across all connected Proxmox clusters, filtered by the user's group permissions.
2. **In-browser VM console access** (noVNC / serial terminal), proxied through the portal backend so users never need network reach or credentials to Proxmox nodes — and every console session is audited.
3. **VM performance and status** — live gauges plus up to a year of CPU/memory/disk/network history.
4. **RBAC** with four roles (Admin, Operator, Read-Only, Auditor) and VM-group-based scoping.
5. **Audit logging** of logins, configuration changes, console sessions, sync events, and API errors.
6. **Notifications** via Email, Slack, and generic webhooks for sync/connector failures, VM state changes, performance thresholds, and security events.

## Key architectural choices

| Area | Choice | Why (short) |
|---|---|---|
| Backend | Go, single binary, role-switchable (api / worker / scheduler) | Stakeholder choice; small static images, excellent concurrency for sync fan-out, one artifact to operate |
| Frontend | React + TypeScript SPA, served by the Go binary | Largest ecosystem for the hard parts (noVNC, xterm.js, charts); zero extra web server to run |
| Database | PostgreSQL 16 + TimescaleDB extension | One database for relational + 1-year time-series metrics (hypertables, continuous aggregates, compression) — no separate metrics stack |
| Cache & queue | Redis 7 + Asynq | One small container provides caching, background job queue, scheduling, and pub/sub for live UI updates |
| Auth | Built-in: argon2id, JWT access/refresh, optional TOTP | Stakeholder choice; no external IdP dependency for an internal tool |
| Console | Backend WebSocket proxy to Proxmox VNC | Users need zero network reach to hypervisors; full session audit |
| Extensibility | In-process connector framework (Go interfaces + registry) | New platform = new Go package implementing `Connector` interfaces; core untouched |
| Deployment | Docker Compose on 1–2 VMs | Matches team size and scale; Kubernetes design documented for later |

## Deliberately out of scope (v1)

Multi-tenancy/SaaS provisions (explicitly dropped per stakeholder), VM lifecycle management (create/clone/migrate), billing, non-Proxmox connectors (framework is built; VMware/cloud connectors are future work).

## Delivery approach

Solo developer, backend-first, 20 one-week-nominal sprints (timeline flexible). A usable backend (API + sync + console proxy) exists by sprint 10; the full portal including notifications and hardening by sprint 20. See [20-roadmap-sprints.md](20-roadmap-sprints.md).

## Success criteria

- A new user can be onboarded, granted a VM group, and open a VM console in under 5 minutes of admin effort.
- Inventory changes on Proxmox appear in the portal within 2 minutes.
- Dashboard loads in < 1 s for 500 VMs; console latency indistinguishable from native Proxmox UI on LAN.
- Every console session, login, and config change is queryable in the audit log.
- Adding a hypothetical second platform requires zero changes to core application packages (proved by a `mock` connector used in tests).
