# Architecture

This document records the boundaries and runtime contracts that are useful to
operators and contributors. The user-facing model — routes and slugs,
provider revisions, keys and limits, attempts and pricing — lives in
[`concepts.md`](concepts.md). Implementation ownership and command details
are kept in [`CONTRIBUTING.md`](../CONTRIBUTING.md).

## Component boundaries

Production dependencies point toward `olp-engine`. It owns canonical types,
vendor protocols, provider transports, routing, inference, and the ports used
by infrastructure. The production crate graph is deliberately one-way:

```text
olp-engine
olp-db   -> olp-engine
apps/olp -> olp-engine, olp-db
```

Within `olp-engine`, `domain` owns the canonical model and routing policy,
`protocols` translates vendor wire formats, `providers` owns outbound
provider/OIDC networking and egress policy, and `inference` owns
transport-neutral execution, pinning, selection, failover, limits, event
collection, terminal accounting, and persistence ports. `olp-db` implements
those ports and owns PostgreSQL, Valkey, migrations, encryption, outbox,
request-metadata ingestion, and usage facts. `apps/olp` owns HTTP delivery, CLI
modes, workers, and process wiring. The engine never depends on the database
crate. Test harnesses are outside the production dependency graph. The console
is a client-only static application; its feature slices share only neutral
`$lib` modules.

The engine preserves the same inward module dependency order: `protocols` may
use `domain`; `providers` may use `domain` and `protocols`; and `inference` may
use all three. `domain` imports no sibling module, and no engine module except
`providers` may use outbound networking libraries or construct concrete
connectors. Inference reaches persistence only through its own ports.

Delivery is grouped by owner: `bootstrap/` contains CLI, process composition,
activation, worker supervision, and shutdown; `application/` owns persisted
provider runtime assembly and durable media-job workflows; `public_http/` contains shared listener,
admission, proxy, origin, and response policy; `gateway/` contains protocol
HTTP adapters; `management/` contains the `/api/v1` control plane;
`observability/` contains the private health/metrics surface; and `console/`
contains static-asset delivery. The database crate exposes subsystem
namespaces rather than a flat persistence API. The role checker validates
semantic package-role edges, the engine's module ordering, and infrastructure
ownership. Delivery surfaces own their typed state; bootstrap constructs those
states without becoming a dependency of HTTP handlers or application workflows.
Background media reconciliation receives application dependencies rather than
gateway state. Provider configuration conversion belongs to the engine; persisted
credential loading belongs to application code; management maps failures to HTTP.

Console API modules own requests and contracts. Neutral list modules own cursor
history and Svelte list contexts. Route and provider editors keep reactive state
and actions in feature-local controllers, with separate presentation components.
Neutral modules cannot depend on features, and API modules cannot own UI state.

Rust APIs likewise follow ownership rather than layers: public items are
imported from the leaf module that defines them, with no crate-root or
layer-level re-export facades. Examples include
`olp_engine::domain::canonical::requests::GenerationRequest`,
`olp_engine::inference::service::Service`, and `olp_db::store::Store`.
Boundary checks reject named and wildcard forwarding exports, including scoped
Rust exports and TypeScript forwarding of imported bindings. Engine-only tests
live with the engine so test imports do not force execution internals public.

The source-size baseline may only shrink: `check-source-size.sh --update` rejects
new exceptions. Transactional helpers retain the caller-owned transaction and
lock order; refactoring does not split commits or change persistent formats.

## Distributed tracing

Distributed tracing is an optional delivery capability. `apps/olp` owns OTLP
exporter construction, configuration, the bounded export queue, and shutdown in
`apps/olp/src/observability/tracing.rs` because those concerns belong to the
process lifecycle. Request spans begin at `public_http` admission. Attempt spans
belong to `olp_engine::inference::execution`, where failover and provider-call
lifetimes are known. `olp_engine::providers::transport_common` injects the
current W3C trace context into HTTP provider requests, with the equivalent SDK
interceptor used for Bedrock. The engine never constructs an exporter, and the
database crate has no tracing responsibility.

The integration uses `tracing-opentelemetry` with default features disabled;
the direct `opentelemetry` and `opentelemetry_sdk` declarations request only
trace support for W3C context, carrier APIs, and span processing. The required
upstream `opentelemetry-otlp/http-proto` feature also unifies the OpenTelemetry
metrics compile features, but OLP constructs no meter provider, metric reader,
or metrics exporter. The delivery-owned processor uses the process Tokio
runtime, a bounded non-blocking queue, and explicit drop accounting.
`opentelemetry-otlp` is delivery-owned, has default
features disabled, and enables only `http-proto` and `reqwest-client`.
`http-proto` supplies OTLP trace protocol support, the client reuses the
workspace's existing Reqwest dependency, and no `tonic` or OTLP/gRPC tree is
allowed. The collector connection is process telemetry, not provider egress;
it may target a private collector without changing provider egress policy. If
the boundary checker classifies the OTLP HTTP client as provider networking,
its ownership table must gain this explicit delivery exception rather than
moving exporter code into the engine. `cargo tree`, `cargo deny check bans
licenses`, and `make machete` verify this dependency decision.

The three HTTP-serving modes expose the same environment and CLI surface. A CLI
value takes precedence over its environment counterpart.

| Environment | CLI | Default and meaning |
|---|---|---|
| `OLP_OTLP_TRACES_ENDPOINT` | `--otlp-traces-endpoint` | Unset disables distributed tracing. When set, it is the complete HTTP or HTTPS OTLP traces endpoint and is used without path rewriting. |
| `OLP_OTLP_HEADERS_FILE` | `--otlp-headers-file` | Unset sends no additional exporter headers. The file is used only when tracing is enabled. |
| `OLP_TRACE_SAMPLE_RATIO` | `--trace-sample-ratio` | `1.0`; a finite number from `0.0` through `1.0` controlling locally rooted traces. |
| `OLP_TRACE_PROPAGATE_UPSTREAM` | `--trace-propagate-upstream` | `true`; inject the current trace context into provider attempts. |
| `OLP_TRACE_ACCEPT_INBOUND` | `--trace-accept-inbound` | `true`; accept a valid inbound `traceparent` as the request parent. Caller-supplied `tracestate` is discarded. |

`OLP_OTLP_HEADERS_FILE` follows the mounted-secret convention. When tracing is
enabled, startup checks the file with the same secret-permission policy as the
other secret files, reads it as UTF-8, and requires a JSON object whose property
names and string values are valid HTTP header names and values. On Unix, any
permission for "other" users is rejected; on other platforms, the file must be
readable. An unreadable file, invalid JSON, a non-string value, or an invalid
header fails startup. Header values are never printed through `Debug`, logs, or
spans. `OTEL_EXPORTER_OTLP_TRACES_HEADERS` and `OTEL_EXPORTER_OTLP_HEADERS` are
rejected so exporter headers can come only from this file. When the endpoint is
unset, no exporter, OpenTelemetry layer, or propagator is installed, the headers
file is not opened, and the sampling and propagation settings have no
request-path effect.

Inbound context is used only when both tracing and inbound acceptance are
enabled; invalid context is ignored and starts a local trace. Upstream context
is injected only when both tracing and upstream propagation are enabled. OLP
injects the context derived from the current span rather than forwarding raw
inbound header values. Exporter authentication headers never become provider
headers, and propagation headers never become span attributes.

Only dedicated `request` and `attempt` spans are exported. Existing logging
spans and events remain JSON-log-only and cannot add attributes or events to an
exported span. Resource attributes are limited to
`service.name=openllmproxy`, the build's `service.version`, and
`olp.process.mode`. The request-span attribute allowlist is exactly:

```text
olp.request_id
olp.surface
olp.operation
olp.route_slug
olp.key_id
olp.installation_id
olp.generation
olp.status
olp.error_class
olp.attempt_count
olp.time_to_first_byte_ms
olp.total_duration_ms
olp.cancelled
```

`olp.request_id` is the accepted or generated `x-request-id` value only when it
is a canonical UUID. Arbitrary caller-supplied correlation text remains part of
the existing HTTP contract but is omitted from tracing rather than copied into
an attribute.
`olp.cancelled` is true only when response delivery is cancelled. A request
span ends at its terminal metadata envelope, including after the last streaming
frame, rather than at first byte. Management and playground requests use
`olp.surface=management`; fields that do not apply, such as a route slug on an
ordinary management request, are omitted rather than fabricated.

The attempt-span attribute allowlist is exactly:

```text
olp.provider_kind
olp.provider_revision
olp.model
olp.outcome_class
olp.upstream_status_class
olp.usage.input_tokens
olp.usage.output_tokens
olp.usage.cached_input_tokens
olp.usage.media_units
olp.pricing_provenance
```

Usage attributes are omitted unless the provider reports the corresponding
unit. Pricing provenance is omitted when it is not known before the attempt
span closes; no value is inferred from missing usage. Outcome, upstream status,
and error attributes contain bounded classifications, never messages.

Prompts, outputs, reasoning content, tool arguments or results, uploads,
request or response headers, raw provider error bodies, credentials, exporter
header values, and arbitrary provider fields are prohibited in span
attributes, names, events, links, and status descriptions. This is a traces-only
integration: metrics remain on the Prometheus observability endpoint, including
trace-export drop accounting, and logs remain `tracing` JSON. There is no
OpenTelemetry metrics or logs exporter.

## Typed HTTP composition

Startup completes mode-specific dependencies before it builds routers. Gateway,
management, and observability states contain their required stores, keys,
emitters, and runtime services as non-optional fields. Axum `FromRef` exposes
only narrower capabilities. Management and observability state never
dereference gateway state; playground access receives only its explicit
inference capability. The optional `ProcessComposition` assembly input is
bootstrap machinery and is exposed to fixtures only through its defining
`olp::bootstrap::state` module when `test-util` is enabled.

## Transport-neutral inference service

Gateway adapters and the authenticated playground call the same cloneable
`olp_engine::inference::service::Service`. It pins one runtime generation
before selection, retains its transports and credential revision for the
request, releases distributed leases on cancellation, and finalizes exactly
one terminal request/attempt metadata envelope. Axum extraction, admission,
protocol rendering, and HTTP problem mapping remain in delivery; provider
construction remains in `olp_engine::providers`, and persistence remains in
the database crate.

### Request and attempt lifecycle

Admission creates one request identity and a bounded execution context. Every
provider call receives its own monotonically ordered attempt identity carrying
the selected generation, provider revision, model, operation, deadline, and
outcome classification. Failover appends an attempt; it never rewrites the
previous one. A successful billable attempt with no observed upstream usage is
therefore retained as incomplete and unpriced. A failed attempt may have no
usage fact, but the gateway never manufactures zero usage or zero cost. Pricing
and token/media units are attached to the exact attempt that produced them,
which keeps retries, provider changes, and partial streams auditable without
storing request content.

The terminal collector is bounded independently of response size. It records
status, timing, transport/error class, usage completeness, and pricing
provenance, then emits one terminal envelope. Cancellation follows the same
terminal path: it releases distributed leases, closes provider streams, and
persists the attempt facts that are already known. A late provider callback
cannot create a second terminal record because request and attempt uniqueness
is enforced in storage.

## Canonical endpoint and provider policy

`apps/olp/src/gateway/endpoint_policy/registry.rs` is the sole inference
endpoint registry. Each entry binds an identity to method, path, surface,
typed operation, handler, admission, route extraction, token estimation, and
metadata behavior. Routing, visibility, and classification consume this same
registry; uniqueness checks reject duplicate identities or method/path pairs.

`crates/olp-engine/src/domain/provider_configuration.rs` is the provider policy
registry: it binds each provider kind to authentication modes, credential
rules, applicable fields, defaults, stable API metadata, compatible-provider
presets, and complete-candidate validation. Presets resolve to ordinary
connector fields and never bypass certification. Management create and update
share the validator.

Gateway capabilities and model visibility are default-deny. A capability is
advertised only when the endpoint policy exposes it and the caller's key has
the matching route permission; model listing applies the same policy. A
provider or model that is configured but not explicitly visible cannot be used
as an accidental discovery or routing path.

## Checked storage access

Static PostgreSQL statements use SQLx checked macros and committed `.sqlx`
metadata. Dynamic filters use `QueryBuilder` but decode through subsystem-owned
typed `FromRow` records; string-key `PgRow` decoding is forbidden. CI checks
offline compilation against a freshly migrated PostgreSQL 18 database.

## Runtime publication

Migration 0032 creates an immutable installation UUID in PostgreSQL. OLP
derives the opaque `olp:v3:<installation>` Valkey namespace from it for
runtime hints, request-metadata streams, RPM/TPM state, concurrency leases,
and daily/monthly cost counters.
Independent installations may share a Valkey logical database; a restored
database preserves its UUID and must not run beside its source in the same
keyspace.

An activation transaction stores a byte-stable compiled release, its digest,
and an outbox row. Workers publish only a generation hint. Gateways consume
the hint, poll authoritative PostgreSQL every five seconds, verify the digest,
build indexes, and atomically replace the full snapshot. PostgreSQL session
advisory locking serializes outbox publication; session loss releases the
owner. Repeated hints are harmless, and non-owning workers continue their
other responsibilities.

Every worker responsibility has a shared PostgreSQL checkpoint. Runtime
publication, request-metadata consumption, maintenance, cost reconciliation,
and gateway-epoch detection use additive counters and monotonic timestamps, so
a restarted replica cannot regress the fleet view. A failed metadata consumer becomes
reclaimable after 30 seconds and is scanned every five seconds. Idempotent
receipts and usage-fact uniqueness make replay one logical request and one
logical attempt/usage fact.

Each request holds one immutable runtime object, so a stream cannot cross a
generation or credential version. Provider activation similarly creates a
numbered revision containing endpoint/cloud context, credential version,
enabled models, and certified capabilities. Draft edits do not affect the
active revision; an ETag-bound probe and certification are required before
atomic activation.

## Distributed limit semantics

Valkey server time, not the gateway process clock, is authoritative for RPM,
TPM, concurrency, cost windows, expiry, and `Retry-After`. The unchanged rate
and concurrency reservation script calls `TIME` once and atomically handles
rollover, expired-lease cleanup, and admission. A separate cost reservation
script checks daily and monthly spend first, preserving the rate script's
argument contract. RPM and TPM are fixed UTC-minute windows; cost uses fixed
UTC days and calendar months, so a boundary burst is possible by design.

```text
<namespace>:{id}:rate           hash(window, rpm, tpm)
<namespace>:{id}:concurrency:v2 sorted set(lease_id -> expiry_ms)
<namespace>:{key_uuid_without_hyphens}:cost:day hash(window, accrued)
<namespace>:{key_uuid_without_hyphens}:cost:month hash(window, accrued, unpriced)
```

Each brace-delimited identity keeps the keys used by one script in a single
cluster slot. Rate and concurrency follow the rotatable lookup ID; cost uses
the stable API-key UUID so credential rotation cannot reset spend. Cost is
stored and compared as an exact decimal string rather than a Lua floating-point
number. A rejection consumes no dimension. Malformed state and Valkey errors
fail closed for budgeted keys; release is idempotent and abandoned leases
expire on server time. New binaries do not read or migrate legacy
rate/concurrency keys, so mixed-version deployments temporarily have separate
pools and must be completed promptly.

The metadata transaction prices newly inserted attempt facts and advances
durable per-key, per-window PostgreSQL counters before it commits. It returns
the cumulative daily/monthly snapshot rather than a delta. The consumer and
the checkpointed reconciliation worker both apply cumulative snapshots to
Valkey, so replay, out-of-order delivery, and every terminal/reconciliation
interleaving are idempotent. Reconciliation rebuilds the current snapshots
from raw and hourly usage facts and only raises valid counters; missing,
stale, or malformed Valkey hashes are replaced from PostgreSQL. Lost Valkey
state is therefore recoverable without inventing or double-counting cost.

## Capability certification

Native-provider activation requires server-owned certification for every exact
provider/model/operation tuple. Probes use production connectors, deadlines,
encoders, streaming decoders, and response codecs; they are bounded and store
no prompt or response. Generic OpenAI-compatible providers keep browser review
as `declared` until an explicit per-model certification succeeds. At most 64
reviewed tuples and eight concurrent probe requests are allowed; unsafe media,
video, asynchronous, and cross-protocol claims fail closed. Only an exact
successful probe receives `source = certified` and `certified_at`. Re-reviewing
a tuple set is a per-tuple diff: removed tuples lose their evidence, unchanged
tuples keep it, and new tuples start as `declared`. Only transport edits
(endpoint, region, project, deployment, API version, auth mode) reset every
tuple to `declared` and clear the probe; renames and credential rotation do
not, though rotation still requires a fresh probe before activation.

## Data-safety invariants

Durable request, attempt, and usage records contain only identifiers, timing,
token or media units, status, error classification, and pricing provenance —
never prompts, responses, reasoning, tool arguments/results, uploads, raw
headers, or credentials. Unknown provider fields stay in source-scoped
in-memory protocol extensions.

The gateway emits one bounded terminal metadata envelope containing the full
attempt list. PostgreSQL enforces request/attempt/usage foreign keys and
uniqueness. Usage and cost are attributed to the exact attempt and provider
revision. Missing upstream usage is incomplete and unpriced, never zero:
successful billable attempts without observed usage remain unpriced, while a
failed attempt is retained without inventing usage or cost. Stream entries are
removed only after the database transaction commits and the consumer
acknowledges them; producers never trim unconsumed events. Spend controls add
zero cost for an unpriced attempt and count it separately instead of weakening
that invariant.
