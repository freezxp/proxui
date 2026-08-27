# ADR 0008 — Detached SSH sessions are reclaimed in minutes, not half an hour

**Status:** accepted · **Date:** 2026-08-27 · **Amends:** the single idle limit in [docs/29-ssh-terminal.md §29.7](../29-ssh-terminal.md) and the wording of SSH-07 in [docs/03-frs.md](../03-frs.md)

## Context

An operator who used the SSH console several times in an afternoon found they could no longer open one. Waiting fixed it. The wait was thirty minutes.

The mechanism is a chain of three deliberate decisions that combine into a defect none of them intended:

1. A user may hold **8 open sessions** (`shellreg.MaxPerUser`). The ninth is a 429.
2. A session leaves the registry only on an explicit disconnect, on the shell exiting, on an administrator closing it, or on the idle sweep.
3. **Closing the browser tab is not one of those.** The terminal bridge keeps the session alive when the socket closes, so that a reconnect does not have to ask for the password again — the portal never stored it (ADR 0005), so a session that ends is a credential the operator has to type once more.

The third decision is sound in the case it was written for: a socket that drops while the page is still open. It is wrong in the case that actually happens, because **the session id lives only in the page's React state.** Once the tab is gone, so is the only reference to that session. Nothing can ever attach to it again. It is not being held for a reconnect that might come; it is unreachable, and it holds a live authenticated login on the guest until the idle sweep takes it at thirty minutes.

Eight of those and the operator is locked out. The 429 tells them to "close one and try again", which they cannot do: there is no list of open sessions, and the tabs that opened them are gone.

Thirty minutes is the right number for an *attached* terminal — an operator who stepped away from a shell they are still in front of. It was never chosen for a session with nobody behind it. SSH-07 says an unattended root shell must not stay open; a shell nobody can reach is the purest case of that, and it is the case getting the most generous treatment.

## Decision

Two changes, one on each side.

**The browser gives the session back on its way out.** `SshPage` releases on unmount and on `pagehide`, via `releaseOnUnload` — a `DELETE` with `keepalive: true`, which lets the request outlive the document. `navigator.sendBeacon` cannot be used: it sets no headers, and this API authenticates with a bearer token held in memory rather than a cookie.

**The server reclaims detached sessions on a short limit.** `shell.ExpiryCheck` takes whether a terminal is attached and applies `DetachedGrace = 2 minutes` when one is not, closing with the reason `abandoned`. The hard 8-hour cap still wins over both.

The limit is measured from **last activity**, not from the moment of detaching. A session with no terminal is a legitimate state — the file browser alone is a reasonable way to use one, as §29.7 already said — so anything touching the session holds it open, terminal or not.

## Rationale

Neither half is sufficient alone, and the split is the point.

The client-side release fixes the common case precisely and immediately: a tab closed on purpose gives its session back at once, and the slot is free before the operator has finished navigating. But it can never be relied on. `pagehide` does not fire for a killed tab, a crashed browser, or a laptop that lost the network; `keepalive` requests can be dropped. A fix that only lived in the browser would leave exactly the situation being fixed, less often and harder to reproduce.

The server-side sweep is therefore the actual guarantee, and the client-side release is an optimisation of it. That ordering — the browser is asked politely, the server does not depend on the answer — is the same one the rest of this design uses for session limits, and it is why the sweep exists at all rather than trusting a heartbeat.

Two minutes is chosen against the one window that must not be caught in it: a session is detached between the moment `OpenShell` returns and the moment the browser's WebSocket attaches. That window is bounded by `TicketTTL`, which is sixty seconds — after which nothing can attach, so the session is already garbage. Two minutes clears it with a minute to spare and still reclaims an abandoned login inside the time it takes to notice one is missing.

`abandoned` is a distinct reason rather than a reuse of `idle_timeout` because the audit trail is read to answer "what happened to this session", and the two answers differ: one is an operator who walked away from a shell, the other is a shell that was orphaned. `close_reason` is free text with no constraint, so this needs no migration.

## Consequences

- **A dropped WebSocket now costs a password after two minutes rather than thirty.** This is the real cost. A page that stays open through a network blip and reconnects promptly is unaffected; one that reconnects slowly types the credential again. Given the reconnect path already asks for the credential whenever the tab was closed, this narrows a window that was mostly theoretical, and it narrows it in the safe direction.
- A session held open by the file browser alone is unaffected: file operations call `Touch`.
- `shell.ExpiryCheck` gains a parameter. The console's identically-named function is untouched — the two contexts keep their own rules, as they do already.
- The 429's advice to "close one and try again" is still not actionable, because there is still no list of open sessions. That remains worth building, and is now much less often needed.

## Alternatives considered

- **Raise `MaxPerUser`.** Moves the wall without removing it, and does nothing about orphaned logins sitting open on guests — which is the security half of the problem, not just the annoyance half.
- **Close the session whenever the WebSocket closes.** The simplest rule, and it discards the case the current behaviour was written for: a socket that drops under a page still open, where the session is genuinely still reachable and reattaching costs nothing. The grace period keeps that case working.
- **Client-side release only.** Rejected above: unreliable by construction, and the failures are exactly the abrupt endings this is meant to catch.
- **Persist the credential so a session can be rebuilt.** Would remove the whole problem, and is the one thing this design will not do (ADR 0005).
