# ADR 0007 — The portal reads node sensors over SSH

**Status:** accepted · **Date:** 2026-08-19 · **Extends:** [0006](0006-portal-owned-ssh-key.md) · **Relaxes:** [SSH-02](../03-frs.md) ("the portal is not an SSH proxy to the rest of the network") · **Requirements:** SENSOR-01…SENSOR-05 in [docs/03-frs.md](../03-frs.md)

## Context

The dashboard shows what a node is doing and not how hot it is getting, which
in a homelab is the number that predicts the failure. Proxmox does not publish
it.

That is not a privilege problem and not a version problem. The published API
schema — every endpoint of it — contains no thermal field of any kind:
`/nodes/{node}/status` returns CPU, load, memory, rootfs, uptime, kernel and
`cpuinfo`, and stops there. The one temperature reachable through the API is a
drive's, buried in the SMART passthrough at `/nodes/{node}/disks/smart`, which
shells out to `smartctl` per disk and says nothing about the CPU.

The number exists on the node. `lm-sensors` reads it from the hwmon devices the
kernel already exposes, and `sensors -j` prints it as JSON. Getting it into the
portal means reaching the node itself.

## Decision

**The portal runs one command, `sensors -j`, on each node, over SSH, using the
key it already owns.**

- **The key is the portal's own** (ADR 0006). No node password or node key is
  stored — the operator installs the portal's public half into the node's
  `authorized_keys`, exactly as they already can for a guest, and takes it back
  by deleting that line.
- **It is not a shell.** The collector calls a `RunCommand` path on the SSH
  client that opens a session, runs one fixed argv, captures stdout and closes.
  It cannot be handed an argument from a request, and no code path turns a node
  connection into a terminal, a port forward or an SFTP session.
- **The node's address comes from the platform**, out of `/cluster/status`,
  never from a request. A node the platform did not name has no address to
  dial.
- **The host key is pinned on first use**, per node, and a change is refused —
  the same rule SSH-04 applies to guests, for the same reason.
- **Sensors are stored per reading**, chip and label intact, rather than reduced
  to one number on the way in. See "Why every sensor" below.

## Rationale

### What SSH-02 was protecting, and why this does not spend it

SSH-02 says the portal connects only to addresses the platform reported for a
VM, and that a host outside that list is refused: *the portal is not an SSH
proxy to the rest of the network*.

The thing that rule keeps out is an operator — or an attacker holding an
operator's session — steering the portal at an arbitrary address and getting an
interactive shell there. Every part of that is a property of the *interactive*
path: the address comes from a request, the session is a terminal, and the
credential is whatever was typed.

None of it describes this. The address is the one the platform reported for a
node it is managing, the credential is the portal's own key, the command is a
constant in the binary, and there is no request in the loop at all — the
collector runs on the scheduler. The reachable set does not grow by one
address that a request can choose.

So the rule is relaxed in one direction, narrowly: **the portal may reach a
node the platform named, non-interactively, to run the fixed sensor command.**
Everything SSH-02 says about guest terminals stands unchanged.

### Why the portal key rather than a stored node credential

ADR 0005 refused to store guest credentials and ADR 0006 explains why a
portal-owned key is a different object: the secret belongs to the portal, it is
installed deliberately by somebody who already had access, and revoking it is
one line in one file.

All of that is more true of a node than of a guest. A node credential is
`root` on the hypervisor — the account that can read every disk in the estate.
Storing one would put the whole estate behind the vault key, which is precisely
the blast radius ADR 0006 was written to avoid. An installed public key with no
matching secret anywhere but the vault, revocable from the node itself without
the portal's cooperation, is the smaller thing.

The cost is that sensors do not work until somebody installs the key. That is
the right cost: it makes reading a node's hardware an act the operator performs
on the node, not a setting the portal grants itself.

### Why every sensor, and not one number

The obvious design records one temperature per host and charts it beside CPU.
It is wrong for the machines this portal is pointed at.

`sensors` on a mid-range board reports a CPU package, a per-core reading for
every core, an NVMe composite, one or two drive sensors and often a chipset or
VRM. Reducing that to a maximum answers "is something hot" and destroys the
only question worth asking next, which is *what*. A package at 78°C and an NVMe
at 78°C are different afternoons.

So readings are stored as they are read — `chip`, `label`, value, and the
`high`/`critical` thresholds the chip itself declares — in their own
hypertable rather than as columns on `metrics_host`. That table is wide and
fixed by design, one row per host per interval, and it is the wrong shape for a
set whose members differ per machine and appear and disappear with hardware.

The thresholds are stored because the chip knows them and nobody else does:
80°C is alarming for a CPU package and unremarkable for a VRM, and a rule
written against "percent of the manufacturer's critical point" is portable
across machines in a way a rule written against 80 is not.

## Consequences

- A node with no key installed, or no `lm-sensors`, reports nothing and says so
  on the host page. It is not an error state; most nodes will start there.
- Readings arrive on their own schedule, slower than the metrics cycle. A
  temperature that moves in seconds is not a temperature anybody acts on, and
  each poll is an SSH handshake.
- Alert rules gain a host subject. Until now every rule was scoped to VMs, and
  a rule over a node's hardware has no VM to name.
- `sensors -j` is a stable enough interface to parse, but it is not an API. The
  parser treats an unreadable chip as absent rather than as a failure, so one
  odd driver cannot cost the whole reading.
