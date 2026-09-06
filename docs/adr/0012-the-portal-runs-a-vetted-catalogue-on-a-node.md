# ADR 0012 — The portal runs a vetted catalogue on a node

**Status:** accepted · **Date:** 2026-09-06 · **Amends:** [ADR 0007](0007-the-portal-reads-node-sensors-over-ssh.md) and [ADR 0011](0011-the-portal-can-install-what-it-needs-on-a-node.md) · **Adds:** APP-01…APP-06 to [docs/03-frs.md](../03-frs.md)

## Context

The [Proxmox VE Helper-Scripts](https://github.com/community-scripts/ProxmoxVE)
are how most people put an application on a Proxmox host. One command on a node
creates an LXC container, installs the application into it and configures it.
There are 590 of them, they are actively maintained, and running one is a
copy-paste from a website into a root shell.

The portal already owns everything around that act. It knows which nodes a
platform has and what address each answers on. It holds an SSH key to each one,
with the host key pinned. The container that results is a first-class inventory
record the moment the next sync runs — and power, the VNC console and the SSH
terminal all already work for it, because the guest type is data threaded
through the connector rather than a branch. The one thing the portal cannot do
is the deploy itself, which leaves an operator moving between a website, a root
shell and the portal to do one thing.

So the feature is obvious. What is not obvious is whether the portal should be
the thing that runs it, because every ADR before this one has said no to
something much smaller.

ADR 0007 drew the boundary at "one fixed command, never a shell". ADR 0011
crossed the half of it that was about reading, and was careful about how:

> **Let the request name the package.** Would make the endpoint general and turn
> it into remote package execution as a service. The identifier indirection
> costs one map lookup and is the difference between a menu and a shell.

This is that, and more. A helper script is thousands of lines of third-party
bash, running as root on a hypervisor, and it fetches more of itself while it
runs. Calling it "the same as installing lm-sensors" would be false, and
building it without saying so would be worse than not building it.

## Decision

**The portal deploys applications from a catalogue it ships, by running the
upstream entry script on a node.**

**A request names a catalogue identifier.** Never a command, never a URL, never
a package. The identifier is validated against a pattern and looked up in a
catalogue compiled into the binary; one it does not recognise goes no further.
This is ADR 0011's indirection, unchanged, and it is what keeps the endpoint a
menu rather than a shell.

**The entry script's bytes are the portal's own.** All 590 `ct/` scripts are
vendored into the binary at a reviewed commit and written to the node from
there. The portal does not fetch the thing it is about to run.

**Both upstream roots are pinned.** The script resolves its engine and its
in-container installer through `COMMUNITY_SCRIPTS_CORE_URL` and
`COMMUNITY_SCRIPTS_URL`, and both are set to a reviewed commit SHA rather than
left to default to `main`. Moving to a newer upstream is a commit in this
repository with a diff somebody can read.

**Everything variable is an environment assignment, not text in a command.**
Cores, memory, disk, hostname, storage, bridge: each is validated as a number or
against a pattern and placed as a `var_*` assignment. Nothing a request carried
is spliced into the script or into the shell that starts it.

**Admin-only, audited as `container.deploy`, and the command is shown in full
before it runs.** The node's host key must already be pinned, as it must for
installing a package (ADR 0011) and preparing a disk (PROV-14): this writes to a
machine, and a machine whose identity the portal is learning in the same breath
is the wrong one to write to.

**The run is detached and its log lives on the node.** A deploy takes minutes,
and `RunCommand` buffers output and discards the buffer when its deadline fires.
The script is launched under `setsid nohup` writing to a log and a status file,
and the portal polls both. That also makes the log survive a portal restart,
because it was never in the portal.

## Rationale

**The identifier is still the whole control.** Ask the question ADR 0011 asked —
can an operator, or somebody holding an operator's session, steer the portal
into running something of their choosing on a machine? They cannot. They can
choose from 590 scripts this repository vendored at a commit somebody reviewed,
and they can set six numbers. That is a much larger menu than ADR 0011's, and it
is still a menu.

**Pinning is what makes this reviewable at all.** An unpinned
`bash -c "$(curl …/main/ct/x.sh)"` is not a thing anyone can approve, because
what it runs is whatever was pushed that morning. A pinned SHA can be read,
diffed against the last one, and rolled back — and it makes two deploys a month
apart the same deploy.

**Vendoring the entry script closes the gap that pinning alone leaves.** A
pinned URL is still a fetch, and a fetch can fail, be intercepted, or be served
something else. The bytes that start the deploy are in the binary.

**A generated catalogue rather than a curated one**, against the instinct that
produced the deliberately tiny image catalogue. The argument there was that
"every entry is a claim that a URL is still right". Here the entries are
generated from the pinned tarball itself, so an entry cannot be wrong about
what is available — it *is* what is available at that commit — and nothing is
hand-maintained, so nothing rots quietly.

## Consequences

- **The portal runs a large third-party program as root on a hypervisor.** That
  is new and it is the point. What bounds it is the vendored entry script, the
  pinned roots, the validated identifier and the admin gate; what makes it
  reviewable afterwards is the audit entry and the log.
- **Pinned is not verified, and the difference matters.** The engine and the
  in-container installer are fetched from the pinned SHAs while the script runs.
  ProxUI has not seen those bytes before they execute — it has only fixed
  *which* bytes they will be. Vendoring the engine repository too, so nothing is
  fetched at run time, is the obvious next step and is deliberately not claimed
  here.
- **The application installs packages from the internet.** Nothing in this
  design bounds that, and nothing could: it is what installing an application
  means. A node without egress cannot use this feature.
- **The portal shows the output of a non-interactive node command for the first
  time.** Everything before this reported structured state — a step, a sentence,
  an exit — and threw the transcript away. A deploy that fails halfway is not
  explicable that way, so the log is kept and shown.
- **A deployment is a record, and the container is not.** The portal remembers
  that it deployed something; it does not own the container afterwards. Updating
  and removing are the container's business and this version does neither.

## Alternatives considered

- **Track `main` and fetch at deploy time**, the way a person does by hand. Half
  the code, always current, and unreviewable by construction: nobody can say
  what will run until it has run.
- **Reimplement the scripts against the Proxmox API.** The portal would create
  the container itself and never execute third-party code. It is also 590
  installers to write and maintain, each duplicating work that is already
  correct and already looked after by people who use it daily.
- **Vendor the engine as well, and run with no network fetch of code.** The
  strongest version, and where this should end up. Held back because it is
  another 900 kB of vendored bash and a second pin to keep straight, and this
  ADR is already asking for a large step.
- **Curate twenty apps by hand.** Smaller surface and each entry vouched for.
  But the request was for what is available, and a hand-list answers a different
  question — and drifts from upstream the day it is written.
