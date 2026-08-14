# 25. Security checklist (ASVS Level 2)

A pass over OWASP ASVS 4.0 Level 2, limited to the categories that apply to a
self-hosted portal with no public registration and no payment handling. Each
line states the evidence, not the intention. Items that are **not met** say so
plainly rather than being quietly omitted.

Verified 2026-08-14 against the running portal and the live PVE 9.2.10
cluster.

## V1 Architecture

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 1.1 | Threat model and trust boundaries documented | ✅ | [docs/15-security-design.md](15-security-design.md); the browser is the least-trusted participant, which is why consoles are proxied rather than redirected |
| 1.2 | Components communicate over authenticated channels | ✅ | platform calls use API tokens over TLS with a configurable trust policy; Redis and Postgres are compose-internal |
| 1.4 | Access control enforced at a trusted layer | ✅ | every route's roles come from the permission map, and boot fails if a wired route is undeclared (`TestEveryWiredRouteIsDeclared`) |

## V2 Authentication

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 2.1.1 | Passwords ≥ 12 characters | ✅ | enforced on create and change |
| 2.1.7 | Passwords checked against breach lists | ❌ | **not implemented.** No k-anonymity lookup against a breach corpus. Deliberate: a self-hosted portal should not phone out by default, and bundling a corpus is impractical. Length and lockout carry the weight instead |
| 2.2.1 | Anti-automation on credentials | ✅ | rate limiter returns 429 after 4 failed attempts; measured `401 401 401 401 429 429 …` |
| 2.4.1 | Passwords stored with an approved KDF | ✅ | argon2id, per-password salt |
| 2.5.4 | No default or shared accounts | ✅ | the bootstrap admin is created once from the environment and must change its password at first sign-in |
| 2.7 | Out-of-band recovery | ❌ | **not implemented.** No self-service reset by design; an administrator sets a new password. Documented in [docs/13-ui-design.md](13-ui-design.md) |

## V3 Session management

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 3.2.1 | New session token on authentication | ✅ | login issues a fresh refresh token and family |
| 3.3.1 | Logout invalidates the session | ✅ | the refresh token is revoked server-side, not only cleared client-side |
| 3.3.2 | Idle and absolute timeouts | ✅ | access token 15 min, session 7 days, both settable in Settings |
| 3.4.1 | Cookies are `Secure` | ⚠️ | set when `PROXUI_SECURE_COOKIES=true`, which the shipped compose config sets. Off for plain-HTTP LAN use, where forcing it would prevent sign-in entirely |
| 3.4.2 | Cookies are `HttpOnly` | ✅ | `Set-Cookie: proxui_rt=…; HttpOnly; SameSite=Strict; Path=/api/v1/auth` |
| 3.4.3 | `SameSite` set | ✅ | `Strict` |
| 3.5.3 | Stateless tokens are validated | ✅ | RS256, issuer and expiry checked |
| 3.7.1 | Re-authentication before sensitive changes | ❌ | **not implemented.** Changing another user's role does not re-prompt. Mitigated by the audit trail, not prevented |

## V4 Access control

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 4.1.1 | Enforced server-side | ✅ | the sidebar filters by role for clarity; every route is gated regardless |
| 4.1.3 | Least privilege | ✅ | four roles, plus per-VM grants. Proven: an operator granted 3 of 25 VMs sees exactly 3, and gets 404 on any other VM's detail, metrics and console |
| 4.1.5 | Fails closed | ✅ | an unknown route is 404 and an undeclared route stops boot |
| 4.2.1 | No IDOR | ✅ | VM access is checked per request against grants, not only by role; a scoped account gets 404 rather than 403, so the response does not confirm the VM exists |
| 4.3.1 | Administrative interfaces protected | ✅ | admin-only routes, verified unauthenticated: `/users`, `/settings`, `/platforms`, `/audit-logs` all answer 401 |

## V5 Validation and encoding

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 5.1.1 | Input validated server-side | ✅ | alert rules reject impossible thresholds, settings reject out-of-range values and unknown keys, channel kinds are an allowlist |
| 5.2.5 | No unsafe template rendering | ✅ | React escapes by default; no `dangerouslySetInnerHTML` anywhere |
| 5.3.4 | SQL injection prevented | ✅ | every query is parameterized through pgx; no string-built SQL carrying user input |
| 5.3.3 | Output encoding contextual | ✅ | mail headers strip CR/LF, so a VM named with a newline cannot inject headers |

## V7 Error handling and logging

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 7.1.1 | No secrets in logs | ✅ | credentials are sealed before they reach any log path; the API never returns a stored secret, only whether one exists |
| 7.2.1 | Security events logged | ✅ | sign-in success and failure, console open and close, permission denials, every configuration change |
| 7.3.1 | Logs protected from injection | ✅ | structured JSON logging; fields are encoded, not concatenated |
| 7.4.1 | Generic messages to the client | ✅ | problem documents carry a request id; detail stays server-side |

## V8 Data protection

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 8.1.1 | Sensitive data protected at rest | ✅ | platform credentials and notification secrets use AES-256-GCM envelope encryption; a per-secret DEK wrapped by the master key |
| 8.2.1 | No sensitive data in the client | ✅ | the browser never receives a platform credential, and — since ADR 0002 — not even the console password |
| 8.3.4 | Sensitive data inventoried | ✅ | [docs/15-security-design.md](15-security-design.md) §15.4 |

## V9 Communications

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 9.1.1 | TLS for all connections | ⚠️ | terminated at the reverse proxy in the shipped config. The portal itself serves plain HTTP and is not meant to face the internet directly |
| 9.2.1 | Certificate validation for outbound | ✅ | four TLS modes; `insecure` is audited and warned about in the UI in plain words |

## V12 Files and resources

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 12.3.1 | No user-supplied paths | ✅ | the portal accepts no file uploads at all |
| 12.6.1 | SSRF protection | ⚠️ | **partial.** An administrator can point a platform or webhook channel at any address, including internal ones. That is the feature — a self-hosted portal exists to reach internal clusters. Restricted to administrators and audited, not blocked |

## V13 API

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 13.1.3 | API URLs do not expose sensitive data | ✅ | identifiers only; console tickets are single-use and redeemed by `GETDEL` |
| 13.2.1 | Methods used correctly | ✅ | reads are GET, state changes are POST/PUT/DELETE |
| 13.2.3 | CSRF defence | ✅ | bearer tokens in memory rather than ambient cookies, refresh cookie is `SameSite=Strict` and scoped to the auth path |

## V14 Configuration

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 14.2.1 | Dependencies current | ✅ | Dependabot weekly; `go mod tidy` enforced in CI |
| 14.3.2 | No debug detail in production | ✅ | no stack traces to clients; no server banner |
| 14.4.1 | Security headers set | ✅ | CSP, `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Permissions-Policy`, and HSTS over TLS only |
| 14.5.1 | HTTP methods restricted | ✅ | chi routes only declared methods; anything else is 405 |

### The CSP, and why two directives are looser

```
default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
img-src 'self' data: blob:; font-src 'self'; connect-src 'self' ws: wss:;
object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
```

- `style-src 'unsafe-inline'` — noVNC sets element styles directly while
  sizing its canvas. Removing it breaks the console. Inline **style** cannot
  execute script; inline script remains blocked.
- `connect-src ws: wss:` — the console bridge and the event stream. Both are
  same-origin, but the scheme has to be named.

## Not covered

Named because a checklist that omits them reads as if they passed:

- **No external penetration test.** The console path has been exercised
  end-to-end and the access-control boundary is tested, but nobody hostile
  has looked at it.
- **No automated DAST run.** A ZAP baseline scan against a running instance
  belongs in CI and is not there.
- **Container images are not signed.** CI builds an image but publishes none;
  signing becomes meaningful when there is a registry to publish to, and
  should be cosign keyless from the release workflow at that point.
Previously listed here and now closed: CI runs `govulncheck` and fails on a
known-vulnerable dependency the code actually reaches. Adding it found six
standard-library vulnerabilities fixed in Go 1.26.6; the toolchain was pinned
to that patch and the scan is now clean.
