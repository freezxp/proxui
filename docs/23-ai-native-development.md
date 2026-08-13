# 23 — AI-Native Development Guide

This project is designed to be built and maintained with AI coding agents as first-class collaborators. This document defines how the repo stays agent-legible and what workflow the (solo) developer uses with agents. The operational entry point for agents is the root [`CLAUDE.md`](../CLAUDE.md).

## 23.1 Why this design is agent-friendly (by construction)

| Design property | Agent benefit |
|---|---|
| Strict layer rules enforced by `depguard` | An agent cannot merge architecture violations — CI teaches the boundary |
| One file per command/query (`app/command/*.go`) | Small, pattern-repeating units: "add endpoint X" is a well-trodden 4-file recipe |
| Contracts as artifacts (`api/openapi.yaml`, sqlc SQL, permission map) | Agents edit declarative sources; `make gen` produces the plumbing; drift is CI-caught |
| Generated RBAC matrix test | Security regressions from generated code are structurally detected |
| Mock connector + testcontainers | Agents can run the *entire* verification loop locally/CI with zero real infrastructure |
| FRS requirement IDs (AUTH-03, SYNC-04…) | Unambiguous task language: prompts and commits reference IDs, tests name them |
| ADRs in `docs/adr/` | Agents read past decisions instead of re-deriving (or contradicting) them |

## 23.2 Repository conventions for agent legibility

- **`CLAUDE.md` (root):** build/test commands, layer map, recipes, guardrails. Kept under ~150 lines; links here for depth. Updated in the same PR as any convention change (doc drift is a review-blocking defect).
- **Requirement traceability:** commits and PRs cite FRS IDs (`feat(sync): mark-and-sweep deletion [SYNC-03]`); tests covering a requirement name it in the test name (`TestSync_MissingThreeTimes_SYNC03`).
- **Recipes (`docs/ai/recipes/`):** step-by-step playbooks for the repeating tasks, each listing exact files to touch and the verification command:
  - `add-endpoint.md` — openapi.yaml → `make gen` → handler → permission-map entry → command/query → tests
  - `add-migration.md` — goose file → sqlc queries → `make gen` → repo test
  - `add-connector.md` — mirrors [09 §9.6](09-connector-architecture.md)
  - `add-audit-event.md`, `add-setting.md`, `add-notification-category.md`
- **Anchor comments:** sparse `// AGENT-ANCHOR: <topic>` markers at the few non-obvious seams (visibility query, outbox relay, console bridge lifecycle) so searches land precisely.
- **Machine-checkable everything:** style is `gofmt`/`golangci-lint`/`prettier` — agents never guess formatting; layer rules, RBAC coverage, and contract drift are all CI checks, not review lore.

## 23.3 Agent workflow (how the solo dev works)

1. **Plan against the docs:** point the agent at the FRS ID + relevant design doc section; require a short plan referencing both before code. Deviations from design docs require an ADR in the same PR.
2. **Implement via recipes:** recipe-shaped tasks are delegated wholesale; novel tasks get smaller steps with human checkpoints at contract edits (openapi.yaml, migrations, permission map — the blast-radius files).
3. **Verify like CI:** agents run `make lint test` locally; a change isn't "done" on compilation, only on the same gates CI enforces ([18 §18.3](18-testing-strategy.md)).
4. **AI review pass:** every PR gets an automated review (e.g. `/code-review`) before self-merge — the solo dev's second pair of eyes; findings triaged, not auto-applied.
5. **Security boundaries for agents:** agents never handle real secrets (dev uses mock connector + generated dev secrets); `.env`, `secrets/` are git-ignored and listed as do-not-read in `CLAUDE.md`; gitleaks backstops.

## 23.4 Documentation freshness rules

- Design package (docs 01–22) is **normative at v1.0**; afterwards, changes land as ADRs + targeted edits — never let docs describe an imagined system.
- `api/openapi.yaml` and migration files are *always* truthful (generated-code checks force it) — when prose and contract disagree, the contract wins and the prose gets fixed.
- Each runbook ends with `Last verified: <date>` — the quarterly restore drill updates it.
