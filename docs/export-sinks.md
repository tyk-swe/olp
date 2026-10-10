# Content-free export sinks

An export sink delivers durable metadata to an operator-controlled HTTPS JSON
endpoint. It supports `requests`, `attempts`, `guardrail_decisions`, and `audit`.
Create, read, replace, and retire it through `/api/v1/sinks` or the generated
management clients. At most 64 active sinks exist per installation; active names
are unique inside their installation or project scope.

These operator-managed sinks have their own delivery queue and signing
credentials. The [observability sinks](operations.md#export-sinks-payload-capture-and-business-metrics) at
`/api/v1/observability/sinks` support additional transports, filters, usage
rollups, and capture policies; their definitions and delivery records are
independent.

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

## Delivery and privacy

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

## Configuration promotion

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
[Terraform workflows](terraform.md).
