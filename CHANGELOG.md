# Changelog

## Unreleased

- **The refresh cookie's `Secure` flag now follows the request.** It was
  decided once at boot, so a portal served over HTTPS with
  `PROXUI_SECURE_COOKIES=false` — the setting that exists so a plain-HTTP LAN
  address can be signed into at all — sent its refresh cookie without the
  flag. The configured value is now a floor: a request that arrived over TLS
  gets `Secure` whatever the setting says, and one deployment can serve both
  addresses correctly. Clearing the cookie makes the same decision, or the
  browser keeps the cookie it was told to drop.
- **The console works on a phone.** A **Keyboard** button summons the
  on-screen keyboard and translates what it types into RFB key events, with a
  strip of the keys a phone keyboard has no room for — Esc, Tab, Enter,
  Backspace and the arrows — which a console is close to unusable without. The
  toolbar scrolls sideways instead of stacking, the clipboard panel takes the
  full screen below `sm`, and the page is sized in `dvh` so the browser's own
  bars no longer cut off the bottom of the console.
- **Copy and paste in the console.** A clipboard panel sends text to the
  guest's clipboard and shows what was copied inside the guest. It moves text
  through a textarea rather than syncing silently, because reading the local
  clipboard needs a permission that does not exist outside a secure context —
  which the plain-HTTP LAN deployment is not.

- **Self-registration and Google sign-in** (ADR 0003), both off until switched
  on. Google's client ID, secret and redirect URL are configured in Settings,
  the secret encrypted like a platform credential.
- **A `newuser` role** for accounts that provision themselves. It reaches
  `GET /auth/me` and the password change endpoint, and nothing else — where
  read-only, the previous default, could survey the estate's hosts, storage
  and networks without ever being granted a VM. Such an account lands on a
  page telling it to ask an administrator for access.
- **Live updates now actually work in a browser.** `GET /api/v1/events` was a
  WebSocket carrying no credential — a browser cannot put an `Authorization`
  header on one — so it had answered 401 in a reconnect loop since it was
  written. It is replaced by `POST /events/ticket` plus
  `GET /ws/events/{ticketID}`, the same single-use ticket the console uses.
- Branding: portal name, logo and login banner, with the name defaulting to
  the host the portal was reached at.
- Password change in the UI, including the forced change on first sign-in.
- Operators no longer see the platform column or administrative navigation.

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
