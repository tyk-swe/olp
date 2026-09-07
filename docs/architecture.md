# Architecture and change map

One Rust package owns production code in `src/`. `src/main.rs` starts the process CLI; `src/lib.rs` names the feature owners. The console mirrors these owners under `console/src/lib/features/`.

| Change | Start here |
| --- | --- |
| Provider configuration, models, credentials, certification, revisions | `src/providers/` and `console/src/lib/features/providers/` |
| Route drafts, target selection, publication, history | `src/routes/` and `console/src/lib/features/routes/` |
| Users, sessions, OIDC, API keys, permissions, audit | `src/access/` and `console/src/lib/features/access/` |
| Installation settings | `src/settings/` and `console/src/lib/features/settings/` |
| Admission, request execution, retries, cancellation | `src/inference/` |
| Canonical operations and OpenAI, Anthropic, Gemini codecs | `src/protocols/` |
| Immutable runtime publication, activation, authority refresh | `src/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `src/limits/` |
| Accounting, pricing, ingestion, request history, retention | `src/usage/` and `console/src/lib/features/usage/` |
| Uploads, durable media jobs, reconciliation | `src/media/` and `console/src/lib/features/media/` |
| Configuration, connections, startup, shutdown | `src/process/`, `src/database.rs` |
| HTTP middleware, development origin, body limits | `src/http/` |
| Outbound networking and secrets | `src/net/`, `src/crypto/` |
| Metrics, traces, readiness, worker health | `src/observability/` |

Feature functions accept concrete pools and own their SQL and transactions. Mutations receive audit provenance explicitly. Management operations are registered with `utoipa-axum`; `/api/v3/openapi.json` and the ignored TypeScript contract come from that registration.

An inference request pins one immutable runtime snapshot. The executor in `src/inference/executor.rs` uses `src/inference/lifecycle.rs` to own attempts, reservations, cancellation, streaming commitment, accounting, and completion. Key authority refresh remains independent of runtime activation, so a failed connector activation cannot retain revoked authorization. Workers are kept with the feature they maintain and composed by process startup.

PostgreSQL is authoritative. Valkey coordinates limits and delivers hints and accounting events; retries and deduplication protect recovery. Media reservations remain durable across request cancellation and worker restart, and retained jobs can resolve the historical credential they were admitted with.

Gateway, control, worker, and all-in-one process modes share feature implementations. The process module supplies only the dependencies and listeners required by each mode. Production serves the static console; development uses Vite and proxies API traffic through the browser origin.

See [CONTRIBUTING.md](../CONTRIBUTING.md) for setup, checks, integration, and the fresh-installation contract.
