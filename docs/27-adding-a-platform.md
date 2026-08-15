# 27. Adding a Proxmox platform, with everything working

A platform is one Proxmox cluster the portal reads from. Adding it is two
jobs: creating a credential on Proxmox that is allowed to do what you want,
and telling the portal where to find it.

Most of the work is on the Proxmox side, and almost every "the portal is
missing X" turns out to be a privilege the token was never granted. The table
in §27.2 is the part to get right; the rest is a form.

## 27.1 Decide: token or password

| | API token | Username and password |
|---|---|---|
| What the portal stores | a scoped secret | a login |
| Can be revoked on its own | yes | no, you change the password |
| Privileges | exactly what you grant | everything that account can do |
| Expires | optional expiry date | no |

Use a token. The portal supports a password because some clusters make token
creation awkward, but a password means the portal holds a credential that can
do everything the account can, and revoking it locks out a human too.

Either way the secret is encrypted with the master key before it is stored,
and is never returned by any API read — the UI shows it as set, with a replace
affordance, and never shows the value again.

## 27.2 The privileges, and what each one buys

The portal names these itself: **Test connection** reports which are missing
and what stops working, so you do not have to keep this table to hand. It is
here to be read before you create the role rather than after.

| Privilege | Without it |
|---|---|
| `VM.Audit` | no virtual machines at all — the inventory is empty and nothing else matters |
| `Sys.Audit` | no nodes, no cluster health, no networks; the dashboard has no hosts |
| `Datastore.Audit` | no storage pools |
| `VM.Console` | consoles fail to open |
| `VM.PowerMgmt` | start, shut down, reboot and force stop are refused **by Proxmox**, not by the portal |
| `VM.GuestAgent.Audit` | no IP addresses for virtual machines, which reads as a broken guest agent unless you know to look here |

Two notes on the last one. It is the **PVE 9** name; on PVE 8 the same access
is gated behind `VM.Monitor`. And it only affects QEMU guests — container IP
addresses come from a different endpoint covered by `VM.Audit`, so a cluster
missing it typically shows IPs for containers and none for VMs, which is a
confusing symptom until you have seen it once.

Performance history needs no privilege of its own: it reads the same RRD data
`VM.Audit` already covers.

### Why not just use PVEAdmin

Because the portal should not be able to delete your VMs. The privileges above
are read-only apart from `VM.Console` and `VM.PowerMgmt`, so a stolen token
cannot create, destroy or reconfigure anything. That ceiling is enforced by
Proxmox rather than by portal code, which is the only place it is worth
enforcing (docs/15-security-design.md §15.4).

## 27.3 Create the credential on Proxmox

On any cluster node, as root:

```bash
pveum role add ProxUI --privs "VM.Audit,Sys.Audit,Datastore.Audit,VM.Console,VM.PowerMgmt,VM.GuestAgent.Audit"
```

Leave out `VM.PowerMgmt` if you want a portal that can look but not touch, and
`VM.Console` if you do not want browser consoles. Nothing else is optional.

```bash
pveum user add proxui@pve
pveum acl modify / --users proxui@pve --roles ProxUI
pveum user token add proxui@pve portal --privsep 0
```

The last command prints the secret **once**. Copy it now; Proxmox cannot show
it again, and the only recovery is to delete the token and make another.

### The mistake everyone makes: privilege separation

`--privsep 0` is doing real work there. With privilege separation on — which
is the default if you create the token in the web UI and leave the checkbox
alone — the token inherits **nothing** from its user and starts with no
privileges at all. The user has full access, the token has none, and every
call fails with 403 while the account it belongs to plainly works.

If you created the token through the web UI, either untick *Privilege
Separation*, or leave it on and grant the token its own ACL:

```bash
pveum acl modify / --tokens 'proxui@pve!portal' --roles ProxUI
```

Note the quoting: `!` is history expansion in an interactive bash shell.

### Adding a privilege later

Roles are edited in place, so widening one is a single command and the portal
picks it up on its next call — no restart, no re-entering the token:

```bash
pveum role modify ProxUI --privs "VM.Audit,Sys.Audit,Datastore.Audit,VM.Console,VM.PowerMgmt,VM.GuestAgent.Audit"
```

`role modify` **replaces** the privilege list rather than adding to it, so
list everything you want, not just the new one.

## 27.4 Add it in the portal

**Platforms → Add platform**, as an administrator.

| Field | What it wants |
|---|---|
| Name | whatever you call this cluster. It must be unique among live platforms |
| API URL | the cluster API address including the port — `https://pve.example.com:8006`. No trailing path; the portal appends `/api2/json` |
| Token ID | the full identifier, `proxui@pve!portal` — user, realm and token name |
| Token secret | the value printed once at creation. Stored encrypted, never shown again |
| Node filter | optional. Comma-separated node names to sync only part of a cluster; empty means all of it |
| Include templates | whether VM templates appear in the inventory alongside real guests |

### TLS

Four modes, in descending order of how much you should want them:

| Mode | Use when |
|---|---|
| Verify | the cluster has a certificate from a public CA |
| Custom CA | your own internal CA issued it — paste the CA bundle |
| Fingerprint | self-signed, which is the Proxmox default. Pin the SHA-256 fingerprint |
| Insecure | never, if you can avoid it. It is audited, and the connection test warns about it every time |

Fingerprint pinning is the right answer for a stock Proxmox install. It is as
strong as a CA for a single known host, and unlike *Insecure* it still detects
someone standing in the middle. Proxmox shows the fingerprint under
*Node → System → Certificates*, on the `pve-ssl.pem` entry.

## 27.5 Test connection before saving

A new platform will not save until the test comes back both reachable and
authenticated — editing an existing one does not re-require it. The report is
worth reading rather than clicking past. It states, in order:

1. **Reachable** — the portal got a TCP connection and a TLS handshake. If
   this fails, it is a firewall, a wrong port, or a certificate mode that does
   not match reality.
2. **Authenticated** — the token ID and secret are right. Reachable but not
   authenticated is a bad secret, a typo'd token ID, or privilege separation
   (§27.3).
3. **Version and node count** — proof it is talking to the cluster you meant.
4. **Missing privileges** — each one named with what it costs you.

Missing privileges are a **warning, not a failure**. The portal will save and
run without them, with exactly the features in §27.2 switched off. That is
deliberate: a cluster you can only read is a legitimate thing to want.

## 27.6 What happens after you save

- The first inventory sync starts immediately. VMs, hosts, storage and
  networks appear within a minute or so, depending on cluster size.
- Inventory re-syncs every **60 seconds** by default, metrics likewise, both
  adjustable in **Settings → Synchronization**.
- Performance charts need history to draw, so they are sparse for the first
  few minutes and fill in from there. Raw samples are kept 7 days by default
  and rolled up beyond that.
- Guest IP addresses appear only for guests actually running an agent, on top
  of the privilege in §27.2.

Platform-owned fields — name, state, CPU, memory, IPs — are overwritten by
every sync, because the platform is the source of truth for them. Portal tags,
notes and group membership are owned by the portal and are never touched by a
sync. The two sets are disjoint by design and never merged.

## 27.7 Nobody can see it yet

A platform makes VMs visible to **administrators**. Everyone else sees an
empty inventory until you grant access, and this catches people out because
the platform looks broken when it is working exactly as intended.

1. **Users & groups → VM groups** — make a group and put VMs in it.
2. **Users & groups → grants** — give a user group access to that VM group.

Roles say what someone may *do*; grants say what they may do it *to*. An
operator with no grants can perform power actions on nothing, and every VM
answers 404 rather than 403 — the portal does not confirm that a machine
exists to someone not entitled to know.

## 27.8 When it does not work

| What you see | Cause | Fix |
|---|---|---|
| Test says reachable, not authenticated | wrong secret, wrong token ID, or privilege separation left on | §27.3 |
| Test passes, inventory stays empty | `VM.Audit` missing, or a node filter naming nodes that do not exist | check the filter spelling against `pvecm nodes` |
| Hosts and networks missing, VMs present | `Sys.Audit` missing | widen the role |
| Storage tab empty | `Datastore.Audit` missing | widen the role |
| Containers show IP addresses, VMs do not | `VM.GuestAgent.Audit` missing (PVE 9) or `VM.Monitor` (PVE 8) — or the guests simply have no agent installed | check the role first, then the guests |
| "The platform credential is not allowed to perform power actions." | `VM.PowerMgmt` missing. The portal's request reached Proxmox and Proxmox refused it | widen the role; no portal restart needed |
| Consoles fail with 4004 | `VM.Console` missing, or the node is unreachable | see [docs/24-runbooks.md](24-runbooks.md) |
| Certificate errors after a cluster rebuild | a pinned fingerprint no longer matches | update the fingerprint on the platform |

The distinction worth internalising: an error naming the **platform**
(`platform.permission_denied`, `platform.unreachable`) means the portal
reached Proxmox and Proxmox said no. Nothing in the portal will fix it. An
error naming the **portal** means the portal decided, and Users & groups is
where you go.

## 27.9 Checking it from the outside

If you would rather confirm the token yourself than trust the report:

```bash
curl -sk -H "Authorization: PVEAPIToken=proxui@pve!portal=YOUR-SECRET" \
  https://pve.example.com:8006/api2/json/access/permissions
```

That returns the effective privilege map the portal reads, keyed by path. It
is the same call **Test connection** makes, and an empty `/` entry is the
privilege-separation problem in §27.3 wearing a different hat.
