# 22 — Future Enhancement Roadmap

Ordered by expected value for this deployment; every item names the seam that makes it cheap because of v1 design decisions.

## Near-term (v1.x)

| Enhancement | Builds on |
|---|---|
| Serial console (xterm.js, `termproxy`) + LXC console support | `ConsoleProvider` kind parameter + console shell already generic (CONS-06) |
| Hosts/Storage/Networks UI depth (charts, capacity trends) | data + metrics already synced from v1 |
| VM snapshots listing (read-only) on detail page | connector `attrs` + one collector method |
| Saved inventory views & CSV export | URL-persisted filters already exist |
| Prometheus remote-write out (feed an existing central Grafana) | metrics pipeline is one writer interface |

## Mid-term (v2)

| Enhancement | Builds on |
|---|---|
| **Second connector: VMware vSphere** (or libvirt) — the framework's proof in production | `internal/connector` contract + conformance suite; checklist in [09 §9.6](09-connector-architecture.md) |
| OIDC federation (corporate SSO) with built-in auth as break-glass | `Authenticator` port ([15 §15.1](15-security-design.md)) |
| Console session recording (RFB stream capture, retention-policied) | bridge already sees all bytes; storage + playback UI needed |
| Scheduled power actions & maintenance windows | Asynq scheduling + power commands exist |
| Per-VM-group operator permissions (finer than global role) | grants table gains a permission column; enforcement point is single ([05 §5.5](05-system-architecture.md)) |
| Capacity reporting (cluster headroom, growth trends, PDF export) | 1-year Timescale history |

## Long-term / strategic

| Enhancement | Notes |
|---|---|
| Multi-tenancy / SaaS | Consciously **not** provisioned (stakeholder decision, risk P4). If revived: tenant scoping migration, per-tenant crypto, OIDC multi-realm, K8s deployment become the program |
| Kubernetes production deployment | Design ready in [14 §14.4](14-deployment.md); adopt when org standardizes |
| Out-of-process connectors (hashicorp/go-plugin over gRPC) | Same interfaces re-exposed over RPC; enables third-party connectors without recompilation |
| OpenTelemetry tracing | Attach at existing request/job-ID middleware seams |
| Cloud connectors (AWS EC2, Azure, GCE) | Collectors map naturally; consoles become SSM/serial-port bridges — capability flags already model partial support |
| Read-only public status page / embed API | Query layer + scoped tokens |

## Explicit non-roadmap

VM provisioning/lifecycle management, configuration drift management, billing/chargeback — these turn the product into a CMP and change its security posture fundamentally (would require write-capable platform tokens). Any such move needs an ADR revisiting the least-privilege token ceiling first.
