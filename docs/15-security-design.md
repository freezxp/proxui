# 15 — Security Design

Threat-model framing: the portal concentrates access to hypervisors, so *it* becomes the high-value target. Design goals: (1) compromise of a portal user ≠ compromise of Proxmox; (2) every privileged action attributable; (3) secrets never recoverable in plaintext from disk artifacts.

## 15.1 Authentication (portal users)

| Control | Design |
|---|---|
| Password storage | argon2id — m=64 MiB, t=3, p=4 (tuned at deploy via bench); per-hash random salt; encoded params stored for future migration |
| Password policy | ≥12 chars, no composition theater; offline check against bundled top-1M breached list; deny username-in-password |
| Access token | JWT **RS256** (key pair generated at install, private key file-mounted; JWKS endpoint internal); 15 min TTL; claims: `sub`, `role`, `sid`, `iat/exp`, `jti`. Verified per request + `sid` checked against a Redis revocation set (deactivation is immediate, not TTL-delayed) |
| Refresh token | 256-bit random, SHA-256 hash stored; httpOnly+Secure+SameSite=Strict cookie scoped `/api/v1/auth`; **rotation with family reuse-detection** (an attacker replaying a stolen rotated token kills the whole family and raises a `security` event) |
| MFA | TOTP (RFC 6238, SHA-1/6 digits/30 s, pinned to the RFC's own test vectors), seed envelope-encrypted at rest, ±1 step window with the accepted step recorded so no code works twice, enrolment inert until confirmed, five attempts per challenge, challenge expires in 5 min, verify rate-limited as strictly as login; disabling costs the account password; admin reset audited against both accounts |
| Lockout | 5 fails / 15 min → 15 min lock (per-account) + per-IP login throttle (per-IP alone is defeatable behind NAT; both are needed) |
| Bootstrap | first-run admin from `PROXUI_ADMIN_*` env/secret; forced password change + MFA prompt on first login |

**OAuth2/OIDC note:** the brief lists OAuth2. For an internal tool with built-in auth, the portal issues its own JWTs (resource-owner flow equivalent) — running a full OAuth2 authorization server adds attack surface with no consumer. The `Authenticator` port keeps an OIDC federation seam for future SSO (see 22-future-enhancements).

## 15.2 Authorization

- Role checks and VM-scope filtering are middleware/query-level (single enforcement point, [05 §5.5](05-system-architecture.md)); out-of-scope resources return 404 (existence not leaked).
- Deny-by-default: an endpoint without an explicit permission annotation fails closed at startup (route table is validated against the permission map at boot — a new endpoint cannot ship unprotected).
- All denials audited with `outcome=denied`.

## 15.3 Secrets & credential encryption

```
master key (32B)         — Docker secret / file mount; never in env dumps, never in DB
  └─ wraps DEK (32B/credential)          AES-256-GCM
       └─ encrypts: Proxmox API token secrets, TOTP seeds,
                    SMTP passwords, Slack webhook URLs, webhook HMAC keys
```

- **Rotation:** master-key rotation = decrypt DEKs with old key, rewrap with new, bump `key_version` — secrets themselves untouched, one short transaction, `proxui rotate-master-key` CLI. Credential rotation = admin re-enters token (old one revoked on the Proxmox side).
- Secrets are **write-only** through the API; logs/error messages pass through a redaction filter (known secret shapes + exact-value scrubbing of loaded secrets).
- Memory hygiene: decrypted secrets live only in call-scoped variables; never cached, never serialized.

## 15.4 Securing the platform link (secure connector authentication)

- One dedicated API token per cluster (`proxui@pve!portal`). Baseline least privilege: `PVEAuditor` + `VM.Console` (+ `VM.PowerMgmt` if power actions enabled).
- ~~**The portal physically cannot create/delete VMs on Proxmox**~~ — **retired by [ADR 0010](adr/0010-the-portal-can-create-and-destroy-guests.md).** A token granted the provisioning privileges (`VM.Allocate`, `VM.Clone`, `VM.Config.*`, `Datastore.AllocateSpace`) can create and destroy guests, and the portal uses the same token for every path. The ceiling was a structural guarantee that survived bugs in the portal; what replaces it is admin-only routes, server-side name confirmation on destroy, templates refused, and an audit trail. Platforms whose token was never widened keep the old ceiling and simply cannot provision.
- TLS to Proxmox: verify against system CAs, a custom CA bundle, or SHA-256 certificate pinning (self-signed clusters); `insecure` mode exists but shows a persistent UI warning and is audit-logged on every enable.
- Egress from the portal host is firewall-allow-listed to cluster IPs:8006.

## 15.5 Console security chain

login → RBAC console check → audit(open) → Proxmox `vncproxy` (per-session, ~10 s validity, single-target VNC ticket) → one-time portal `sid` (60 s TTL, bound user+VM, consumed on WS upgrade) → byte-level bridge (no protocol interpretation; VNC password = the Proxmox ticket) → idle 30 min / max 8 h → audit(close, duration, bytes). Admin can enumerate and kill live sessions. The browser never learns Proxmox addresses, tickets, or certificates.

## 15.6 Web/API hardening

- TLS 1.2+/1.3 at Caddy (internal CA or ACME); HSTS; HTTP→HTTPS redirect only.
- Headers: strict CSP (self-only; no inline script — Vite build emits hashed assets), `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`.
- CSRF: state-changing endpoints require the Bearer header (not readable cross-site); the refresh cookie is SameSite=Strict and only touches `/auth/*`; WS upgrades validate `Origin`.
- Rate limits per [08 §8.10](08-api-specification.md) via Redis token buckets.
- Input validation at the OpenAPI boundary (schema-enforced) + domain invariants; SQL exclusively parameterized (sqlc); JSONB inputs size-capped.
- Dependency/container hygiene in CI: `govulncheck`, `npm audit`, Trivy image scan, Dependabot; distroless runtime (no shell), non-root UID, read-only root FS.

## 15.7 Audit trail integrity

- Append-only enforced by DB grants (app role: INSERT+SELECT only on `audit_logs`).
- Every entry carries `request_id`; API errors (5xx / upstream) are audited with sanitized detail (AUD-01).
- Optional syslog/webhook mirror of security-category events to an external SIEM (config flag) so a portal-host compromise cannot silently erase its own trail.

## 15.8 Security-relevant events → notifications

`login_failed` (burst), `account_locked`, `refresh_token_reuse`, `user_created/role_changed`, `platform_credential_changed`, `tls_insecure_enabled`, `permission_denied` (burst) — all flow through the `security` notification category (FRS NOTIF-02).

## 15.9 Review gates

- OWASP ASVS L2 checklist pass before v1.0 (tracked in repo as `docs/security/asvs-checklist.md`).
- `/security-review`-style static pass + manual pentest of the console path (highest-value target) before production exposure.
