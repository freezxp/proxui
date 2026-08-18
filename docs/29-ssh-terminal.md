# 29. SSH terminal and file transfer

> **Status:** implemented. Requirements SSH-01…SSH-10 in
> [03-frs.md](03-frs.md); the credential decision is
> [ADR 0005](adr/0005-ssh-credentials-are-never-stored.md).

The console gives an operator a machine's keyboard and screen through its
hypervisor. That is the right tool for a machine that will not boot, and the
wrong tool for almost everything else: no scrollback, no copy and paste worth
the name, no way to move a file, and a redraw of the whole screen for every
keystroke. Day-to-day work is a shell, and until now doing it meant leaving the
portal — with all the network reach and credential sprawl that implies.

This adds a terminal that reaches the guest over SSH, and a file browser
running on the same connection.

## 29.1 What is different from the console

They look alike from the browser — a ticket, a WebSocket, an audited session —
and are unalike underneath in the way that matters:

| | Console | SSH |
|---|---|---|
| Reaches the guest through | the hypervisor's API | the guest's own network stack |
| Credential | the platform token the portal holds | typed by the operator, per session |
| Portal can re-establish it | yes, at any time | no, never |
| Survives a portal restart | the session record does; a reconnect is free | the session ends |
| State | none; the bridge is stateless | the open connection *is* the session |

The last two rows are the whole design. Because the portal deliberately does
not keep the credential, an SSH session cannot be rebuilt, so the connection
lives in the memory of the process that opened it and everything else follows
from that.

## 29.2 Shape

```
POST /api/v1/vms/{id}/ssh          authorize, resolve address, dial, authenticate,
                                   register the live connection, mint a ticket
GET  /ws/ssh/{ticket}              attach a terminal to that connection (once)
GET  /api/v1/ssh-sessions/{id}/…   the file browser, over the same connection
DELETE /api/v1/ssh-sessions/{id}   close it
```

Authentication happens during the POST rather than when the socket opens, which
is the opposite of the console. It has to: the credential exists only for the
duration of that request. The happy side effect is that a wrong password is a
401 on a JSON request — something a browser can render — rather than a
WebSocket that closes for reasons it has to guess at.

The terminal socket carries **binary frames for terminal bytes and text frames
for control messages**. A keystroke is a byte and a control sequence is bytes;
wrapping either in JSON would cost an allocation per character and mangle
anything that is not valid UTF-8. Because the two frame types cannot collide,
neither needs a header. There is exactly one control message:

```json
{ "type": "resize", "cols": 132, "rows": 43 }
```

## 29.3 Where the credential goes

Nowhere. It arrives in the POST body, is passed to the dialer, and is dropped
(SSH-03, ADR 0005). It is not written to Postgres, not written to Redis — which
in the documented deployment runs with `appendonly yes` and would put it on
disk — not logged, and not echoed by any response. The audit trail records the
*username* it was used with, because "who logged into that box as root" is the
question an audit exists to answer, and the username is not the secret.

Go gives no way to wipe a string's backing array, so the mitigation is lifetime
rather than erasure: the value exists for one dial and is never copied into
anything that outlives it.

The session ticket is held in process memory for the same reason (`shellreg`),
rather than in the Redis store the console ticket uses. A ticket that another
process could read would be a ticket it could not honour.

## 29.4 Which address, and only which address

The guest agent reports every address on every interface, in no useful order,
including ones that will never accept a connection from the portal: loopback,
link-local, and the host side of whatever container bridge the guest runs.
Picking the first entry is how a terminal ends up dialling `172.17.0.1` and
timing out with no explanation, so `shell.PickAddress` ranks them — ordinary
IPv4 first, then routable IPv6, then a container bridge, never loopback or
link-local — and is unit tested on its own.

An operator may name a different address, and it is checked against the list
the platform reported for that VM. This is a security boundary, not a
convenience: the portal can reach a great deal of network that an operator's
grant over one VM says nothing about, and letting the request name an arbitrary
host would turn the portal into an SSH proxy to all of it.

## 29.5 Host keys

Pinned on first use, one key per VM, refused on change (SSH-04).

Per VM rather than per address, because a guest that takes a new DHCP lease is
the same machine and treating it as a new trust decision trains operators to
click past the one prompt that matters.

Pinned at the moment the operator confirms the fingerprint, not at the moment
they successfully log in — this is what OpenSSH does, and following it means a
mistyped password does not cost a second trust decision.

A *changed* key is refused outright, and accepting the new fingerprint at the
connect form does not override it. The benign explanation (the guest was
rebuilt) and the alarming one (something is answering in its place) are
indistinguishable from the portal, and only one of them is survivable by
clicking through. Clearing a pin is `DELETE /vms/{id}/ssh-host-key`, admin
only, audited, and deliberately somewhere else.

The portal asks for the same host key algorithms OpenSSH prefers, in the same
order, so the fingerprint it shows is the one the operator's own `ssh` or
`ssh-keyscan` shows them. A portal that pinned the ECDSA key while their
terminal displayed the Ed25519 one would make the comparison it is asking for
impossible to perform.

## 29.6 Files

SFTP over the same connection (SSH-09): list, download, upload, mkdir, rename,
delete, chmod. One connection because a second would mean a second
authentication, and the credential is gone by then.

The guest's own permissions are the authorization. The portal does not try to
jail an operator inside a directory they could reach from the terminal in the
next panel; what it does refuse is a path that is not absolute, a path
containing a NUL byte, and an upload whose *name* contains a separator — the
last of these is how a file called `../../etc/cron.d/x` would otherwise land
somewhere nobody chose.

Recursive deletion is absent on purpose. A file browser that can erase a
subtree by mis-click is a different tool, and `rm -rf` in the terminal beside
it is at least explicit about what it is doing.

Uploads and downloads stream: the body goes straight to the guest rather than
through a buffer, so a gigabyte is not a gigabyte of portal memory. Both detach
from the router's 30-second request timeout, which is right for an API call and
wrong for moving a disk image.

**Audited:** upload, download, delete, rename, chmod, mkdir. **Not audited:**
listing and stat. A browser panel lists a directory on every click, and a trail
that fills with "looked at /var" buries the entries that matter.

## 29.7 Limits and endings

Idle 30 minutes, hard cap 8 hours — the console's numbers, because an
unattended root shell is the same risk whichever door it came through
(SSH-07). Enforced by a sweep in the registry rather than by the terminal
socket, because a session with no terminal attached is a legitimate state: the
file browser alone is a reasonable way to use one.

Every ending is recorded, including the ones nobody asked for — swept, closed
by an administrator, or lost to a restart. An audit trail that only shows the
endings someone requested is not an audit trail.

Ceilings: 8 open sessions per user, 64 per process. One terminal per session; a
second tab is refused rather than interleaving two people's keystrokes into one
shell.

## 29.8 Deployment note

**A session belongs to the process that opened it.** With `--role=api` behind a
load balancer, the terminal socket and the file requests must reach the same
instance, so that deployment needs session affinity. The documented default —
a single binary, [14-deployment.md](14-deployment.md) — is unaffected. This is
recorded rather than solved: solving it means putting the credential somewhere
shared, which is the one thing this design will not do.

A restart ends every session. That is correct rather than unfortunate.

## 29.7a Keys a touch keyboard does not have

A phone's on-screen keyboard has no arrows, no Tab and no Ctrl, which makes a
browser terminal something you can read output in but not work in: no history
recall, no completion, and no way to stop a runaway command.

So the terminal carries a key bar — Esc, Tab, Ctrl, Alt, the four arrows, Home,
End, PgUp, PgDn, and one-tap `^C ^D ^Z ^L ^R` for the combinations common
enough that arming a modifier first would be a tap too many. It defaults on for
a coarse pointer and is a toggle everywhere else, because a desktop operator
may still want an interrupt within reach.

Three details carry the weight:

- **Modifiers are sticky, not held.** There is no chording on a touchscreen, so
  Ctrl arms and the next keystroke spends it — from the bar or from the
  on-screen keyboard, which is what makes Ctrl then C interrupt. An armed
  modifier is shown as pressed, or the model would be guesswork.
- **The bar is sized against the visible viewport.** It is the last row of a
  full-height page, and on a phone `100vh` measures the height with the
  browser's bars hidden while neither it nor `100dvh` shrinks for the soft
  keyboard. Either one leaves the bar off screen exactly when it is needed, so
  the page follows `visualViewport` (`web/src/lib/viewport.ts`) and falls back
  to `100dvh` where that API is missing. The console's key strip is sized the
  same way.
- **The encoding is asked for, not assumed.** Cursor keys change form when the
  guest turns on application cursor mode (vim, less), so the mode is read from
  the terminal; and a modifier is a parameter rather than a prefix, so Ctrl+Left
  is `ESC [ 1 ; 5 D` and word-jumping works. Getting either wrong does not
  throw — it prints `^[[A` into somebody's shell, which is why the sequences
  are pinned by tests rather than left to a canvas jsdom cannot render.

## 29.7b Copying out of a program that has the mouse

tmux, vim and htop turn on mouse reporting the moment they start, and xterm.js
obliges: every click and drag goes to the guest. That is what makes those
programs usable in a browser, and it is also what makes a drag stop selecting
anything to copy. The classic complaint — "I can't copy text when I'm in tmux"
— is not a bug in the terminal, it is the guest holding the mouse.

A desktop operator can hold Shift for one drag, which xterm.js honours by
taking the mouse back, the same escape hatch a native terminal has. It is
undiscoverable, and on a phone there is no Shift at all — a drag on the canvas
scrolls rather than selects.

So the portal does not fight for the drag. **Select** lifts the buffer out as
plain text over the terminal, where selecting works the way it works everywhere
else: a drag with a mouse, a long press with a finger, and Copy all for when
selecting precisely is more trouble than it is worth. The bytes are already in
the browser's scrollback, so nothing is asked of the guest and nothing
interrupts what is running. **Copy** with nothing selected opens the same
panel, because that is the button somebody presses when the drag did nothing.

Three details:

- **It is a snapshot, not a view.** Text that reflowed under a live selection
  would lose the selection on every redraw, and a screen with tmux on it
  redraws constantly. The panel takes the buffer when it opens, and again when
  the scope changes between the screen and the whole scrollback.
- **A wrapped row is not a line.** Breaking where the window broke would cut a
  command in half at the width of the terminal instead of where it ends, so
  rows whose successor is a continuation are joined and not trimmed; the blank
  rows below the last output are dropped, because pasting them is pasting
  nothing (`bufferText`, pinned by tests — xterm fills a buffer only against a
  canvas jsdom does not have).
- **Copying still degrades.** The panel hands its text to the same path as
  every other copy in the portal, so on a plain-HTTP origin, where there is no
  clipboard API, it falls back to the textarea the operator can copy from by
  hand (SSH-08).

What this does not give is a rectangle. Selection is line-wise, so copying one
pane of a vertical tmux split takes the other pane's columns with it — the same
thing a native terminal does with Shift+drag, and the reason `tmux -CC` and
copy-mode still exist.

## 29.8a The portal's own key

Design: [ADR 0006](adr/0006-portal-owned-ssh-key.md). Requirements: SSH-11…SSH-14.

Retyping a password on every connect is the cost ADR 0005 accepted and named as
the reason to revisit it. The answer is not a stored guest password — that is
the option 0005 rejected, and for reasons that have not changed — but one key
pair the portal owns.

- **Generated by the portal, sealed in the vault.** Ed25519, one pair,
  `ssh_portal_key`. Admin-only to generate, rotate or delete. The private half
  is read at the moment of a dial and appears in no response and no log.
- **Installed deliberately, one account at a time.** The install runs over an
  SSH session the operator already authenticated, so the write happens with
  that guest account's permissions and grants the portal nothing it was not
  already given. The public key line can also be pasted into cloud-init, which
  is the path for a guest nobody has a password on.
- **Appending, never rewriting.** Adding a line to somebody's
  `authorized_keys` must not be able to lose the lines already in it. `~/.ssh`
  is created 0700 and the file set 0600, because sshd silently ignores one that
  is group-writable — the most common "I installed it and it still asks for a
  password".
- **Connecting with it sends a boolean.** `use_portal_key: true` and nothing
  else; the key never travels to a browser. Which method a session used is on
  the audit entry, for every session, not only the ones that used the key.
- **Revocation is one line.** Removing it takes out only the portal's own line
  and forgets the record either way, so an operator who deleted it by hand is
  not left with a portal that believes otherwise. Rotation invalidates every
  install at once; what is left behind is listed as stale rather than shown as
  working.

`ssh_key_installs` records what the portal did, not what a guest will accept.
The file on the guest is the authority; the table exists so the connect form
can offer the key where it is likely to work, and so a rotation can show what
it is about to break.

## 29.9 What was left out

- **Stored guest credentials.** Considered and rejected; see ADR 0005. The
  fatigue it named was answered with a portal-owned key (29.8a, ADR 0006)
  rather than with a per-VM password store.
- **Per-user keys, and automatic install.** A key that arrives somewhere nobody
  chose to put it is the property that made a credential store unacceptable.
  Installing is always an explicit act on one account.
- **A jump host.** Direct reach from the portal to the guest is assumed, which
  is what the reference deployment has. A guest on a network the portal cannot
  reach has no terminal, and says so.
- **Port forwarding and agent forwarding.** Both turn the portal into a network
  path rather than a terminal. Neither is needed to run a command on a box.
- **Session recording.** The trail records that a session happened, by whom,
  for how long, and what files moved — not what was typed. Keystroke capture on
  a session that routinely contains passwords is a liability, not a feature.
