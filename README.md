# ProxUI — VM Access Portal

Self-hosted web portal for virtualization estates: a single permission-scoped dashboard for VM inventory, in-browser consoles (noVNC, proxied through the backend), and up to a year of performance history — starting with Proxmox VE, extensible to other platforms through a connector framework.

**Status:** design phase. The complete technical design package lives in [`docs/`](docs/README.md) — start there. No application code yet; implementation follows the [roadmap & sprint plan](docs/20-roadmap-sprints.md).

## Stack (decided)

Go backend (Clean Architecture, single role-switchable binary) · React + TypeScript SPA · PostgreSQL 16 + TimescaleDB · Redis 7 + Asynq · Docker Compose deployment · built-in auth (argon2id / JWT / TOTP).

## Key documents

| | |
|---|---|
| [Design package index](docs/README.md) | all 23 documents + locked stakeholder decisions |
| [Executive summary](docs/01-executive-summary.md) | what and why in two pages |
| [System architecture](docs/05-system-architecture.md) | layering, diagram, every tech decision with rationale |
| [Sprint plan](docs/20-roadmap-sprints.md) | 20 sprints, backend first |
| [CLAUDE.md](CLAUDE.md) | guide for AI coding agents working in this repo |
