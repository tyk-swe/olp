# Architecture and change map

The Go module owns production code under `internal/`; `cmd/olp` starts the CLI.
The console mirrors feature ownership under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/database/migrations/`.

| Change | Start here |
| --- | --- |
| Provider configuration, models, credentials, certification, revisions | `internal/providers/` and `console/src/lib/features/providers/` |
| Route drafts, target selection, publication, history | `internal/routes/` and `console/src/lib/features/routes/` |
| Users, sessions, OIDC, projects, API keys, budgets, tokens, audit | `internal/access/` and `console/src/lib/features/access/` |
| Installation settings and notification destinations/rules | `internal/access/` and console access/settings features |
| Configuration export, plan, and apply | `internal/configuration/` |
| Content-policy validation and matching | `internal/contentpolicy/` |
| Provider-resource mappings and stored-response accounting | `internal/resources/` and `internal/gateway/` |
| Admission, request execution, retries, cancellation | `internal/gateway/` |
| Canonical operations and OpenAI, Anthropic, Gemini, Bedrock codecs | `internal/protocols/` |
| Immutable runtime publication, activation, authority refresh | `internal/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `internal/limits/` |
| Accounting, pricing, request history, ingestion, retention, notification delivery | `internal/usage/` and `console/src/lib/features/usage/` |
| Uploads, durable media jobs, reconciliation | `internal/media/` and `console/src/lib/features/media/` |
| Configuration, connections, startup, shutdown | `internal/config/`, `internal/process/`, `internal/database/` |
| HTTP middleware, development origin, body limits | `internal/process/` |
| Outbound networking and secrets | `internal/egress/`, `internal/secrets/` |
| Metrics, traces, readiness, worker health | `internal/observability/` |

Feature packages own their SQL, transactions, and workflows; mutations receive
explicit audit provenance. `openapi/management.json` defines the management
contract. `make api` generates Go transport types and ignored TypeScript
declarations; `/api/v3/openapi.json` serves the embedded contract. Integration
tests check handler/contract parity.

Inference pins an immutable runtime snapshot. `internal/gateway/attempts.go`
owns shared attempt progression, reservations, settlement, health and failover
for canonical inference and ordinary media. `executor.go` and `media.go` retain
their transport deadlines, streaming commitment and delivery; adjacent resource,
video, Bedrock, and realtime paths handle their specific lifecycles. Protocol
codecs live in `internal/protocols/`. Independent key-authority refresh prevents
a failed activation from retaining revoked access.

PostgreSQL owns durable state. Valkey coordinates limits, hints, and accounting
events; retries and deduplication support recovery. Durable media jobs and
provider resources retain their admitted credential references through rotation
and restart. Workers remain with the feature they maintain.

Process startup composes gateway, control, worker, and all-in-one modes, owning
resource lifetimes and listeners. Draining, metadata delivery, worker shutdown,
and trace flushing share one deadline. Production serves static console assets;
development uses Vite with configured API proxies.

See [gateway execution](gateway.md), [access control](access.md), and the
[worker runbook](operations.md#replicated-worker-health) for their operational
contracts. See [CONTRIBUTING.md](../CONTRIBUTING.md) for setup, checks,
integration, and the fresh-installation contract.
