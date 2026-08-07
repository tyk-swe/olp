# crates/ — production library guide

- `domain` — infrastructure-free canonical model, provider configuration, and
  routing. `routing.rs` is a narrow facade over provider, route, snapshot, and
  selection owners.
- `protocols` — OpenAI/Anthropic/Gemini wire DTOs, canonical translation, and
  bounded streaming codecs.
- `providers` — all outbound provider/OIDC networking, authentication, egress
  policy, endpoint construction, and vendor error mapping. Its factory API is
  explicit; never restore wildcard exports.
- `storage` — PostgreSQL, Valkey, encryption, migrations, and direct subsystem
  queries. Public callers import through namespaces such as `authentication`,
  `configuration`, `identity`, `idempotency`, `media_jobs`, `oidc`,
  `operations`, `request_metadata`, `runtime`, `security`, and `usage`.
- `inference` — transport-neutral application execution: generation pinning,
  selection, circuits, failover, distributed limit leases, event collection,
  terminal accounting, telemetry, and the shared `InferenceService`. It must
  not depend on Axum/Tower/Clap, SQLx/Redis, or concrete provider constructors.

Dependencies are one-way by architecture role: domain is the base;
protocol and provider code point inward; storage stays separate from provider
transport; inference composes the production library roles; delivery composes
them at the process boundary. Same-role dependencies are allowed so a
responsibility can be decomposed without changing policy. Every workspace
package declares its role under `[package.metadata.olp]`, and
`scripts/check-boundaries.sh` enforces role edges and infrastructure ownership.

Static PostgreSQL uses SQLx checked macros and committed `/.sqlx` metadata.
Dynamic filters decode through typed subsystem records; string-key `Row::get`
is forbidden by `scripts/check-storage-sqlx.sh`. Migrations remain sequential
and forward-only.

Service-backed configuration and operations contracts retain a single ordered
database lifecycle, with provider eligibility, query, and retention phases in
named sibling modules under `tests/integration/`.

Keep divergent vendor policy inside provider trees. Shared bounded response
bodies live in `providers/src/transport_io.rs`; deadline/event stream mechanics
live under `providers/src/transport_io/`. Unit tests stay beside their owner,
and ignored service-backed tests run with `make db-test`.
