# ADR 0002 — The portal answers the console handshake, not the browser

**Status:** accepted · **Date:** 2026-08-14 · **Refines:** [docs/06-sequence-diagrams.md §6.2](../06-sequence-diagrams.md), [docs/15-security-design.md §15.4](../15-security-design.md)

## Context

The console bridge built in sprint 9 is a blind byte relay. That was a deliberate property: RFB, serial and anything else a platform speaks pass through untouched, so one piece of code stays correct for every connector.

Sprint 14 connected a real browser to it and hit the thing a byte relay cannot do. Probing the live cluster:

```
security types: count=1 -> ['VNC Auth']
```

Proxmox offers exactly one RFB security type, and it demands a password. Something has to answer that challenge, and a relay that does not read bytes cannot.

The obvious answer — send the console password to the browser and let noVNC answer, which is what Proxmox's own web UI does — collides with a locked decision. [docs/15-security-design.md §15.4](../15-security-design.md) states the browser never learns the node address, the upstream ticket or the platform credential; the whole reason consoles are proxied rather than redirected is that the browser is the least trusted participant.

Timing rules it out independently. A Proxmox VNC ticket is valid for about 30 seconds, so the upstream session is created when the browser connects, not when the portal issues its own ticket. At the moment the browser would need a password, none exists yet.

## Decision

The portal completes the console handshake on both sides before relaying begins.

Against the node it plays the RFB client: version exchange, security type 2, DES challenge-response using a password requested from `vncproxy` with `generate-password=1`. Towards the browser it plays the RFB server: version exchange, security type **None**, `SecurityResult` OK.

Both streams come out of this aligned at `ClientInit`, and the bridge relays blindly from there exactly as before.

The exchange lives in the connector (`internal/connectors/proxmox/console_rfb.go`) behind an optional `connector.ConsoleAuthenticator` interface. The bridge calls it if an endpoint implements it and otherwise relays from the first byte.

## Rationale

- **The browser holds nothing.** It authenticates with "None" against a socket it can only reach through the portal, after the portal proved to the node who it is. There is no secret to leak from browser memory, devtools, or a screenshare.
- **The bridge stays protocol-neutral.** RFB knowledge sits in the Proxmox connector, which is where every other Proxmox-specific fact already lives. A platform needing nothing here implements nothing.
- **`generate-password` fits the protocol.** VNC auth truncates passwords to eight bytes. The API ticket is far longer, so using it would silently authenticate with its first eight characters — a fixed, guessable prefix. The generated password is eight random characters scoped to one session.
- **It works over plain HTTP.** With security type None the browser never reaches for WebCrypto, which is unavailable outside a secure context. A LAN deployment on `http://` therefore works, which matters for a self-hosted tool used before TLS is configured.

## Consequences

- The portal now understands a little RFB. That is a real cost: a protocol change on the platform side breaks the console rather than passing through.
- Serial console (termproxy, planned for v1.x) has its own handshake and will need its own implementation of the same interface.
- The handshake happens before `MarkConnected`, so a session that fails authentication is recorded as an upstream failure rather than a connected session — which is the honest reading.
- Anyone reading the bridge must know that the first bytes of a session may already have been consumed. The interface contract states it and the bridge comments point here.

## Alternatives considered

- **Send the password to the browser.** Rejected: contradicts §15.4, and the password does not exist when the browser would need it.
- **Request no password and forward the API ticket as one.** Rejected: truncation to eight bytes makes the effective secret a constant prefix.
- **Teach the bridge RFB directly.** Rejected: puts platform-specific protocol knowledge in the one component whose value is not having any.

## Verification

Against the live PVE 9.2.10 cluster, a browser-shaped client completed the full handshake through the bridge with no credentials:

```
1. server version -> b'RFB 003.008\n'
2. security types offered to browser -> ['None']
3. SecurityResult -> OK
4. ServerInit -> framebuffer 1280x800  desktop name='QEMU (devops)'
```
