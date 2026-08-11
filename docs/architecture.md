# Architecture

This document records the boundaries and runtime contracts that are useful to
operators and contributors. Implementation ownership and command details are
kept in [`CONTRIBUTING.md`](../CONTRIBUTING.md) and the agent guides.

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

Delivery is grouped under `bootstrap/`, `public_http/`, `gateway/`,
`management/`, `observability/`, and `console/`. The database crate exposes
subsystem namespaces rather than a flat persistence API. The role checker
validates semantic package-role edges, the engine's module ordering, and
infrastructure ownership.

## Typed HTTP composition

Startup completes mode-specific dependencies before it builds routers. Gateway,
management, and observability states contain their required stores, keys,
emitters, and runtime services as non-optional fields. Axum `FromRef` exposes
only narrower capabilities. Management and observability state never
dereference gateway state; playground access receives only its explicit
inference capability. The optional `ProcessComposition` assembly input is
private bootstrap machinery and is exposed to fixtures only through the
`test-util`-gated `olp::test_support` namespace.

## Transport-neutral inference service

Gateway adapters and the authenticated playground call the same cloneable
`olp_engine::inference::InferenceService`. It pins one runtime generation
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

`apps/olp/src/gateway/endpoint_policy.rs` is the sole inference endpoint
registry. Each entry binds an identity to method, path, surface, typed
operation, handler, admission, route extraction, token estimation, and
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
runtime hints, request-metadata streams, RPM/TPM state, and concurrency leases.
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
publication, request-metadata consumption, maintenance, and gateway-epoch
detection use additive counters and monotonic timestamps, so a restarted
replica cannot regress the fleet view. A failed metadata consumer becomes
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
TPM, concurrency, window expiry, and `Retry-After`. The reservation script
calls `TIME` once and atomically handles rollover, expired-lease cleanup, and
admission. RPM and TPM are fixed UTC-minute windows, so a boundary burst is
possible by design.

```text
<namespace>:{id}:rate           hash(window, rpm, tpm)
<namespace>:{id}:concurrency:v2 sorted set(lease_id -> expiry_ms)
```

The `{id}` hash tag keeps all keys in one cluster slot. A rejection consumes
no dimension. Malformed state and Valkey errors fail closed for hard-limited
keys; release is idempotent and abandoned leases expire on server time. New
binaries do not read or migrate legacy rate/concurrency keys, so mixed-version
deployments temporarily have separate pools and must be completed promptly.

## Capability certification

Native-provider activation requires server-owned certification for every exact
provider/model/operation tuple. Probes use production connectors, deadlines,
encoders, streaming decoders, and response codecs; they are bounded and store
no prompt or response. Generic OpenAI-compatible providers keep browser review
as `declared` until an explicit per-model certification succeeds. At most 16
reviewed tuples and four concurrent probe requests are allowed; unsafe media,
video, asynchronous, and cross-protocol claims fail closed. Only an exact
successful probe receives `source = certified` and `certified_at`, and a
replaced tuple set removes previous evidence.

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
acknowledges them; producers never trim unconsumed events.
