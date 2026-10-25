# Observability

Configure content-free exports, metrics, traces and notifications here. Use
[Operations](operations.md) for incidents and recovery, [FinOps](finops.md) for
FOCUS/metering/invoice formats, and [Console](../use/console.md) for the controls.

| API | Purpose | Delivery |
| --- | --- | --- |
| `/api/v1/observability/sinks` | Multi-transport facts, usage rollups, FOCUS, metering and capture destinations | Durable metadata; capture is memory-only |
| `/api/v1/sinks` | Scoped HTTPS JSON fact exports with exact-byte HMAC signing | Independent durable queue and delivery history |

These APIs are independent. Neither aliases or migrates the other's definitions.
Ordinary traces and accounting stay content-free. Payload capture requires its
own explicit opt-in and storage contract below.

## Observability sinks

A sink is a destination for durable facts, scoped to the installation or to a
project with the same authorization as notification destinations. Use
`/api/v1/observability/sinks`. The narrower operator-managed `/api/v1/sinks`
interface has independent definitions and delivery records; it is not an alias
or a migration mechanism for these sinks.

| Field | Contract |
| --- | --- |
| `type` | `otlp_logs`, `s3`, `gcs`, `azure_blob`, `https` or `stripe_meter` |
| `destination` | Endpoint, bucket or container, validated against the provider egress policy at creation and at every dial |
| `credential` | Write-only, sealed under a `sink_credential` purpose, or workload identity where the cloud supports it |
| `streams` | Any of `requests`, `attempts`, `usage_rollups`, `guardrail_decisions`, `audit`; `metering` for CloudEvents/Stripe and `focus` for FOCUS datasets |
| `format` | JSON Lines for object stores; OTLP log records; JSON for HTTPS. FOCUS uses gzip-compressed CSV; Parquet is unsupported. |
| `filter` | Optional project, route and outcome filters |

- A worker task, `export_delivery`, delivers transactional fact snapshots from
  PostgreSQL with per-sink acknowledgements and stream cursors. Late commits
  cannot be skipped by a timestamp-only checkpoint. Object stores receive
  deterministic object names per event, so redelivery overwrites rather than
  duplicates; HTTPS deliveries carry the event identifier in `X-OLP-Event-ID`
  and, when a signing secret is configured, an `X-OLP-Signature`. Delivery is at
  least once.
- Failures retry with backoff and never block inference or accounting.
  Retention keeps pending snapshots until all sink deliveries are acknowledged
  or the retention period expires, whichever is first. Expiry before
  acknowledgement is recorded as an export gap.
- Metrics: `olp_export_lag_seconds{sink,stream}`,
  `olp_export_deliveries_total{sink,outcome}`, plus readiness of the task.
- Exported records keep the content-free schema of the durable facts. Audit
  export reaches security tools through OTLP collectors or HTTPS.

### Delivery bounds and credentials

Observability exports use stable `stream/YYYY-MM-DD/event_id.jsonl` object keys.
Two-minute claims commit before network calls; retries back off one minute–one
hour. Twenty-second rotating passes prevent sink starvation. Expiry creates
`olp.export_gaps` / `olp_export_gap_total`, not proof that the receiver never got
the event. Receivers deduplicate immutable IDs. Neither delivery path adds a
remote wait to inference.

Destination changes require explicit credential replacement/removal. GCS/Azure
tokens stay on their public storage HTTPS/443 hosts; provider egress exceptions
cannot widen those bindings. HTTPS supports JSON/CloudEvents, OTLP log sinks use
OTLP, and object sinks use JSONL or FOCUS 1.4 gzip CSV. Stripe uses meter events.
A sink's type, streams, format and project scope are fixed at creation; edits
cover name, destination, per-event filters and credentials. Credentials contain
transport headers or workload-identity material and are never read back.
FOCUS basis is immutable; CloudEvents source and Stripe event/value/customer
mapping settings are described in [FinOps](finops.md).

## Managed export sinks

An export sink delivers durable metadata to an operator-controlled HTTPS JSON
endpoint. It supports `requests`, `attempts`, `guardrail_decisions`, and `audit`.
Create, read, replace, and retire it through `/api/v1/sinks` or the generated
management clients. At most 64 active sinks exist per installation; active names
are unique inside their installation or project scope.

These sinks have their own delivery queue and signing credentials.

Project sinks require the `keys` operation and project-change authority.
Installation sinks require `settings`. Reads require `read` and project
visibility. Project sinks receive only facts associated with keys belonging to
that project; keyless facts are installation-only. Audit export is
installation-only. Replacements and retirement require the observed `If-Match`
and an `Idempotency-Key`; successful mutations are audited and completed retries
replay their original result. Project and type are immutable.

```json
{
  "name": "Security facts",
  "type": "https",
  "destination": "https://collector.example.com/olp",
  "streams": ["requests", "attempts", "guardrail_decisions", "audit"],
  "enabled": true
}
```

The optional write-only `credential` signs deliveries with HMAC-SHA256. Its
value is sealed under `sink_credential`; reads expose only `has_credential`.
Omitting it during replacement retains the current value; `null` removes it.
Retirement disables delivery, cancels pending rows, and deletes the signing
material while retaining the sink's identity and delivery history. A request
already in flight may finish; its acknowledgement cannot resurrect a cancelled
delivery. Independent installations must use their own keys and destinations.

The provider egress policy validates the endpoint at configuration and every
connection, including DNS answers. Redirects are refused. Plain HTTP requires an
explicit permitted hostname, as in isolated local qualification fixtures.

### Delivery and privacy

Fact insertion queues events in the same asynchronous accounting transaction.
An uncommitted or duplicate source cannot escape that transaction. Each sink
receives a durable event ID; timestamp-based polling cannot skip a source that
commits late. No network call or extra database query is added to inference
admission. Workers deliver bounded passes with bounded HTTP deadlines, expiring
claims, and exponential retries. An unreachable destination leaves facts queued
and records a content-free error classification.

A delivery is JSON with `version: 1`, `id`, `stream`, `occurred_at`, and `data`.
The header `Idempotency-Key` equals `id`. If configured, `X-OLP-Signature` is
`sha256=` followed by the hexadecimal HMAC of the exact request bytes. Retries
retain both ID and bytes. Receivers should deduplicate by ID: a crash or an
uncertain response can redeliver an accepted event.

Requests export routing, operation, status, and timing facts. Attempts export
usage completeness, token counts, and exact decimal cost strings; a later
pricing correction is a distinct event. Guardrail evidence exports rule IDs,
phases, actions, and outcomes. Audit records omit actor identities, resource IDs,
network addresses, and user-agent data. Arbitrary attribution labels, credential
identities and values, prompts, outputs, and tool arguments are never exported.

Delivery payloads expire after seven days, independently of source retention.
Workers remove them in bounded batches and count expired pending deliveries as
gaps. Disabling a sink pauses its pending work; enabling resumes eligible work
before expiry. Removing a stream stops its delivery. Reads report delivered,
failed, and expired counts plus the last delivery time. The shared PostgreSQL
worker task `managed_export_delivery` appears in health and operational metrics.

### Configuration promotion

Artifacts contain logical scope/name, destination, stream choices, enabled
state, and an opaque signing-credential reference. They contain no local sink,
delivery, or secret IDs and no signing values. Supply destination signing bytes
separately through `secret_bindings`; external provider-credential bindings do
not apply to signing material. A new unbound reference blocks application.
Existing matching credentials may be reused, and unchanged definitions keep
their ETags. Removing `credential_ref` explicitly removes the destination's
signing material. Queued events, delivery counters, and history remain local.

Promotion requires the ordinary sink scope's write authority in addition to
configuration authority. Endpoint and stream validation is shared with direct
management writes. See [configuration promotion](configuration.md) and
[Terraform workflows](automation.md#terraform-and-opentofu). For the FOCUS datasets, CloudEvents
metering and Stripe meter sinks the observability API offers, see
[finops](finops.md).

## Payload capture

Capture requires installation-owner opt-in and an enabled, installation-owned
JSON, JSONL or OTLP sink. Project managers may manage their own project policies;
route-only policies require installation settings authority. Delivery is
best-effort from bounded memory. `payload_captured` means queued, not delivered.
Payloads never enter PostgreSQL, Valkey, durable export staging, logs or spans.

### Publish and refresh redactors

`redact` is an ordered array of up to 128 distinct guardrail definition names.
Creating a policy, or explicitly including `redact` in a PATCH, resolves each
name to its active immutable revision in the policy's project. A route-only
policy uses the route's project; an unassigned route cannot publish redactors.
Names are case-insensitive within that project. Missing, ambiguous, inactive,
retired and unsupported definitions are refused. Only `builtin.regex`,
`builtin.pii`, `builtin.secrets` and `builtin.keywords` are eligible. External,
model, webhook, plugin, schema, tool-governance and masking evaluation are excluded.

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

The read-only `redactor_revisions` response lists each name, guardrail ID,
revision ID, revision number and detector type in execution order. It contains
no configuration or captured text. Subsequent detector activation, rename or
retirement does not change an existing pin. Unrelated PATCH requests must omit
`redact`; including the same names explicitly refreshes every pin. Sending `[]`
removes redaction. `null` is invalid. The console supports ordered selection,
pin visibility and **Refresh pinned revisions on save**.

Migration `0074_capture_redactors.sql` replaces the empty-list constraint and
adds ordered bindings with foreign keys to immutable revisions. Existing empty
policies retain their settings. Detector compilation happens when the policy
cache reloads, outside inference processing. Invalid or unavailable bindings
suppress capture; they never fall through to an unredacted policy.

### Capture behavior and bounds

Selected input is the prepared input available to capture, including serving
transformations. Capture applies its own independent inspection; serving
redaction is not proof of capture redaction. Input JSON values and tool argument
JSON strings are decoded before inspection and serialized after replacements.
The caller's original request, provider invocation and response frames are not
changed. Empty `redact` means **unredacted capture**, even when serving guardrails
are configured. Configured detectors provide their documented bounded coverage;
they cannot promise to identify every secret or personal detail.

Streaming capture assembles text and tool arguments by native choice, content
block, item or tool index. Supported families are Chat Completions, legacy text
completions, Responses, Anthropic, Gemini GenerateContent, Bedrock
ConverseStream, Mistral FIM and Cohere Chat. Responses terminal
item/text/argument snapshots are deduplicated; conflicting snapshots drop
capture. Completion signals are required, and truncated, malformed,
unsupported or incomplete streams drop the whole capture. Gemini's partial-argument
extensions, opaque media and unrecognized output types are not capture surfaces.

Policy `max_bytes` accepts 1 KiB–1 MiB. Limits remain 64 KiB per input field,
output and combined tool calls, 1 MiB per
record (or the policy's smaller `max_bytes`), 256 active collectors, 64 queued
records, 4 MiB per project per minute and 4,096 tracked projects. Inspection also
limits JSON nesting to 64 and decoded segments to 2,048. Redaction has a total
100 ms deadline, further bounded by each detector's timeout and caller
cancellation. The local engine checks elapsed deadlines after detector evaluation;
this is cooperative cancellation, not preemption of an individual RE2 call.

Capture always forces `fail_closed`, even if a detector serves with `fail_open`.
Invalid spans, unavailable fields, malformed arguments, deadline expiry and
exhausted inspection bounds drop the entire record and increment content-free
drop/redaction-failure counters. Final transformed fields must still fit the
original bounds. Only the final redacted serialized record can enter the queue.
Inference status, response bytes and usage accounting remain independent.

A policy/sink disable, delete, revision refresh or other ETag change makes queued
records stale. The sender rechecks the current policy, ETag and owner switch
before delivery. There is no durable retry store. Cache refresh is every five
seconds, with a fifteen-second freshness ceiling. Queue and project-budget drops
retain the existing best-effort semantics. Monitor `olp_capture_dropped_total`
and `olp_capture_redaction_failed_total` alongside delivery counters.

### Qualification

Focused unit and component checks cover the implementation. The new service
fixtures were compiled but their execution remains open: publication isolation,
forward migration, redacted sink readback, unchanged inference and absence of
payloads from durable metadata. See [capture qualification](../develop/qualification.md#guardrail-and-capture-qualification)
for commands and acceptance conditions. Release qualification remains open.

## Distributed tracing

Use [tracing configuration](configuration.md#runtime-variables) with a full
OTLP/HTTP `/v1/traces` endpoint and private JSON header file. Start production
sampling low (for example `0.01`); inbound trust and upstream propagation are
independent. Observe `olp_trace_export_dropped_total`: bounded async export never
extends provider latency. Only allowlisted content-free attributes are permitted,
including reviewed OTel `gen_ai.*` schema 1.41.0 metadata.

Development-only Jaeger:

```console
docker compose -f deploy/compose.yaml -f deploy/compose.tracing.yaml up -d
```

Open `http://127.0.0.1:16686`; memory-only local traces use full sampling. Stop
with the same files and `down`. Never add bodies/tools/raw headers/errors/secrets
to production traces or incident bundles.

## Business metrics

- Spans carry the OpenTelemetry GenAI attributes for operation, provider,
  requested model (the route slug), response model, token usage and finish
  reasons, at a semantic-convention version pinned in the configuration
  reference. Content never enters ordinary trace spans; a payload-capture
  policy targeting OTLP exports content through separate OTLP log records.
- Prometheus exposes bounded usage series:
  `olp_tokens_total{route,provider_kind,direction}`,
  `olp_cost_total{route,currency}`, `olp_time_to_first_token_seconds{route}`
  and `olp_output_tokens_per_second{route}`. Cost series approximate the
  exact-decimal totals in PostgreSQL and are documented as such.
- Project, key or end-user labels are opt-in through
  `OLP_METRICS_TENANT_LABELS`, with a series cap that stops adding new label
  values and counts the overflow.
- The [Grafana dashboard](../../deploy/monitoring/grafana-dashboard.json) and
  bundled alert rules cover the new series.

## Notifications

Manage destinations/rules/delivery metadata under `/api/v1/notifications`.
Destination types: webhook, Slack, Teams, Discord, PagerDuty and SMTP email.
Secrets are sealed/write-only; URL changes require explicit replacement/removal.
Webhook timeout is five seconds, typed channels fifteen; PagerDuty needs 202.
Email requires SMTPS/STARTTLS. Egress is checked at creation and every dial.
No channel follows redirects; refused redirects record `redirect`. Email is
sent as quoted-printable text. SMTP replies record `smtp_4xx`/`smtp_5xx`.

Rules subscribe to immutable events: budget thresholds/exhaustion, key expiry,
grant lapse, circuits/error rate/credentials, latency, retirement, runtime failure,
stale workers and spend reports. [OpenAPI](../../openapi/management.json) owns event
schemas and defaults. Project events require matching project subject/destination
and manager authority; installation events require settings authority. Missing
fresh measurement is UNKNOWN, not recovery. Thresholds fire once per calendar
window; key reminders deduplicate reason/date and cancel superseded pending sends;
retired unused grants do not emit lapse.

HMAC-SHA256 webhook signatures cover exact bodies in
`X-OLP-Signature: sha256=<hex>`. Payloads are metadata-only; failures retain safe
categories, never response bodies/URLs/secrets. Disabled rules/destinations pause
unsent delivery. Grant-lapse evidence identifies the affected version/slots and
observed principal, never token material or refresh error.

Notifications need Valkey only for limiter-dependent signals. Capability
`notifications_active` means configured, not healthy; check notification task
checkpoints and delivery status counters.

| Event | Fires when |
| --- | --- |
| `budget.exhausted` | A key, group, project, organization or end-user budget is exhausted |
| `key.expiring` | A key expires or reaches its rotation interval within a configured lead time |
| `provider.circuit.open`, `provider.circuit.closed` | Fleet-shared health ([fleet health](../use/provider-routing.md#explain-and-observe)) opens or closes a provider's circuit |
| `provider.error_rate` | A provider's failure ratio exceeds a threshold over a window |
| `route.latency` | A route's p95 latency or time to first token exceeds a threshold over a window |
| `provider.credential.failing` | A credential version keeps failing authentication |
| `model.retirement` | A routed model approaches its [catalog](../develop/catalog.md#calibration) retirement date |
| `runtime.install_failed` | A gateway cannot install the newest runtime generation |
| `worker.stale` | A worker task misses its checkpoint bound |
| `report.spend` | A scheduled daily, weekly or monthly spend digest is due |
