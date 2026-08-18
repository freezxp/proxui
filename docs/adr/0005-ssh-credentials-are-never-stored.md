# ADR 0005 — SSH credentials are never stored

**Status:** accepted, amended by [0006](0006-portal-owned-ssh-key.md) · **Date:** 2026-08-16 · **Plan:** [docs/29-ssh-terminal.md](../29-ssh-terminal.md) · **Requirements:** SSH-01…SSH-10 in [docs/03-frs.md](../03-frs.md)

> **Amended the same day.** The decision below stands for *guest* credentials:
> a password or key typed into the connect form is still used once and dropped.
> ADR 0006 adds a portal-owned key pair, which is a different object with a
> different blast radius — see the "Revisiting" section at the end, and 0006
> for why the answer there was a portal key rather than the per-VM credential
> store this ADR anticipated.

## Context

The portal grew a terminal: SSH to a guest, with a file browser, instead of
driving it through the hypervisor's console.

Every other credential the portal touches, it keeps. A Proxmox API token and a
Cloudflare account token are both sealed with the envelope encryption in
`internal/infra/crypto` and used on the portal's own initiative — a sync runs
at three in the morning with nobody signed in, and it has to.

An SSH credential is not like that. It belongs to an account on a guest
operating system, not to the portal, and the portal never needs it except while
an operator is sitting there.

Three options were on the table:

1. **Prompt per session, keep nothing.**
2. **Store a credential per VM**, sealed in the vault, so an operator clicks
   Connect and is in.
3. **Store a credential per user**, so guest-side logs name the real person.

## Decision

**Option 1.** The credential is typed into the connect form, posted once, used
to open one connection, and dropped. It is not written to Postgres, not written
to Redis, not logged, and not returned by any response.

## Rationale

### What storing them would actually mean

Option 2 is the better user experience by a distance, and it changes what a
compromise of the portal is worth. Today the portal holds tokens scoped to a
hypervisor API: bad, bounded, revocable in one place. A per-VM SSH credential
store holds `root` on every guest in the estate — a lateral movement kit with a
web interface, and the blast radius no longer stops at the platform boundary.

It also quietly changes what an *operator* is. A grant over a VM currently
means "you may see it, console into it, power it". With stored credentials it
would additionally mean "you may become root on it without ever knowing the
password", which is a different grant that nobody has agreed to make.

Option 3 avoids the shared-secret problem and needs every user provisioned on
every guest, which the estate does not do and the portal cannot make it do.

### What it costs

Retyping. On every connect, and again after an idle timeout. This is a real
cost and it is the reason to revisit the decision later rather than a reason to
dismiss it now.

Two consequences fall out of it, and both are recorded rather than worked
around:

- **A session lives in one process.** It cannot be rebuilt from the database,
  because the thing that would rebuild it was deliberately not kept. A
  load-balanced `--role=api` deployment therefore needs session affinity for
  `/ws/ssh/` and `/api/v1/ssh-sessions/`. The single-binary default is
  unaffected.
- **A restart ends every session.** The alternative is persisting a credential.

### What is kept instead

The *username*, on the session record and in the audit trail. It is the part an
audit needs — "who logged into that box as root, and when" — and it is not a
secret. The password or key that went with it appears in no row and no log
line, and a test asserts that no audit detail ever contains it.

Host keys are kept, and they are not credentials: a host key is public by
construction, pinned only so that a *change* is detectable.

## Consequences

- `internal/domain/shell` is its own bounded context rather than a `kind` on
  the console. The two share a shape and disagree about the thing that matters.
- `internal/app/shellreg` holds live connections in memory, with the ticket
  store beside it for the same reason.
- The connect form is part of the security design, not a UX detail: it is the
  only moment the secret exists, which is why the response never echoes it and
  the page clears it once spent.

## Revisiting

The credible reason to change this is operator fatigue, and the credible change
is option 2 *as an explicit, separately granted capability* — not as a default
— behind the same vault as a platform credential, with its own permission-map
entry and its own audit category. It should not arrive as a convenience flag on
the existing connect form.
