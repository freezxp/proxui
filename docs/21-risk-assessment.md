# 21 — Risk Assessment

Likelihood/Impact: L/M/H. Owner is the solo dev unless noted; "trigger" = the early-warning signal to watch.

## 21.1 Technical risks

| # | Risk | L | I | Mitigation | Trigger / contingency |
|---|---|---|---|---|---|
| T1 | **Console proxy fragility** — VNC ticket timing, WS quirks across Proxmox point releases, proxy timeouts | M | H | Built + tested earliest practical (sprint 9); byte-level bridge (no RFB parsing); conformance cassettes per PVE version; Caddy WS timeouts tuned | Console e2e flakes in CI → pin PVE test matrix, add version detection to connector |
| T2 | Proxmox API changes in 8.x point releases / 9.0 | M | M | Token-authed stable `/api2/json` endpoints only; version captured at TestConnection; nightly lab conformance run | Lab run fails → version-gated code paths in connector only |
| T3 | TimescaleDB operational surprises (aggregate refresh lag, compression locks) | L | M | Managed via declarative policies; sizing shows 100× headroom; monitoring on aggregate freshness | Fallback: plain-PG partitioned rollup tables (schema kept compatible) |
| T4 | Metrics rate computation wrong (counter resets, VM migration between nodes) | M | L | Reset detection (counter decrease → drop sample), per-VM not per-node identity, unit tests with recorded sequences | Spot-check portal vs Proxmox graphs during S7 |
| T5 | WS scale on one box (consoles + event streams) | L | M | 10-session target is trivial for Go; load test includes 25 concurrent consoles | Degrade: cap concurrent consoles per settings |
| T6 | Solo-dev architecture drift (layers erode under time pressure) | M | M | depguard CI enforcement; ADR discipline; AI review pass on every PR | depguard exceptions accumulating → refactor sprint |

## 21.2 Security risks

| # | Risk | L | I | Mitigation | Contingency |
|---|---|---|---|---|---|
| S1 | **Portal compromise ⇒ hypervisor console access** (the concentrated-value risk) | L | H | Least-privilege platform tokens (no create/delete capability at PVE level); MFA for admins; short sessions; egress allow-list; append-only audit + optional SIEM mirror | Revoke the per-cluster token at Proxmox (single point of cutoff); IR runbook |
| S2 | Master key loss | L | H | Documented backup of key material separate from DB backups; rotation CLI | Accepted path: re-enter platform tokens (minutes of work) |
| S3 | Master key theft (host compromise) | L | H | Key never in DB or images; file perms; host hardening baseline in deploy docs | Rotate master key + all platform tokens |
| S4 | Built-in auth bugs (we own the IdP) | M | H | Boring, standard constructions only (argon2id, RFC-standard TOTP, rotation w/ reuse detection); exhaustive auth test suite; ASVS L2 + pentest gate | OIDC federation seam allows outsourcing identity later |
| S5 | Webhook/Slack secrets leakage via logs | M | M | Central redaction filter tested with canary secrets; secrets write-only in API | Rotate affected channel secrets |

## 21.3 Product / project risks

| # | Risk | L | I | Mitigation |
|---|---|---|---|---|
| P1 | Scope creep toward VM management (create/clone/etc.) | H | M | Non-goals stated in PRD; capability ceiling of the platform token makes it structurally impossible without an explicit decision (new token + ADR) |
| P2 | Solo-dev bus factor | H | H | This design package + AI-native docs ([23](23-ai-native-development.md)) make the project resumable by anyone; monorepo, boring stack, no exotic infra |
| P3 | Timeline drift ("flexible" becomes "never ships") | M | M | Sprint exit-demos are binary done/not-done; v0.5 (S10) and v0.8 (S15) are real usable checkpoints delivering value early |
| P4 | Future SaaS pivot despite "truly internal" decision | L | H | **Accepted consciously by stakeholder** (2026-08-13): no tenancy columns. Pivot cost = tenant-scoping migration across ~15 tables + auth rework, est. 4–6 weeks. Revisit trigger: first external-user conversation |
| P5 | Ops handover (portal outlives its author's attention) | M | M | Runbooks (S19), monitoring overlay, quarterly restore drill on the ops calendar, upgrade = compose pull |

## 21.4 Standing assumptions to re-verify

- Portal host can reach every cluster on 8006 (re-check when networks change).
- PVE 8.x remains the fleet baseline (connector tested against it).
- < 500 VMs / < 50 users (4× headroom designed; beyond that, revisit NFR-S2).
