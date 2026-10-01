# M5: Observability, export and alerting

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M4](m04-tenancy-identity.md) | [M2](m02-provider-catalog.md) (retirement events), [M3](m03-routing-resilience.md) (circuit events, sessions), [M7](m07-guardrails.md) (redaction, guardrail decisions) | [M6](m06-cost-management.md) |

OLP records precise, content-free facts for every accounted request and
attempt, and exposes them through its usage API, Prometheus metrics and OTLP
traces. LiteLLM sends its logs to about 50 destinations, alerts through chat
and paging tools, and can optionally store prompts. This milestone delivers the
same reach through standards instead of bespoke callbacks: durable export
sinks, OpenTelemetry GenAI conventions, and alert channels, plus opt-in payload
capture that keeps content out of OLP's own storage.

## Outcome

- Request, attempt, usage, guardrail and audit facts export durably to object
  storage, OTLP collectors and HTTP endpoints, per installation or per project.
- Operators who need prompts and outputs for debugging or evaluation capture
  them, sampled and redacted, directly to a sink they own.
- Traces and metrics follow the OpenTelemetry GenAI semantic conventions, so
  Langfuse, LangSmith, Arize Phoenix, Datadog and other OpenTelemetry-native
  tools ingest OLP without custom integrations.
- Alerts reach Slack, Microsoft Teams, Discord, PagerDuty and email for budget,
  provider, latency, key and system events.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Durable facts | Requests, attempts, priced usage and gaps in PostgreSQL for key-owned traffic; playground traffic is not accounted ([accounting](../operations.md#accounting-delivery-and-shutdown)) | Spend logs in PostgreSQL |
| Export | None | [Logging callbacks](https://docs.litellm.ai/docs/observability/callbacks) and [team logging](https://docs.litellm.ai/docs/proxy/team_logging) |
| Content | Never captured | [Opt-in prompt storage](https://docs.litellm.ai/docs/proxy/ui_logs), content-bearing callbacks and [per-request logging controls](https://docs.litellm.ai/docs/proxy/dynamic_logging) |
| Telemetry | Operational Prometheus metrics; OTLP traces with an allowlist ([tracing](../operations.md#distributed-tracing)) | [Prometheus](https://docs.litellm.ai/docs/proxy/prometheus) token, spend and budget series; [OpenTelemetry](https://docs.litellm.ai/docs/observability/opentelemetry_v2) |
| Alerts | Webhooks, optionally HMAC-signed, for `budget.threshold` and `provider.grant.lapsed` ([notifications](../operations.md#notifications)) | [Slack, Discord, Teams](https://docs.litellm.ai/docs/proxy/alerting), [PagerDuty](https://docs.litellm.ai/docs/proxy/pagerduty) and [email](https://docs.litellm.ai/docs/proxy/email) |

## Scope

### M5.1 Export sinks

A sink is a destination for durable facts, scoped to the installation or to a
project with the same authorization as notification destinations.

| Field | Contract |
| --- | --- |
| `type` | `otlp_logs`, `s3`, `gcs`, `azure_blob` or `https` |
| `destination` | Endpoint, bucket or container, validated against the provider egress rules at creation and at every dial, as notification webhooks are today |
| `credential` | Write-only, sealed under a new `sink_credential` purpose, or workload identity where the cloud supports it |
| `streams` | Any of `requests`, `attempts`, `usage_rollups`, `guardrail_decisions`, `audit` |
| `format` | JSON Lines or Parquet for object stores; OTLP log records; JSON for HTTPS |
| `filter` | Optional project, route and outcome filters |

- A worker task, `export_delivery`, reads facts from PostgreSQL through a
  durable cursor per sink and stream. The cursor is new state: today's
  [cursor helpers](../../internal/usage/cursor.go) only encode list
  pagination, and the nearest precedent is the worker task checkpoint. Object
  stores receive deterministic object names per cursor range, so redelivery
  overwrites rather than duplicates; HTTPS deliveries carry the event
  identifier as an idempotency key and an `X-OLP-Signature`. Delivery is at
  least once.
- Failures retry with backoff and never block inference or accounting.
  Retention keeps facts until every enabled sink's cursor has passed them or
  the retention period expires, whichever is first, and an expiry ahead of a
  sink is recorded as an export gap.
- Metrics: `olp_export_lag_seconds{sink,stream}`,
  `olp_export_deliveries_total{sink,outcome}`, plus readiness of the task.
- Exported records keep the content-free schema of the durable facts. Audit
  export reaches security tools through OTLP collectors or HTTPS.
- The `guardrail_decisions` stream carries content-policy decisions until
  [M7](m07-guardrails.md) ships, and guardrail decisions afterwards.

### M5.2 Payload capture

A capture policy on a project or route opts into content export:

```json
{
  "sink": "0199…",
  "sample_ratio": "0.05",
  "include": ["input", "output", "tool_calls"],
  "redact": ["builtin.pii", "builtin.secrets"],
  "max_bytes": 262144
}
```

- Content goes from gateway memory to a content-capable sink (`otlp_logs`,
  object stores or HTTPS) through a bounded asynchronous queue. It is never
  written to PostgreSQL or Valkey. Overflow drops the capture and counts the
  loss, mirroring how metadata delivery protects request latency.
- Redaction uses the [M7](m07-guardrails.md) detectors before content leaves
  the gateway. Until M7 ships, only unredacted capture is available, and only to
  owner-configured installation sinks.
- Capture never changes the upstream invocation, so strict routes may use it.
- **Caller opt-out.** A key policy may let callers exclude a request from
  capture with `X-OLP-Capture: off`. No caller control can opt a request in.
- Creating or widening a capture policy requires the `settings` operation for
  installation scope or project-manager access for a project, and writes an
  audit record. Request history marks captured requests.
  `GET /api/v1/auth/capabilities` reports `payload_capture_active`; that
  endpoint is unauthenticated, so the flag is a single boolean with no sink or
  scope detail.
- [Concepts](../concepts.md#what-is-stored--and-what-never-is) and the
  [security architecture](../security.md) document capture as the single,
  explicit exception to content-free telemetry.

### M5.3 Telemetry conventions and business metrics

- Spans carry the OpenTelemetry GenAI attributes for operation, provider,
  requested model (the route slug), response model, token usage and finish
  reasons, at a semantic-convention version pinned in the configuration
  reference. Content attributes appear only when a payload-capture policy
  targets an OTLP sink.
- Prometheus gains bounded usage series:
  `olp_tokens_total{route,provider_kind,direction}`,
  `olp_cost_total{route,currency}`, `olp_time_to_first_token_seconds{route}`
  and `olp_output_tokens_per_second{route}`. Cost series approximate the
  exact-decimal totals in PostgreSQL and are documented as such.
- Project, key or end-user labels are opt-in through
  `OLP_METRICS_TENANT_LABELS`, with a series cap that stops adding new label
  values and counts the overflow.
- The [Grafana dashboard](../../deploy/monitoring/grafana-dashboard.json) and
  bundled alert rules cover the new series.

### M5.4 Alert channels and events

- **Channels.** Notification destinations gain the types `slack` (incoming
  webhooks with Block Kit), `msteams` (Workflows webhooks with Adaptive Cards),
  `discord`, `pagerduty` (Events API v2 with deduplication keys and automatic
  resolution) and `email` (SMTP with STARTTLS or implicit TLS). Secrets use the
  existing `notification_secret` purpose, and every channel passes the same
  egress rules as today's webhooks.
- **Events.** An event whose source belongs to another milestone ships when
  both sides exist.

| Event | Fires when | Source |
| --- | --- | --- |
| `budget.exhausted` | A key, group, project, organization or end-user budget is exhausted | [M4.2](m04-tenancy-identity.md#m42-budget-hierarchy-windows-and-templates) for the new levels |
| `key.expiring` | A key expires or reaches its rotation interval within a configured lead time | [M4.3](m04-tenancy-identity.md#m43-access-ergonomics) for rotation intervals |
| `provider.circuit.open`, `provider.circuit.closed` | Fleet-shared health opens or closes a provider's circuit | [M3.5](m03-routing-resilience.md#m35-proactive-and-fleet-shared-health) |
| `provider.error_rate` | A provider's failure ratio exceeds a threshold over a window | This milestone |
| `route.latency` | A route's p95 latency or time to first token exceeds a threshold over a window | This milestone |
| `request.hanging` | Requests on a route stay open beyond a configured duration | This milestone |
| `provider.credential.failing` | A credential version keeps failing authentication | This milestone |
| `model.retirement` | A routed model approaches its catalog retirement date | [M2.4](m02-provider-catalog.md#m24-reference-catalog) |
| `runtime.install_failed` | A gateway cannot install the newest runtime generation | This milestone |
| `worker.stale` | A worker task misses its checkpoint bound | This milestone |
| `report.spend` | A scheduled daily, weekly or monthly spend digest is due | This milestone |

- Threshold events use hysteresis and a cooldown so a flapping signal does not
  page repeatedly. Delivery keeps today's guarantees: each attempt is recorded
  before it is made, retries back off, and payloads are metadata only.

### M5.5 Reports and sessions

- Usage and request lists export to CSV from the console and API.
- Requests are grouped by session, as
  [M3.2](m03-routing-resilience.md#m32-capacity-aware-selection-and-session-affinity)
  defines it. Reports use the session label, which is stored like any other
  attribution label, and the console's request history can show one session as
  a timeline. A session taken from a dialect field is used for affinity only
  and is not stored.
- `report.spend` digests summarize spend by project, route and top keys for the
  period, through any channel.

## Non-goals

- Storing prompts or outputs in PostgreSQL, Valkey or OLP's logs. Captured
  content goes only to an operator-owned sink.
- One bespoke integration per logging vendor. The logging and alert rows close
  under the [breadth rule](parity.md#how-to-read-the-matrix): standards-based
  sinks and signed webhooks reach the rest.
- In-process callbacks.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Sinks, capture policies, channels and rules | PostgreSQL | Until deleted | None |
| Sink credentials | PostgreSQL, sealed | Until rotated or the sink is deleted | New seal purpose `sink_credential` |
| Channel secrets, including SMTP credentials | PostgreSQL, sealed | Until rotated | Existing `notification_secret` |
| Export cursors and export gaps | PostgreSQL | With the sink | None |
| Captured payloads | Gateway memory, then the operator's sink | Never stored by OLP | None |
| Session labels | PostgreSQL request records, as attribution | Request retention | None |

## Change map

| Change | Start here |
| --- | --- |
| Sinks, cursors and delivery | `internal/usage/`, new `internal/export/` |
| Payload capture queue | `internal/usage/emitter.go`, `internal/gateway/accounting.go` |
| Spans and metrics | `internal/telemetry/`, `internal/observability/` |
| Events, destinations and rules | `internal/access/notifications.go` |
| Channel delivery | `internal/usage/notifications.go` |
| Console | `console/src/lib/features/access/notifications/`, `console/src/lib/features/settings/`, `console/src/lib/features/usage/` |

## Decisions to settle

1. Parquet support: a pure-Go writer dependency, or JSON Lines only at first
   (recommended: JSON Lines first, and Parquet when the FOCUS export in M6
   needs it).
2. The pinned GenAI semantic-convention version, which is still in development
   status upstream (recommended: the latest release when M5 starts, with
   attribute renames treated as a breaking change under 0.x rules).
3. Whether project managers may create capture policies without an
   installation-wide opt-in (recommended: no; an owner enables capture for the
   installation first).

## Exit criteria

- [ ] **M5.1** Each sink type delivers every stream in integration tests,
      survives worker restarts without loss, and records a gap when retention
      overtakes it.
- [ ] **M5.1, M5.4** Sinks, channels and rules round-trip through configuration
      export, plan and apply without their secrets.
- [ ] **M5.2** Payload capture delivers sampled, redacted content to a sink, a
      caller opt-out suppresses it, and no captured content appears in
      PostgreSQL, Valkey or OLP logs.
- [ ] **M5.3** Langfuse and Arize Phoenix display OLP traces with model, usage
      and latency through their OpenTelemetry ingestion, verified against
      pinned versions.
- [ ] **M5.3** Usage metrics stay within the configured series cap under a
      high-cardinality load test.
- [ ] **M5.4** Every alert channel delivers every event whose source has
      shipped in integration tests against local fakes, and PagerDuty incidents
      resolve on recovery.
- [ ] **M5.5** CSV exports match the usage API for the same filters, and a
      session timeline shows every request that carried the session key.
- [ ] The [parity matrix](parity.md) observability rows are `Parity` or better,
      with the breadth rows closed under the
      [breadth rule](parity.md#how-to-read-the-matrix).
