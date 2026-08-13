# 18 — Testing Strategy

## 18.1 Shape: a pragmatic pyramid

| Layer | Tooling | Scope | Target |
|---|---|---|---|
| Unit (domain + app) | `go test`, table-driven; in-memory fakes for all ports | invariants (lockout, missing×3, rollup selection, alert cooldown), command/query handlers | ≥ 85% coverage of `domain/` + `app/`; fast (<10 s) |
| Repository/integration | testcontainers-go: Postgres+Timescale, Redis | every sqlc query, migrations up from zero, hypertable policies, outbox relay, rate limiter | all repos covered; runs in CI on every PR |
| Connector conformance | `connectortest.Run` suite | contract rules per capability, run against **mock** always and **proxmox against an in-tree fixture API server**, plus opt-in live tests against a real cluster (`PROXUI_LIVE_PVE_*`, verified on PVE 9.2) | every connector, every capability |
| API/e2e (backend) | spawned binary (`--role=all`) + testcontainers + mock connector; requests via generated client | auth flows incl. refresh rotation/reuse, RBAC matrix (every endpoint × every role — generated from the permission map), sync lifecycle, console ticket flow (fake VNC echo endpoint), rate limits | the RBAC matrix test is the security backbone; must be exhaustive |
| Frontend unit/component | Vitest + Testing Library | permission-aware rendering, formatters, ws cache patching | key components |
| Frontend e2e | Playwright vs. the real binary + mock connector | login→dashboard→filter→VM detail→console (echo)→admin flows; light/dark screenshots; responsive breakpoints | the 8 golden paths, on every PR |
| Load | `scripts/loadgen` (mock connector at 2,000 VMs, k6 for HTTP) | NFR-P1/P2/P4 assertions | nightly + before release |
| Security | govulncheck, npm audit, Trivy, gitleaks in CI; ZAP baseline vs. staging; manual console-path pentest pre-v1 | — | CI-blocking |

## 18.2 Test design decisions & rationale

- **Mock connector as the universal test double** ([09 §9.5](09-connector-architecture.md)): the entire stack is testable with zero Proxmox infrastructure — CI needs only Docker. Recorded go-vcr cassettes keep the real-Proxmox client honest without a live cluster; a nightly optional job runs the conformance suite against a real lab cluster when its secret is configured.
- **RBAC matrix as generated test:** the route→permission map that enforces authz at boot also *generates* the test cases (all roles × all routes × in-scope/out-of-scope), so a new endpoint automatically gains denial tests. This is the single highest-value test in the project.
- **Time control:** `Clock` port everywhere; lockout windows, token expiry, missing×3, cooldowns are tested with a fake clock, not sleeps.
- **Migration discipline:** CI runs `goose up` from empty *and* from the previous release's schema + seeded data (upgrade-path test).
- **Determinism:** no test talks to the internet; testcontainers pinned by digest; `-race` on all Go tests.

## 18.3 Quality gates (CI-blocking)

1. `make lint` — golangci-lint (incl. depguard layer rules), eslint, prettier, `go vet`.
2. `make gen && git diff --exit-code` — generated code (sqlc, oapi, TS client) is current.
3. Unit + integration + conformance + API e2e green, `-race`, coverage floor 80% on `domain/`+`app/` (ratchet, never lowered).
4. Playwright golden paths green.
5. Vulnerability scans: no criticals (documented, expiring exceptions only).
6. Docker image builds & `--role=all` boots against fresh DB in CI (smoke).

## 18.4 Definition of Done (per feature)

Code + tests at the right layers + OpenAPI updated + audit events wired + permission map entry + docs touched (including runbook if operational) + demo-able via mock connector in dev.
