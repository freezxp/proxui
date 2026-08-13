# ProxUI — Agent Guide

Self-hosted VM access portal: dashboard, browser consoles, performance history for Proxmox clusters. Go backend + React/TS frontend, monorepo. **Status: design phase — see `docs/` for the complete technical design package; no application code exists yet.**

## Commands (once scaffolded — sprint 1 establishes these)

```
make dev        # compose up (pg+timescale, redis) + api (--role=all, mock connector) + vite
make test       # unit + integration (testcontainers) + api e2e — same gates as CI
make lint       # golangci-lint (incl. depguard layer rules) + eslint + prettier
make gen        # sqlc + oapi-codegen + openapi-typescript; commit output; CI checks drift
make migrate    # goose up against dev DB
```

## Architecture in one breath

Clean Architecture, lightweight CQRS, single binary with roles (`api|worker|scheduler|all`). Import rule (CI-enforced): `domain` ← `app` ← `infra`/`transport`/`jobs`; `connectors/*` import only `internal/connector`. Postgres+TimescaleDB is the only store of record; Redis = cache/queue(Asynq)/pub-sub. Platform is source of truth for synced fields; portal owns `portal_tags`, `notes`, groups — the sets are disjoint, never merge.

## Key docs

- `docs/README.md` — package index + locked stakeholder decisions (don't re-litigate them; deviations need an ADR in `docs/adr/`)
- `docs/03-frs.md` — requirement IDs (cite them in commits/tests: `[SYNC-03]`)
- `docs/07-database-design.md` / `docs/08-api-specification.md` — data & API contracts
- `docs/09-connector-architecture.md` — how to add a platform
- `docs/ai/recipes/` — step-by-step playbooks for repeating tasks (endpoints, migrations, connectors, audit events)

## Guardrails

- Contract files have blast radius — flag changes to `api/openapi.yaml`, `internal/infra/postgres/migrations/`, the permission map, and crypto code in your summary.
- Every new endpoint needs: OpenAPI entry, permission-map entry (boot fails without it), audit event where state changes, tests at the layer that owns the logic.
- Never log or echo secrets; dev uses the mock connector — never point tests at a real cluster.
- Do not read `.env` or `secrets/`.
- Generated code is committed: after editing SQL/OpenAPI run `make gen`, commit the output.
- Audit table is append-only; migrations are forward-only (expand/contract for breaking changes).
