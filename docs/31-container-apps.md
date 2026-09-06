# 31. Container apps

Installing an application into an LXC container, from a catalogue the portal
ships. What it runs, where that came from, and what the portal will not do.

## 31.1 What this is

The [Proxmox VE Helper-Scripts](https://github.com/community-scripts/ProxmoxVE)
are how most people put an application on a Proxmox host: one script creates a
container, installs the application into it and configures it. There are 590 of
them and they are actively maintained.

**Platforms → the platform → … or Infrastructure → Container apps** lists them,
and deploying one runs it on a node you pick. The container that results is an
ordinary inventory record within a minute, with its console, terminal and power
controls already working — none of which needed anything new, because the
portal has always treated a guest's type as data.

This is not **Published apps**. That feature exposes a hostname through a
Cloudflare tunnel and runs nothing; this one runs something and exposes nothing.

## 31.2 What actually runs

A deploy is one command on the node, and everything in it is fixed except six
numbers:

```
export MODE=default
export COMMUNITY_SCRIPTS_URL='https://raw.githubusercontent.com/community-scripts/ProxmoxVE/<commit>'
export COMMUNITY_SCRIPTS_CORE_URL='https://raw.githubusercontent.com/community-scripts/core/<commit>'
export var_cpu='2' var_ram='2048' …
bash adguard.sh          # the copy vendored with ProxUI
```

The deploy dialog shows this before it runs, filled in with what you chose. That
is deliberate: the portal is asking to run a large third-party program as root
on a hypervisor, and the honest way to ask is to say what it is.

**The script is not fetched.** All 590 are vendored in
`internal/app/deploy/scripts/ct` at a reviewed commit and written to the node
from the binary. **Both upstream repositories are pinned** to commits, so what
the script pulls in while it runs is fixed rather than whatever is on a branch
that morning.

**Nothing you type becomes a command.** The request names a catalogue
identifier; an identifier the binary does not know is refused before anything is
dialled. Hostname, cores, memory, disk, storage and bridge are validated as
numbers or as identifiers and reach the node inside a file the portal wrote.
[ADR 0012](adr/0012-the-portal-runs-a-vetted-catalogue-on-a-node.md) is the
argument for why this is a reasonable thing for a VM portal to do, and — more
importantly — what it does not claim.

## 31.3 What the portal cannot promise

Worth being blunt about, because the rest of the portal is careful here.

- **Pinned is not verified.** The engine and the in-container installer are
  fetched from the pinned commits while the script runs. ProxUI has fixed *which*
  bytes those will be; it has not seen them.
- **The application installs packages from the internet.** Nothing bounds that,
  and nothing could. A node with no egress cannot use this.
- **The portal does not own the container afterwards.** There is no update and
  no remove. The record says what was deployed and what the script printed; the
  container is yours.

## 31.4 Watching one

A deploy takes minutes. The script is detached on the node and keeps going
whether or not the page is open — and whether or not the portal restarts, since
its log is a file on the node rather than anything the portal is holding.

The deployment view shows the transcript as it arrives. This is the only place
in the portal that shows the output of a non-interactive command run on a node;
everywhere else reports a state and a sentence, which is enough for a step that
either worked or did not, and not enough for a fifteen-minute script that
stopped two thirds of the way through.

A deploy that never finishes is closed out after forty minutes with whatever it
printed. Nothing is cleaned up: a half-built container is left in place, for the
same reason a failed provisioning run leaves its guest (PROV-06).

## 31.5 Requirements

| On the node | Why |
|---|---|
| the portal's public key in `root`'s `authorized_keys`, **and its host key already pinned** | the deploy writes to the machine and runs a program on it; a node the portal is meeting for the first time is the wrong one to do that on |
| internet egress | the script fetches its engine, and the application fetches its packages |

The host key is pinned by **Platforms → Readiness → Check** (§14.6), which is
also where you find out whether the key is installed at all.

## 31.6 Updating the catalogue

`make gen-apps` fetches the pinned upstream tarball, rewrites
`internal/app/deploy/scripts/ct` and regenerates the catalogue. Change the two
commits at the top of `internal/app/deploy/gen/main.go` first.

**Read the diff.** It is the whole compensating control: everything else in this
feature assumes somebody looked at what changed in the scripts that are about to
run as root. That is also why they are vendored as plain files rather than an
archive — a tarball's diff says only that some bytes changed.
