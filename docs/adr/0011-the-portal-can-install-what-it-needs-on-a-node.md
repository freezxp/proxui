# ADR 0011 — The portal can install what it needs on a node

**Status:** accepted · **Date:** 2026-09-05 · **Amends:** the decision in [ADR 0007](0007-the-portal-reads-node-sensors-over-ssh.md) · **Adds:** NODE-01…NODE-05 to [docs/03-frs.md](../03-frs.md)

## Context

A template built on `cx1` came out with no guest agent, because `cx1` had no
`libguestfs-tools` and only `pve` did. Nothing failed: the build finished, the
template worked, and the request recorded *"install libguestfs-tools on cx1 and
rebuild to have one."* It still cost a round trip, because that sentence was
written where nobody would read it — `ProvisionStatus` renders only while you
stay on the inventory page, and no screen lists past requests at all.

That is the second time this shape has appeared. `docs/30-node-sensors.md` tells
an operator to `apt install lm-sensors` by hand, and `sensor.describe()` exists
to turn a failed poll into *"the node has no `sensors` command"*. Each
prerequisite is discovered the same way: something stops working, and somebody
finds the message that says why.

The portal does not own the Proxmox nodes and cannot make anything standard on
them. It can stop the requirement being invisible — say what a node needs, check
whether it has it, and offer to install what it can.

The last part is what needs deciding, because ADR 0007 drew a boundary around
node access and its rationale rested on four things at once:

> *It is not a shell … one fixed argv … It cannot be handed an argument from a
> request … there is no request in the loop at all — the collector runs on the
> scheduler.*

Installing a package keeps the first two and breaks the last two. A person
presses a button, and the node is modified as a result. ADR 0010's template
preparation already put a request in the loop and already wrote to a disk, but
it wrote to a *guest's* disk; this changes the software on the hypervisor
itself, which is a larger thing and should not arrive as a footnote to a
feature.

## Decision

**The portal checks node prerequisites on demand, and can install the ones it
knows about.**

**What it may install is a constant in the binary.** The request names a
prerequisite by identifier; the server maps that identifier to a package list
and refuses one it does not recognise. Nothing a caller sends reaches the
command line. The argv is as fixed as `sensors -j` was — what became variable is
only *which* fixed command runs, chosen from a set the binary defines.

**Two prerequisites are installable** — `libguestfs-tools`, for preparing a
template's disk, and `lm-sensors`, for node temperatures. Both are already
required today; both are discovered today by something failing.

**Two are checked and deliberately not fixable.** The portal's own SSH key
cannot be installed by the portal, because installing it needs the access it
grants: that bootstrap is an operator's, once per node, and `docs/30` already
explains it. The API token's privileges are the platform's own configuration,
and widening them is a decision with its own ADR (0010) rather than a button.

**Admin-only, confirmed, and audited** under `AuditCategorySecurity` as
`node.install`, naming the node and the packages.

**Checking is on demand.** Every node, every page load, would mean an SSH
handshake per node per view for an answer that changes about once a year.

## Rationale

**The package list is the whole control.** SSH-02 and ADR 0007 are ultimately
about one question: can an operator, or somebody holding an operator's session,
steer the portal into running something of their choosing on a machine? The
address still comes from the platform, the credential is still the portal's own
key, the host key is still pinned, and the command is still a constant. A
request selects from a menu the binary wrote. That is a materially smaller
capability than "run a command on a node", and it is the reason this does not
reopen what SSH-02 closed.

**It is honest about what it cannot do.** A readiness view that offered to fix
the SSH key would be lying, and one that offered to widen the token would be
routing around ADR 0010. Both are reported with the exact command or privilege
and a pointer to the page that explains them. A check that says "I cannot fix
this, here is how" is more useful than one that quietly omits what it cannot
reach.

**On demand rather than continuous**, because the failure this fixes is not that
prerequisites change — they almost never do — but that nobody knew about them
until something broke. A button pressed once when a platform is added, or when
something is not working, catches that.

## Consequences

- **The portal can change software on a hypervisor.** That is new, and it is the
  point. What bounds it is the compiled package list and the admin gate; what
  makes it reviewable is the audit entry. Anyone auditing this should be reading
  `node.install` entries, not the diff of a UI.
- **ADR 0007's "no request in the loop" no longer holds for all node access.**
  Sensors still run from the scheduler with no request anywhere near them. The
  readiness check and the install are request-driven, and that distinction is now
  a property of the individual path rather than of node access as a whole.
- **Two gaps close on the way past.** `POST /platforms/test` builds the
  provisioning and template capability report and its response drops it, so the
  separate reporting ADR 0010 introduced has been invisible; and that endpoint
  works only before a platform is saved, since the credential is write-only.
  Readiness reports both for a saved platform, reading the credential from
  storage.
- **A node that cannot be reached reports exactly that.** It is the most common
  real answer — the key is not installed yet — and it is the one the previous
  design turned into "sensors are unavailable".
- **`apt-get` is Debian's.** The list lives on the Proxmox connector, which knows
  its nodes are Debian. A future connector whose nodes are not would supply its
  own, or none, and the check would report nothing rather than guess.

## Alternatives considered

- **Check only, and print the command.** Untouched boundary, and it is what
  happens today: the note on the failed build named the package and the node,
  and it still took a person noticing. The information was never the missing
  part.
- **Install automatically when something is missing.** Removes the button and
  the audit trail's meaning with it. A portal that installs packages on
  hypervisors unprompted is a different product, and nobody asked for that one.
- **Let the request name the package.** Would make the endpoint general and turn
  it into remote package execution as a service. The identifier indirection
  costs one map lookup and is the difference between a menu and a shell.
- **Ship the tools in the portal's own container.** They have to run against the
  node's disks and hardware, on the node. Nothing in the portal's container can
  do that.
