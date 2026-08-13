# 11 — Project & Folder Structure

Monorepo — one repository holds backend, frontend, deploy assets, and docs. **Rationale:** solo developer, atomic changes across API contract + client, single CI pipeline.

## 11.1 Repository layout

```
proxui/
├── cmd/
│   └── proxui/
│       ├── main.go              # flag parsing, config load, DI wiring, role dispatch
│       └── connectors.go        # blank imports registering connectors (the plugin manifest)
├── internal/
│   ├── domain/                  # ── pure domain: no external imports ──
│   │   ├── identity/            # User, Role, Session, password/lockout invariants
│   │   ├── inventory/           # VM, Host, StoragePool, Network, VMGroup, sync_state rules
│   │   ├── sync/                # SyncRun, reconciliation value objects, DomainEvent defs
│   │   ├── telemetry/           # Sample, MetricKind, range→rollup selection
│   │   ├── auditing/            # AuditEntry, categories, actions
│   │   └── notification/        # Channel, Rule, AlertRule, severity, cooldown logic
│   ├── app/                     # ── application layer (CQRS handlers + ports) ──
│   │   ├── ports/               # ALL interfaces: repositories, Clock, Crypto, Queue,
│   │   │                        #   EventBus, Mailer, Authenticator, ConnectorProvider
│   │   ├── command/             # auth_login.go, platform_create.go, vm_power.go,
│   │   │                        #   console_create.go, user_create.go, ... (one file each)
│   │   └── query/               # vm_list.go, dashboard.go, vm_metrics.go, audit_search.go ...
│   ├── connector/               # connector port: interfaces, registry, normalized records,
│   │   └── connectortest/       #   + shared conformance test suite
│   ├── connectors/
│   │   ├── proxmox/             # client.go, inventory.go, metrics.go, console.go, power.go
│   │   └── mock/                # fixtures, mutation & failure injection
│   ├── infra/                   # ── adapters implementing app/ports ──
│   │   ├── postgres/            # sqlc-generated + hand-written repos, migrations/ (goose SQL)
│   │   ├── redis/               # cache, locks, rate limiter, pub/sub event bus
│   │   ├── queue/               # Asynq client/server/scheduler setup
│   │   ├── crypto/              # argon2id, envelope encryption (AES-256-GCM), JWT keys
│   │   ├── notify/              # smtp.go, slack.go, webhook.go (HMAC signing)
│   │   └── config/              # env parsing, settings-table snapshot w/ pub/sub refresh
│   ├── transport/
│   │   ├── http/                # chi router, middleware (auth, RBAC, ratelimit, reqid,
│   │   │                        #   logging, recover), oapi-codegen handlers, RFC7807 mapping
│   │   └── ws/                  # console bridge, events broadcaster (scoped fan-out)
│   └── jobs/                    # Asynq task handlers: sync_inventory.go, sync_metrics.go,
│                                #   alerts_evaluate.go, outbox_relay.go, janitor.go, notify_send.go
├── api/
│   └── openapi.yaml             # OpenAPI 3.1 source of truth (server + TS client generated)
├── web/                         # frontend (see 11.3); `go:embed` of web/dist into the binary
├── deploy/
│   ├── compose/                 # docker-compose.yml, compose.prod.yml, Caddyfile, .env.example
│   └── k8s/                     # future-option manifests (see 14-deployment.md)
├── docs/                        # this design package + ADRs (docs/adr/NNN-*.md)
├── scripts/                     # dev.sh, seed.sh, backup.sh, restore.sh, loadgen/
├── .github/workflows/           # ci.yml, release.yml
├── Dockerfile                   # multi-stage: web build → go build → distroless
├── Makefile                     # make dev, test, lint, gen, migrate, build, loadtest
├── CLAUDE.md                    # AI agent guide (see 23-ai-native-development.md)
└── go.mod
```

**Enforced dependency rule** (golangci-lint `depguard`): `domain` imports stdlib only; `app` imports `domain`; `infra`/`transport`/`jobs` import `app`+`domain`; nothing imports `cmd`. `connectors/*` import only `connector`. Violations fail CI.

## 11.2 Backend structure notes

- **CQRS shape:** each command/query is a struct + `Handle(ctx, input) (output, error)` — no mediator framework; the HTTP handler calls the handler directly. Commands write, audit, and append outbox events in one transaction (a `UnitOfWork` port provides the tx scope). Queries may use hand-tuned SQL and return read-model DTOs directly.
- **DI:** `cmd/proxui/main.go` builds the graph explicitly: config → pgx pool → repos → crypto → connector provider → handlers → router/workers. ~80 lines, no framework, trivially debuggable.
- **Code generation:** `make gen` runs sqlc (queries → typed Go), oapi-codegen (openapi.yaml → server interfaces + models), and openapi-typescript (→ `web/src/api/schema.d.ts`). Generated code is committed; CI verifies it's current (`git diff --exit-code` after gen).

## 11.3 Frontend structure (`web/`)

```
web/
├── src/
│   ├── api/                 # generated schema.d.ts + thin fetch client (auth refresh interceptor)
│   ├── app/                 # router (React Router), providers (Query, Theme, Auth), layout shell
│   ├── components/          # shared: DataTable (TanStack, virtualized), StatCard, StateBadge,
│   │                        #   TimeRangePicker, ConfirmDialog, EmptyState, ErrorBoundary
│   ├── features/
│   │   ├── auth/            # login, mfa, password-change pages; useAuth store
│   │   ├── dashboard/
│   │   ├── inventory/       # vm-list, vm-detail (tabs: overview, performance, history, console)
│   │   ├── console/         # NoVncViewer (lazy), SerialTerminal (lazy), session toolbar
│   │   ├── metrics/         # MetricChart (ECharts wrapper), range logic
│   │   ├── platforms/       # list, form (schema-driven fields), test-connection, sync history
│   │   ├── hosts/ storage/ networks/
│   │   ├── audit/           # filter bar, table, CSV export
│   │   ├── admin/           # users, user-groups, vm-groups, grants
│   │   ├── notifications/   # channels, rules, alert-rules, delivery log
│   │   └── settings/
│   ├── lib/                 # ws client (reconnecting, scoped events), formatters (bytes, uptime),
│   │                        #   permissions.ts (role→capability map mirroring RBAC-02)
│   └── styles/              # tailwind.css, theme tokens (light/dark via class strategy)
├── index.html  vite.config.ts  tailwind.config.ts  package.json
```

- **State:** TanStack Query for all server state (no Redux); WS events invalidate/patch query caches. Local UI state via component state + a small Zustand store for auth/theme.
- **Feature-folder rule:** a feature imports `components`/`lib`/`api`, never another feature — keeps the SPA modular the same way `depguard` keeps Go honest.
- **Serving:** `make build` produces `web/dist`, embedded via `go:embed`; the Go binary serves the SPA with immutable-asset caching and an index fallback. One artifact ships everything.
