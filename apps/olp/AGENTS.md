# apps/olp — delivery crate map

The 30 top-level modules in `src/` are flat but layered:

- **Transport**: `listener` (bind/serve), `proxy` (trusted-proxy handling),
  `request_admission/` (capacity + body limits), `request_cookies`.
- **HTTP surfaces**: `gateway/` (inference data plane; `endpoint_policy.rs` is
  the endpoint registry), `management_api/` (`/api/v1` CRUD + auth),
  `operations/` (requests/usage/audit/settings read APIs), `oidc/`,
  `playground`, `static_console` (embedded SPA), `router` (composition),
  `observability` (private `127.0.0.1:9090` listener: health + metrics).
- **Infrastructure**: `circuit`, `connectors`, `media_spool`, `runtime`,
  `mode_dependencies`, `provider_adapter`, `cli/` (subcommands: all, gateway,
  control, worker, migrate, doctor, master-key, health-probe).
- **Shared utilities**: `event_completion`, `image_response`, `json_media`,
  `problem`, `public_origin`, `relative_url`, `semantic_validation`,
  `streaming_response`.

## Same-name modules — pick the right one

- `gateway/limits.rs` = per-route/key limit enforcement during inference;
  `request_admission/limits.rs` = process-wide admission capacity;
  `crates/storage/src/limits.rs` = persisted limit state.
- `gateway/media_jobs.rs` = inference-side job handling;
  `operations/media_jobs.rs` = read API; `crates/storage/src/media_jobs.rs` =
  persistence.
- `operations/usage/{breakdown,completeness,series,summary}.rs` = HTTP
  delivery; the identically named files under `crates/storage/src/usage/` =
  SQL queries.
- `gateway/multipart.rs` = provider-bound multipart assembly;
  `request_admission/multipart.rs` = inbound size policing.

## Notes

- `examples/` is build tooling, not documentation: `export_openapi` emits
  `openapi/management.json` (`make openapi`); `sdk_smoke_fixture` is built by
  `tests/sdk-smoke/run.sh`.
- `tests/*_http_postgres.rs` are `#[ignore]`d; run via `make db-test`.
- `apps/olp/src/gateway/endpoint_policy.rs` is the documented registry
  (CONTRIBUTING change map) — extend it, don't parallel it.
