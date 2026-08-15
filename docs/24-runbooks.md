# 24. Runbooks

Operational procedures, written for someone who did not build this and is
reading at an inconvenient hour. Each one states how to tell whether it
applies, what to do, and how to know it worked.

## 24.1 Backup and restore

### Taking a backup

```bash
PROXUI_DATABASE_URL=postgres://proxui:...@db:5432/proxui \
  BACKUP_DIR=/var/backups/proxui scripts/backup.sh
```

Two things must be kept, and they must be kept **apart**:

| What | Where | Why |
|---|---|---|
| the database dump | backup storage | inventory, users, audit trail, encrypted credentials |
| `PROXUI_MASTER_KEY` | a secret store, not the backup directory | without it the credentials in the dump cannot be decrypted |

A dump and the key that opens it, sitting in the same archive, is one stolen
backup away from every platform credential you own.

**The dump client must match the server major version.** `backup.sh` refuses
to run otherwise. A PostgreSQL 17 client writes `SET transaction_timeout`,
which a 16 server rejects on restore — the dump succeeds and the restore
fails, which is the worst place to discover it.

### Restoring

```bash
PROXUI_DATABASE_URL=postgres://proxui:...@db:5432/proxui \
  scripts/restore.sh /var/backups/proxui/proxui-20260814T044951Z.dump
```

The script refuses a non-empty target unless `FORCE=1`, verifies the
checksum, and checks the restore afterwards.

**TimescaleDB is why this is a script and not a `pg_restore` one-liner.** A
plain restore produces a portal that looks healthy — it logs in, the
inventory is there, the audit trail is intact — and has silently lost every
metric, because the hypertables come back as ordinary tables with broken
triggers. The first restore drill produced exactly that: 109 ignored errors
and charts returning HTTP 500 while the rest of the portal appeared fine.

The verification at the end of `restore.sh` exists because of that failure.
It fails loudly if the hypertables or continuous aggregates are missing,
rather than leaving a database that only looks restored.

### Restore drill

Run this quarterly, and after any change to the schema or the scripts.

```bash
# 1. take a backup
BACKUP_DIR=/tmp/drill PROXUI_DATABASE_URL=$PROD_URL scripts/backup.sh

# 2. restore into a scratch database
createdb proxui_drill
PROXUI_DATABASE_URL=postgres://.../proxui_drill scripts/restore.sh /tmp/drill/<dump>

# 3. start the portal against it, on a spare port
PROXUI_DATABASE_URL=postgres://.../proxui_drill PROXUI_HTTP_ADDR=:8099 \
  ./bin/proxui --role=api

# 4. check what a plain restore silently loses
#    - sign in
#    - open a running VM's performance tab; every range must return points
#    - check the audit log is present
# 5. drop the scratch database
```

**Measured 2026-08-14:** backup 1 s, restore and verify 11 s, full drill
including portal start and metric checks under 2 minutes, on 8,232 metric
samples across 19 VMs (672 KB dump). The design's target is under 30 minutes;
this is comfortably inside it, and the number will grow with retention rather
than with VM count.

## 24.2 A platform stopped synchronizing

**Symptom:** platform health shows `unreachable` or `breaker open`; VM data
is going stale.

1. **Platforms → the platform → Recent synchronizations.** The failed run's
   error is the platform's own words, not ours.
2. Common causes, in the order they actually occur:
   - **credential expired or revoked** — the error mentions authentication.
     Edit the platform, supply a new token, use Test connection before saving.
   - **certificate changed** — the error mentions x509 or the fingerprint.
     A node reinstall changes it. Re-pin the new fingerprint.
   - **node unreachable** — network or the node is down. Nothing to fix in
     the portal.
   - **privileges narrowed** — Test connection reports which privileges are
     missing by name.
3. The circuit breaker suspends polling after three consecutive failures and
   retries on its own. **Sync now bypasses the wait**, which is what to press
   after fixing the cause rather than waiting out the cooldown.

## 24.3 Consoles will not open

**Symptom:** the console page spins, or closes immediately.

| What you see | Cause | Fix |
|---|---|---|
| "closed before it was established (code 1006)" | something between browser and portal is filtering WebSockets | check the reverse proxy forwards `Upgrade`/`Connection` and does not buffer |
| "This VM is not running" (409) | exactly that | start the VM |
| "not permitted" (403) | the account has no grant covering this VM | Users & groups → grants |
| "platform console unavailable" (4004) | the node refused or dropped the console | check the node is up and the token has `VM.Console`. If it fails for *every* container while virtual machines work, the connector is sending a QEMU-only option to LXC — that was a real bug, fixed, and the shape to recognize |
| black screen, connected | the guest is not producing video | not a portal fault; check the guest |

The portal answers the platform's RFB handshake itself (ADR 0002), so a
console failure is never a browser credential problem — the browser holds no
platform secret to be wrong.

## 24.4 Power actions are refused

**Symptom:** start, shut down, reboot or force stop reports "The platform
credential is not allowed to perform power actions."

The wording is precise and worth reading literally: the portal's request
**reached Proxmox, and Proxmox refused it**. Nothing in the portal will change
that — the account is permitted, the grant is in place, and the API token's
role simply lacks `VM.PowerMgmt`. The stock recommendation of `PVEAuditor` +
`VM.Console` does not include it.

Find what the token is actually bound to — do not assume a role name:

```bash
pveum acl list
```

If it names a role you created, widen it with `pveum role modify`. If it names
a built-in role (`PVEAuditor`, `PVEVMUser`, anything starting `PVE`), you
cannot edit it; add a second role alongside, since ACL entries are cumulative:

```bash
pveum role add ProxUIPower --privs "VM.PowerMgmt"
pveum acl modify / --tokens 'proxui@pve!portal' --roles ProxUIPower
```

Full detail in [docs/27 §27.3](27-adding-a-platform.md). The next action picks
it up with no restart and no re-entering the token.

Any other message means the portal decided, not the platform:

| What you see | Cause |
|---|---|
| the buttons are not there at all | the account is neither an administrator nor an operator |
| "This VM is no longer visible to your account." | no grant covers it, or it was removed from the platform |
| "Too many actions." | the rate limit; wait and retry |
| the platform's own words, e.g. "CT 126 already running" | the request reached Proxmox and it declined — a state conflict, or a config lock held by another task. Not a portal fault and not worth retrying |
| the state never changes, no error | the platform accepted the task and then failed it — check the node's task log |

## 24.5 The portal is unreachable after an edge change

**Symptom:** a published-app change was made and now `vm.example.com` — the
portal itself — does not answer. You cannot use the portal to fix the portal.

This is the failure the whole published-apps design is arranged around, so it
should be prevented rather than met. But prevention has a gap worth knowing:
`PROXUI_PUBLIC_HOSTNAME` is what identifies the portal's own rule, and with it
unset the guard is off.

**First, check whether the portal is actually down**, or only its route is.
The portal answers on its own address regardless of any tunnel:

```bash
curl -s http://<portal-lan-address>:8080/healthz
```

`{"status":"ok"}` means the process is fine and only the routing is wrong.

### Restoring the routing table without the portal

The snapshot is in the portal's database, and the portal does not need to be
reachable from outside to read it:

```bash
psql "$PROXUI_DATABASE_URL" -Atc "
  SELECT ingress FROM edge_config_snapshots
  ORDER BY taken_at DESC LIMIT 1;" > /tmp/ingress.json
```

Then put it back with Cloudflare's API directly — no portal involved:

```bash
curl -X PUT \
  "https://api.cloudflare.com/client/v4/accounts/$ACCOUNT_ID/cfd_tunnel/$TUNNEL_ID/configurations" \
  -H "Authorization: Bearer $CF_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"config\":{\"ingress\":$(cat /tmp/ingress.json)}}"
```

The token is the one registered with the provider; it is sealed in the
database, so use a token from your password manager rather than trying to
decrypt that one.

**If there is no snapshot**, the Cloudflare dashboard still works —
Zero Trust → Networks → Tunnels → the tunnel → Public Hostnames. Re-add the
portal's hostname pointing at its address and port. That is the same fix, done
by hand.

### The rule that matters

Whatever else is in the table, this one has to be there and has to sit before
any catch-all:

```
hostname: <your portal hostname>    service: http://<portal-address>:8080
```

and the last entry must be a catch-all with no hostname, conventionally
`http_status:404`. A table with no catch-all, or with rules after it, breaks
everything else on that tunnel too.

### Afterwards

Set `PROXUI_PUBLIC_HOSTNAME` if it was not set. Without it the portal cannot
recognise its own rule and every guard that protects it is inert — which is
almost certainly how the change got through.

## 24.6 Google sign-in

Setting it up, and what Google will not accept, is
[docs/26-google-sign-in.md](26-google-sign-in.md). The short version: Google
refuses a redirect URI on an IP address, on plain HTTP, or on a domain
without a public suffix — so a portal reached at `http://10.x.x.x:8080` or at
`something.vm` cannot use it until it has a real name and a certificate.

## 24.7 Notifications are not arriving

1. **Notifications → Deliveries.** If entries are `failed`, the reason is
   recorded verbatim from the channel.
2. If there are **no entries at all**, nothing was routed. Check
   Notifications → Routing has a rule whose category and minimum severity
   match the events you expect.
3. **Send test** on the channel isolates channel configuration from routing.
4. Delivery is retried three times, but a misconfigured channel fails
   immediately and permanently — a missing webhook URL fails identically
   every time, and the log says so rather than retrying into the same wall.

## 24.8 Alerts are noisy, or silent

- **Too noisy:** raise the sustained duration so a spike stops qualifying, or
  lengthen the cooldown. A cooldown of zero means *never repeat*, which is a
  legitimate setting for a rule you only want to hear about once.
- **Silent when it should not be:** check the rule is enabled, that the VM is
  in the rule's group scope, and that the VM has reported within the last
  three minutes — a VM that stopped reporting drops out of evaluation rather
  than holding an alert open on stale data.
- **Fires and never resolves:** the metric is still breaching. The firing
  list shows the last value the evaluator saw.

## 24.9 Losing the master key

There is no recovery. `PROXUI_MASTER_KEY` decrypts platform credentials and
notification secrets; nothing else can.

If it is lost:

1. The portal still starts, and inventory, users and audit are unaffected.
2. Every platform will fail to authenticate, with errors that look like a
   wrong password against settings that look correct.
3. Re-enter each platform's credential through the UI. Test connection
   confirms each one.
4. Re-enter notification channel secrets the same way.

Losing the key costs an afternoon of re-entering credentials. Losing the
key *and* the database costs the estate's history. Back up both, separately.

## 24.10 Upgrading

1. Take a backup. Migrations are forward-only.
2. Deploy the new image. Migrations run on API start under an advisory lock,
   so several API replicas starting at once is safe.
3. Watch the log for `migrations applied` and the schema version.
4. `/readyz` reports ready only once migrations have applied.

Rolling back a release whose migration added a column is safe: the old binary
ignores it. Rolling back across a migration that *removed* something is not —
which is why breaking changes are expand-and-contract, in two releases.
