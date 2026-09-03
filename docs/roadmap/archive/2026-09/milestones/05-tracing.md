# Milestone 5 — Distributed tracing (OpenTelemetry)

| | |
|---|---|
| Dates | Mon 2026-09-28 → Sun 2026-10-04 |
| Goal | A request can be followed client → gateway → provider attempt in any OTLP backend, with zero prompt or response content in spans, and with tracing off by default costing nothing |
| Backlog items | OTEL-01, OTEL-02, OTEL-03, OTEL-04 |
| Prerequisites | None beyond a green `main`; the design record (OTEL-01) is written and reviewed before code |

## OTEL-01 — Design record (S) — into `docs/architecture.md` before any code

- [x] Crates: `tracing-opentelemetry`, `opentelemetry_sdk`, `opentelemetry-otlp` with `http-proto` + `reqwest-client` features (reuses the existing `reqwest` line and avoids a `tonic` tree). Check `cargo tree`, `cargo deny check bans licenses`, and `make machete` before the first commit
- [x] Ownership: the exporter is delivery-side in `apps/olp/src/observability/tracing.rs`; outbound header injection lives in `olp_engine::providers` (`transport_common`). If `scripts/check-boundaries.sh` flags the OTLP crate under its networking-library rule, extend the ownership table deliberately and say why in the commit
- [x] Configuration, following the existing conventions (env-driven, CLI flag per setting, secrets as files): `OLP_OTLP_TRACES_ENDPOINT` (unset = off), `OLP_OTLP_HEADERS_FILE`, `OLP_TRACE_SAMPLE_RATIO` (default `1.0`), `OLP_TRACE_PROPAGATE_UPSTREAM` (default `true`), `OLP_TRACE_ACCEPT_INBOUND` (default `true`)
- [x] Attribute allowlist, written down first. Request span: surface, operation, route slug, key id, installation id, generation, status, error class, attempt count, time to first byte, total duration. Attempt span: provider kind, provider revision, model, outcome class, upstream status class, usage units when observed, pricing provenance. Never: prompts, outputs, tool payloads, headers, raw provider error bodies, credentials
- [x] Scope: traces only. Metrics stay Prometheus; logs stay `tracing` JSON

## OTEL-02 — Implementation (L)

- [x] `apps/olp/src/observability/tracing.rs`: layer construction, resource attributes (`service.name=openllmproxy`, `service.version`, process mode), bounded export queue, exporter shutdown inside the existing SIGTERM sequence
- [x] Request span at `public_http` admission — one per admitted request, `x-request-id` as an attribute; inbound `traceparent` honoured only when `OLP_TRACE_ACCEPT_INBOUND`, with caller-supplied `tracestate` discarded
- [x] Attempt span in `inference/execution` around each provider call; `traceparent` injected into outbound provider requests in `transport_common` when propagation is on; Bedrock through the SDK interceptor; confirm every provider tolerates the header
- [x] Streaming: the request span ends at the terminal envelope, not at first byte; cancellation sets `cancelled = true`
- [x] Playground and management: a request span with `surface = management`; attempt spans only when the playground runs inference
- [x] Off means off: no layer installed, no allocation on the request path. On with an unreachable endpoint: latency unaffected; drops counted in `olp_trace_export_dropped_total`

## OTEL-03 — Proof (M)

- [x] Unit test: an attribute-allowlist test that fails on any span attribute key outside the list — the same spirit as `decoder_debug_output_does_not_expose_buffered_content`
- [x] `tests/e2e`: a small OTLP/HTTP receiver in the harness; one streamed chat completion with a forced failover asserts one request span, two attempt spans, correct parent linkage, and no attribute containing the prompt text (reuse the `secret prompt` pattern from the console suite)
- [x] HA suite: propagation through two gateways keeps one trace id
- [x] `make check`, `make db-test`, `make e2e` green; the coverage floor holds

## OTEL-04 — Docs and deployment (S)

- [x] `docs/configuration.md` runtime variables; `docs/operations.md` "Objectives and monitoring" gains a tracing paragraph (sampling guidance, Tempo / Jaeger / Honeycomb examples)
- [x] Helm: `tracing.endpoint`, `tracing.headersSecretName` / `Key`, `tracing.sampleRatio` across `values.yaml`, schema, and templates; `make helm-verify`
- [x] `deploy/compose.tracing.yaml` overlay with Jaeger all-in-one for local exploration (development only, digest-pinned)
- [x] CHANGELOG `[Unreleased]` "Added" with the exact attribute list

## Exit criteria

- [ ] A trace is visible end-to-end in Jaeger from the compose overlay; attempt spans show provider, model, outcome, and nothing else
- [ ] With tracing unset, a quick `oha` run (or `make bench` once milestone 7 lands) shows no measurable latency change
- [x] Allowlist and e2e collector tests are in `Required`

## Carry-over

- Jaeger compose-overlay UI smoke remains local exit evidence; this environment cannot access the Docker API. The digest, rendered Compose contract, and OTLP export path are verified.
- The tracing-off comparison remains local exit evidence. `oha` is installed,
  but PostgreSQL and Valkey cannot be started because this environment cannot
  access the Docker API. Disabled-path construction and allocation guards are
  covered structurally and by unit tests.
