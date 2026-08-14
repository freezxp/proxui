# ProxUI — VM Access Portal

Self-hosted web portal for virtualization estates: a single permission-scoped dashboard for VM inventory, in-browser consoles (noVNC, proxied through the backend), and up to a year of performance history — starting with Proxmox VE, extensible to other platforms through a connector framework.

**Status:** v1.0.0-rc.1. All 20 sprints implemented and verified against a live Proxmox VE 9.2.10 cluster. See the [changelog](CHANGELOG.md) for what is done, what is measured, and what is knowingly missing.

## Running it

```bash
cp .env.example .env          # set PROXUI_MASTER_KEY and the bootstrap admin
make dev                      # postgres+timescale, redis, api, vite
```

Then add a platform at `/platforms`: the form is built from the connector's own schema, and **Test connection** must pass before Save is enabled.

```bash
make ci        # everything CI enforces: tidy, lint, govulncheck, tests, build
make test-integration   # adds the database-backed tests
```

Back up before upgrading — migrations are forward-only:

```bash
PROXUI_DATABASE_URL=postgres://... scripts/backup.sh
```

## Stack (decided)

Go backend (Clean Architecture, single role-switchable binary) · React + TypeScript SPA · PostgreSQL 16 + TimescaleDB · Redis 7 + Asynq · Docker Compose deployment · built-in auth (argon2id / JWT / TOTP).

## Key documents

| | |
|---|---|
| [Design package index](docs/README.md) | the full design package + locked stakeholder decisions |
| [Runbooks](docs/24-runbooks.md) | backup and restore, platform not syncing, consoles failing, lost master key |
| [Security checklist](docs/25-security-checklist.md) | ASVS L2 pass with evidence, and what is not met |
| [Changelog](CHANGELOG.md) | releases, measurements, known limitations |
| [Executive summary](docs/01-executive-summary.md) | what and why in two pages |
| [System architecture](docs/05-system-architecture.md) | layering, diagram, every tech decision with rationale |
| [Sprint plan](docs/20-roadmap-sprints.md) | 20 sprints, backend first |
| [CLAUDE.md](CLAUDE.md) | guide for AI coding agents working in this repo |
| [ADRs](docs/adr/) | decisions that deviate from the design, with reasoning |
