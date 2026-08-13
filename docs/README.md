# ProxUI — VM Access Portal: Technical Design Package

A self-hosted portal giving administrators, infrastructure engineers, and operations teams a single dashboard to view virtual machines (scoped by permission), open VM consoles in the browser, and check VM performance — starting with Proxmox VE, extensible to other platforms via a connector framework.

**Design context (agreed with stakeholder, 2026-08-13):**

| Decision | Choice |
|---|---|
| Backend | Go 1.23+ |
| Frontend | React 18 + TypeScript |
| Tenancy | Internal single-organization tool (no SaaS provisions) |
| Scale target | 1–3 Proxmox clusters, < 500 VMs, tens of users |
| Console access | Backend WebSocket proxy (noVNC/xterm.js) |
| Authentication | Built-in (argon2id + JWT, optional TOTP) |
| Platform auth | Proxmox API tokens, PVE 8.x–9.x (verified against 9.2) |
| Metrics retention | 1 year+ (TimescaleDB) |
| Notifications | Sync failures, VM state changes, perf thresholds, security events |
| Deployment | Docker Compose on VMs (Kubernetes documented as future option) |
| Team | Solo developer, flexible timeline, backend first |

## Document index

| # | Document | Covers deliverables |
|---|---|---|
| 01 | [Executive Summary](01-executive-summary.md) | 1 |
| 02 | [Product Requirements (PRD)](02-prd.md) | 2 |
| 03 | [Functional Requirements (FRS)](03-frs.md) | 3 |
| 04 | [Non-Functional Requirements](04-nfr.md) | 4 |
| 05 | [System Architecture](05-system-architecture.md) | 5, 6 (HLA diagram) |
| 06 | [Sequence Diagrams](06-sequence-diagrams.md) | 7 |
| 07 | [Database Design & ERD](07-database-design.md) | 8, 15 |
| 08 | [API Specification](08-api-specification.md) | 9 |
| 09 | [Connector Architecture](09-connector-architecture.md) | 10 |
| 10 | [Synchronization Engine](10-sync-engine.md) | 11 |
| 11 | [Project & Folder Structure](11-project-structure.md) | 12, 13, 14 |
| 12 | [Domain Model & Class Diagram](12-domain-model.md) | 16, 17 |
| 13 | [UI Design](13-ui-design.md) | UI design section |
| 14 | [Deployment Design](14-deployment.md) | 18, 19 |
| 15 | [Security Design](15-security-design.md) | 20 |
| 16 | [Logging & Monitoring](16-observability.md) | 21, 22 |
| 17 | [Error Handling Strategy](17-error-handling.md) | 23 |
| 18 | [Testing Strategy](18-testing-strategy.md) | 24 |
| 19 | [CI/CD Pipeline](19-cicd.md) | 25 |
| 20 | [Roadmap & Sprint Plan (20 sprints)](20-roadmap-sprints.md) | 26, 27 |
| 21 | [Risk Assessment](21-risk-assessment.md) | 28 |
| 22 | [Future Enhancements](22-future-enhancements.md) | 29 |
| 23 | [AI-Native Development Guide](23-ai-native-development.md) | AI-native docs (with root `CLAUDE.md`) |

## How to read this package

- Start with 01–04 for *what* is being built and why.
- 05–12 are the engineering core: architecture, data, APIs, connectors, sync.
- 13–19 cover delivery concerns: UI, deployment, security, operations, quality.
- 20–23 cover execution: plan, risks, future, and AI-assisted workflow.

Every significant decision carries a **Rationale** so a future maintainer (or AI agent) understands why, not just what.
