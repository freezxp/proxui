# 17 — Error Handling Strategy

## 17.1 Principles

1. **Errors are values with class.** Every layer wraps errors with typed classes; behavior (retry? notify? 4xx or 5xx?) is decided by class, never by string matching.
2. **One boundary translates.** Only the transport layer converts errors to HTTP (RFC 7807); only the job layer converts them to retry/park decisions. Inner layers never know about HTTP codes.
3. **Fail loud internally, fail useful externally.** Users get an actionable message + `request_id`; logs/audit get the full chain; secrets never appear in either.

## 17.2 Error taxonomy

| Class (Go sentinel/type) | Meaning | HTTP | Job behavior |
|---|---|---|---|
| `ErrValidation{fields}` | bad input | 422 | park (bug) |
| `ErrUnauthenticated` | no/invalid token | 401 | — |
| `ErrForbidden{code}` | RBAC denial | 403 (404 for out-of-scope resources) | — |
| `ErrNotFound` | absent entity | 404 | park |
| `ErrConflict` | state conflict (e.g. start a running VM) | 409 | drop |
| `ErrRateLimited` | bucket exceeded | 429 + Retry-After | backoff |
| `connector.ErrUnreachable` | network/timeout upstream — nothing answered | 502 | retry w/ backoff |
| `connector.ErrRefused` | upstream answered and declined (5xx): VM already in that state, config lock held, pool full | 409, carrying the platform's own words | **no retry**; retrying cannot succeed |
| `connector.ErrAuth` | platform rejected credentials | 502 (admin-facing detail) | **no retry**; breaker + notify |
| `connector.ErrPermission` | token lacks privilege | 502 detail lists missing perms | no retry; notify |
| `connector.ErrThrottled` | upstream 429 | 503 | longer backoff |
| `ErrInternal` (anything unclassified) | bug | 500 generic | retry once, then park |

## 17.3 HTTP boundary

- All handler errors funnel through one middleware → RFC 7807 body (`type`, `title`, `status`, `code`, `detail`, `request_id`, optional `fields{}` for 422).
- 5xx and upstream (502/503) responses are additionally written to `audit_logs` category `api_error` (AUD-01) with sanitized detail.
- Panics: recovered by middleware → 500 + stack to logs (never to client) + audit entry. A panic in a WS bridge closes that session with code 1011, audited with reason.

## 17.4 Job boundary (workers)

- Retry policy per task type (sync: 5 attempts exp. backoff+jitter; notifications: 3; janitor: none). After final failure the job is **parked** in Asynq's dead/archived set — visible via `/system/info` and a `proxui_jobs_parked` metric, replayable by an admin.
- Idempotency guarantees make retries safe: upsert-based sync, deterministic job IDs, delivery rows guarded by unique keys.
- The circuit breaker ([10 §10.5](10-sync-engine.md)) prevents retry storms against a dead platform; breaker state changes are events, not just logs.

## 17.5 Frontend

- The generated API client normalizes RFC 7807 into a typed `ApiError`; global handling: 401 → silent refresh, then re-login preserving return URL; 403 → inline "no permission" states (never a blank page); 5xx/network → toast with retry + `request_id`; 422 → field-level form errors from `fields{}`.
- TanStack Query retries idempotent GETs ×2 with backoff; mutations never auto-retry.
- WS disconnects: exponential reconnect with "live updates paused" banner; console disconnects show the close-code-specific panel ([13 §Console](13-ui-design.md)).
- An `ErrorBoundary` per route keeps one crashed view from taking down the shell; boundary hits report to the log endpoint with the component stack.

## 17.6 User-facing message rules

- Say what happened, what it means, what to do: "Console unavailable — platform `pve-dc1` is unreachable (last seen 4 m ago). Inventory shown may be stale." 
- Auth failures are deliberately vague ("invalid credentials") — no username/password disambiguation, no lockout-state leakage beyond the generic 423 message.
- Admin-facing surfaces (platform test, sync errors) are deliberately precise — they are the debugging tool.
