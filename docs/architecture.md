# Architecture and change map

The Go module owns production code under `internal/`; `cmd/olp` starts the CLI.
The console mirrors feature ownership under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/database/migrations/`.

| Change | Start here |
| --- | --- |
| Provider configuration, models, credentials, certification, revisions | `internal/providers/` and `console/src/lib/features/providers/` |
| Route drafts, target selection, publication, history | `internal/routes/` and `console/src/lib/features/routes/` |
| Users, sessions, OIDC, API keys, permissions, audit | `internal/access/` and `console/src/lib/features/access/` |
| Installation settings | `internal/access/` and `console/src/lib/features/settings/` |
| Admission, request execution, retries, cancellation | `internal/gateway/` |
| Canonical operations and OpenAI, Anthropic, Gemini codecs | `internal/protocols/` |
| Immutable runtime publication, activation, authority refresh | `internal/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `internal/limits/` |
| Accounting, pricing, ingestion, request history, retention | `internal/usage/` and `console/src/lib/features/usage/` |
| Uploads, durable media jobs, reconciliation | `internal/media/` and `console/src/lib/features/media/` |
| Configuration, connections, startup, shutdown | `internal/process/`, `internal/database/` |
| HTTP middleware, development origin, body limits | `internal/process/` |
| Outbound networking and secrets | `internal/egress/`, `internal/secrets/` |
| Metrics, traces, readiness, worker health | `internal/observability/` |

Feature functions accept concrete pools and own their SQL and transactions. Mutations receive audit provenance explicitly. The checked-in `openapi/management.json` defines the management contract. `make api` regenerates Go transport types and the ignored TypeScript declarations; `/api/v3/openapi.json` serves the embedded definition. Handler/contract parity is exercised by integration tests.

An inference request pins one immutable runtime snapshot. The executor in `internal/gateway/executor.go` uses `internal/gateway/execution.go` to own attempts, reservations, cancellation, streaming commitment, accounting, and completion. Key authority refresh remains independent of runtime activation, so a failed connector activation cannot retain revoked authorization. Workers are kept with the feature they maintain and composed by process startup.

PostgreSQL is authoritative. Valkey coordinates limits and delivers hints and accounting events; retries and deduplication protect recovery. Media reservations remain durable across request cancellation and worker restart, and retained jobs can resolve the historical credential they were admitted with.

Gateway, control, worker, and all-in-one process modes share feature implementations. The process module supplies only the dependencies and listeners required by each mode. Production serves the static console; development uses Vite and proxies API traffic through the browser origin.

See [CONTRIBUTING.md](../CONTRIBUTING.md) for setup, checks, integration, and the fresh-installation contract.
