# Observability

Observability covers OLP's content-free exports, Prometheus metrics, distributed traces, inbound OTLP trace ingestion, and notifications. Ordinary request facts and accounting records never contain prompts, responses, or credentials. Payload capture is a separate owner-enabled feature governed by the explicit storage contract below.

Use [Operations](operations.md) during incidents and recovery, [FinOps](finops.md) for FOCUS, metering, and invoice formats, and [Console](../use/console.md) for browser controls.

| API | Purpose | Delivery |
| --- | --- | --- |
| `/api/v1/observability/sinks` | Multi-transport facts, usage rollups, runs, trace spans, FOCUS, metering, and capture destinations | Durable metadata queue, while payload capture is memory-only |
| `/api/v1/sinks` | Scoped HTTPS JSON fact exports with exact-byte HMAC signing | Independent durable queue and delivery history |

These two sink APIs are independent, and neither aliases or migrates the other's definitions. Ordinary traces and accounting remain content-free, while payload capture requires its own explicit opt-in and storage boundaries.

## Observability sinks

An observability sink defines a destination for durable facts scoped either to the installation or to a single project using the same authorization rules as notification destinations. Manage these sinks through `/api/v1/observability/sinks`.

| Field | Contract |
| --- | --- |
| `type` | `otlp_logs`, `s3`, `gcs`, `azure_blob`, `https`, or `stripe_meter` |
| `destination` | Endpoint, bucket, or container validated against the provider egress policy on creation and on every dial |
| `credential` | Write-only secret sealed under the `sink_credential` purpose, or workload identity where supported |
| `streams` | Between 1 and 6 distinct streams chosen from `requests`, `attempts`, `usage_rollups`, `guardrail_decisions`, `audit`, `runs`, `trace_spans`, `metering`, and `focus`. `metering` works on every sink type, while `cloudevents` and `stripe` formats accept only `metering`, and `focus` requires a `csv_gzip` object sink carrying only `focus` |
| `format` | `jsonl` or `csv_gzip` for object stores, `otlp` for `otlp_logs`, `json` or `cloudevents` for `https`, and `stripe` for `stripe_meter`. Parquet is unsupported |
| `filter` | Optional project, route, and outcome filters |

The `export_delivery` worker task delivers transactional fact snapshots from PostgreSQL using per-sink acknowledgements and stream cursors so late commits cannot be skipped by a timestamp checkpoint. Object stores receive deterministic object names per event so redelivery overwrites rather than creating duplicates, and HTTPS deliveries include `X-OLP-Event-ID` plus `X-OLP-Signature` when a signing secret is configured. Delivery is at least once.

Failed deliveries retry with exponential backoff and never block inference or accounting. Retention holds pending snapshots until all subscribed sinks acknowledge them or the retention period expires, recording an export gap if expiry happens first. Exported records preserve the content-free schema of durable facts so audit exports can feed security tools over OTLP collectors or HTTPS. Monitor export backlog and delivery outcomes through the `olp_export_*` and `olp_focus_*` series described under [Business and operational metrics](#business-metrics) alongside task checkpoint readiness.

### Delivery bounds and credentials

Observability object exports write stable `stream/YYYY-MM-DD/event_id.jsonl` keys. Two-minute claim leases commit before network calls, retries back off from one minute to one hour, and rotating 20-second passes prevent sink starvation. Expiry creates `olp.export_gaps` rows and increments `olp_export_gap_total`, which records retention expiry rather than proving the receiver never got the event. Receivers should deduplicate immutable event IDs. Neither delivery path adds remote waits to inference.

Changing a sink destination requires explicitly replacing or removing its credential. GCS and Azure tokens stay bound to their public HTTPS port 443 storage hosts and cannot be widened by provider egress exceptions. A sink's type, streams, format, and project scope are fixed at creation, while updates may modify its name, destination, filters, settings, enabled flag, and credentials. Credentials store transport headers or workload-identity material and are never read back. FOCUS basis is immutable, and CloudEvents source and Stripe mapping settings are documented in [FinOps](finops.md).

## Managed export sinks

A managed export sink delivers durable metadata to an operator-controlled HTTPS JSON endpoint. It supports `requests`, `attempts`, `guardrail_decisions`, `audit`, `runs`, and `trace_spans`, allowing each sink to subscribe to between 1 and 4 distinct streams. Manage these sinks through `/api/v1/sinks` or the generated management clients. An installation supports at most 64 active sinks, and active names must be unique inside their installation or project scope.

Managed export sinks keep their own durable delivery queue and signing credentials. Project sinks require the `keys` operation and project-change authority, installation sinks require `settings`, and reads require `read` plus project visibility. Project sinks receive only facts linked to keys belonging to that project, while keyless facts and `audit` exports are installation-only. Replacements (`PUT`) and retirement (`DELETE`) require the observed `If-Match` header and an `Idempotency-Key`, record audit entries, and replay completed retries idempotently. Project scope and sink type cannot change after creation.

```json
{
  "name": "Security facts",
  "type": "https",
  "destination": "https://collector.example.com/olp",
  "streams": ["requests", "attempts", "guardrail_decisions", "audit"],
  "enabled": true
}
```

An optional write-only `credential` string (1 to 4,096 bytes) signs requests with HMAC-SHA256. OLP seals it under `sink_credential` and returns only `has_credential` on reads. Omitting `credential` during replacement keeps the current secret, while sending `null` removes it. Retiring a sink disables delivery, cancels pending rows, and removes signing material while keeping the sink identity and historical counters. An attempt already in flight may finish without resurrecting a cancelled row. Independent installations must keep separate keys and destinations.

The provider egress policy validates the endpoint at configuration time and on every connection, including resolved DNS answers. Redirects are refused, and plain HTTP requires an explicit host allowlist entry such as an isolated local fixture.

### Delivery and privacy

Fact insertion enqueues deliveries inside the same asynchronous accounting transaction so uncommitted or duplicate sources cannot escape. Each sink receives a durable event ID so late-committing rows are never skipped by polling timestamps, and inference admission incurs no extra query or network call. Workers process bounded passes with 5-second HTTP deadlines, expiring leases, and exponential retries, keeping facts queued with a content-free error category when a receiver is unreachable.

Each delivery posts a JSON object containing `version: 1`, `id`, `stream`, `occurred_at`, and `data`. The `Idempotency-Key` header matches `id`, and when a signing secret is configured, `X-OLP-Signature` carries `sha256=` followed by the lowercase hex HMAC-SHA256 of the exact request body bytes. Retries preserve both `id` and exact bytes so receivers can deduplicate safely after crashes or ambiguous responses.

Requests export routing, operation, status, and timing facts. Attempts export usage completeness, token counts, and exact decimal cost strings, emitting a separate event for any later pricing correction. Guardrail evidence exports rule IDs, phases, actions, and outcomes. Runs export agent run lifecycle facts, and trace spans export content-free metadata ingested via `/v1/traces`. Audit exports omit actor identities, resource IDs, network addresses, and user-agent strings. Arbitrary attribution labels, credential identities or secrets, prompts, outputs, and tool arguments are never exported.

Queued delivery payloads expire after seven days independently of source retention. Workers remove expired payloads in bounded batches and count expired pending deliveries as gaps. Disabling a sink pauses pending rows and re-enabling resumes eligible rows before expiry, while removing a stream stops its delivery. Reads expose `delivered`, `failed`, and `expired` totals plus `last_delivered_at`. The PostgreSQL-only worker task `managed_export_delivery` reports health in worker checkpoints and operational metrics.

### Configuration promotion

Promotion artifacts carry logical scope and name, destination, streams, enabled state, and an opaque `credential_ref`, without local sink, delivery, or secret IDs or raw signing bytes. Supply destination signing secrets through `secret_bindings`, noting that external provider-credential bindings do not apply to sink signing material. An unbound reference blocks apply, existing matching credentials can be reused, unchanged definitions preserve ETags, and removing `credential_ref` removes destination signing material. Queued deliveries, counters, and history remain local.

Promoting sinks requires both configuration authority and the normal sink scope's write permission, sharing endpoint and stream validation with direct API writes. See [configuration promotion](configuration.md#configuration-promotion-artifacts) and [Terraform workflows](automation.md#terraform-and-opentofu). For FOCUS datasets, CloudEvents metering, and Stripe meter sinks under `/api/v1/observability/sinks`, see [FinOps](finops.md).

## Payload capture

Payload capture requires an installation-owner switch (`GET` and `PATCH /api/v1/observability/capture`) and an enabled, installation-owned `json`, `jsonl`, or `otlp` sink. List eligible sinks through `GET /api/v1/observability/capture-sinks`, which returns each eligible sink's `export_sink_id`, `name`, and `type`. Manage capture policies through `/api/v1/observability/capture-policies`. Project managers can manage policies for their own projects, whereas route-only policies require installation settings authority. Capture delivery runs best-effort from bounded memory, and `payload_captured` means queued rather than confirmed delivered. Captured payloads never enter PostgreSQL, Valkey, durable export staging, logs, or trace spans.

Set `expires_at` to an RFC 3339 timestamp to bound how long a policy captures. The API normalizes and returns the deadline in UTC. At that timestamp, each replica stops matching the policy and discards in-flight payloads not yet queued without affecting inference. Expired payloads do not increment `olp_capture_dropped_total`, and records queued before the deadline may still be sent. Expiry never deletes previously delivered records or alters sink retention.

Policies without `expires_at` stay active while enabled. Send `PATCH` with a new `expires_at` timestamp to change the deadline, send `null` to clear it, or omit the field to keep the stored deadline unchanged when toggling or editing other fields. Every `PATCH` records `capture_policy.update` in the audit log, and changing the stored deadline also records `capture_policy.expiry.update`.

### Publish and refresh redactors

`redact` accepts an ordered array of up to 128 distinct guardrail definition names. Creating a policy, or sending `redact` explicitly in a `PATCH`, resolves each name to its active immutable revision inside the policy's project. A route-only policy resolves definitions in the route's project, and an unassigned route cannot publish redactors. Name matching is case-insensitive inside the project, and missing, ambiguous, inactive, retired, or unsupported definitions are rejected. Only `builtin.regex`, `builtin.pii`, `builtin.secrets`, and `builtin.keywords` are eligible for capture redaction, excluding external, model, webhook, plugin, schema, tool-governance, and masking definitions.

```json
{
  "project_id": "01900000-0000-7000-8000-000000000001",
  "sink": "01900000-0000-7000-8000-000000000002",
  "sample_ratio": "0.05",
  "include": ["input", "output", "tool_calls"],
  "redact": ["customer-pii", "credentials"],
  "max_bytes": 65536
}
```

The read-only `redactor_revisions` array lists each pinned name, guardrail ID, revision ID, revision number, and detector type in execution order without exposing configuration or captured text. Later detector activation, renaming, or retirement leaves existing pins unchanged. Unrelated `PATCH` updates must omit `redact`, while sending the same names explicitly refreshes every pin. Passing `[]` removes redaction, and `null` is rejected. In the console, operators manage ordered selections, inspect pins, and use **Refresh pinned revisions on save**.

Redactor bindings are ordered and linked to immutable revisions by foreign keys. Detector compilation happens when the policy cache reloads outside the inference request path. Invalid or unavailable bindings suppress capture completely and never fall back to unredacted capture.

### Capture behavior and bounds

Selected input reflects the prepared request available to capture, including serving transformations. Capture runs its own independent inspection pass so serving-time redaction never proves capture redaction. Input JSON values and tool argument JSON strings are decoded before inspection and re-serialized after replacements without modifying the caller's request, provider call, or response stream. An empty `redact` list performs **unredacted capture** even when serving guardrails exist. Built-in detectors offer bounded pattern coverage and cannot guarantee catching every secret or personal detail.

Streaming capture reconstructs text and tool arguments by native choice, content block, item, or tool index across Chat Completions, legacy text completions, Responses, Anthropic, Gemini GenerateContent, Bedrock ConverseStream, Mistral FIM, and Cohere Chat. Terminal Responses snapshots deduplicate matching items and drop capture on conflicting snapshots. Completion signals are required, and truncated, malformed, unsupported, or incomplete streams drop the entire capture. Gemini partial-argument extensions, opaque media, and unrecognized output shapes are excluded from capture.

Policy `max_bytes` accepts values from 1 KiB to 1 MiB. Runtime bounds enforce 64 KiB per input field, output, and combined tool calls, 1 MiB per record (or the policy's smaller `max_bytes`), 256 active collectors, 64 queued records, 4 MiB per project per minute, and 4,096 tracked projects, plus JSON nesting depth 64 and 2,048 decoded segments. Redaction shares a 100 ms total deadline bounded further by per-detector timeouts and caller cancellation. Elapsed deadlines are checked cooperatively between detector evaluations rather than preempting an in-progress RE2 call.

Capture always enforces `fail_closed` even when a detector serves with `fail_open`. Invalid spans, missing fields, malformed tool arguments, deadline expiry, or exceeded inspection bounds drop the whole record and increment content-free drop and redaction-failure counters. Final redacted fields must still stay within original bounds, and only the final redacted serialized record can enter the queue. Inference status, response bytes, and usage accounting remain isolated from capture.

Any policy or sink disable, deletion, revision refresh, or other ETag change marks queued records stale. The sender rechecks the active policy, ETag, and owner switch before delivering, with no durable retry queue. Policy caches refresh every five seconds with a 15-second freshness ceiling. Queue and project-budget drops keep best-effort behavior. Monitor `olp_capture_queued_total`, `olp_capture_delivered_total`, `olp_capture_dropped_total`, `olp_capture_failed_total`, and `olp_capture_redaction_failed_total` together.

### Qualification

Service-level capture behavior has no automated test coverage. Publication isolation, forward migration, redacted sink readback, unchanged inference, and absence of payloads from durable metadata were former service-fixture checks. See [capture qualification](../develop/qualification.md#guardrail-and-capture-qualification) for current status and validate capture changes by exercising the affected flow directly.

## Distributed tracing

Configure outbound tracing through the [runtime tracing variables](configuration.md#runtime-variables) using a full OTLP/HTTP `/v1/traces` endpoint and a private JSON headers file. Start production sampling low (such as `0.01`), remembering that inbound trace acceptance and upstream context propagation are independent toggles. Watch `olp_trace_export_dropped_total` because bounded asynchronous export never delays provider calls. Spans carry only allowlisted content-free attributes, including OpenTelemetry `gen_ai.*` schema `1.41.0` metadata.

For local development with Jaeger, start the tracing Compose overlay.

```console
docker compose -f deploy/compose.yaml -f deploy/compose.tracing.yaml up -d
```

Open `http://127.0.0.1:16686` to inspect memory-only local traces collected at full sampling, and tear down the stack with the same Compose files and `down`. Never place prompts, tool payloads, raw headers, provider error bodies, or secrets into production traces or incident bundles.

## Inbound OTLP trace ingestion

The inference gateway exposes `POST /v1/traces` (`internal/gateway/traceingest`) so agent runtimes and instrumented applications can send content-free OTLP trace batches directly into OLP for correlation with agent runs and downstream export.

Callers authenticate with `Authorization: Bearer <api_key>` using a project-owned API key granted the `traces_write` scope for that project. Keys lacking `traces_write`, unassigned keys, or keys forbidden for the project receive a protobuf error with HTTP 401 or 403. Ingestion requires both PostgreSQL and the Valkey limiter, returning HTTP 503 when either store is unavailable. In addition to the key's and budget group's configured RPM and concurrency limits, OLP enforces a built-in per-key ingestion cap of 60 requests per minute (returning HTTP 429 when exceeded).

Requests must send `Content-Type: application/x-protobuf` with an OTLP `ExportTraceServiceRequest` protobuf payload using identity or `gzip` `Content-Encoding`. Decompressed bodies are capped at 1 MiB (HTTP 413 when exceeded) and batches at 256 spans (HTTP 400 when exceeded). Error responses serialize a protobuf `google.rpc.Status` payload with `Content-Type: application/x-protobuf`. Each span must carry a non-zero 16-byte `trace_id`, a non-zero 8-byte `span_id`, either an empty or 8-byte `parent_span_id`, and `end_time_unix_nano >= start_time_unix_nano`, with at most 32 allowlisted attributes and string values of at most 256 bytes.

OLP drops every attribute outside its content-free allowlist so prompts, outputs, and arbitrary tags are never stored.

| Allowlisted span attribute | Stored handling |
| --- | --- |
| `olp.run_id` | UUID correlated to `olp.agent_runs` only when the run belongs to the same project, API key, and principal digest |
| `olp.attempt_id` | UUID correlated to `olp.agent_steps.dispatch_id` only when its parent run belongs to the same project, API key, and principal digest |
| `gen_ai.operation.name` | Normalized into span category `chat`, `embeddings`, `text_completion`, `execute_tool`, `invoke_agent`, `create_agent`, or `other` |
| `gen_ai.usage.input_tokens` | Non-negative integer input token count |
| `gen_ai.usage.output_tokens` | Non-negative integer output token count |
| `http.response.status_code` | HTTP status integer between 100 and 599 |

Accepted spans insert idempotently into `olp.trace_spans` deduplicated by `(project_id, api_key_id, principal_digest, trace_id, span_id)`. Retained spans expire after `retention.requests_days` (default 30 days) and can be streamed to external backends through the `trace_spans` stream on `/api/v1/observability/sinks` or `/api/v1/sinks`.

## Business metrics

Outbound trace spans carry the OpenTelemetry GenAI attributes for operation, provider, requested model (the route slug), response model, token usage, and finish reasons at the schema version documented in the [configuration reference](configuration.md#runtime-variables). Content stays out of trace spans, while payload-capture policies targeting OTLP send captured data as separate OTLP log records.

Prometheus scrapes on private port 9090 expose bounded process-local business series (`internal/platform/telemetry/business.go`). Opt in to `project`, `key`, or `end_user` tenant labels through `OLP_METRICS_TENANT_LABELS`. Active sample series stay bounded by `OLP_METRICS_SERIES_CAP` (default 5,000, where each histogram counts its buckets plus three series), refusing new series once full and incrementing `olp_metrics_series_overflow_total`. Cost series approximate the exact-decimal ledger stored in PostgreSQL and must not replace accounting totals.

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `olp_tokens_total` | Counter | `route`, `provider_kind`, `direction` (plus opt-in tenant labels) | Observed provider attempt tokens (`input` or `output`), omitting unknown usage |
| `olp_cost_total` | Counter | `route`, `currency` (plus opt-in tenant labels) | Approximate process-local cost from pinned attempt prices |
| `olp_time_to_first_token_seconds` | Histogram | `route` (plus opt-in tenant labels) | Time to first meaningful streaming output token |
| `olp_output_tokens_per_second` | Histogram | `route` (plus opt-in tenant labels) | Observed streaming output throughput after first meaningful output |
| `olp_guardrail_decisions_total` | Counter | `route`, `phase`, `action`, `outcome` (plus opt-in tenant labels) | Guardrail evaluations by phase, action, and outcome |
| `olp_guardrail_duration_seconds` | Histogram | `route`, `phase`, `action`, `outcome` (plus opt-in tenant labels) | Guardrail evaluation latency by phase, action, and outcome |
| `olp_cache_requests_total` | Counter | `route`, `result` (plus opt-in tenant labels) | Response cache lookup counts by result |
| `olp_cache_bytes_stored_total` | Counter | `route` (plus opt-in tenant labels) | Response payload bytes written to the response cache |
| `olp_cache_errors_total` | Counter | `route` (plus opt-in tenant labels) | Response cache operations that encountered backend failures |
| `olp_metrics_series_active` | Gauge | None | Business metric sample series currently admitted under `OLP_METRICS_SERIES_CAP` |
| `olp_metrics_series_overflow_total` | Counter | None | Observations refused because admitting new series would exceed the series cap |

### Export, capture, admission queue, and shadow metrics

The observability endpoint also exposes Prometheus series for durable exports, FOCUS datasets, in-memory payload capture, opt-in priority admission queueing, and shadow mirroring.

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `olp_export_lag_seconds` | Gauge | `sink`, `stream` | Age in seconds of the oldest undelivered export record per sink and stream |
| `olp_export_deliveries_total` | Counter | `sink`, `outcome` | Durable export records `delivered` or `failed` per sink |
| `olp_export_gap_total` | Counter | `sink` | Export records expired by retention before a sink delivered them |
| `olp_export_pending` | Gauge | None | Total export records currently awaiting delivery across sinks |
| `olp_export_delivery_attempts_total` | Counter | None | Process-local export delivery attempts claimed by this worker |
| `olp_export_delivery_failed_total` | Counter | None | Process-local export delivery attempts that failed in this worker |
| `olp_focus_pending_datasets` | Gauge | `sink` | FOCUS datasets awaiting initial delivery or restatement per sink |
| `olp_focus_dataset_deliveries_total` | Counter | `sink`, `outcome` | Process-local FOCUS dataset deliveries (`delivered` or `failed`) per sink |
| `olp_capture_queued_total` | Counter | None | Payload captures accepted into the process-local memory queue |
| `olp_capture_delivered_total` | Counter | None | Payload captures successfully delivered to their sink |
| `olp_capture_dropped_total` | Counter | None | Payload captures dropped due to memory, rate, or staleness bounds |
| `olp_capture_failed_total` | Counter | None | Payload capture sink deliveries that failed |
| `olp_capture_redaction_failed_total` | Counter | None | Payload captures refused because redaction could not complete safely |
| `olp_admission_queue_depth` | Gauge | `class` | Inference requests waiting in the admission queue by priority class (`critical`, `high`, `normal`, `low`) |
| `olp_admission_queue_wait_seconds` | Summary | `class` | Time admitted requests spent waiting in the admission queue by priority class |
| `olp_admission_queue_rejections_total` | Counter | `class` | Queued inference requests refused because their queue timeout, route deadline, or caller context expired |
| `olp_shadow_requests_total` | Counter | `outcome` | Mirrored shadow attempts `mirrored` or `dropped` because the shadow pool was full |

The bundled [Grafana dashboard](../../deploy/monitoring/grafana-dashboard.json) and Helm PrometheusRule templates cover these series alongside core availability, worker health, and metadata completeness metrics. See [Metric aggregation and incidents](operations.md#metric-aggregation-and-incidents) for multi-replica aggregation rules.

## Notifications

Notification destinations, rules, and delivery logs are managed under `/api/v1/notifications`, including `destinations`, `rules`, and `deliveries`. Supported destination types are webhook, Slack, Teams, Discord, PagerDuty, and SMTP email.

Destination secrets are sealed and write-only, and changing a destination URL requires explicit credential replacement or removal. Webhooks time out after five seconds, typed chat and incident channels time out after fifteen seconds, and PagerDuty must respond with HTTP 202. Email requires SMTPS or STARTTLS. Provider egress rules run at creation and on every dial, redirects are never followed and record `redirect`, email sends quoted-printable text, and SMTP errors record `smtp_4xx` or `smtp_5xx`.

Use **Send test** on a saved, enabled webhook or email destination (or `POST /api/v1/notifications/destinations/{notification_destination_id}/tests/{action_id}`) to verify connectivity without triggering a rule. Testing uses the same permissions as editing the destination, records results and audit entries separately from incident delivery history, and allows three new tests per minute per destination. When the console displays **Result pending**, it polls `GET .../tests/{action_id}` with the original action UUID, and replaying the `POST` with the same UUID avoids sending a duplicate message. **Delivery unconfirmed** indicates the receiver may already have accepted the test, so starting a fresh test can send a second message.

Rules subscribe to immutable events defined in the [OpenAPI contract](../../management/management.json), which owns all event schemas and defaults. Project-scoped events require matching project subject and destination ownership plus project manager authority, while installation-wide events require `settings` authority. Missing fresh measurements evaluate as `UNKNOWN` rather than recovery. Budget threshold alerts fire once per calendar window, and budget delivery evidence freezes the subject, window, and measured amounts at enqueue time so editing a rule cannot relabel retries. Legacy rows migrated during upgrade freeze their current rule identity at migration, and older producers lacking a snapshot receive the current identity at insertion. Key expiry reminders deduplicate by rule, key, reason, and date and cancel superseded pending reminders. Retired unused grants never emit lapse notifications.

A `spend.spike` rule requires a project and compares the most recent complete UTC day of priced spend against the mean of complete priced days inside its trailing `baseline_days` window (default 14). It fires once per rule and day only when at least `minimum_history_days` days qualify (default 7) and the day clears both `increase_percent` (default `50`) and `minimum_increase` in the pricing currency (default `10`). Days with unpriced or incomplete usage never count as zero, and incompletely priced days re-evaluate on later worker passes instead of firing early. Any day overlapping a request-metadata ingestion gap is excluded from the baseline mean and cannot trigger a spike alert. The payload freezes actual spend, baseline, increase, and a relative `usage_path` for that project and day. Spike rules only notify and never modify budgets or block traffic.

Webhook requests carry HMAC-SHA256 signatures over exact body bytes in `X-OLP-Signature: sha256=<hex>`. Payloads remain metadata-only, and failure records keep safe error categories without response bodies, URLs, or secrets. Disabling a rule or destination pauses unsent deliveries. Grant-lapse notifications identify the affected credential version, slots, and observed principal without exposing token material or refresh errors.

Example payload for creating a `budget.threshold` rule through `POST /api/v1/notifications/rules`.

```json
{
  "name": "API key 80 percent monthly warning",
  "project_id": "01900000-0000-7000-8000-000000000001",
  "event": "budget.threshold",
  "subject_kind": "api_key",
  "subject_id": "01900000-0000-7000-8000-000000000020",
  "window_kind": "month",
  "threshold_percent": 80,
  "destination_id": "01900000-0000-7000-8000-000000000010",
  "enabled": true
}
```

Example payload for creating a project-scoped `route.latency` rule through `POST /api/v1/notifications/rules`.

```json
{
  "name": "Chat route p95 TTFT alert",
  "project_id": "01900000-0000-7000-8000-000000000001",
  "event": "route.latency",
  "destination_id": "01900000-0000-7000-8000-000000000010",
  "enabled": true,
  "configuration": {
    "metric": "ttft",
    "window_seconds": 300,
    "minimum_samples": 20,
    "threshold": "1500",
    "recovery_threshold": "1000",
    "cooldown_seconds": 900
  }
}
```

During planned maintenance, edit a project-scoped `route.latency` webhook rule via `PATCH /api/v1/notifications/rules/{notification_rule_id}` (with `If-Match`) or the **Notification silence** controls in the console to configure a UTC silence window. The start instant is inclusive and the end instant is exclusive, and sending `{"silence": null}` cancels a scheduled or active silence early.

```json
{
  "silence": {
    "starts_at": "2026-11-28T02:00:00Z",
    "ends_at": "2026-11-28T04:00:00Z"
  }
}
```

Silenced rules continue evaluating measurements in the background. New triggers and cooldown repeats are suppressed during the window, whereas already queued deliveries and recoveries for previously announced incidents still send. Once the silence expires, a fresh breaching measurement announces any incident still open, while incidents that opened and recovered strictly inside the silence send neither a trigger nor a recovery.

Notifications depend on Valkey only for limiter-backed signals. Capability `notifications_active` reflects configuration rather than live worker health, so monitor `notification_delivery` task checkpoints and delivery status counts.

| Event | Fires when |
| --- | --- |
| `budget.threshold` | An API key or budget group reaches or exceeds a configured percentage (1 to 100) of its daily, weekly, or monthly cost or charge budget |
| `budget.exhausted` | A key, group, project, organization, or end-user budget is exhausted |
| `key.expiring` | An API key approaches expiry or its rotation interval within the configured `lead_time_seconds` (default 86,400) |
| `provider.grant.lapsed` | An active enrolled provider grant lapses and can no longer refresh its access token |
| `provider.circuit.open` | Fleet-shared health ([fleet health](../use/provider-routing.md#explain-and-observe)) opens a provider's circuit |
| `provider.circuit.closed` | Fleet-shared health closes a previously opened provider circuit |
| `provider.error_rate` | A provider's failure ratio meets or exceeds `threshold` (default `0.1`) over `window_seconds` (default 300) with at least `minimum_samples` (default 20) |
| `route.latency` | A route's p95 total latency (`latency`) or time to first token (`ttft`) in milliseconds meets or exceeds `threshold` (default `1000`) over `window_seconds` (default 300) with at least `minimum_samples` (default 20) |
| `provider.credential.failing` | A credential version records at least `threshold` (default `3`) authentication failures over `window_seconds` (default 300) |
| `model.retirement` | A routed model approaches its [catalog](../develop/catalog.md#calibration) retirement date within `lead_days` (default 30) |
| `runtime.install_failed` | A gateway cannot install the newest runtime generation |
| `worker.stale` | A worker task misses its checkpoint stale bound |
| `report.spend` | A scheduled `daily`, `weekly`, or `monthly` spend digest is due |
| `spend.spike` | A project's last complete UTC day of priced spend exceeds its trailing baseline by both `increase_percent` and `minimum_increase` |
