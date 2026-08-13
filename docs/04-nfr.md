# 04 — Non-Functional Requirements

Sized for the agreed target: 1–3 clusters, ≤ 30 nodes, ≤ 500 VMs, ≤ 50 users, ≤ 10 concurrent consoles. Numbers below are acceptance thresholds, not aspirations.

## 4.1 Performance

| ID | Requirement |
|---|---|
| NFR-P1 | API p95 ≤ 500 ms, p99 ≤ 1 s for list/detail/dashboard endpoints at target scale. |
| NFR-P2 | Metrics chart queries (any range up to 1 y) p95 ≤ 800 ms — achieved via continuous aggregates, never scanning raw samples for long ranges. |
| NFR-P3 | Console: added latency through the proxy ≤ 20 ms over direct connection on LAN; supports 10 concurrent sessions with < 5% CPU per session. |
| NFR-P4 | Full inventory sync of 500 VMs in ≤ 15 s (Proxmox `cluster/resources` is a single call per cluster); metrics cycle for 500 VMs in ≤ 45 s using ≤ 8 parallel node fetches. |
| NFR-P5 | Frontend: first meaningful paint ≤ 2 s on LAN; bundle ≤ 1 MB gzipped initial (noVNC/xterm/charts lazy-loaded). |

## 4.2 Scalability

| ID | Requirement |
|---|---|
| NFR-S1 | Horizontal scaling seam: the binary runs as `api`, `worker`, `scheduler`, or `all`. Single `all` instance is the default deployment; splitting requires only Compose changes (stateless API, queue-backed workers, Redis-lock singleton scheduler). |
| NFR-S2 | Design headroom 4× target (2,000 VMs) without architectural change — verified by a load-test fixture generating synthetic inventory. |
| NFR-S3 | Metrics volume at 500 VMs ≈ 0.75M samples/day; with Timescale compression, ≤ 15 GB/year total DB growth. |

## 4.3 Availability & DR

| ID | Requirement |
|---|---|
| NFR-A1 | Deployment model: single VM Compose stack. Target availability 99.5% (business-hours critical). Portal outage never affects workloads — Proxmox native UI remains the break-glass path. |
| NFR-A2 | Optional warm standby: second VM with the same Compose stack, Postgres streaming replica, manual DNS/VIP failover. RTO ≤ 30 min, RPO ≤ 5 min (WAL shipping). Documented, not default. |
| NFR-A3 | Backups: nightly `pg_dump` + WAL archiving to off-host storage (NFS/S3-compatible); config volume backup; **quarterly restore drill is a standing operational requirement**. RPO 24 h (dump-only) or ≤ 5 min (with WAL). |
| NFR-A4 | Graceful degradation: platform unreachable → portal serves last-synced data flagged `stale`, dashboards show connector health, console attempts fail with a clear message. |
| NFR-A5 | Crash safety: jobs are idempotent and at-least-once; a killed worker's job is retried; no partial sync corrupts inventory (per-asset transactions). |

## 4.4 Security (summary — full design in [15-security-design.md](15-security-design.md))

| ID | Requirement |
|---|---|
| NFR-SEC1 | TLS 1.2+ everywhere: browser→portal (Caddy/Traefik with internal CA or Let's Encrypt), portal→Proxmox (verified CA or pinned fingerprint). |
| NFR-SEC2 | Secrets: platform credentials envelope-encrypted (AES-256-GCM); master key via Docker secret/env file with documented rotation; no secrets in images or logs. |
| NFR-SEC3 | Rate limiting: login 5/min/IP + account lockout; API 100 req/min/user (Redis token bucket); console session creation 10/min/user. |
| NFR-SEC4 | Full audit trail (FRS 3.8); audit table append-only for the app DB role. |
| NFR-SEC5 | OWASP ASVS L2 as the review checklist; dependency and container scanning in CI (govulncheck, npm audit, Trivy). |

## 4.5 Multiple datacenters

Platforms in different DCs are registered as separate platform entries; the portal is centrally deployed and reaches each over routed/VPN links (TCP 8006 only). Per-platform circuit breakers isolate a DC link failure. A `datacenter` label on platforms drives UI grouping. No portal federation in v1.

## 4.6 Maintainability & operability

| ID | Requirement |
|---|---|
| NFR-M1 | One-command dev environment (`docker compose up` + seeded mock connector); one-command prod deploy. |
| NFR-M2 | Structured JSON logs, Prometheus `/metrics`, `/healthz` + `/readyz`; see [16-observability.md](16-observability.md). |
| NFR-M3 | Zero-downtime-tolerant migrations (goose, forward-only, expand/contract pattern for breaking changes). |
| NFR-M4 | All configuration via env vars + `settings` table; no config files baked into images; sane defaults for everything. |
| NFR-M5 | Upgrade path: `docker compose pull && up -d`; migrations run automatically on API start with an advisory lock. |

## 4.7 Caching strategy (summary)

| Layer | What | TTL / invalidation |
|---|---|---|
| Redis | Dashboard aggregates, permission-scoped VM-id sets per user | 30 s TTL + explicit bust on grant/group change |
| Redis | Rate-limit buckets, session revocation list, scheduler locks | Native expiry |
| In-process | Connector registry, settings snapshot | Settings bust via Redis pub/sub |
| HTTP | Static SPA assets immutable (hashed filenames), API `Cache-Control: no-store` | Build-time |
| Deliberately not cached | VM lists/details (DB is fast at this scale; correctness > cleverness) | — |

**Rationale:** at 500 VMs the relational queries are milliseconds; caching entity data would add invalidation bugs for no visible gain. Cache only aggregates, auth artifacts, and expensive permission set computations.
