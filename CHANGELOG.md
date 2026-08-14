# Changelog

## v1.0.0-rc.1 — 2026-08-14

First release candidate. Every sprint in the roadmap is implemented and
verified against a live Proxmox VE 9.2.10 cluster (19 VMs, 4 nodes, 10
storage pools, 13 networks).

### What it does

- **Inventory** synced from Proxmox: VMs, hosts, storage and networks, with
  change history per field and portal-owned tags and notes kept disjoint from
  platform-owned ones.
- **Browser consoles** over a WebSocket bridge. The portal answers the
  platform's RFB handshake itself, so the browser holds no platform secret
  (ADR 0002).
- **Performance history** in TimescaleDB, from one-minute samples to
  three-hourly rollups, over ranges from an hour to a year.
- **Access control**: four roles plus per-VM grants through user groups and
  VM groups.
- **Notifications** to SMTP, Slack and signed webhooks, routed by category,
  severity, platform and VM group.
- **Alerting** on CPU, memory, disk and network thresholds, with sustained
  duration, cooldown and recovery.
- **Administration** entirely in the browser: platforms with connector-declared
  forms and a gating connection test, users and grants, notification channels
  and routing, alert rules, settings, and an audit explorer with CSV export.

### Verified

| Target | Requirement | Measured |
|---|---|---|
| API latency | p95 ≤ 500 ms (NFR-P1) | 3.9–61.7 ms p95 across eight endpoints |
| Chart queries | p95 ≤ 800 ms any range (NFR-P2) | 4.2–6.6 ms p95, 1 h through 1 y |
| Initial bundle | ≤ 1 MB gzipped (NFR-P5) | 103 KB; noVNC and charts load on demand |
| Restore drill | < 30 min (docs/19) | 12 s, verified end to end |
| Vulnerabilities | none reachable | `govulncheck` clean, gated in CI |
| Upgrade path | v0.5.0 → current | schema 6 → 10, clean, new endpoints live |

### Known limitations

Named rather than omitted; see [docs/25-security-checklist.md](docs/25-security-checklist.md).

- No user acceptance testing. The console has been driven by hand; the other
  pages have been verified through their APIs and read, not used in anger.
- No external penetration test and no DAST in CI.
- Container images are built but not signed; there is no registry to publish
  to yet.
- No breach-corpus password check and no re-authentication before sensitive
  administrative changes.
- Kubernetes manifests are documented but untested.

---

## v0.8.0 — 2026-08-14

Product MVP: an administrator can onboard a platform and a user entirely in
the browser.

- Console UI (noVNC) with the portal-side RFB handshake (ADR 0002)
- VM detail with performance charts over stored rollups (ADR 0001)
- Platform administration with connector-declared schemas and a gating
  connection test
- Users, groups and grants, including the VM group membership that made
  grants confer access to anything at all
- Migration 00007: platform name uniqueness scoped to live rows

## v0.5.0 — 2026-08-13

Backend complete through sprint 10: identity, access control, the connector
framework, the Proxmox connector, the sync engine, the metrics pipeline, the
console proxy, power actions and live events.
