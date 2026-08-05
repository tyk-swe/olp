# apps/olp — delivery crate guide

`olp` owns process composition and HTTP delivery. Transport-neutral request
execution belongs in `olp-inference`; SQL and provider networking remain in
their designated library crates.

## Source ownership

- `bootstrap/` — CLI/configuration, process-mode validation, dependency and
  provider construction, runtime activation, listeners, workers, supervision,
  shutdown, and the private optional assembly state.
- `public_http/` — listener/router composition and shared HTTP-boundary policy:
  admission, trusted proxies, cookies, public origin, media/body policing,
  problems, and response rendering.
- `gateway/` — protocol-specific Axum adapters and the canonical inference
  endpoint registry in `endpoint_policy.rs`. Execution delegates to the narrow
  `InferenceService`.
- `management/` — the complete `/api/v1` control plane: auth/session policy,
  `access/{profile,users,invitations}`, configuration resources, operations,
  OIDC, playground, and OpenAPI.
- `observability/` — the private health/metrics listener and its narrow state.
- `console/` — embedded static-console fallback and asset delivery.

`bootstrap::mode_dependencies` defines fully required routed states.
`ManagementState` and `ObservabilityState` do not inherit or dereference
`GatewayState`; the playground receives only its explicit inference capability.
The optional `ProcessComposition` input is private bootstrap machinery in
normal builds; external fixtures reach it only through the `test-util`-gated
`olp::test_support` namespace.

## Similar names

- Inference selection, failover, distributed leases, telemetry finalization,
  circuits, and runtime snapshots are in `crates/inference/src/`.
- `gateway/media_jobs.rs` is the HTTP adapter; inference-side media execution
  and reconciliation are exposed by `InferenceService`; persistence is under
  `crates/storage/src/media_jobs/`.
- `management/operations/usage/*` renders HTTP responses; the matching
  `crates/storage/src/usage/*` files own SQL reads.
- `gateway/multipart.rs` assembles provider-bound requests;
  `public_http/request_admission/multipart.rs` polices inbound sizes.

## Notes

- `examples/export_openapi.rs` emits `openapi/management.json`; regenerate with
  `make openapi` after endpoint or schema changes.
- Ignored service-backed suites live under `tests/integration/` and run through
  `make db-test`. Long configuration and OIDC lifecycles keep one database
  transaction/order while placing resource phases and mock-browser/IdP
  harnesses in sibling modules.
- Extend `gateway/endpoint_policy.rs`; never add a parallel endpoint registry.
