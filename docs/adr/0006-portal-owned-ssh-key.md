# ADR 0006 — The portal owns one SSH key

**Status:** accepted · **Date:** 2026-08-16 · **Amends:** [0005](0005-ssh-credentials-are-never-stored.md) · **Plan:** [docs/29-ssh-terminal.md](../29-ssh-terminal.md) · **Requirements:** SSH-11…SSH-14 in [docs/03-frs.md](../03-frs.md)

## Context

ADR 0005 refused to store SSH credentials and named the cost: retyping, on
every connect and again after every idle timeout. It also named the condition
under which that decision should be revisited — operator fatigue — and it
arrived within a day.

The revisit ADR 0005 anticipated was its own option 2: store a guest credential
per VM, sealed in the vault. That is not what this decides. The problem with
option 2 was never the encryption; it was that a compromise of the portal would
yield `root` on every guest in the estate, and that an operator's grant over a
VM would quietly come to mean "become root on it without knowing the password".

An SSH key changes the shape of that. The secret is the portal's own rather
than a guest account's; it is installed deliberately, one account at a time, by
somebody who already had a shell there; and taking it back is deleting one line
from one file rather than rotating a password that a dozen other things use.

## Decision

**The portal generates and holds exactly one Ed25519 key pair.** The private
half is sealed with the same envelope encryption as a platform credential
(`ssh_portal_key`). The public half is installed into an account's
`authorized_keys` on a guest — either by the portal, over an SSH session the
operator has already authenticated, or by hand from the line the settings page
displays.

**Guest passwords remain unstored.** SSH-03 is untouched: a password typed into
the connect form is used for one dial and dropped. What the key removes is the
need to type one at all, on guests where somebody has chosen to install it.

## Rationale

### Why one key rather than one per user or per VM

Per user would name the real person in the guest's own logs, which is better,
and needs every operator provisioned on every guest — the same thing that
sank option 3 in ADR 0005. Per VM would mean a key store whose compromise is
the estate, which is what sank option 2.

One portal key is a single revocable object. Its blast radius is not "every
guest" but "every account somebody installed it into", and that list is a table
the portal can show. The audit trail keeps naming the portal user who connected
and the guest account they connected as, which is what an audit needs; the
guest's own logs will say `proxui-portal`, which is honest about what opened
the session.

### Why installing runs over an existing session

The alternative is a portal that can write to `authorized_keys` on a machine
using a credential it holds, which is a portal that can grant itself access.
Running the write through a session the operator opened means the authorization
is one they already had and already used: they authenticated as that account,
the write happens with that account's permissions, and the portal gains nothing
it was not already given.

Cloud-init is the second path for guests nobody has a password on. It is a
copy-and-paste of a public key, which needs no portal privilege at all.

### What it costs

- **A stored secret that grants shell.** Mitigated by scope (only where
  installed), by revocability (one line, removable from the same UI), and by
  the vault. Not eliminated. This is the trade ADR 0005 declined and this ADR
  accepts, with the reasons above.
- **Rotation is estate-wide.** One key means rotating it invalidates every
  install at once. The rotation confirmation says so and names the count; the
  installs left behind are listed as stale rather than silently relabelled.
- **The install table can lie.** It records what the portal did, not what the
  guest will accept. An operator who edits `authorized_keys` by hand leaves a
  row stale, and the connect form will offer a key that fails. The guest is the
  authority; a failed key auth is a clean 401, and the password form is still
  there.

### What is deliberately not built

No agent forwarding, no per-user keys, no automatic install on first connect.
Each would make the key arrive somewhere nobody chose to put it, which is the
property that makes this different from option 2.

## Consequences

- Reading the public half is an operator's permission; holding the pair —
  generating, rotating, deleting, and listing every install — is an
  administrator's. A rotation is an estate-wide act, and the install list is a
  map of where the key opens a door.
- `ports.RemoteFS` grew `Append`, because adding a line to a file the portal
  does not own must not be able to lose the lines it did not write. The first
  implementation trusted the SFTP append flag; a server that ignores it takes
  the client's offset instead and overwrites from the start, which erased every
  key already in the file. It seeks to the end now, and a test pins it.
- The audit trail gained `auth_method` on every session open and every denial,
  so "which door was used" is answerable across the whole SSH history rather
  than only for sessions that used the key.
