# `apps/olp` delivery guide

This crate owns process composition and HTTP delivery. Transport-neutral
execution belongs in `olp_engine::inference`; SQL remains in `olp-db`, and
provider networking belongs in `olp_engine::providers`.

## Source ownership

- `bootstrap/`: CLI/configuration, mode validation, dependency composition,
  provider activation, listeners, workers, supervision, and shutdown. Concrete
  connector construction remains in `olp_engine::providers`.
- `public_http/`: listener/router composition and shared admission, proxy,
  cookie, origin, media/body, problem, and response policy.
- `gateway/`: protocol Axum adapters and the sole endpoint registry,
  `endpoint_policy.rs`; execution delegates to `InferenceService`.
- `management/`: `/api/v1` auth/session, access, configuration, operations,
  OIDC, playground, and OpenAPI resources.
- `observability/`: private health/metrics listener and narrow state.
- `console/`: embedded static-console fallback and assets.

`bootstrap::mode_dependencies` builds complete routed states. Management and
observability never dereference `GatewayState`; the playground receives only
its explicit inference capability. `ProcessComposition` is private bootstrap
machinery except for the `test-util`-gated `olp::test_support` namespace.

## Similar names

- Selection, failover, leases, circuits, snapshots, telemetry, and terminal
  accounting are in `crates/olp-engine/src/inference/`.
- `gateway/media_jobs.rs` is an HTTP adapter; execution/reconciliation belongs
  to `InferenceService` and persistence to `crates/olp-db/src/media_jobs/`.
- `management/operations/usage/` renders responses; matching SQL is under
  `crates/olp-db/src/usage/`.
- `gateway/multipart.rs` assembles provider requests; inbound size policing is
  `public_http/request_admission/multipart.rs`.

Regenerate `openapi/management.json` with `make openapi`; ignored service suites
use `make db-test`. Extend the existing endpoint policy; never add a parallel
registry.
