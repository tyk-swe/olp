# `crates` production library guide

- `domain`: infrastructure-free canonical model, provider configuration, and
  routing; `routing.rs` is the narrow facade.
- `protocols`: OpenAI/Anthropic/Gemini DTOs, translation, and bounded streams.
- `providers`: all outbound/OIDC networking, auth, egress, endpoint building,
  vendor errors, and explicit factory APIs. Do not restore wildcard exports.
- `storage`: PostgreSQL, Valkey, encryption, migrations, and subsystem query
  namespaces (`authentication`, `configuration`, `identity`, `idempotency`,
  `media_jobs`, `oidc`, `operations`, `request_metadata`, `runtime`,
  `security`, and `usage`).
- `inference`: transport-neutral execution, generation pinning, selection,
  circuits, failover, distributed leases, event collection, terminal
  accounting, telemetry, and `InferenceService`.

Dependencies follow the role graph: domain is the base; protocol/provider code
points inward; storage stays separate from provider transport; inference
composes production libraries; delivery composes them at the process boundary.
Every package declares `[package.metadata.olp]`; the boundary checker enforces
role edges and infrastructure ownership. Inference must not depend on
Axum/Tower/Clap, SQLx/Redis, or concrete provider constructors.

Static PostgreSQL uses SQLx checked macros and committed `.sqlx` metadata.
Dynamic filters decode through typed subsystem records; string-key
`Row::get` is forbidden. Migrations are sequential and forward-only. Service
configuration/operations tests retain one ordered database lifecycle with
named sibling phases; ignored suites run through `make db-test`.

Keep vendor-specific policy inside provider trees. Shared bounded response
bodies live in `providers/src/transport_io.rs`; deadline and event-stream
mechanics live under `providers/src/transport_io/`. Unit tests stay beside
their owners.
