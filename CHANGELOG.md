# Changelog

## Unreleased

- **Container apps** (APP-01…APP-06,
  [ADR 0012](docs/adr/0012-the-portal-runs-a-vetted-catalogue-on-a-node.md)).
  590 applications you can install into an LXC container from the portal —
  the community Proxmox VE Helper-Scripts, which are how most people put an
  application on a Proxmox host and which otherwise mean copy-pasting a command
  from a website into a root shell.

  The portal already owned everything around that act: it knows the nodes and
  their addresses, holds a pinned key to each, and shows the resulting container
  in the inventory with its console, terminal and power controls already
  working. None of that needed anything new, because a guest's type has always
  been data threaded through the connector rather than a branch.

  **This is the largest thing the portal does to a node, and it needed an ADR
  before any code.** Every ADR before it held node access to one fixed command;
  ADR 0011 explicitly refused to let a request name a package because that would
  become "remote package execution as a service". This is that and more. What
  bounds it: a request names a **catalogue identifier**, never a command, a URL
  or a package; all 590 scripts are **vendored in the binary** at a reviewed
  commit, so the portal does not fetch the thing it is about to run; **both
  upstream repositories are pinned to commits**, so what the script pulls in is
  fixed rather than whatever was on a branch that morning; and the dialog shows
  the exact command before it runs. Admin-only, audited as `container.deploy`,
  and the node's host key must already be pinned.

  What it does not claim, and the ADR says so plainly: pinned is not verified.
  The engine and the in-container installer are fetched from those pinned
  commits while the script runs, so ProxUI has fixed *which* bytes will run
  without having seen them. Vendoring the engine too is the named next step.

  Two things fell out of it. The scripts are vendored as **plain files rather
  than an archive**, because the whole compensating control is that a pin bump
  is a diff somebody reads, and a tarball's diff says only that some bytes
  changed. And the deployment view is the **first place in the portal to show
  the output of a non-interactive command run on a node** — everything else
  reports a state and a sentence, which cannot explain a fifteen-minute script
  that stopped two thirds of the way through. The transcript lives on the node,
  so it survives a portal restart and the install keeps going through one.

  Deploy only. No update, no remove: the portal records what it deployed and
  does not pretend to own the container afterwards.

- **A guest built with the portal's key was told to install the portal's key**
  (SSH-15). Provisioning offers to put the public half into cloud-init, and it
  works — the key is on the guest and authenticates — but nothing recorded it,
  so the SSH page said *the portal's key is not installed for this account here
  yet* about a key that was installed and working.

  The record was only ever written by the portal's own **Install portal key**
  button, which made it a record of what the portal did rather than of what is
  true. Every other way the key reaches a guest — cloud-init on a new guest, a
  golden image, somebody's configuration management — left it silent.

  Three changes, and the middle one is the load-bearing one. A guest provisioned
  with the portal's key is recorded once it appears in the inventory, matched by
  key rather than by the trailing comment. **A connect that authenticates with
  the portal key records it too**, which makes the whole thing self-correcting:
  getting in is better proof than a row saying somebody once put it there, and
  guests provisioned before this change fix themselves the first time anyone
  uses the key. And where there is no record the form now says it has none
  rather than asserting the key is absent — the guest's `authorized_keys` is the
  authority, and the portal cannot see it without connecting.

- **A guest that never boots is no longer reported as a clean success**
  (PROV-16). Provisioning gained a `verifying` step between `starting` and
  `ready`: once the portal has started a guest it waits, up to six minutes, for
  the guest's agent to answer.

  That answer is the only signal separating *the machine came up* from *the
  platform accepted every call*. An AlmaLinux 10 template built for a processor
  the guest was not given panicked before `init`, and behind that panic the
  clone had succeeded, the configuration had been accepted, the start task had
  finished and the platform reported the guest as running. The request said
  `ready`. Nothing anywhere said the machine was dead.

  A guest that never answers still reaches `ready` — it exists, and was created
  exactly as asked; the portal is in no position to call a quiet machine a
  failed one — but the request now carries the note, naming the three things
  that cause it, including the one that cost an afternoon. The deadline lives on
  the row, so a portal restarted mid-wait resumes the same wait rather than
  starting a new one.

  While it waits, the status panel says so in words rather than showing a state
  name. And a note on a finished request stopped being red: a template built
  without a guest agent and a guest that never answered are both worth reading
  and neither is a failure.

- **An AlmaLinux template produced guests that never booted**, and the symptom
  was a guest reporting no address — which reads as "the agent is not
  configured" and sent me looking at the agent for an hour.

  It was the processor. Proxmox's API default is `kvm64`, the plain x86-64
  baseline with no SSE4.2 and no POPCNT. RHEL 10 and everything rebuilt from it
  — AlmaLinux 10, Rocky 10 — are compiled for **x86-64-v3**, so glibc aborts
  before `init` runs:

  ```
  Fatal glibc error: CPU does not support x86-64-v3
  Kernel panic - not syncing: Attempted to kill init!
  ```

  Nothing above the serial console can see that. There is no agent, no address,
  no log inside the guest to read, and every layer the portal can reach reports
  success: the template built, the clone succeeded, the guest is "running".

  The baseline is a property of the image, so that is where it lives. The
  shipped catalogue records `cpu` for the entries that need it, the build form
  sends it and says so beside the image, and `POST /platforms/{id}/templates`
  takes `cpu` for an image the portal does not ship. Guests otherwise get
  `x86-64-v2-AES`, which is Proxmox's own default for a new guest and a strict
  improvement on `kvm64`. It is deliberately not v3 everywhere: v3 needs
  Haswell or Zen and would break both older nodes and migration between unlike
  ones.

  Verified against a live cluster: template built with `cpu: x86-64-v3`, guest
  cloned from it, agent up and an address in the inventory about two minutes
  later. The two minutes are the guest running its own SELinux relabel and
  rebooting, which is also why nothing needed to change in image preparation —
  `--selinux-relabel` was tried there first, made no difference, and was
  removed rather than left in claiming a fix it did not make.

- **The look is a choice now.** Appearance has two independent axes in the
  account menu: **Mode** (light, dark, or whatever the machine says) and
  **Theme** — **Slate**, the dnsprox palette with IBM Plex, or **Classic**, the
  original blue and slate with the system typeface. Each theme defines every
  token in both modes, so picking a palette never decides light-or-dark and
  picking dark never decides a palette. The typeface is part of the theme, so
  Classic gets its old system stack back rather than keeping Plex.

  Two things about four token blocks are silently fragile, and both are tested
  now rather than commented. A block that forgets a token does not fail — it
  inherits the other theme's value, which is how one palette's teal ends up
  inside the other's blue. And the block order is load-bearing, because
  `[data-theme]` and `.dark` have equal specificity: a `.dark` rule appended
  below the themes would quietly take dark mode away from Classic, and nothing
  but the screen would say so.

- **The portal looks like dnsprox now** — same palette, same typography, same
  shape of shell. Two tools run by the same team out of the same rack should
  not look like two products, and the one that was already right about this was
  the other one.

  IBM Plex Sans and Mono, bundled through `@fontsource` rather than linked from
  a font CDN: a portal somebody runs on their own network should not need Google
  to render, and an air-gapped install would otherwise sit in a fallback face.
  Latin subsets only — six files and 140 kB, against 68 and a megabyte for the
  whole family.

  The palette is a cool slate ground with a single teal accent. Two things it
  brings that the old one did not: the page, a panel on it and a fill inside a
  panel are now three distinguishable grounds rather than two that were a shade
  apart, so a hover is always exactly one step of separation from whatever it
  sits on — and there is a flat `accent-wash` for a selection, which an accent
  at ten percent could never be, because an alpha tint picks up whatever is
  behind it.

  Making that scale mean something exposed three floating panels painted in the
  page colour. It never showed while the two shades were a shade apart; on the
  new palette a dialog in the page colour reads as a hole rather than as
  something on top of the page. The drawer, the password dialog and the
  two-step dialog are on the raised surface now.

  **The shell is one sidebar** instead of a top bar and a nav. It carries the
  portal's name, the navigation, and the account menu at its foot, and it
  collapses to an icon rail as before. The eleven destinations are grouped
  under foldable headings — Infrastructure, Platform, Administration — because
  the list had grown past the point where anybody scans it and nothing but
  position said "Storage" and "Settings" were unrelated. A folded heading is
  remembered; the icon rail ignores the fold, since a rail has no heading to
  unfold and hiding links there would leave no way back to them.

  **And there is a phone layout at last.** The sidebar was previously always on
  screen, taking a third of a narrow one; it is a drawer behind a ☰ now, and it
  closes itself on navigation rather than sitting over the page the tap just
  loaded.

- **A platform can now say what its nodes are missing, and install it**
  (NODE-01…NODE-05, [ADR 0011](docs/adr/0011-the-portal-can-install-what-it-needs-on-a-node.md)).
  A template built on `cx1` came out with no guest agent because only `pve` had
  `libguestfs-tools`. Nothing failed — the build finished, the template worked,
  and the request recorded exactly which package to install on exactly which
  node. It still cost a round trip, because that sentence was written where
  nobody would read it.

  That is the shape every one of these has: `lm-sensors` missing means an empty
  temperature chart, `libguestfs-tools` missing means guests with no address, and
  the portal's key missing means neither works at all. None of them is an error.
  Each is found weeks later by somebody noticing an absence.

  **Platforms → the platform → Readiness → Check** now asks every node what it
  has, in one SSH handshake each, and offers *Install* beside the two the portal
  can fix. On demand from a button, never on page load: it changes about once a
  year. A node that would not let the portal in reports why — no key, changed
  host key, port 22 shut — and reports **no** prerequisites, because unknown is
  not the same as missing.

  Installing is admin-only, confirmed, and audited as `node.install` naming the
  node and the packages. **The request names an identifier, never a package**:
  the server maps it to a command compiled into the binary and refuses one it
  does not recognise, which is what keeps this inside ADR 0007's boundary while
  crossing the part of it that was drawn around reading. The command is shown in
  full before it runs, because the portal is asking to run it as root on a
  hypervisor.

  Two things it deliberately will not fix, and says so with the exact command or
  privilege: **the portal's own SSH key**, whose installation needs the access it
  grants, and **the credential's privileges**, which are widened on the cluster.

  Two gaps closed on the way past. `POST /platforms/test` was building the
  provisioning and template capability report and dropping it from the response,
  so the separate reporting ADR 0010 added had never been visible — it is
  returned now, and the readiness report carries it for a platform that is
  already saved, which the test endpoint cannot do because the credential is
  write-only. And checking pins an unmet node's host key, which fixes a case
  nothing else could reach: the sensor collector pins only after a node answers
  `sensors -j`, so a node without lm-sensors was never pinned at all — and
  installing lm-sensors is what needs the pin.

- **Create VM moved to the inventory.** It used to live inside a platform's
  drawer, which meant deciding which cluster you wanted before you could ask for
  a machine at all. The button is now on the inventory page, where somebody who
  wants a VM is already looking, and the form asks for the platform as its first
  question.

  It also offers two things it should have from the start: the **VM group** to
  file the new guest into — the API has accepted that since provisioning shipped
  and nothing sent it, so every guest created through the portal was visible to
  administrators only — and the **target node**, defaulting to the template's
  own, which is the only one a linked clone can use.

  Building a template stays on the platform page. It is about that cluster's
  nodes and storage, and it is configuration rather than day-to-day work.

- **Templates are prepared before they are sealed** (PROV-14, PROV-15). A
  published cloud image carries no `qemu-guest-agent`, so every guest built from
  one reported no IP address — which is exactly what left the portal unable to
  offer it an SSH session, since it had nowhere to connect. The build now
  installs the agent into the disk before converting it, and clears the identity
  a clone must not inherit: machine-id, SSH host keys, and cloud-init's record of
  having run. Without that reset every clone of a template shares a machine-id,
  which hands them all the same DHCP lease, and presents host keys its siblings
  also have.

  It is done on the node with `virt-customize`, over the SSH channel the portal
  already opens for sensors — two fixed commands, no shell, the portal's own key
  (ADR 0007). **A node needs `libguestfs-tools`.** Without it the template still
  builds and works; the request records that its guests will report no address
  and names the node to install it on.

  The service is deliberately not enabled: on Debian and its derivatives the
  package ships a udev rule that starts the agent when the virtio port appears,
  which the template has. `systemctl enable` there reports success, creates no
  symlink, and would only mislead the next person to read it.

- **Fixed: node host keys were never actually pinned.** `Pin` used
  `ON CONFLICT DO NOTHING`, and a node begins life with no portal key installed
  — so the first poll fails, a placeholder row is written on purpose (it is what
  the host page shows for "tried and refused"), and every later pin then
  conflicted with that row and was discarded in silence. The `fingerprint`
  column stayed empty forever, and ADR 0007's guarantee that a node cannot be
  swapped underneath a portal that has already met it did not hold, because the
  portal never finished meeting it. A pin now fills in a row that carries no key
  and still refuses to overwrite one that does.

- **Favourites and folders on the inventory page** (INV-06…INV-09). The VM list
  was a flat table, and the handful of machines you actually work with were
  scattered through it. Star a VM and it sorts to the top whatever column the
  table is sorted by; file VMs into folders of your own and sort by folder to
  see them grouped, with an "Unfiled" heading at the end.

  Both are **private to you**. Two people can arrange the same VM differently
  and neither sees the other's list — a favourite is an opinion about a machine,
  not a fact about it. They are also deliberately *not* the existing VM groups:
  those are what a user group is granted, so tidying your own view must not be
  able to change who can see what.

  The ordering is done by the database, not the browser. The list is paginated
  fifty at a time, so sorting the page you already have would float a starred VM
  to the top of page 4 and leave page 1 exactly as it was.

  Organising is open to any signed-in role — starring a machine you can already
  see changes nothing but your own list — but only for VMs you can already see,
  so the feature cannot be used to probe for machines behind a grant you do not
  have. Deleting a folder frees its VMs rather than removing them.

  There is also a **Folders view**, switched from the filter bar: a pane listing
  All VMs, Favourites, each folder with its count, and Unfiled, beside the same
  table scoped to whatever you pick. It is where folders can finally be renamed
  and deleted — those endpoints shipped working and unreachable — and deleting
  one says plainly that the machines inside are kept. The selection lives in the
  URL, so a folder can be bookmarked and pasted into a ticket like any other
  filtered view. The flat list stays the default: a tree is the wrong shape for
  searching, where you want matches rather than a hierarchy.

- **Build cloud-init templates from the portal** (PROV-09…PROV-12,
  [ADR 0010](docs/adr/0010-the-portal-can-create-and-destroy-guests.md)).
  Provisioning shipped and the first thing it told you was to go away and do
  something else: *"build one on the cluster first — import a cloud image,
  attach a cloud-init drive, then convert it with `qm template`."* That empty
  state is now a **Build one** button.

  Pick an image from a short shipped list — Debian 12 and 13, Ubuntu 24.04,
  Rocky 10, AlmaLinux 10 — or paste any URL. The node downloads it, imports it
  as a disk, attaches the cloud-init drive and converts the result; the portal
  never streams the image through itself. The guest is created with what a
  cloud image actually needs, including a serial console, which is the usual
  reason a hand-built template boots to a blank screen.

  A checksum is required. The catalogue does not bundle digests — a point
  release moves and a stale digest that gets skipped teaches people to skip —
  so it links the distribution's own checksum file instead. Building without
  verification is possible, has to be asked for deliberately, and writes an
  audit entry naming who asked and which image, because this file becomes the
  ancestor of every guest cloned from it.

  A guest that becomes a template now leaves the VM list as a conversion. For
  the half-minute before it is converted it is a real guest, so a sync can file
  it as one — and it would then have been reported **missing** for three cycles,
  which is the word for a machine that vanished on its own. The synchronization
  now asks the platform which guests are templates, so it can tell the two
  apart: the row is closed out in a single run and the history says
  `converted_to_template`. This also covers a template converted by hand
  outside the portal, which produced the same misleading "missing" before.

  An image already on the storage is not fetched again, and template-building
  privileges are reported separately from provisioning ones: cloning from a
  template someone else built needs strictly less than building one. The four
  extra privileges are `Datastore.AllocateTemplate`, `Sys.AccessNetwork`,
  `VM.Config.CDROM` and `VM.Config.HWType` — deliberately **not** `Sys.Modify`,
  which Proxmox also accepts but which permits reconfiguring the node.

- **Create guests from cloud-init templates, and destroy them**
  (PROV-01…PROV-08, [ADR 0010](docs/adr/0010-the-portal-can-create-and-destroy-guests.md)).
  Everything about a guest's life already lived in the portal — who may see it,
  its console, its history, the portal's own SSH key on it — except the moment
  it came into existence. An administrator can now pick a template, name the
  machine, size it, put it on a bridge, and have it built and booted.

  Access is by SSH key only. There is no password field in the form, the API,
  or the type the connector takes: cloud-init receives a user name and public
  keys, and no guest credential is ever carried by the portal. The portal's own
  key is offered first, so the browser terminal and file browser reach the new
  machine immediately.

  A request is a durable record, not a request handler: cloning runs for
  minutes, and a portal restarted halfway through picks the work back up rather
  than leaving a half-made guest that nothing is watching. A step that fails
  leaves the partial guest alone and says which step failed — the portal will
  not destroy a machine on the strength of an error it may have misread.

  Destroying is admin-only, requires typing the guest's name (checked on the
  server, not just in the browser), and refuses templates outright.

  **This changes the portal's security posture, and the ADR says how.**
  `docs/15` §15.4 used to state that "the portal physically cannot create or
  delete VMs" — a guarantee enforced by the platform token rather than by
  portal code, which meant it held whatever the code did. Widening that token
  removes it for every path, not just this one. The privileges are optional per
  platform: a token that was never widened syncs exactly as before and simply
  cannot provision, and "Test connection" now says which privileges
  provisioning would need.

  Also fixed on the way past: the `include_templates` option had been in the
  Proxmox connector's config form since it was written and was read nowhere.

- **Fixed: one node going down blanked the whole portal**
  (PLAT-01, PLAT-05, [ADR 0009](docs/adr/0009-a-platform-is-reached-through-any-cluster-member.md)).
  A platform was configured with one API address and reached through only that
  one. When the node behind it went down on 4 September the portal lost
  inventory, metrics, history and consoles for the entire cluster — including
  the three nodes that were up and quorate the whole twenty minutes.

  A platform is now configured with one endpoint and reached through a list of
  them: the address you entered stays first, and the other cluster members are
  discovered from the platform itself and used when the first one cannot be
  reached. Failover happens on network failures alone — a rejected token still
  fails immediately and loudly, because it would be rejected everywhere. Under
  a pinned certificate each member carries its own pin, learned from the
  cluster over a connection already verified; a member whose certificate cannot
  be established is never used. The platform drawer lists the addresses in play.

  What this does not fix: a node that is down still shows as online. Host
  status is only ever written from a successful listing, and with failover the
  listing now succeeds — so an absent node is simply absent. Surfacing that is
  a separate change.

- **Fixed: the SSH console stopped letting you connect after a few sessions**
  (SSH-07, [ADR 0008](docs/adr/0008-detached-ssh-sessions-are-reclaimed-early.md)).
  Closing the browser tab did not give the session back, so each terminal
  opened and abandoned held one of the eight slots an operator gets — for the
  full thirty-minute idle timeout. Eight of them and the next connect was a
  429, with nothing to do but wait it out.

  The tab now releases its session on the way out, and the server reclaims a
  session no terminal is attached to after 2 minutes of inactivity rather than
  30, closing it as `abandoned`. The short limit is measured from last
  activity, so a session being used by the file browser alone is untouched.
  The trade is that a WebSocket that drops under a page left open costs the
  password again after two minutes instead of thirty.

- **Performance charts for nodes.** The Hosts page grows a per-node chart board,
  reached by expanding a node beside its sensors: CPU and memory over the usual
  1h–1y windows, and a **Temperature** chart that overlays every sensor the node
  reports — CPU package, cores, NVMe — on one axis, so the hot one is obvious at
  a glance. The server picks the stored resolution and the chart labels it, so a
  five-minute average never reads as per-minute detail.

  Nodes report only CPU and memory through the platform API (there is no
  per-node disk or network series), and the host rollups stop at five minutes,
  so a window wider than the raw retention is answered from the five-minute
  aggregate rather than a coarser table that does not exist. Temperature is the
  data read from the node over SSH (ADR 0007); a node with no key installed
  simply shows no temperature chart. New endpoints: `GET /hosts/{id}/metrics`
  and `GET /hosts/{id}/sensors/history`.

- **A setup guide on each platform.** Getting node readings takes two commands
  on the node, and both of them are specific to the deployment: the portal's
  own public key, and which of your nodes still has neither half installed. So
  the guide lives on the platform beside the sync history rather than only in
  the documentation — it lists each node with either its sensor count or the
  reason it is silent, and hands over the exact `authorized_keys` line with the
  real key already in it. Once every node answers, the steps disappear.

- **Node temperatures** (SENSOR-01…SENSOR-05, [ADR 0007](docs/adr/0007-the-portal-reads-node-sensors-over-ssh.md)).
  The Hosts page grows a temperature column, and every node opens to show each
  sensor it reports — CPU package, per core, NVMe composite, whatever the board
  has — beside the limits the chip declares for itself.

  Proxmox publishes no temperature anywhere in its API. Not at a different
  privilege and not on a newer version: the field does not exist, and the only
  one reachable is a drive's, inside the SMART passthrough. So the portal reads
  it from the node: one SSH connection, one fixed command (`sensors -j`), and
  the portal's own key — the same key pair SSH-11 already gives it. No node
  credential is stored, and revoking the access is deleting one line from one
  `authorized_keys`.

  That crosses a line SSH-02 drew — "the portal is not an SSH proxy to the rest
  of the network" — so the ADR is the argument for why this particular crossing
  is narrow, and the code holds the boundary rather than describing it: node
  connections are made by a path that returns bytes instead of a session, so
  there is no route from one to a terminal, an SFTP browser or a forwarded
  port. The address comes from the platform's own `/cluster/status`, never from
  a request. Host keys are pinned on first sight and a change is refused.

  Readings are stored per sensor rather than reduced to one number, because
  "something is hot" is only useful next to "what". Which one is *hottest* is
  decided by headroom to the chip's own critical point, not by degrees: a VRM
  at 75°C with a 125°C limit is idling, and it must not outrank a package at
  70°C that criticals at 84°C.

  Alert rules take a subject now, and a **node** rule can watch either the
  temperature or the headroom left. Headroom is the portable one — it means the
  same thing across machines whose CPUs disagree about what hot means. Rules
  written before this keep meaning what they meant: no subject is a VM rule.

  Setup is two commands on each node, in [docs/30](docs/30-node-sensors.md).
  A node with nothing installed reports nothing, says which of the two halves
  is missing, and stays quiet about it.

- **Google sign-in returns to the address you signed in at** (AUTH-01,
  ADR 0003). A portal is routinely reachable under more than one name — a LAN name, a public
  one, a tunnel hostname — and the redirect URI was a single configured string,
  so a sign-in started at one name came back at another. Google returns the
  browser to that URI verbatim, so the session landed on a host the person was
  not looking at, and their cookies were not there either.

  The redirect URI now comes from the request that started the sign-in:
  `X-Forwarded-Host` when a proxy rewrote `Host`, otherwise `Host`, with the
  scheme from `X-Forwarded-Proto` — the same signal the cookie's `Secure` flag
  already reads, and for the same reason. It is resolved once, at the start,
  and recorded against the attempt server-side, because the token exchange has
  to send Google the same string the authorize request did and the two happen
  on separate requests.

  **The Redirect URL setting is now optional, and empty is the right answer.**
  A value pins one address and wins over the request, which is what a portal
  behind a proxy whose public address it never sees still needs. Google sign-in
  no longer requires it to be offered at all: a client ID and secret are the
  whole configuration.

  Every address has to be registered with Google, and that is the whole of the
  safety here — a forged `Host` can only name a redirect URI Google already
  knows, and Google refuses anything else before the browser goes anywhere.

- **Fixed: live updates stopped after 30 seconds** (INV-04, SYNC-06). The
  request deadline meant for JSON calls was on the root router, so it also
  reached the three WebSockets. The live event stream watches that context and
  returns when it is cancelled, so every stream died half a minute after it
  opened and the lists it feeds quietly stopped updating until the page was
  reloaded.

  Consoles and terminals were never cut — their relays do not look at the
  request context — but each one that lasted longer than 30 seconds finished by
  writing a 504 into the log and the HTTP metrics, so a working session read as
  a failed request. That is what led here: a journal full of errors for
  terminals nobody had any trouble with.

  The deadline now sits on `/api/v1`, out of reach of anything long-lived.

- **Delete a user** (ADM-04). The users table gets a Delete action beside
  Disable, confirmed by typing the username: the account, its sessions, its
  group memberships and its second factor go, and any SSH session it still had
  open is closed rather than left running until the idle sweep notices.

  What it does not take is the audit trail. Those entries name their actor by a
  string copied at write time and hold no foreign key to `users` — a decision
  the schema made in the first migration — so the record of what an account did
  outlives the account.

  Two deletions are refused, both at the command layer where the rule is
  testable: your own account, which would succeed and then lock you out of the
  portal you were administering, and the last administrator who can still sign
  in, because the only way back from that state is the first-run bootstrap and
  it only runs against an empty portal. A disabled administrator does not count
  towards that check — an account that cannot sign in is not holding the door
  open — and is deletable at any time.

  Deactivation is still the right answer for somebody who has merely left: it
  keeps the account and the name attached to everything it did. Deletion is for
  the account that should not exist — a self-registered stranger, a duplicate,
  a test account.

- **Copy text out of tmux** (SSH-08, docs/29.7b). Full-screen programs — tmux,
  vim, htop — ask the terminal for every click and drag, so a drag selects
  nothing to copy. Shift+drag takes the mouse back on a desktop; a phone has no
  Shift.

  The terminal toolbar gets **Select**, which lays the buffer out as plain text
  over the terminal: select it the way text selects everywhere, or copy the
  whole thing, for the screen alone or with the scrollback behind it. Nothing
  is asked of the guest and nothing running is interrupted — the bytes were
  already in the browser. **Copy** with nothing selected now opens the same
  panel instead of doing nothing, since that is the button somebody presses
  when the drag did not take. Copying still falls back to a textarea on a
  plain-HTTP origin, where there is no clipboard API.

  Selection is line-wise, so one pane of a vertical split copies with the other
  pane's columns beside it — the same thing a native terminal does.

- **An SSH terminal, with a file browser** (docs/29, ADR 0005, SSH-01..10).
  Operators and admins get a real shell on a guest — scrollback, copy and
  paste, resize — instead of driving it through the hypervisor's console, plus
  an SFTP panel beside it: browse, drag-and-drop upload with progress,
  download, mkdir, rename, chmod, delete.

  The credential is typed per session and kept nowhere: not in Postgres, not in
  Redis, not in a log line, not in a response. That is the decision the rest of
  the design falls out of — a session cannot be rebuilt, so the open connection
  lives in the memory of the process that opened it, a restart ends every
  session, and a load-balanced `--role=api` deployment would need session
  affinity (the single-binary default does not).

  Host keys are pinned per VM at the moment an operator confirms the
  fingerprint, as OpenSSH does. A changed key is refused outright and cannot be
  clicked past; clearing a pin is an admin-only endpoint. The portal negotiates
  the host key algorithms OpenSSH prefers, in its order, so the fingerprint it
  shows is the one the operator's own `ssh` shows them.

  The address is chosen from what the platform reported, skipping loopback,
  link-local and container-bridge addresses that would hang rather than fail.
  An operator may name a different one, and it is checked against that same
  list: the portal reaches far more network than a grant over one VM implies.

  Copy and paste degrade to a panel on a plain-HTTP origin, where the browser
  offers no clipboard API at all — the LAN deployment this portal documents.

- **On-screen keys in the SSH terminal** (docs/29 §29.7a, SSH-08): Esc, Tab,
  Ctrl, Alt, arrows, Home/End, PgUp/PgDn, and one-tap `^C ^D ^Z ^L ^R`. A
  phone's keyboard has none of them, which left the browser terminal readable
  but not usable — no history recall, no completion, no way to stop a runaway
  command. The bar defaults on for touch and toggles from the toolbar
  everywhere else.

  Modifiers are sticky rather than held, since a touchscreen cannot chord: Ctrl
  arms, the next keystroke spends it, and an armed modifier shows as pressed.
  Cursor keys are encoded in the form the guest is currently asking for —
  application cursor mode in vim and less — and modifiers travel as a
  parameter, so Ctrl+Left jumps a word instead of printing `^[[D`.

  Fixed: the bar did not appear on the phone it exists for. It is the last row
  of a full-height page, and the SSH page was still sized in `100vh` — the
  height with the browser's own bars hidden — so the bar sat under them on a
  page that does not scroll. Worse, neither `100vh` nor `100dvh` shrinks when
  the soft keyboard opens, which put the bar behind the very keyboard it is
  there to complete. Both pages now measure `visualViewport` and size
  themselves to what is actually on screen, and the console's key strip was
  hidden the same way.

- **Live VM state on page load** (docs/10 §10.6). The VM list and detail pages
  now ask the platform for current power state instead of showing what the last
  sync found up to a minute ago — so the Shut down and Reboot buttons reflect
  the machine as it is, and a state change appears in seconds rather than at
  the next sync tick.

  It is an overlay, not a second copy of the inventory: the live read never
  writes to `vms`. The reconciler decides what changed by diffing that table,
  and a live read that updated it first would leave nothing to diff — silently
  ending the `vm.state_changed` events that history and notifications are built
  on. Only state and uptime are overlaid; names, tags and whether a VM has gone
  missing stay the sync's judgement.

  On Proxmox a read is one `/cluster/resources` call for the whole cluster,
  coalesced across concurrent viewers, reused for 3 s, bounded at 2.5 s, and
  skipped for a platform whose breaker is open. A cluster that is down costs
  one attempt per interval, not one per page load, and every failure falls back
  to the synced row — the behaviour the portal had before. A power action drops
  the cached snapshot for its platform so the next page is a real read.

  `sync.live_reads` in Settings turns it off without a redeploy, and fails open.

- **Fixed a data race in the SSH terminal.** The stdout/stderr merge pipe was
  built lazily on first read, under one `sync.Once`, while Close read the same
  fields under another — two Onces guarding the same fields exclude nothing. A
  browser closing its socket during the first read could see a half-written
  pipe writer and then never close it, stranding the copy goroutines until the
  session sweep. The pipe is built at construction now. Found by `-race`, and
  it reproduced in two runs out of six.

- **Two-step verification** (AUTH-04, docs/15 §15.1), the TOTP the design has
  specified since day one and nothing implemented.

  RFC 6238, six digits, thirty seconds, written against the RFC's own published
  test vectors rather than taken from a dependency — the vectors are the reason
  it can be trusted, and they are in the test file. Any authenticator app
  works; the QR code is drawn in the browser from the otpauth URL, so the seed
  reaches no other origin, and the key is shown as text beside it for anything
  that cannot scan.

  What makes it worth having is the closed failure modes, each with a test:

  - **A code works exactly once.** A correct code stays arithmetically correct
    for up to ninety seconds across the accepted window, so the step it matched
    is recorded and a repeat is refused — one glance over a shoulder is not a
    sign-in.
  - **The password alone mints nothing.** A challenged login returns no token,
    no cookie, and records no success; the session, the cleared failure count
    and the last-login stamp all wait for the code.
  - **Guessing is bounded.** Five attempts per challenge, the challenge dies
    with the fifth, and it expires after five minutes regardless. The verify
    endpoint carries login's rate limit.
  - **Enrolment is inert until confirmed.** A badly scanned QR is a retry, not
    an account locked out of its own portal.
  - **Turning it off costs the password**, or an unlocked screen would be
    enough to remove it.
  - **An account disabled mid-sign-in does not get in** — the checks run again
    at the code step, not only at the password.

  The seed is sealed with the same envelope encryption as a platform
  credential. Migration 00018 replaces the single `totp_secret_enc` column the
  original schema reserved, which could never have held a wrapped data key and
  was never written to.

  Admins get the lost-phone path: `DELETE /users/{id}/totp`, audited against
  both accounts, with a 2FA badge in the user list showing who has one.

- **The portal can hold one SSH key of its own** (docs/29.8a, ADR 0006,
  SSH-11..14), so connecting stops meaning typing a password every time.

  Ed25519, generated by the portal, private half sealed with the same envelope
  encryption as a platform credential and returned by nothing. An operator
  connects with a password once, clicks Install portal key, and the public half
  goes into that account's `authorized_keys` over the session they already
  authenticated — with that account's permissions, so the portal gains no reach
  it was not given. The line can also be copied out of Settings and pasted into
  cloud-init for a guest nobody has a password on.

  ADR 0005 is amended, not overturned: a guest password is still used once and
  dropped. What is stored is the portal's own credential, whose blast radius is
  the accounts somebody deliberately installed it into, and whose revocation is
  deleting one line — offered in the same toolbar that installed it.

  Rotating replaces the single pair and invalidates every install at once; the
  confirmation says so and names the count, and the installs left behind are
  listed as stale rather than quietly relabelled. Generating, rotating,
  deleting and the estate-wide install list are admin-only; reading the public
  key is not, because pasting it is a supported install path.

  Connecting with it sends `use_portal_key: true` and no key material, and
  every session open and denial now records which method was used.

  One bug worth naming, caught by the end-to-end test rather than by review:
  the install first opened `authorized_keys` with the SFTP append flag and
  trusted the server to honour it. A server that ignores the flag takes the
  client's offset — zero — and overwrites from the start, erasing every key
  already in the file. It seeks to the end now, and two tests pin it.

- **A Published apps panel** (docs/28 P5), admin only. Connect a Cloudflare
  account, see the tunnel's live routing table with each rule joined to the VM
  behind it, and publish a service by picking a VM and a port. Rules the portal
  did not create are marked and read-only; the rule serving the portal is
  labelled; a rule pointing at an address no VM holds is flagged.
- **Publishing an app through a Cloudflare Tunnel** (docs/28 P4). Creates the
  routing rule and the DNS record together, and rolls the rule back if the DNS
  write fails — ingress first, because a rule with no DNS record is invisible
  while a DNS record with no rule serves error 1033 to everyone. Unpublishing
  removes only the DNS record the portal created. Every write goes through the
  P3 rails: fresh read, staleness check, whole-table validation, snapshot.
- **Safety rails for edge changes, before anything can write** (docs/28 P3).
  `POST /{id}/snapshot` stores the current routing table verbatim, and
  `POST /{id}/preview` says exactly what a proposed table would add, remove,
  change or reorder — and refuses it outright if it would delete or shadow the
  rule serving this portal. Staleness is detected with the provider's own
  version counter rather than by guessing. New `PROXUI_PUBLIC_HOSTNAME` tells
  the portal which rule is its own; without it that guard is inert.
- **A tunnel's routing table can be read, joined to the VM inventory** (docs/28
  P2). Each rule is annotated with the VM holding its target address, whether
  it points at an address no VM holds any more, and whether it is the rule
  serving this portal. Still read-only.
- **Cloudflare Tunnel providers can be registered** (ADR 0004, docs/28 P1).
  Admin-only, read-only: register an account credential, test it, list its
  tunnels. The token is sealed with the same envelope encryption as a platform
  credential and is never returned by any read. Nothing here can change a
  tunnel; publishing lands in a later sprint behind the invariants in
  `internal/domain/publish`.
- **The layer import rule is now enforced** rather than only documented. It
  found eleven existing `app -> infra` violations, frozen as a ratchet that can
  only shrink.

- **The refresh cookie's `Secure` flag now follows the request.** It was
  decided once at boot, so a portal served over HTTPS with
  `PROXUI_SECURE_COOKIES=false` — the setting that exists so a plain-HTTP LAN
  address can be signed into at all — sent its refresh cookie without the
  flag. The configured value is now a floor: a request that arrived over TLS
  gets `Secure` whatever the setting says, and one deployment can serve both
  addresses correctly. Clearing the cookie makes the same decision, or the
  browser keeps the cookie it was told to drop.
- **A platform that answers is no longer reported as unreachable.** Any
  upstream 5xx was classified `ErrUnreachable` — documented as a network
  failure, and retryable — with the platform's own explanation discarded in
  favour of the status code. So Proxmox declining an operation ("VM is already
  running", "config lock held") read as "The platform could not be reached",
  sending an operator to debug a healthy network while the sync engine retried
  a call that could never succeed. A new `connector.ErrRefused` covers reached
  and declined: 409 rather than 502, no retry, and the platform's message
  passed through verbatim.
- **Power actions in the UI.** Start, shut down, reboot and force stop on the
  VM detail page, for administrators and operators — the API and its audit
  trail have existed since sprint 10 but nothing called them. Everything that
  interrupts service confirms first, and force stop says plainly that it is
  the equivalent of pulling the power lead. The platform answers 202, so the
  page reports that the request was accepted and polls until the state
  actually changes rather than showing a state that has not happened yet.
  Requires `VM.PowerMgmt` on the platform token; without it the portal reports
  that the credential was refused, and by whom.
- **The console works on a phone.** A **Keyboard** button summons the
  on-screen keyboard and translates what it types into RFB key events, with a
  strip of the keys a phone keyboard has no room for — Esc, Tab, Enter,
  Backspace and the arrows — which a console is close to unusable without. The
  toolbar scrolls sideways instead of stacking, the clipboard panel takes the
  full screen below `sm`, and the page is sized in `dvh` so the browser's own
  bars no longer cut off the bottom of the console.
- **Copy and paste in the console.** A clipboard panel sends text to the
  guest's clipboard and shows what was copied inside the guest. It moves text
  through a textarea rather than syncing silently, because reading the local
  clipboard needs a permission that does not exist outside a secure context —
  which the plain-HTTP LAN deployment is not.

- **Self-registration and Google sign-in** (ADR 0003), both off until switched
  on. Google's client ID, secret and redirect URL are configured in Settings,
  the secret encrypted like a platform credential.
- **A `newuser` role** for accounts that provision themselves. It reaches
  `GET /auth/me` and the password change endpoint, and nothing else — where
  read-only, the previous default, could survey the estate's hosts, storage
  and networks without ever being granted a VM. Such an account lands on a
  page telling it to ask an administrator for access.
- **Live updates now actually work in a browser.** `GET /api/v1/events` was a
  WebSocket carrying no credential — a browser cannot put an `Authorization`
  header on one — so it had answered 401 in a reconnect loop since it was
  written. It is replaced by `POST /events/ticket` plus
  `GET /ws/events/{ticketID}`, the same single-use ticket the console uses.
- Branding: portal name, logo and login banner, with the name defaulting to
  the host the portal was reached at.
- Password change in the UI, including the forced change on first sign-in.
- Operators no longer see the platform column or administrative navigation.

## v1.0.0-rc.1 — 2026-08-14

First release candidate. Every sprint in the roadmap is implemented and
verified against a live Proxmox VE 9.2.10 cluster (19 VMs, 4 nodes, 10
storage pools, 13 networks).

### What it does

- **Inventory** synced from Proxmox: VMs, hosts, storage and networks, with
  change history per field and portal-owned tags and notes kept disjoint from
  platform-owned ones.
- **Browser consoles** over a WebSocket bridge. The portal answers the
  platform's RFB handshake itself, so the browser holds no platform secret
  (ADR 0002).
- **Performance history** in TimescaleDB, from one-minute samples to
  three-hourly rollups, over ranges from an hour to a year.
- **Access control**: four roles plus per-VM grants through user groups and
  VM groups.
- **Notifications** to SMTP, Slack and signed webhooks, routed by category,
  severity, platform and VM group.
- **Alerting** on CPU, memory, disk and network thresholds, with sustained
  duration, cooldown and recovery.
- **Administration** entirely in the browser: platforms with connector-declared
  forms and a gating connection test, users and grants, notification channels
  and routing, alert rules, settings, and an audit explorer with CSV export.

### Verified

| Target | Requirement | Measured |
|---|---|---|
| API latency | p95 ≤ 500 ms (NFR-P1) | 3.9–61.7 ms p95 across eight endpoints |
| Chart queries | p95 ≤ 800 ms any range (NFR-P2) | 4.2–6.6 ms p95, 1 h through 1 y |
| Initial bundle | ≤ 1 MB gzipped (NFR-P5) | 103 KB; noVNC and charts load on demand |
| Restore drill | < 30 min (docs/19) | 12 s, verified end to end |
| Vulnerabilities | none reachable | `govulncheck` clean, gated in CI |
| Upgrade path | v0.5.0 → current | schema 6 → 10, clean, new endpoints live |

### Known limitations

Named rather than omitted; see [docs/25-security-checklist.md](docs/25-security-checklist.md).

- No user acceptance testing. The console has been driven by hand; the other
  pages have been verified through their APIs and read, not used in anger.
- No external penetration test and no DAST in CI.
- Container images are built but not signed; there is no registry to publish
  to yet.
- No breach-corpus password check and no re-authentication before sensitive
  administrative changes.
- Kubernetes manifests are documented but untested.

---

## v0.8.0 — 2026-08-14

Product MVP: an administrator can onboard a platform and a user entirely in
the browser.

- Console UI (noVNC) with the portal-side RFB handshake (ADR 0002)
- VM detail with performance charts over stored rollups (ADR 0001)
- Platform administration with connector-declared schemas and a gating
  connection test
- Users, groups and grants, including the VM group membership that made
  grants confer access to anything at all
- Migration 00007: platform name uniqueness scoped to live rows

## v0.5.0 — 2026-08-13

Backend complete through sprint 10: identity, access control, the connector
framework, the Proxmox connector, the sync engine, the metrics pipeline, the
console proxy, power actions and live events.
