# ADR 0003: Connector and policy extension trust boundaries

- Status: Accepted target (approval pending)
- Decision issue: XOD-85
- Decision owners: gateway/security, domain, provider platform, storage/API
- Implementation milestone: M4 for connectors and M5 for policy
- Qualification milestone: M8

## Context

OLP 2.0 has a deliberately closed provider implementation. `ProviderKind` in
`crates/domain/src/routing.rs` is a seven-variant enum. `ProviderConfig` and the
private `ConcreteConnector` in `crates/providers/src/factory.rs` repeat that
closed set, and `ProviderTransport` in `crates/domain/src/ports.rs` is an
in-process Rust trait. The management API, pricing model, console, storage
parsers, and runtime compiler also use the closed provider kind.

Those boundaries are appropriate for the current native connectors, but they
do not satisfy the enterprise connector requirement. Adding a provider today
requires modifying and rebuilding OLP. The trait does not define a remotely
negotiated manifest, connector identity, health, discovery, cancellation,
resource isolation, upgrade compatibility, or secret-delivery protocol. There
is no general policy program or policy execution boundary in the repository.

This ADR freezes the target contract. It does not claim that the current code
implements or qualifies it. The machine-readable connector and policy
contracts are `docs/enterprise/contracts/connector-v1.json` and
`docs/enterprise/contracts/policy-v1.json`. Those contracts pin the normative
Connector protobuf, policy program schema, and policy golden vectors by digest.

## Decision

### 1. Connectors run outside the OLP process

The supported enterprise extension is an out-of-process connector speaking the
versioned Connector v1 protocol. OLP is the protocol client. A connector is a
separately packaged workload or process and is never loaded into an OLP address
space.

OLP will not support any of the following as an enterprise-beta extension
surface:

- a Rust `cdylib`, `dylib`, or other native dynamic-library ABI;
- `libloading`, an `extern "C"` provider ABI, or an ABI coupled to Rust layout;
- customer WebAssembly, JavaScript, shared objects, or arbitrary code executed
  in the gateway, control, or worker process;
- a connector with direct PostgreSQL, Valkey, OLP control API, or runtime-cache
  access.

Native connectors may remain compiled into `olp-providers`, but they must be
adaptable to the same conformance semantics. Their existence is not a promise
that a native Rust interface is stable.

### 2. Provider type identity is open and data-owned

Connector-backed provider types use a bounded, lowercase DNS-style `type_id`
such as `com.example.private-model`, plus a semantic connector version and a
content-addressed manifest digest. `type_id` replaces the closed provider enum
as the persisted and public identity. Unknown well-formed type IDs remain
round-trippable data; they are not coerced to a native fallback.

Installation operators install connector type versions in the registry.
Organizations create provider connections from installed types. An environment
may use an organization-owned connection only through its explicit grant. An
immutable provider revision and environment runtime generation pin all of:

- connector `type_id`, version, artifact digest, and manifest digest;
- validated non-secret configuration digest;
- secret revision or external-secret reference revision;
- certified capability tuples;
- network and workload-isolation policy.

An upgrade never changes those values beneath an in-flight request. Activating
a new connector version or rolling back to an old artifact publishes a new,
monotonically newer provider and environment revision.

### 3. Connector v1 uses protobuf gRPC over an authenticated channel

Connector v1 uses protobuf messages over gRPC/HTTP2. TCP connections require
TLS 1.3 with mutual workload authentication. A same-pod connector may use a
permission-restricted Unix-domain socket, but it still uses TLS 1.3 mutual
workload authentication. The socket uses distinct non-root UIDs, mode `0660`, a
dedicated group shared only by OLP and that connector, and verified peer
credentials. Socket permissions and peer credentials are defense in depth, not
a substitute for workload identity. Plaintext TCP or UDS is not a production
mode.

The normative wire IDL is
`proto/olp/connector/v1/connector.proto`, package `olp.connector.v1`, service
`ConnectorService`. Its service names, RPC shapes, field numbers, enum values,
and oneof frame tags are permanent within v1. Removed fields must be reserved;
new fields must be optional and cannot silently create authority when an older
peer ignores them. `connector-v1.json` pins the IDL digest and the semantic
rules that protobuf cannot express, including stream order and cumulative
bounds. A build or release with a different IDL digest must deliberately update
and review both artifacts under the API compatibility rules.

Every protobuf `string` field has a named UTF-8 byte and syntax profile in the
machine contract. CI compares that registry to the IDL so a newly added string
cannot inherit the much larger gRPC frame ceiling. Repeated string and control
collections also have explicit item limits. Oversized, invalid, control-bearing,
or out-of-profile connector values are rejected before application dispatch and
are not copied into logs.

The connector presents a workload identity authorized for its exact
installation, connector type, version, and instance. OLP presents a gateway or
control workload identity. The handshake binds the negotiated protocol major,
minor feature set, manifest digest, artifact digest, and maximum message sizes
to that authenticated channel. A mismatch prevents configuration and
activation; it never falls back to a connector with a similar display name.

Handshake negotiation is fail-closed. Selected features are a unique, known
subset of those OLP offered, never the unspecified enum, and Connector v1 usage
support is mandatory. Every advertised `ProtocolLimits` scalar is nonzero,
does not exceed the frozen contract ceiling or OLP's offered message limit, and
satisfies the payload/frame/aggregate relationships. OLP may accept a smaller
connector limit but never raises one; a zero, unknown, duplicate, unoffered,
oversized, or internally contradictory value rejects the handshake without
activation or fallback.

OLP initiates every RPC. Connectors have no callback into the management API.
Each request uses an OLP-issued opaque execution ID and deadline. Tenant,
project, environment, credential, route, target, and generation identifiers are
server assertions, not connector-selected authority.

### 4. Configuration and secrets are separate

The connector manifest contains a bounded JSON Schema for non-secret
configuration and named secret slots. The control plane validates configuration
before it can enter a provider revision. Secret values are rejected in config,
manifest, status, discovery, error, usage, and health payloads.

Configuration schema evaluation is a deliberately restricted, fail-closed
Draft 2020-12 profile. The exact metaschema is embedded; validation never
resolves a network or filesystem resource. Only same-document JSON Pointer
`$ref` values are accepted, reference cycles and dynamic references are
rejected, and reference depth and visits are bounded. Unknown keywords,
connector-declared vocabularies, and unsupported formats are rejected. Patterns
must compile under the bounded, linear-time RE2-compatible profile. Schema
syntactic nesting, compilation, applicator branches, evaluation steps, errors,
and wall time all have hard limits in `connector-v1.json`. The OLP profile
recognizes the standard format-annotation vocabulary but explicitly strengthens
its allowlisted formats under the Draft 2020-12 format-assertion vocabulary.
Exhausting any bound rejects the manifest or configuration rather than accepting
a partial result. Content keywords remain annotations and never trigger decoding
or retrieval.

Connector v1 supports two explicit secret modes:

1. `olp_ephemeral`: OLP resolves one immutable encrypted credential revision
   and sends its plaintext only over the authenticated connector channel for
   the lifetime of the configured connector instance. The value is held in
   zeroizing memory and is never returned.
2. `external_reference`: the provider revision contains a bounded opaque
   reference to an immutable secret-manager version. The connector resolves it
   with its own workload identity. Mutable aliases such as `latest` cannot
   activate. OLP never treats the reference as a path, URL to fetch, or
   credential value.

Secret delivery is least-privilege per provider connection. A connector
instance must not receive credentials for another organization, connection, or
revision. A secret-bearing instance binds exactly one provider connection and
provider revision so retirement does not expose or interrupt an unrelated
configuration.

Rotation and revocation produce new authority; they do not mutate an already
pinned request. Retirement first disables new executions for the old
configuration. Non-emergency rotation may drain already admitted work only
until its bounded deadline. Revocation or drain expiry terminates the connector
instance and revokes its workload identity. OLP calls the idempotent
`Deconfigure` RPC to request cleanup, but an acknowledgement from an untrusted
connector is not proof of zeroization: process termination and memory
reclamation are the enforcement boundary for `olp_ephemeral` plaintext. Logs,
metrics, traces, crash reports, and support bundles may contain slot names and
revision IDs but never values or reference payloads.

### 5. Lifecycle and execution semantics are complete and bounded

The stable v1 surface covers manifest/handshake, configure/deconfigure, model
discovery, capability certification, health, execution, server streaming,
cancellation, media transfer, errors, and usage. The exact operations, fields,
limits, and failure mapping are frozen in `connector-v1.json`.

Important invariants are:

- Discovery and certification use the exact config, secret, connector version,
  and network policy that activation will pin. Results are bounded and become
  evidence only if the provider draft ETag is still current.
- Discovery starts with an empty cursor. Only the final model frame in a page
  may carry the next cursor; every non-empty cursor must advance and remain
  unique for the exact configuration, draft ETag, and manifest digest. Duplicate
  model IDs, cursor repetition or loops, misplaced cursors, page overflow, and
  page failure reject the whole discovery without persisting partial inventory.
- Connector capability claims are advisory. OLP activates only exact tuples
  that both platform policy and server-owned certification accept.
- Canonical input content is sent only to the connector selected for an
  authorized attempt. Media is streamed as bounded chunks or opaque,
  request-scoped handles; a connector never receives an OLP filesystem path.
- Request and response media use separate namespaces and state machines.
  Request IDs are declared in `BeginExecution`; every response ID is introduced
  by one bounded `ResponseMediaStart` descriptor carrying its content type and
  declared maximum. Each direction then requires contiguous chunks from zero
  and exactly one matching end frame. Undeclared, reused, cross-direction,
  mistyped, over-limit, post-end, or digest-mismatched media is a protocol error.
  Descriptor counts and the sum of declared maxima are reserved against the
  direction's item, per-item, and execution-total limits before dispatch.
- Canonical request, unary result, stream-event, and media chunk payload limits
  are smaller than their serialized protobuf frame limits. The reserved
  envelope headroom is explicit and the negotiated gRPC message maximum covers
  the largest valid frame; metadata and unknown protobuf fields still count
  toward that maximum.
- Per-frame checks do not replace execution-wide checks. OLP counts raw
  canonical result/event bytes cumulatively and separately counts response
  media bytes and items across every media ID. Crossing either aggregate bound
  cancels the stream, discards uncommitted output, and records incomplete usage.
- OLP assigns and validates event sequence, message limits, deadlines, and the
  externally visible response-commit point. A connector cannot make a retry
  safe by setting a flag.
- Cancellation is carried by gRPC cancellation and an idempotent explicit
  cancel operation. Disconnect, deadline, and policy denial all trigger it.
- Missing, contradictory, overflowing, or malformed usage is recorded as
  incomplete and unpriced. It is never converted to zero usage or zero cost.
- A usage report, when present, follows every output/media frame and moves the
  server stream to a terminal-only state. Pre-acceptance usage may be followed
  only by an error. `ExecutionDone` accepts only the known succeeded or
  cancelled statuses; unspecified or unknown values are protocol errors.
- Connector strings are untrusted. OLP maps stable error codes to its own
  bounded, redacted errors and never logs arbitrary connector detail.
- Health freshness starts when OLP completes a valid response on its monotonic
  clock. Connector wall time is diagnostic only and cannot prolong eligibility;
  invalid timestamps are discarded. Retry hints are clamped and cannot extend
  the stale-health deadline, while unspecified or unknown health status is a
  protocol error.

### 6. Failure isolation is deny-safe

| Failure | Required behavior |
| --- | --- |
| Incompatible manifest, protocol, or artifact | Reject installation or activation; retain the last verified provider/environment revision. |
| Connector unavailable before response commitment | Record a bounded connect failure and use only the route's existing deterministic failover budget. |
| Connector slow | Enforce the earlier of request, route, attempt, and connector deadlines; cancel and reclaim resources. |
| Connector fails after response commitment | Terminate the response safely; never retry or switch connector. |
| Malformed or oversized output | Treat as protocol failure, discard uncommitted output, quarantine unhealthy instances, and preserve incomplete usage evidence. |
| Compromised connector | Limit impact to its workload, granted egress, assigned provider connections, and requests routed to it; revoke identity/secrets, remove it from readiness, and publish a replacement revision. |
| Health endpoint unavailable | Do not infer health from discovery or traffic success. Exclude the instance after bounded grace while preserving safe local route fallback. |
| Secret resolver unavailable | Fail provider configuration/attempts closed; never substitute an older or differently scoped secret. |
| Deconfiguration unavailable or drain expires | Admit no new execution, revoke the old workload identity, terminate the instance, and reclaim its memory; never trust a cleanup acknowledgement as zeroization evidence. |

Connectors run as a non-root identity with a read-only root filesystem,
resource requests and limits, bounded file descriptors and processes, no host
mounts, no service-account token unless required, and an egress allowlist. A
secret-bearing workload is dedicated to one provider connection and revision.
The supported deployment must enforce those controls; manifest declarations
alone are not trusted enforcement.

### 7. Upgrades are content-addressed and reversible

The registry never overwrites an installed `(type_id, version, artifact_digest)`.
A candidate must pass signature/provenance policy, handshake validation,
connector conformance, and organization-specific certification before a
provider revision can select it. Runtime generations pin the selected tuple.

Rolling upgrades may run old and new connector instances concurrently. Requests
already pinned to the old runtime may finish there only within the bounded drain
deadline. The old configuration accepts no new executions and its instance is
then terminated. Rollback selects the previous verified artifact in a new
provider/environment revision; it never reuses retired secret authority.
Connector protocol major versions are not negotiated across a request; v1
clients reject another major, and optional minor features require explicit
feature negotiation.

### 8. Policy is a deterministic data program, not customer code

Enterprise policy is an immutable, versioned, declarative program represented
by a typed OLP policy IR. The trusted OLP policy evaluator interprets data; it
does not load customer native code, Wasm, scripts, or general-purpose bytecode.
The IR is loop-free, has no network or filesystem access, no ambient clock or
randomness, fixed decimal semantics, canonical ordering, and hard bounds on
program bytes, aggregate compiled bytes per environment, nodes, nesting,
evaluation steps, input collection sizes, and output actions. The aggregate
bound is part of the runtime payload envelope, not the Cartesian product of
every independent cardinality maximum.

The machine contracts and CI gate enumerate the complete forbidden executable
surface: native/shared libraries, C ABIs and dynamic loading, customer Wasm or
scripts, general-purpose bytecode, runtime code generation, and network or
filesystem extensions. A later contract edit cannot silently open one of those
surfaces while retaining the same enterprise gate.

The normative program envelope, closed type/operator/action vocabulary,
canonical decimal representation, exact bounds, and golden evaluation vectors
are pinned by `policy-v1.json`. A program that does not validate against the
pinned schema, or whose action parameters or phase visibility do not match that
contract, cannot be compiled or activated.

Compilation occurs before activation. A runtime pins the compiled program
digest, language version, compiler version, and required input-schema version.
Simulation and enforcement call the same evaluator and phase functions.
Unknown operators, missing required inputs, version mismatch, bound exhaustion,
or evaluator failure reject activation or fail the applicable hard decision
closed. Only a policy action explicitly declared `advisory` may fail open, and
that outcome is auditable.

### 9. Policy phases have fixed visibility

| Stable beta phase | May observe | Must not observe | Bounded actions |
| --- | --- | --- | --- |
| `credential` | server-derived organization/project/environment IDs, principal and credential IDs/types, credential security revision, operation/surface/mode, source class | credential material, raw headers, request body, client-asserted scope | deny; require stronger credential class; select a named admission policy |
| `admission` | request context, canonical operation metadata, declared/observed byte and token bounds, quota/budget state, rounded server time input | prompt/output/tool content, media bytes, provider secrets | allow/deny; reserve named quota/budget; cap requested output; attach bounded decision tags |
| `request_guard` | bounded canonical user/system text and tool schema/arguments plus media type/size metadata for this request only | provider credentials, raw headers, media bytes, prior requests, provider response | allow/deny; deterministic bounded redaction or field removal explicitly supported by the operation schema |
| `route` | request context, certified candidate IDs and metadata, grant/policy facts, coarse freshness-bounded health and cost classes | request/response content, secrets, arbitrary telemetry labels | deny or filter candidates; choose a bounded score profile; lower attempts/concurrency/timeouts; never add an uncertified target |
| `attempt_result` | attempt metadata, stable error class/phase, commitment state, bounded usage metadata | provider error text, response content, raw headers | stop or continue within the precomputed route budget; mark usage incomplete; no hedging or post-commit retry |
| `reconcile` | reservations, terminal usage units/completeness, pricing provenance, budget/quota versions | request/response/tool/media content, credentials | commit/release reservation; record bounded discrepancy and hard-control health |

Content presented to `request_guard` exists only in request memory. Policy input,
output, explanation, audit, telemetry, and durable decision records contain
metadata and bounded reason/action identifiers, never copied content. A policy
that needs an unsupported content surface cannot activate for that operation.

Phase order is fixed. A handler cannot call a connector without the credential,
admission, request-guard (when bound), and route decisions associated with the
pinned request context. Hard quota and budget reservations precede connector
execution and reconcile exactly once after terminal accounting.

Every applicable organization, project, environment, application, and
credential binding is composed in the frozen broad-to-narrow order. Narrower
bindings cannot weaken ancestor hard controls: denies and stops dominate,
candidate filters intersect, numeric caps take the minimum, redactions union,
and every distinct reservation applies. Any other unresolved conflict rejects
activation.

Program and binding revisions move through validation and compilation before
approval and activation. Active bytes are immutable. Activation publishes a
new monotonic environment runtime generation, and rollback selects a prior
verified program through new program, binding, and environment revisions after
current authority checks; it never mutates the active revision in place.

## Stable and deferred surfaces

Connector v1 and the six policy phases above are the enterprise-beta stable
surfaces. Their schemas may gain optional fields under the compatibility rules,
but existing meaning cannot change within the major version.

Deferred from the beta contract are in-process extension ABIs, arbitrary custom
policy functions, connector-initiated callbacks, generic tool execution,
unbounded extension payloads, response-content policy, semantic routing,
hedging, and complete vendor-native API parity.

## Required conformance evidence

M0 includes a dependency-free contract/build fixture at
`tests/reference-connector-v1/main.rs`. It compile-checks an external
implementation shape for every frozen Connector v1 RPC name, binds an open
string `type_id`, and is mechanically rejected if it reaches into
`olp-domain`, `ProviderKind`, or another OLP implementation crate. This is only
a build proof: it is not a generated protobuf binding, published SDK, gRPC
transport, runtime integration, conformance suite, or qualified connector, and
it does not satisfy the M4 gate below.

M4 must provide a reference external connector that imports only the published
Connector v1 SDK/protobuf package, registers a new `type_id`, and runs without a
new `ProviderKind` variant or an OLP rebuild. Conformance must cover compatible,
incompatible, slow, unavailable, malicious, and malformed connectors;
streaming commitment; cancellation; media cleanup; secret isolation; usage
completeness; resource exhaustion; secret rotation/revocation drain and forced
instance termination; and upgrade/rollback pinning.

M5 must provide golden and property tests for every policy phase, input
visibility, deterministic replay, simulation/enforcement equality, program and
input bounds, hard/advisory failure behavior, quota/budget races, and redacted
decision explanations.

M8 qualification requires the matching scorecard evidence and accepted
residual risks from `docs/enterprise/threat-model.md`.

## Consequences

The connector protocol and policy IR add versioned compatibility surfaces that
must be maintained independently from Rust internals. The process and network
boundary costs more than an in-process call, and content sent to a connector is
still exposed to that connector. In return, a connector defect or compromise is
constrained by an enforceable workload boundary, provider types stop requiring
core enum changes, and customer policy cannot corrupt or escape the OLP process.
