# 30. Node temperatures

How to get a temperature out of a Proxmox node, and why it takes more than
turning something on.

## 30.1 Why the API cannot answer this

**Proxmox publishes no temperature anywhere in its API.** Not at a different
privilege, not on a newer version — the field does not exist. `GET
/nodes/{node}/status` returns CPU, load, memory, rootfs, uptime, kernel and
`cpuinfo`, and stops.

The one temperature reachable through the API is a drive's, inside the SMART
passthrough at `/nodes/{node}/disks/smart`. It shells out to `smartctl` per
disk and says nothing about the CPU.

The number does exist on the node. The kernel exposes it through hwmon,
`lm-sensors` reads it, and `sensors -j` prints it as JSON. So the portal goes
and gets it: one SSH connection per node, one fixed command, the portal's own
key. [ADR 0007](adr/0007-the-portal-reads-node-sensors-over-ssh.md) is the
argument for why that is a reasonable thing for a VM portal to do and what it
is not allowed to grow into.

## 30.2 Setting it up

Two things on each node, both on the node.

**1. Install lm-sensors.**

```
apt install lm-sensors
sensors-detect --auto      # answer nothing; the defaults are right
sensors -j                 # should print JSON, not an error
```

If `sensors -j` prints an empty object, the kernel found no hwmon devices it
recognises. That is a hardware and driver question, and nothing in the portal
can help with it. A VM pretending to be a node — the usual case in a lab —
genuinely has no sensors to report.

**2. Install the portal's public key.**

**Settings → SSH key** shows the portal's public half. Put it in the node's
`/root/.ssh/authorized_keys`:

```
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo 'ssh-ed25519 AAAA…' >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
```

Nothing else. No password is stored, no node credential is entered in the
portal, and taking the access back is deleting that line.

Readings appear on **Hosts** within five minutes. Click a node to see every
sensor it reports.

## 30.3 What the portal does with the connection

Worth being precise about, because it is `root` on a hypervisor.

- **One command**, `sensors -j`, a constant in the binary. It is not assembled
  from anything a request carried, and there is no endpoint that runs a command
  of your choosing on a node.
- **Not a shell.** The node connection is made by a different code path from
  the terminal, one that returns bytes rather than a session. There is no
  route from it to a terminal, an SFTP browser or a forwarded port.
- **The address comes from the platform**, out of `/cluster/status`. A node the
  cluster did not name has no address to dial.
- **On the scheduler**, every five minutes, with nobody signed in.

## 30.4 The host key

The first successful poll pins the node's SSH host key. Every poll after that
refuses a node presenting a different one, and says so on the host page.

Nobody is present at that first connection to compare a fingerprint, so this is
trust-on-first-use — weaker than the operator-confirmed pinning a guest gets
under SSH-04. The fingerprint is shown on the host page so the comparison can
be made by hand afterwards, against `ssh-keyscan` from your own terminal.

A node that has genuinely been rebuilt will fail every poll until its pin is
cleared: **Hosts → the node → clear the pinned key**, which is admin-only and
audited.

## 30.5 Alerting on it

Alert rules take a subject now. A rule on **node** watches:

| Metric | Reads | Good for |
|---|---|---|
| `temp_c` | the hottest sensor, in degrees | one machine you know |
| `temp_headroom_pct` | how much of the chip's own critical point is left | an estate whose CPUs disagree about what hot means |

Headroom is the portable one. A package at 84°C is fine on a chip that critical
at 100°C and nearly throttling on one that criticals at 90°C, and the chip is
the only thing that knows which it is. A rule of "headroom below 15%" means the
same thing on every node; "above 80°C" does not.

Node rules cover the whole estate — nodes are not in VM groups, and a portal
with three of them does not need a grouping vocabulary to say so.

## 30.6 When there are no readings

The host page says which of these it is.

| What it says | What to do |
|---|---|
| *the node refused the portal's key* | the public key is not in that node's `authorized_keys`, or is not in root's |
| *lm-sensors is not installed on the node* | `apt install lm-sensors` there |
| *the node could not be reached on port 22* | sshd is not listening, or the address from `/cluster/status` is not reachable from the portal |
| *the node presented a different host key* | the node was rebuilt (clear the pin), or it is not the node the portal met |
| *This node has not been read yet* | the first poll has not run, or the portal has no SSH key of its own — generate one in **Settings → SSH key** |

Nothing here is an error state in the portal's sense. A node with no key
installed is the normal starting condition for every node, and it stays quiet
about it rather than filling the log.
