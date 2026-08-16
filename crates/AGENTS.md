# `crates` production library guide

- `olp-engine` owns the application model and transport-neutral behavior. Its
  `domain/` module contains canonical types, provider configuration, routing,
  and ports; `protocols/` contains OpenAI/Anthropic/Gemini DTOs, translation,
  and bounded streams; `providers/` contains outbound/OIDC networking, auth,
  egress, endpoint building, vendor errors, and explicit factories; and
  `inference/` contains execution, generation pinning, selection, circuits,
  failover, limits, event collection, terminal accounting, telemetry, and
  `inference::service::Service`. Cross-crate persistence DTOs and ports belong
  under `inference/` when they support runtime execution.
- `olp-db` implements engine persistence ports with PostgreSQL and Valkey and
  owns encryption, migrations, and subsystem query namespaces
  (`authentication`, `configuration`, `identity`, `idempotency`, `media_jobs`,
  `oidc`, `operations`, `request_metadata`, `runtime`, `security`, and `usage`).

Dependencies follow the role graph: engine is the base, database code depends
on engine-defined ports, and delivery composes both at the process boundary.
The engine must not depend on `olp-db`. Every package declares
`[package.metadata.olp]`; the boundary checker enforces role edges and
infrastructure ownership. `olp_engine::providers` owns Reqwest/AWS/Google auth,
database code owns SQLx/Redis, and delivery owns Axum/Tower/Clap. Keep concrete
provider construction inside `olp-engine/src/providers/` and do not restore
re-export facades. Every public item has one defining owner-module path; use
paths such as `domain::canonical::requests`, `providers::factory::assembly`,
`inference::service`, `olp_db::store`, and `olp_db::error`. Within the engine,
`domain` imports no sibling module,
`protocols` may use only `domain`, `providers` may use `domain` and `protocols`,
and `inference` may use all three. Only `providers` may use outbound networking
libraries; inference reaches the database through engine-owned ports.

Static PostgreSQL uses SQLx checked macros and committed `.sqlx` metadata.
Dynamic filters decode through typed subsystem records; string-key
`Row::get` is forbidden. Migrations are sequential and forward-only. Service
configuration/operations tests retain one ordered database lifecycle with
named sibling phases; ignored suites run through `make db-test`.

Keep vendor-specific policy inside `olp-engine/src/providers/`. Shared bounded
response bodies live in `olp-engine/src/providers/transport_io.rs`; deadline
and event-stream mechanics live under
`olp-engine/src/providers/transport_io/`. Unit tests stay beside their owners.
