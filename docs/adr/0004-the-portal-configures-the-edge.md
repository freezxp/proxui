# ADR 0004 — The portal configures the edge

**Status:** accepted · **Date:** 2026-08-15 · **Widens:** the product scope in [docs/02-prd.md](../02-prd.md) and [docs/01-executive-summary.md](../01-executive-summary.md) · **Plan:** [docs/28-published-apps.md](../28-published-apps.md)

## Context

ProxUI was scoped as a **VM access portal**: see the estate, watch it, open a
console, and — added later — change a power state. Everything it does is
about virtual machines, and everything it writes is bounded by what a Proxmox
token may do to one guest.

The stakeholder asked for a panel that publishes internal services through
their Cloudflare Tunnel: pick a service, give it a hostname, have it reachable.

That is not a VM feature. It edits a tunnel's routing table and a DNS zone —
infrastructure shared by everything else behind that tunnel — and its effect
is to move something from a private network onto the public internet.

## Decision

ProxUI takes on a second kind of integration: **edge configuration**, starting
with Cloudflare Tunnel. It lives in `internal/edge`, a sibling to
`internal/connector`, and is admin-only.

The requirement set and sprint plan are [docs/28](../28-published-apps.md).
This ADR records only what is architecturally load-bearing.

## Rationale

### Why this is not a connector

The obvious move is to make Cloudflare "just another connector". It is wrong.

`connector.Connector` is a VM-shaped contract: `VirtualMachineCollector`,
`HostCollector`, `MetricsCollector`, `ConsoleProvider`, `PowerManager`. A
tunnel provider implements none of them. It collects no VMs, has no hosts, and
its capability set — list tunnels, read ingress, write ingress, write DNS —
intersects the connector's at zero points.

Forcing it in means one of two things. Either the `Connector` interface grows
methods that every existing implementation must stub out, which taxes the mock
connector and any future platform for a capability they will never have; or
the capability set becomes so loose that "connector" stops meaning anything,
and the type-switch-free design that lets the UI hide a console button for a
platform without consoles starts having to ask which kind of connector it is.

`internal/edge` copies the *shape* that works — typed error classes, small
capability interfaces, a registry, a fake for tests — without sharing the
contract. If a second edge provider ever appears (Traefik, nginx, Tailscale),
the abstraction has a second example to be shaped by. Until then it is one
package with one implementation, which is the honest state of it.

### Why the portal is worth it at all when Cloudflare's dashboard exists

Because the portal knows the inventory and the dashboard does not. Publishing
is "expose port 8080 of the VM named `kasm`", not "type an IP and hope". The
portal can then notice when that VM's address changes or it is deleted, which
today leaves a broken ingress rule that nothing reports.

If the inventory link is not built, this feature is a worse copy of the
Cloudflare dashboard. That is the test to hold it to.

### Why safety comes before capability in the plan

The portal is published through the tunnel it will be editing. The
configuration API replaces the entire ingress array on every write — there is
no "add one rule" — so a stale read silently deletes other people's routes,
and a wrong write takes the portal offline, with the portal being the tool one
would use to fix it.

So the sprint order puts snapshots, diffing, conflict detection, the catch-all
invariant, self-protection and an out-of-band recovery procedure **before**
anything can write. That is unusual sequencing and it is deliberate: the
read-only phases are independently useful, and if the write half is never
built nothing is wasted.

### Why admin-only

Publishing is a bigger power than any the portal has granted before. An
operator with a grant over a VM may console into it and power-cycle it; that
is a statement about one machine. Exposing it to the internet is a statement
about the network's boundary, and conflating the two would let a per-VM grant
imply a permission nobody intended to give.

## Consequences

- **ProxUI is no longer only a VM portal.** The name, the summary and the PRD
  describe something narrower than what it now does. That is a real widening,
  recorded here rather than absorbed silently.
- A new bounded context — domain, repositories, migrations, HTTP surface, UI —
  with the maintenance that implies.
- A **second class of secret** to protect: a Cloudflare API token with
  account-wide tunnel rights and zone-wide DNS rights. Its blast radius is
  larger than a Proxmox token's, which can only touch guests.
- The portal acquires a dependency on a third-party control plane it does not
  run. Cloudflare being down must degrade to read-only, not break the portal.
- **A permanent temptation to widen further**: Access policies, WAF rules,
  load balancers, private network routes. Each is out of scope in docs/28 §28.7
  by decision, not by omission, and each should need its own justification.

## Alternatives considered

- **Don't build it; use the Cloudflare dashboard.** Genuinely reasonable, and
  the right answer if the inventory link is dropped. Rejected because the
  inventory link is the thing that makes it more than a copy.
- **Make it a connector.** Rejected above.
- **A generic reverse-proxy abstraction from day one**, with Cloudflare as one
  driver. Rejected: designing an abstraction against a single example produces
  an abstraction shaped like that example, wearing a general name. The
  interface is kept small so a second implementation can reshape it honestly.
- **Shell out to `cloudflared` or Terraform.** Rejected: it moves the problem
  to managing state files and a binary's version, and gives worse errors than
  the API returns directly.
- **Let operators publish VMs they are granted.** Rejected under admin-only
  above.
