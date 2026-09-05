# ADR 0010 — The portal can create and destroy guests

**Status:** accepted · **Date:** 2026-09-04 · **Amends:** §15.4 of [docs/15-security-design.md](../15-security-design.md), the non-goals in [docs/02-prd.md §5](../02-prd.md) and [docs/01-executive-summary.md](../01-executive-summary.md), risk P1 in [docs/21-risk-assessment.md](../21-risk-assessment.md), the explicit non-roadmap in [docs/22-future-enhancements.md](../22-future-enhancements.md) · **Adds:** PROV-01…PROV-13 to [docs/03-frs.md](../03-frs.md)

## Context

Until now the portal could not create a virtual machine, and that was not a policy — it was arithmetic. The platform token holds `PVEAuditor` + `VM.Console` + `VM.PowerMgmt`, and §15.4 states the consequence as a security property in as many words: **"The portal physically cannot create/delete VMs on Proxmox — capability ceiling enforced at the platform, not just in portal code."**

That sentence is worth more than any amount of checking inside the portal, and it is worth being precise about why. A portal-side guard is only as good as the portal: a routing mistake, a missing permission-map entry, a confused deputy in a handler, a stolen session belonging to an administrator. None of those could destroy a VM, because the credential the portal holds was not able to. The guarantee survived the portal's own bugs. Every design document in this package leans on it — `docs/21` counts it as the mitigation for scope creep (risk P1), and `docs/22` says removing it "needs an ADR revisiting the least-privilege token ceiling first."

This is that ADR, and the ceiling is coming down.

The reason is that the gap it leaves is now the sharpest edge in the product. Everything about a guest's life already lives in the portal — who may see it, which group it belongs to, its console, its performance history, the portal's own SSH key installed on it, the audit trail of everyone who touched it. Everything except the moment it comes into existence, for which an operator leaves for the Proxmox UI, clones a template by hand, and comes back. The portal knows what a fleet is; it just cannot add to one.

Cloud-init cloning is the specific shape being added, and it is not a small privilege ask. Proxmox needs `VM.Allocate` and `VM.Clone` to make the copy, `VM.Config.Disk`, `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options` and `VM.Config.Cloudinit` to configure the result, and `Datastore.AllocateSpace` to put a disk somewhere. Destroying additionally needs the token to be allowed to delete. There is no subset of these that permits creation but forbids destruction of what was created.

## Decision

**One token, widened.** The existing per-platform credential gains the privileges above. There is no separate provisioning credential; sync, metrics, consoles, power actions and provisioning all authenticate as the same token.

**The privileges are optional, not required.** `requiredPrivileges` is untouched, so every existing installation keeps working exactly as it did. A parallel `provisioningPrivileges` list is checked separately, and `TestConnection` reports the result as a capability — `ProvisioningAvailable`, plus the names of anything missing — rather than as a failure. A platform whose token was never widened syncs normally and simply cannot provision, which is both the safe default and the honest description of what is true.

**Admin only.** Provisioning and destruction are `RoleAdmin`, declared in the permission map like every other route.

**SSH keys only, structurally.** cloud-init receives `ciuser` and `sshkeys`; `CloudInitSpec` has no password field at all. ADR 0005 established that guest credentials are never stored, and a `cipassword` would technically honour that — it would only pass through. But it would pass through a form, a request body, a job payload and a state row, each of which is a place it could come to rest. A type with nowhere to put it cannot acquire the habit later.

**Create and destroy, with compensating controls.** Since the structural guarantee is gone, its replacements are named here so they are understood as load-bearing rather than decoration:

- destruction requires the caller to submit the guest's name and the server to match it, so the confirmation is a control and not a UI courtesy;
- templates are refused outright — they are what everything else is cloned from;
- both the intent and the outcome of every provision and destroy are audited under `AuditCategorySecurity`;
- a provisioning run that fails partway leaves the partial guest in place.

## Rationale

**The single token is the weakest decision in this ADR, and it was made deliberately.** A second, provisioning-only credential would have preserved §15.4 for every path except the one that needs it: sync, metrics, console and power — the overwhelming majority of what the portal does, and all of its continuously-running code — would still be structurally incapable of destroying anything. It was rejected on configuration cost: a second secret per platform to create, envelope-encrypt, rotate and explain, a second way for a platform to be half-configured, and a failure mode ("provisioning stopped working") invisible in the platform's health. The stakeholder chose the simpler configuration with the trade stated. **If the portal grows a second write-capable feature, this decision should be reopened before that feature ships, not after.**

**Optional privileges rather than required ones** keep the blast radius opt-in per platform. An administrator who wants inventory and consoles from a cluster and nothing else grants nothing else, and the portal tells them precisely what provisioning would need rather than refusing to start. It also means this ADR does not silently widen anything: a token only gains these privileges when a human edits it on the cluster.

**Failure leaves the partial guest** because the alternative is worse. A clone that reports failure has frequently produced a working machine — a timeout on the task poll, a resize that was refused, a start that raced the disk. Cleaning up automatically means the portal destroying a guest on the strength of an error it may have misread, which is exactly the capability this ADR is most careful about. The request records which step failed and an administrator decides.

**Admin-only in v1** because the interesting question — whether an operator should provision into groups they already reach, under a quota — deserves its own design rather than being answered by default. Nothing here forecloses it.

## Consequences

- **§15.4 is no longer true, and now says so.** The doc is amended rather than left to age into a comforting inaccuracy. This is the point of writing it down: someone reading the security design a year from now must not believe a guarantee that was retired today.
- **The blast radius of every other bug in the portal grew.** A path traversal in a handler, a permission-map entry pointed at the wrong role, a stolen admin session — each of these now reaches guest destruction, where before the credential stopped them. The audit trail changes from a record into a primary control, and it is the only one that operates after the fact.
- **Egress allow-listing and certificate pinning are unchanged and were never what enforced this.** They constrain who the portal talks to, not what it may ask for. Nothing about ADR 0009's failover changes the picture either: a wider token is wider at every cluster member.
- **Provisioning is inert until a template exists, so the portal builds them too.** The cluster this was designed against reported zero of them, and the first thing provisioning did on it was tell an operator to go and run four commands on a node — the same gap this ADR exists to close, one step earlier. Building is therefore part of the feature rather than a prerequisite left outside it, and it costs four privileges beyond provisioning's own.
- **A guest that becomes a template leaves the inventory as a conversion.** For the half-minute between being created and being converted it is a real guest, so a sync running in that window files it as one; afterwards the platform stops listing it, and the mark-and-sweep would report it `missing` for three cycles — the word for an asset that vanished unexpectedly, which is the opposite of what happened.

  The decision is made **in the sweep**, not when a build finishes, and that placement is the whole point. Closing the row out at completion loses a race that was observed against a live cluster: the sync which files the guest is often still in flight when the build ends, so the tidy-up runs before the row exists and the row lands afterwards anyway. The sweep already asks "which guests were absent from this run"; it now also asks the platform which ones are templates, so the answer does not depend on when anything happened. It costs one `ListTemplates` per inventory run, and it covers a template somebody converted by hand outside the portal entirely — the same phenomenon, previously reported the same misleading way.
- **A destroyed guest leaves the portal at the next sync,** not at the moment of deletion, and reconciliation marks it missing before deleted. That is the existing SYNC-03 behaviour and it is correct here: the portal reports what the platform says, and the platform is the one that knows.
- **Risk P1 loses its mitigation.** "Structurally impossible without an explicit decision" was the whole control, and this is the explicit decision. What remains against scope creep is ordinary judgement about what the product is for.

## Alternatives considered

- **A second, provisioning-only credential.** The security-preferred answer, rejected on configuration cost and recorded above with its reasons. Reopen it before the next write-capable feature.
- **Scope the token to a Proxmox pool.** Grant the allocate and destroy privileges on `/pool/portal` rather than `/`, so the credential can only create and remove guests inside a pool an administrator nominated, and the rest of the estate stays out of reach even under one token. This is a genuinely good middle ground and it is not being done now only because pools are not in the portal's inventory model. It is the first thing to build if this decision starts to feel too wide.
- **Guard in portal code only, leave the token alone.** Not an option, and worth saying plainly: the platform enforces the ceiling, so a portal-side rule cannot create the capability it is pretending to restrain.
- **Hand provisioning to Terraform/OpenTofu and have the portal write a plan.** Moves the credential rather than removing it, and splits the fleet's state across two systems that disagree the moment anyone touches either. The portal would still need to read back what happened.
