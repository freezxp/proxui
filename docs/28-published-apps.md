# 28. Published apps — Cloudflare Tunnel management

**Status: proposal.** Nothing here is built. This is the requirement set and
plan for a panel that publishes internal services through a Cloudflare Tunnel.

## 28.1 What this is, and why it is not obvious

Today the portal *reads* a Proxmox cluster and does two narrow writes: it opens
a console and it changes a VM's power state. Both are reversible, both affect
one machine, and both are bounded by what a Proxmox token may do.

Publishing an app is a different class of action. It takes something on a
private network and **puts it on the public internet**, and it does so by
editing shared infrastructure — a tunnel's routing table and a DNS zone —
where a mistake affects every other app on the same tunnel, including this
portal.

That difference drives most of what follows. The interesting requirements are
not "add a hostname"; they are about not taking yourself off the air.

### Why it earns its place

Cloudflare's own dashboard already does this. The portal is worth building
only for what the dashboard cannot know: **it already has the inventory.**
Publishing becomes "expose port 8080 of the VM called `kasm`" rather than
"type an IP address and hope it is still right". The portal can then keep the
two in step — a VM whose address changes, or which is deleted, currently
leaves a silently broken ingress rule behind and nothing notices.

If that link to inventory is not built, this feature is a worse copy of the
Cloudflare dashboard and should not exist.

## 28.2 How Cloudflare Tunnel actually works

Verified against Cloudflare's API documentation, because the design depends on
these specifics.

**Publishing one hostname is two writes to two different systems:**

1. **An ingress rule** on the tunnel —
   `PUT /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations` with a
   body of `{"config": {"ingress": [...]}}`. Each rule is a `hostname`, a
   `service` (`http://10.0.30.20:8080`), an optional `path`, and optional
   `originRequest` settings.
2. **A DNS record** in the zone — a CNAME from `app.example.com` to
   `<tunnel-id>.cfargotunnel.com`.

Miss either and you get a specific, confusing failure: DNS without ingress
gives Cloudflare error 1033; ingress without DNS means the name simply does
not resolve. **Both writes must succeed or neither must stick.**

**Three facts that shape the design:**

| Fact | Consequence |
|---|---|
| The PUT **replaces the entire ingress array**. There is no "add one rule". | Every change is read-modify-write over the whole routing table. A stale read silently deletes other people's rules. |
| The **last rule must be a catch-all** with no hostname, conventionally `service: http_status:404`. Rules match top-down, first match wins. | The portal must always re-emit the catch-all last, and must never let a rule sort after it, or every unmatched request breaks. |
| Only **remotely-managed** tunnels (`config_src: "cloudflare"`) can be configured by API. A locally-managed tunnel reads `config.yml` on the host and ignores the API entirely. | **If the existing tunnel is locally-managed, none of this works on it.** See §28.9. |

**API token scopes required:** `Cloudflare Tunnel: Edit` (or
`Cloudflare One Connectors Write`) at account level, and `DNS: Edit` on the
zone. Nothing more — notably not Zone:Edit, and not Access unless §28.7 is
built.

## 28.3 Functional requirements

Priority: **M**ust / **S**hould / **C**ould.

### Provider configuration (PUB-0x)

| ID | Pri | Requirement |
|---|---|---|
| PUB-01 | M | An administrator can register a Cloudflare account credential: API token, account ID. The token is stored with the same envelope encryption as a platform credential and is never returned by any read. |
| PUB-02 | M | A connection test verifies the token before saving, reporting reachability, which scopes are present, and which tunnels the account holds. Missing scopes are named individually with what they prevent. |
| PUB-03 | M | The portal lists the account's tunnels and their `config_src`. A locally-managed tunnel is shown but **cannot be selected**, with the reason stated — not hidden, because "my tunnel is missing" is a worse experience than "your tunnel cannot be managed this way, here is why". |
| PUB-04 | M | An administrator selects one tunnel and one or more DNS zones the portal is permitted to write to. Zones outside that list are refused even if the token could reach them. |
| PUB-05 | S | Multiple tunnels may be registered; each published app names exactly one. |

### Reading what exists (PUB-1x)

| ID | Pri | Requirement |
|---|---|---|
| PUB-10 | M | The portal reads the tunnel's current ingress rules and presents them, including rules it did not create. Cloudflare is the source of truth. |
| PUB-11 | M | Rules the portal did not create are shown **read-only and clearly marked as external**. The portal preserves them byte-for-byte on every write. |
| PUB-12 | M | Tunnel connection health (is `cloudflared` actually connected?) is surfaced, because a perfectly correct rule on a dead tunnel is indistinguishable from a wrong rule to the person reporting it. |
| PUB-13 | S | Ingress state is refreshed on a schedule and on demand, following the existing sync-run pattern with its circuit breaker. |
| PUB-14 | S | Drift is surfaced: a portal-managed app whose rule was changed in the Cloudflare dashboard is flagged rather than silently overwritten on the next write. |

### Publishing (PUB-2x)

| ID | Pri | Requirement |
|---|---|---|
| PUB-20 | M | An administrator can publish an app: hostname, target service, optional path. Both the ingress rule and the DNS record are created. |
| PUB-21 | M | The target may be **chosen from inventory** — a VM the portal knows, plus a port — and the portal resolves the address itself. A free-text address is also allowed for things outside the inventory. |
| PUB-22 | M | Publishing is **atomic in effect**: if the DNS write fails after the ingress write succeeded, the ingress change is rolled back, and vice versa. A half-published app is never left behind silently. |
| PUB-23 | M | Unpublishing removes both the ingress rule and the DNS record, and only the DNS record the portal created — an operator's pre-existing CNAME on the same name is never deleted. |
| PUB-24 | M | An app can be edited (target, path, origin settings) and disabled without being deleted. A disabled app keeps its row and loses its ingress rule. |
| PUB-25 | S | Rule order is editable, since first-match-wins makes order semantic when paths overlap. |
| PUB-26 | C | Origin settings per app: TLS verification off for self-signed origins, `httpHostHeader`, connect timeout. These are the settings people actually reach for; the rest can wait. |

### Not taking yourself off the air (PUB-3x)

These are the requirements this feature exists to get right.

| ID | Pri | Requirement |
|---|---|---|
| PUB-30 | M | Every write is **read-modify-write over the full ingress array**, and the read must be fresh — never from cache. |
| PUB-31 | M | A write is refused if the ingress array changed between the portal's read and its write. The administrator is shown the difference and re-submits deliberately. Silently winning a race here deletes someone's app. |
| PUB-32 | M | The **catch-all rule is always re-emitted last**. A submitted configuration that would leave a hostname rule after it is rejected before it is sent. |
| PUB-33 | M | The portal **identifies the rule that serves the portal itself** and refuses to delete, disable or reorder it behind a catch-all. Locking yourself out of the tool you would use to fix it is the failure mode to design against, not to document. |
| PUB-34 | M | Every mutation is preceded by a stored snapshot of the previous ingress array, and a one-click **revert to previous configuration** is available. This is the honest answer to "we cannot anticipate every mistake". |
| PUB-35 | M | Publishing, unpublishing, editing and reverting are audited with the before and after configuration. |
| PUB-36 | S | A dry-run diff is shown before any write: these rules added, these removed, these unchanged. |

### Access control (PUB-4x)

| ID | Pri | Requirement |
|---|---|---|
| PUB-40 | M | Managing published apps is **admin only**. An operator's grant over a VM does not permit exposing it to the internet — those are different powers and must not be conflated. |
| PUB-41 | M | Every new endpoint carries a permission-map entry; boot fails without one, per the existing rule. |
| PUB-42 | S | Non-admins may *view* published apps for VMs they are granted, since knowing a machine is internet-facing is useful and harmless. Admin-only for the first cut. |
| PUB-43 | M | Publishing an app with **no Cloudflare Access policy in front of it** requires an explicit acknowledgement that the service will be reachable by anyone on the internet. It is the single most consequential thing this panel can do. |

## 28.4 Non-functional requirements

| ID | Requirement |
|---|---|
| PUB-N1 | Cloudflare API calls are rate-limited and retried with backoff, reusing the connector's classification: a 5xx is a refusal, not an unreachable platform (see [docs/17](17-error-handling.md)). |
| PUB-N2 | The Cloudflare token is never logged, never echoed, and never returned by an API read. |
| PUB-N3 | A Cloudflare outage degrades to read-only with a clear banner; it must not make the rest of the portal unusable. |
| PUB-N4 | Reconciliation of a tunnel with 100 rules completes within the existing sync budget. |
| PUB-N5 | The published-apps UI adds no more than 30 KB gzipped to the initial bundle, or is lazily loaded (NFR-P5 keeps the initial bundle ≤ 1 MB). |

## 28.5 Data model

Two new tables. Migrations are forward-only; expand/contract for anything
breaking.

```
edge_providers                       -- one Cloudflare account + tunnel
  id, name, kind ('cloudflare_tunnel')
  account_id, tunnel_id, tunnel_name
  allowed_zone_ids     jsonb          -- PUB-04, the write boundary
  credential_encrypted bytea          -- envelope encryption, as platforms
  health, health_detail, last_seen_at
  consecutive_failures, breaker_open_until
  created_at, updated_at, deleted_at

published_apps
  id, provider_id -> edge_providers
  hostname             citext         -- unique per provider among live rows
  path                 text
  service_url          text           -- resolved target
  vm_id                uuid null      -- set when the target came from inventory
  vm_port              int  null
  origin_request       jsonb
  dns_record_id        text null      -- only ours; drives PUB-23
  is_enabled           boolean
  last_applied_at, last_error
  created_at, updated_at, deleted_at

edge_config_snapshots                 -- PUB-34
  id, provider_id, taken_at, ingress jsonb, taken_by
```

**Ownership, following the platform rule.** Cloudflare owns the live ingress
array. The portal owns `vm_id`, `vm_port` and the app's name and enablement.
The two sets are disjoint and are never merged — a rule the portal did not
create is mirrored, not adopted.

**Why `vm_id` is nullable and not a hard FK cascade.** Deleting a VM must not
silently delete an internet-facing route; it should surface the app as
orphaned and let a human decide.

## 28.6 API surface

All admin-only, all needing a permission-map entry.

| Method & path | Purpose |
|---|---|
| `GET /edge-providers` | list configured providers |
| `POST /edge-providers` | register one |
| `POST /edge-providers/test` | verify a candidate credential before saving |
| `PATCH /edge-providers/{id}` | edit; `DELETE` to remove |
| `GET /edge-providers/{id}/tunnels` | tunnels on the account, with `config_src` |
| `GET /edge-providers/{id}/ingress` | live ingress, portal-owned and external marked |
| `POST /edge-providers/{id}/sync` | refresh now |
| `GET /published-apps` | list |
| `POST /published-apps` | publish (ingress + DNS) |
| `PATCH /published-apps/{id}` | edit or enable/disable |
| `DELETE /published-apps/{id}` | unpublish |
| `POST /published-apps/preview` | dry-run diff, PUB-36 |
| `POST /edge-providers/{id}/revert` | restore the previous snapshot, PUB-34 |

## 28.7 Deliberately out of scope for a first cut

Named so they are decisions rather than oversights:

- **Cloudflare Access policies.** Creating and managing Access applications is
  a feature of similar size to this one. First cut: detect whether a hostname
  is covered by an Access application and *show* it, so PUB-43's warning is
  informed. Managing policies comes later, if at all.
- **Private network routes** (`cloudflared` as a VPN into a subnet) — a
  different Cloudflare concept from public hostnames.
- **Non-HTTP services** (SSH, RDP, TCP). The ingress schema supports them;
  the UI would need to be a different shape. Model allows it, UI does not.
- **Multiple Cloudflare accounts**, load balancers, WAF rules, page rules.
- **Any provider other than Cloudflare.** The interface should be shaped so
  Traefik or nginx could slot in, but building a second one now would be
  designing an abstraction against a single example.

## 28.8 Where the code goes

Clean Architecture, same import rule (`domain` ← `app` ← `infra`/`transport`),
CI-enforced.

```
internal/domain/publish/      hostname validation, ingress ordering, catch-all
                              invariants, self-protection (PUB-32, PUB-33)
internal/app/command/         publish, unpublish, edit, revert
internal/app/query/           list apps, read ingress, diff
internal/edge/                provider interface + errors  (sibling to
                              internal/connector, NOT part of it)
internal/edge/cloudflare/     the API client
internal/infra/postgres/      repositories + migrations
internal/transport/http/      routes, permission map, problem details
web/src/features/publishing/  the panel
```

**Why `internal/edge` rather than a new connector.** The connector contract is
about virtual machines — `VirtualMachineCollector`, `PowerManager`,
`ConsoleProvider`. A tunnel provider shares none of those capabilities.
Forcing it in would widen an interface that six VM-shaped implementations
depend on, to serve one that is not VM-shaped. A sibling package with the same
*shape* — typed error classes, capability interfaces, a fake for tests — gets
the reuse without the contortion. The `depguard` rules need a matching entry:
`edge/*` may import `internal/edge` only.

## 28.9 Risks

| Risk | Severity | Mitigation |
|---|---|---|
| **The existing tunnel is locally-managed.** Then the API cannot configure it at all, and the feature requires migrating to a remotely-managed tunnel — which means moving config out of `config.yml` and re-pointing every existing app. | **Blocking** | Establish this before any code is written. §28.10 Q1. |
| **The portal publishes itself through this tunnel.** A bad write takes the portal offline, and the portal is the tool you would use to fix it. | **High** | PUB-33 self-protection, PUB-34 snapshot and revert, and a documented `cloudflared`-side recovery that does not need the portal. |
| Concurrent edits in the Cloudflare dashboard are silently reverted by a stale read-modify-write. | High | PUB-31 conflict detection; refuse rather than merge. |
| Half-published app after a partial failure. | Medium | PUB-22 rollback; reconciliation detects and reports the inconsistent state. |
| Exposing an internal service unintentionally. | High | PUB-40 admin-only, PUB-43 explicit acknowledgement, full audit. |
| Scope: the portal becomes an edge-configuration tool as well as a VM portal. | Medium | Accepted deliberately, recorded in an ADR — this is a real widening of what ProxUI is. |
| Cloudflare API changes. | Low | Single client package, contract tests against a fake. |

## 28.10 Questions that change the plan

1. **Is the existing tunnel remotely-managed (`config_src: cloudflare`) or
   locally-managed (`config.yml`)?** If local, the whole feature is blocked
   until the tunnel is migrated, and that migration is the first sprint rather
   than an afterthought. Check with
   `GET /accounts/{account_id}/cfd_tunnel` and read `config_src`.
2. **Is this portal published through the same tunnel?** If so PUB-33 is a
   must and ships in the first cut, not later.
3. **Should publishing require a Cloudflare Access policy**, or is
   acknowledging the exposure enough? The former is materially more work and
   materially safer.
4. **Which zones may the portal write to?** The blast radius of `DNS: Edit` is
   the whole zone.

## 28.11 Sprint plan

Sequenced so the dangerous part comes last and behind a read-only phase that
proves the model is right.

| Sprint | Delivers | Done when |
|---|---|---|
| **P0 — spike** | Confirm `config_src`, token scopes, conflict semantics of the configurations PUT, and whether Cloudflare returns a usable version for optimistic concurrency. No production code. | The answers to §28.10 are facts, not assumptions. An ADR records the scope widening. |
| **P1 — provider + credential** | `edge_providers`, envelope-encrypted token, connection test naming missing scopes, tunnel listing with `config_src`. Read-only. | An admin can register an account and see their tunnels; a locally-managed tunnel is refused with a reason. |
| **P2 — read the world** | Ingress reflection, external-rule marking, tunnel health, sync runs with the existing breaker. Still no writes. | The panel shows the true current state of a real tunnel, including rules the portal did not create. |
| **P3 — safety rails first** | Snapshots, diff/preview, conflict detection, catch-all and self-protection invariants — all with unit tests, all before anything can write. | The invariants are enforceable and tested against a fake Cloudflare. |
| **P4 — publish** | Create, edit, disable, unpublish; ingress + DNS with rollback; full audit. | An app on a VM chosen from inventory is reachable at its hostname, and unpublishing removes both records. |
| **P5 — UI** | The panel: list, publish from inventory, preview diff, revert, exposure acknowledgement. | An admin does the whole flow in the browser without reading this document. |
| **P6 — drift and docs** | Drift detection, orphaned-app handling, runbook entry, recovery procedure that does not need the portal. | A rule changed in the Cloudflare dashboard is reported rather than clobbered. |

P0–P2 are useful on their own: a read-only view of what a tunnel is currently
serving, joined to the inventory, is worth having even if publishing is never
built.
