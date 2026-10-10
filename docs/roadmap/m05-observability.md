# M5: Observability, export and alerting

| Status      | Depends on                    | Unlocks                      |
| ----------- | ----------------------------- | ---------------------------- |
| Implemented | [M4](m04-tenancy-identity.md) | [M6](m06-cost-management.md) |

OLP records precise, content-free facts for every request and attempt, and
exposes them through its usage API, Prometheus metrics and OTLP traces. LiteLLM
sends its logs to about 50 destinations, alerts through chat and paging tools,
and can optionally store prompts. This milestone delivers the same reach through
standards instead of bespoke callbacks: durable export sinks, OpenTelemetry
GenAI conventions, and alert channels, plus opt-in payload capture that keeps
content out of OLP's own storage.

## Outcome

- Request, attempt, usage, guardrail and audit facts export durably to object
  storage, OTLP collectors and HTTP endpoints, per installation or per project.
- Operators who need prompts and outputs for debugging or evaluation capture
  them, sampled, directly to a sink they own — until M7 redaction lands,
  capture exports unredacted content only.
- Traces and metrics follow the OpenTelemetry GenAI semantic conventions, so
  Langfuse, LangSmith, Arize Phoenix, Datadog and other OpenTelemetry-native
  tools ingest OLP without custom integrations.
- Alerts reach Slack, Microsoft Teams, Discord, PagerDuty and email for budget,
  provider, latency, key and system events.

## Pre-M5 baseline

|               | OLP before M5                                                                                                             | LiteLLM reference                                                                                                                                                                     |
| ------------- | ------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Durable facts | Requests, attempts, priced usage and gaps in PostgreSQL ([accounting](../operations.md#accounting-delivery-and-shutdown)) | Spend logs in PostgreSQL                                                                                                                                                              |
| Export        | None                                                                                                                      | [Logging callbacks](https://docs.litellm.ai/docs/observability/callbacks) and [team logging](https://docs.litellm.ai/docs/proxy/team_logging)                                         |
| Content       | Never captured                                                                                                            | [Opt-in prompt storage](https://docs.litellm.ai/docs/proxy/ui_logs) and content-bearing callbacks                                                                                     |
| Telemetry     | Operational Prometheus metrics; OTLP traces with an allowlist ([tracing](../operations.md#distributed-tracing))           | [Prometheus](https://docs.litellm.ai/docs/proxy/prometheus) token, spend and budget series; [OpenTelemetry](https://docs.litellm.ai/docs/observability/opentelemetry_v2)              |
| Alerts        | Signed webhooks for `budget.threshold` and `provider.grant.lapsed` ([notifications](../operations.md#notifications))      | [Slack, Discord, Teams](https://docs.litellm.ai/docs/proxy/alerting), [PagerDuty](https://docs.litellm.ai/docs/proxy/pagerduty) and [email](https://docs.litellm.ai/docs/proxy/email) |

## Scope

### M5.1 Export sinks

A sink is a destination for durable facts, scoped to the installation or to a
project with the same authorization as notification destinations.

| Field         | Contract                                                                                                   |
| ------------- | ---------------------------------------------------------------------------------------------------------- |
| `type`        | `otlp_logs`, `s3`, `gcs`, `azure_blob` or `https`                                                          |
| `destination` | Endpoint, bucket or container, validated against the provider egress policy at creation and at every dial  |
| `credential`  | Write-only, sealed under a new `sink_credential` purpose, or workload identity where the cloud supports it |
| `streams`     | Any of `requests`, `attempts`, `usage_rollups`, `guardrail_decisions`, `audit`                             |
| `format`      | JSON Lines for object stores; OTLP log records; JSON for HTTPS. Parquet is deferred to M6's FOCUS export.  |
| `filter`      | Optional project, route and outcome filters                                                                |

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

### M5.2 Payload capture

A capture policy on a project or route opts into content export:

```json
{
  "sink": "0199…",
  "sample_ratio": "0.05",
  "include": ["input", "output", "tool_calls"],
  "redact": [],
  "max_bytes": 262144
}
```

- Content goes from gateway memory to a content-capable sink (`otlp_logs`,
  object stores or HTTPS) through a bounded asynchronous queue. It is never
  written to PostgreSQL or Valkey. Overflow drops the capture and counts the
  loss, mirroring how metadata delivery protects request latency.
- Redaction uses the [M7](m07-guardrails.md) detectors before content leaves
  the gateway. Until M7 ships, only unredacted capture is available, and only to
  operator-configured installation sinks, after the owner enables capture.
- Capture never changes the upstream invocation, so strict routes may use it.
- Creating or widening a capture policy requires the `settings` operation for
  installation scope or project-manager access for a project, and writes an
  audit record. `GET /api/v1/auth/capabilities` reports
  `payload_capture_active`, and request history marks queued captures without
  claiming confirmed delivery. Pure policy disabling remains available when the
  installation switch or sink is disabled.
- [Concepts](../concepts.md#what-is-stored--and-what-never-is) and the
  [security architecture](../security.md) document capture as the single,
  explicit exception to content-free telemetry.

### M5.3 Telemetry conventions and business metrics

- Spans carry the OpenTelemetry GenAI attributes for operation, provider,
  requested model (the route slug), response model, token usage and finish
  reasons, at a semantic-convention version pinned in the configuration
  reference. Content never enters ordinary trace spans; a payload-capture
  policy targeting OTLP exports content through separate OTLP log records.
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
  existing `notification_secret` purpose, and every channel passes the egress
  policy.
- **Events.**

| Event                                              | Fires when                                                                                                                         |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `budget.exhausted`                                 | A key, group, project, organization or end-user budget is exhausted                                                                |
| `key.expiring`                                     | A key expires or reaches its rotation interval within a configured lead time                                                       |
| `provider.circuit.open`, `provider.circuit.closed` | Fleet-shared health ([M3.5](m03-routing-resilience.md#m35-proactive-and-fleet-shared-health)) opens or closes a provider's circuit |
| `provider.error_rate`                              | A provider's failure ratio exceeds a threshold over a window                                                                       |
| `route.latency`                                    | A route's p95 latency or time to first token exceeds a threshold over a window                                                     |
| `provider.credential.failing`                      | A credential version keeps failing authentication                                                                                  |
| `model.retirement`                                 | A routed model approaches its [catalog](m02-provider-catalog.md#m24-reference-catalog) retirement date                             |
| `runtime.install_failed`                           | A gateway cannot install the newest runtime generation                                                                             |
| `worker.stale`                                     | A worker task misses its checkpoint bound                                                                                          |
| `report.spend`                                     | A scheduled daily, weekly or monthly spend digest is due                                                                           |

- Threshold events use hysteresis and a cooldown so a flapping signal does not
  page repeatedly. Delivery keeps today's guarantees: each attempt is recorded
  before it is made, retries back off, and payloads are metadata only.

### M5.5 Reports and sessions

- Usage and request lists export to CSV from the console and API.
- A `session` attribution dimension groups requests, and the console's request
  history can show one session as a timeline.
- `report.spend` digests summarize spend by project, route and top keys for the
  period, through any channel.

## Change map

| Change                      | Start here                                                              |
| --------------------------- | ----------------------------------------------------------------------- |
| Sinks, cursors and delivery | `internal/usage/`, new `internal/export/`                               |
| Payload capture queue       | `internal/gateway/events.go`                                            |
| Spans and metrics           | `internal/telemetry/`, `internal/observability/`                        |
| Channels and events         | `internal/usage/notifications.go`                                       |
| Console                     | `console/src/lib/features/settings/`, `console/src/lib/features/usage/` |

## Implementation decisions

1. Object-store exports use JSON Lines. Parquet is deferred until M6's FOCUS
   export needs it; M5 does not add a second serialization dependency.
2. GenAI semantic conventions are pinned to schema `1.41.0`, as documented in
   the [configuration reference](../configuration.md). Attribute renames are
   breaking changes under the project's 0.x rules.
3. An owner must enable capture installation-wide before project managers may
   create policies for their projects. Policies deliver only to enabled,
   operator-configured installation sinks.
4. Redaction identifiers are reserved for M7. Nonempty redaction configurations
   fail closed until those detectors are available; M5 never presents
   unredacted content as redacted.

## Exit criteria

- [x] Each sink type delivers every stream in integration tests, survives worker
      restarts without loss, and records a gap when retention overtakes it.
- [x] Payload capture delivers sampled content directly to an operator-owned
      sink, rejects unavailable redaction policies, and keeps captured content
      out of PostgreSQL, Valkey and OLP logs.
- [x] Langfuse and Arize Phoenix display OLP traces with model, usage and
      latency through their OpenTelemetry ingestion, verified against pinned
      versions.
- [x] Every alert channel delivers every event in integration tests against
      local fakes, and PagerDuty incidents resolve on recovery.
- [x] Usage metrics stay within the configured series cap under a
      high-cardinality load test.
- [x] Scoped usage/request CSV exports preserve exact values and completeness;
      session timelines and scheduled spend digests are available in the console.
- [x] The [parity matrix](parity.md) observability rows are `Parity` or better.

## Delivered and verified

The implementation includes installation/project sink controls, durable
metadata snapshots and acknowledgements, bounded memory-only capture,
content-free GenAI tracing, bounded business metrics, all notification channels
and events, exact CSV exports, session filtering/timelines and scheduled reports.
The console exposes these controls with scoped permissions and ETag updates.
Operations, privacy, configuration, Helm and monitoring documentation accompany
the implementation.

Qualification covers:

- All five durable streams through all five transports, including sealed
  credentials, deterministic object keys, retry/restart delivery and retention
  gaps. Object-store wire tests use local fixtures; they do not claim live cloud
  account qualification.
- All thirteen notification events through six channels, including signed
  webhooks and PagerDuty recovery, plus real alert-producer regression tests.
- Sampling, bounded queues, unavailable-redaction refusal, unary/streaming
  capture and absence of captured content from ordinary persistence and logs.
- Real pinned Langfuse and Phoenix collector readback for unary and streaming
  traces, including model, usage, latency, parent relationships and privacy.
- High-cardinality metric caps, exact and scoped reporting, and console
  lifecycle/accessibility journeys.

`make check`, uncached race tests and the release build passed. A single
unmodified `make integration` invocation subsequently completed with exit 0:
all 614 expected top-level service tests, internal integration, extension
tests, official SDK/client qualification, all 34 packaged/Vite browser cases,
hosted-console journeys and replacement recovery passed. The recovery case
intentionally skips before restoration and passes in the restored installation.

The initial monolithic `make integration` runs were not green: fixture failures
were corrected, and the remaining service package exceeded its 30-minute
deadline. The runner now partitions non-code tests into two disjoint groups
without increasing that deadline or omitting tests, retaining the original race
flags and build tags. The complete partitioned runner is now verified end to
end. This milestone status does not imply a commit, merge or release.
