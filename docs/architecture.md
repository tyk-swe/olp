# Architecture

This document describes how OpenLLMProxy is structured: the crate boundaries
and their dependency rules, how configuration reaches running gateways, how
provider capabilities are certified before activation, and the invariants that
keep durable records free of request content.

## Contents

- [Component boundaries](#component-boundaries)
- [Typed HTTP composition](#typed-http-composition)
- [Canonical endpoint and provider policy](#canonical-endpoint-and-provider-policy)
- [Checked storage access](#checked-storage-access)
- [Runtime publication](#runtime-publication)
- [Distributed limit semantics](#distributed-limit-semantics)
- [Capability certification](#capability-certification)
- [Data-safety invariants](#data-safety-invariants)

## Component boundaries

Production dependencies point toward `olp-domain`, which owns domain types,
routing, and ports without infrastructure dependencies. `olp-protocols` maps
vendor wire formats to canonical operations. `olp-providers` implements
upstream transports, discovery, authentication, and outbound network policy,
while `olp-storage` owns PostgreSQL, the outbox, encryption, request metadata
ingestion, usage accounting, and Valkey integration. The `olp` package in
`apps/olp` owns the HTTP API, process modes, and dependency wiring.

```text
apps/olp (olp) ─┬─> olp-domain
                ├─> olp-protocols ──> olp-domain
                ├─> olp-providers ──> olp-protocols
                │       └───────────> olp-domain
                └─> olp-storage ────> olp-domain
```

`olp-conformance` is a test-only workspace package that exercises
`olp-domain`, `olp-protocols`, and `olp-providers` against the repository's
conformance corpus; it is outside the production dependency graph. The console
is a client-only static application with no server routes or production Node
adapter.

## Typed HTTP composition

Startup first finalizes mode-specific dependency bundles and only then builds
routers. Gateway, management, and observability surfaces have immutable state
types whose mandatory stores, keys, emitters, and runtime services are
non-optional. Axum `FromRef` exposes narrower capabilities to handlers; the
startup assembly object is never an endpoint service locator. Consequently a
routed handler cannot represent a mode that is missing one of its required
dependencies.

## Canonical endpoint and provider policy

`apps/olp/src/gateway/endpoint_policy.rs` is the inference endpoint registry.
Each entry binds one identity to its HTTP method and path, Axum handler,
surface, typed operation, body admission, route extraction, token estimation,
and metadata behavior. Routing and classification both consume that registry,
and uniqueness tests reject duplicate identities or method/path pairs.

`crates/domain/src/provider_configuration.rs` is the provider capability
registry. It exhaustively binds each `ProviderKind` to supported/default
authentication, credential rules, supported and required fields, stable API
metadata, and complete-candidate validation. Management create and update use
the same validator; the console obtains the matrix from the management
capability endpoint and uses generated OpenAPI enums for wire values.

## Checked storage access

Static PostgreSQL statements use SQLx checked macros and the committed `.sqlx`
metadata. Large or conditionally assembled reads use `QueryBuilder` but decode
only through subsystem-owned `FromRow` records. String-key `PgRow` decoding is
forbidden in production storage, and `scripts/check-storage-sqlx.sh` enforces
the single execute-only dynamic statement exception. CI compiles with
`SQLX_OFFLINE=true` and verifies the metadata against a freshly migrated
PostgreSQL 18 database.

## Runtime publication

Activation stores a byte-stable compiled release, its SHA-256 digest, and an
outbox row in one transaction. The worker publishes only a generation hint to
Valkey. Gateways consume hints and poll PostgreSQL every five seconds, verify
the digest, build indexes, and atomically replace the full snapshot. Each
request holds one `Arc` containing its configuration, key indexes, and provider
transports, so a stream cannot cross a generation or credential version.

Activating a provider creates an immutable numbered revision containing the
endpoint or cloud context, credential version, enabled models, and certified
capabilities. Edits and credential rotation affect only the draft; unrelated
key or route publications continue using the active revision. A current
ETag-bound connectivity probe and capability certification are required before
activation atomically replaces that revision. Runtime and fallback credential
lookup are validated against the release revision, preventing newer
configuration credentials from entering an older generation.

![Routes published as immutable runtime revisions](assets/screenshots/routes.png)

Revision diffs are bounded to 2,000 models and 32,000 capability tuples per
side. The database reads at most each limit plus one row, and the API returns an
RFC 9457 `422` problem when a revision exceeds a limit. Full revisions remain
available through the cursor-paginated model endpoint.

## Distributed limit semantics

Valkey server time is authoritative for distributed requests-per-minute (RPM),
tokens-per-minute (TPM), and concurrency decisions. The atomic reservation
script calls `TIME` and derives the UTC minute identity, time remaining in that
minute, rate-state expiry, expired-lease cleanup, new lease scores, and
`Retry-After` duration from that one result. Gateway process clocks are not an
input, and obtaining time does not add a network round trip.

RPM and TPM are fixed UTC-minute windows, not rolling windows or token buckets.
The stable rate hash stores only `window`, `rpm`, and `tpm`; the script compares
its server-derived window ID and replaces the three fields once at rollover.
The hash expires at the fixed minute boundary, and requests within the same
window do not move that logical deadline. Fixed windows permit boundary bursts:
a key can use its full allowance just before a minute boundary and its next full
allowance immediately afterward.

The cluster-safe state for lookup ID `id` is:

```text
<namespace>:{id}:rate           hash(window, rpm, tpm)
<namespace>:{id}:concurrency:v2 sorted set(lease ID -> server-time expiry ms)
```

Both keys have the same `{id}` Valkey Cluster hash tag, so the reservation
script declares every key it accesses and keeps RPM, TPM, and concurrency
admission atomic in one hash slot. A rejection does not consume any dimension.
Malformed stored state and Valkey failures fail closed for hard-limited keys.
Concurrency release remains idempotent, and abandoned leases expire according
to Valkey time.

This layout is a forward-only rollout. Earlier binaries used
`<namespace>:{id}:rpm:<client-window>` and
`<namespace>:{id}:tpm:<client-window>` plus
`<namespace>:{id}:concurrency`. New binaries neither read, write, migrate, nor
delete those keys; legacy state expires naturally. The versioned concurrency
key also prevents a clock-skewed old process from deleting or rescoring a new
lease. During mixed-version deployment, old and new binaries consequently
enforce separate RPM, TPM, and concurrency pools. Each request remains
fail-closed, but traffic split across versions can temporarily consume up to
each pool's allowance. Complete the gateway rollout promptly and do not use an
N-1 rollback as a steady state.

## Capability certification

Enabled native-provider tuples require server-owned certification for the
exact provider, model, and operation. Safe operations use bounded live probes.
Each enabled native model must have at least one tuple, and every tuple must be
certified. OpenAI media and video operations that would require user media,
billable generation, or job mutation may instead use credentialed bounded
discovery and the closed native connector matrix. Generic OpenAI-compatible
providers cannot use that fallback. Probe results are stored only when the
captured draft ETag is still current.

Browser-reviewed tuples for a generic provider are stored as `declared` and
remain ineligible. The explicit per-model certification action reuses the
production connector, SSRF controls, deadlines, encoders, streaming decoder,
and response codecs. It permits at most 16 reviewed tuples and four concurrent
requests. Safe probes cover OpenAI generation (unary and streaming),
embeddings, Responses input-token counting, and unary moderation. Media upload
or generation, asynchronous video, and cross-protocol claims fail closed.

Every attempted tuple is downgraded before results are applied; only an exact
successful probe receives `source = certified` and `certified_at`. Declared-only
tuples cannot activate, enter a runtime, validate a route, or pass route
simulation. Replacing a model's tuple set removes its previous evidence.

![Providers with active certified revisions](assets/screenshots/providers.png)

## Data-safety invariants

Durable request, attempt, and usage records contain only identifiers, timing,
token or media units, status, error classification, and pricing provenance.
They must not contain prompts, responses, reasoning, tool arguments or results,
uploads, raw headers, or credentials. Unknown provider fields remain in
source-scoped in-memory protocol extensions.

The gateway emits one bounded terminal metadata envelope containing the full
attempt list. PostgreSQL enforces composite foreign keys from attempts and
usage facts to the partitioned request. Missing upstream usage is incomplete
and unpriced, never zero. Stream entries are removed only after the database
transaction commits and the consumer acknowledges them; producers do not trim
unconsumed events.
