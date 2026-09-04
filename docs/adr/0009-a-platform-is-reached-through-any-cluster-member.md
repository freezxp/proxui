# ADR 0009 — A platform is reached through any of its cluster members

**Status:** accepted · **Date:** 2026-09-04 · **Amends:** the single "API base URL" of PLAT-01 in [docs/03-frs.md](../03-frs.md), the TLS and Health rows of [docs/09-connector-architecture.md §9.4](../09-connector-architecture.md), and the `platforms` table in [docs/07-database-design.md](../07-database-design.md)

## Context

On 4 September 2026 the node `pve` went off the network between 12:04:00 and 12:05:12 UTC. It came back twenty minutes later, on the operator's own power cycle. Three other nodes — `pve2`, `pve3`, `cx1` — were up and quorate the entire time.

For those twenty minutes the portal saw nothing at all. Not a degraded view of the cluster: **zero metric samples for every host and every VM**, and five consecutive failed inventory syncs (runs `24461`–`24465`), each recording the same four scope errors against the same address:

```
connector: platform unreachable (cluster_resources):
  Get "https://10.0.30.111:8006/api2/json/cluster/resources":
  dial tcp 10.0.30.111:8006: connect: no route to host
```

`10.0.30.111` is `pve`. It is also the whole of `platforms.endpoint_url`, which is the whole of what the connector knows about how to reach the cluster. `newClient` parses that one string into `base *url.URL` at construction, and every call the connector will ever make is `c.base.JoinPath(...)`. One node was off; the portal was blind to four.

The consoles were down too, and for the same reason. `console.go` mints a ticket and dials `c.client.base` with a `/nodes/{node}/...` path, relying on Proxmox to proxy through to the node that owns the guest. That indirection is what lets one endpoint serve the cluster — and what makes one endpoint enough to lose it.

Two things did work, and neither should be disturbed:

- **Reconciliation refused to run on a failed listing.** Every failed run logged `inventory listing failed; skipping reconciliation to avoid mass deletion`. Nothing was marked missing, nothing was soft-deleted, and the 12:24 recovery came back to 4 hosts and 35 VMs with zero changes. SYNC-03 would otherwise have counted three consecutive absences and begun deleting the fleet.
- **The breaker did its job as written.** Three failures opened it (`BreakerFailureThreshold`), and the 5-minute `BreakerCooldown` is why the retries thin out from 12:05 to 12:07 to 12:13 to 12:18 rather than hammering a machine that was switched off.

What makes this worth an ADR rather than a shrug is that **the restriction is entirely ours**. Proxmox serves the same clustered API from every member: any node answers `/cluster/resources` for the whole cluster and proxies `/nodes/{n}/...` to the node that owns the resource. The API token is cluster-wide. There was, at every moment of the outage, a correct answer available three different ways.

Worse, the portal already knew where to ask. `NodeAddresses` calls `/cluster/status`, which returns every member's name, IP and `online` flag, and the portal stores those addresses in `host_ssh.address` for the sensor collector. The information needed to survive this outage was fetched sixty seconds before it began, used for something else, and thrown away.

The obstacle is TLS, and it is a real one. `tlsConfig` sets `ServerName` from the configured hostname, and in `fingerprint` mode it pins exactly one leaf certificate — the mode this cluster uses. Each member of a Proxmox cluster presents its own certificate, signed by the cluster CA. Swapping the host in the URL and keeping the pin would fail with `certificate fingerprint mismatch`, and it would be *right* to fail. Failover cannot be a string substitution.

## Decision

**A platform is configured with one endpoint and reached through a list of them.**

**The list is seeded by the operator and kept current by discovery.** A new `platform_endpoints` table holds `(platform_id, address, fingerprint, source, online, last_ok_at)`, where `source` is `configured` or `discovered`. The address an operator typed is the first row and stays first in preference order; the rest are learned from `/cluster/status` on every successful sync, which is a call the connector already makes. `platforms.endpoint_url` is unchanged — it remains the address an operator entered, the one the UI shows, and the seed the table is built from. The migration is additive in both directions.

**The client fails over on `ErrUnreachable` and on nothing else.** A call that fails with that class is retried against the next candidate, in order, once each. Auth, permission, refused, throttled and invalid-config failures are returned immediately, unchanged. The first candidate that answers becomes the preferred one for subsequent calls and stays preferred until it too fails — there is no drift back to the configured address until it is the one that answers.

**Only members the cluster last reported as `online` are candidates**, and a candidate that fails is demoted rather than forgotten. A node that is deliberately down does not cost a timeout on every cycle, and it returns to the rotation when the cluster says it is back.

**Each candidate carries its own trust.** Under `verify` and `custom_ca` nothing changes: system roots or a cluster CA already cover every member. Under `insecure` nothing changes either. Under `fingerprint`, each discovered member gets its own pin, recorded during a successful sync — that is, over the connection to an endpoint that is already pinned and already authenticated. A pin is never learned from the address being failed over to.

**A failure is recorded only when every candidate is unreachable.** `recordFailure`, the breaker, and the `sync_failure` event keep their current semantics and gain their intended meaning: the comment above `recordFailure` already claims that opening the breaker means "the portal has stopped trying, and data is going stale from here on", which today it can also mean that one machine was switched off.

## Rationale

The fix is proportionate to the failure, and most of it is already paid for. No new data source, no new credential, no new token privilege, no new call in the hot path — `/cluster/status` is already fetched, and the addresses are already stored. What is missing is that nothing keeps them for this purpose.

**Failing over only on `ErrUnreachable`** is the same argument `Retryable` already makes about auth: a 401 from `pve` will be a 401 from `pve2`, because the token is cluster-wide. Trying all four turns one clear, actionable failure into four identical ones and delays the critical-severity alert `recordFailure` raises for exactly that case. `ErrUnreachable` is the only class where a different address can plausibly change the answer, so it is the only class that gets one.

**Sequential with a sticky preference, rather than racing every member,** keeps the client-side rate limiter honest. Querying four members in parallel every cycle to save a few seconds during an outage would quadruple the steady-state load on an API the connector deliberately treats as "not a CDN", and the limiter's 10 req/s budget is per platform, not per node. Sequential failover costs extra requests only while something is actually broken.

**Per-member pinning, learned over an already-trusted channel,** is the part worth being strict about. The tempting shortcut — drop to `insecure` when failing over, just this once — inverts the security posture at precisely the wrong moment. Failover happens when the network is already misbehaving, which is when a wrong answer is both most likely and least likely to be noticed. Learning pins while the cluster is healthy, from a cluster describing itself over a connection whose certificate we have already verified, means the pins are known-good before they are ever needed. Trust-on-first-use at the failover address would be trust-on-first-use under exactly the conditions TOFU is weakest.

**Keeping `endpoint_url`** rather than migrating to a list column keeps one address that is unambiguously the operator's. It is what "Test connection" tests, what the UI edits, and what an operator recognises. The other addresses are cluster facts, and cluster facts belong in a table the sync engine owns and can rewrite.

## Consequences

- **A single node going down stops being a portal outage.** Replaying 4 September: the portal would have lost `pve`'s own metrics, correctly, because it was off — and kept inventory, metrics, consoles and history for the other three nodes and every VM on them.
- **The portal will now confidently display a down node as `online`, for longer.** This ADR does not fix that, and makes it more visible: throughout the outage `hosts.status` read `online` for all four nodes and `asset_state_history` recorded nothing, because status is only ever written from a successful listing. With failover in place the listing succeeds, and a node that is off is simply absent from a response nobody compares against the previous one. **Surfacing an unreachable or absent host as stale is a separate change and remains open.** It is named here so that failover is not mistaken for having solved it.
- **A sync that fails over is slower**, by up to the 30-second client timeout per dead candidate. It cannot overrun the cycle that asked, though: every attempt is bounded by the caller's context as well as by the client timeout, so the deadline cuts a hanging member off. The loop stops starting new attempts once the remaining budget is too small for one to complete.

  The first implementation instead demanded a *full client timeout* of headroom before trying another address, which reads as prudence and was in fact a silent disabling of failover exactly where it was needed. The health probe runs under a 30-second task deadline (`jobs.NewSyncHealthTask`) and the client timeout is also 30 seconds, so there was never room for a second address. With the configured endpoint blackholed against a live cluster, every inventory sync failed over and succeeded while every health probe reported the platform unreachable — the platform flapped between `healthy` and `unreachable` once a minute. No unit test caught it, because the shapes that expose it are a deadline equal to the timeout and a first attempt that fails in milliseconds.
- **`sync_errors` becomes less noisy and more precise.** Today an unreachable endpoint writes one error per scope — four rows per failed run, all the same cause. Failover means those rows are written only when every member has been tried, and the detail can name which addresses were attempted.
- **New table, new migration, additive.** Rolling back is ignoring `platform_endpoints`; `endpoint_url` alone still works exactly as it does now.
- **Testing splits along the same seam as the change.** The mock connector reports a configurable member list, so the sync engine's discovery, staleness and storage run in CI with no Proxmox anywhere, keeping §9.5's promise. The failover itself is transport behaviour — a refused connection, a 401, a certificate that does not match a pin — and is tested against real `httptest` TLS servers in the connector package, which answers those questions better than any fake could.
- **The endpoint in use becomes operator-visible.** A platform answering through a failover endpoint is a reportable state, not a silent one; it belongs next to sync status in PLAT-05.

## Alternatives considered

- **A VIP or round-robin DNS in front of the cluster.** Genuinely the better answer where an operator already runs one, and nothing here prevents it — a name that resolves to several members works today. Rejected as *the* answer because it requires infrastructure the portal cannot assume, and the small-cluster deployments this product targets (1–3 clusters, per the design context) are exactly the ones with no load balancer to put in front. Note this is unrelated to NFR-A2, which is about keeping *the portal* available; this is about the portal's view of a platform that is already available.
- **Let the operator type several endpoints and stop there.** Simple, honest, and it decays: the list is correct on the day it is written and goes stale as nodes are added, renamed or replaced, in a way nobody notices until the outage it was meant to cover. Discovery keeps it correct at no cost. Configured entries remain supported as the seed and as an override.
- **Retry harder against the configured endpoint.** This is what the breaker already does, and it did it correctly. No retry policy reaches a machine that is powered off.
- **Query all members in parallel and take the first answer.** Faster failover, four times the load on a rate-limited API in the 99.9% of cycles where nothing is wrong. Rejected above.
- **Relax TLS verification when failing over.** Rejected above, and worth stating plainly: an outage is not a reason to trust more, it is a reason to trust exactly as much as before.
