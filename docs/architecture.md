# Architecture and change map

The Go module owns production code under `internal/` and the public provider
plugin SDK under `sdk/`; `cmd/olp` starts the CLI.
The console mirrors feature ownership under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/database/migrations/`.

| Change | Start here |
| --- | --- |
| Provider configuration, models, credentials, certification, revisions | `internal/providers/` and `console/src/lib/features/providers/` |
| Provider plugin install, approval, confined runtime, hosted plugin code and ABI | `internal/plugins/`, `sdk/plugin/` and `console/src/lib/features/plugins/` |
| Grants beneath credential versions, grant enrollment sessions and grant refresh | `internal/grants/`, with its API in `internal/providers/grants.go` |
| Route drafts, publication, history | `internal/routes/` and `console/src/lib/features/routes/` |
| Snapshot schema, routing policies, selection, compiled target contracts and revision/pin validation | `internal/routing/` |
| Users, sessions, OIDC, projects, API keys, budgets, tokens, audit | `internal/access/` and `console/src/lib/features/access/` |
| Installation settings and notification destinations/rules | `internal/access/` and console access/settings features |
| Configuration export, plan, and apply | `internal/configuration/` |
| Content-policy validation and matching | `internal/contentpolicy/` |
| Provider-resource mappings and stored-response accounting | `internal/resources/` and `internal/gateway/` |
| Admission, request execution, retries, cancellation | `internal/gateway/` |
| Code-mode contracts, conversation pins, hard-token reservations | `internal/codemode/`, `internal/resources/code_mode.go`, `internal/limits/` |
| Code-mode public management and immutable publication | Code-mode files in `internal/providers/`, `internal/access/`, `internal/routes/`, `internal/runtime/`, `internal/usage/` |
| Official coding-client qualification and controlled wire peers | `tests/codecli/`, `tests/fixtures/codex-qualified/`, `tests/fixtures/coding-plans/`, `tests/integration/code_qualification*_test.go`, `tests/integration/code_coding_plans_test.go`, `tests/integration/code_client_journeys_test.go` |
| Upstream failure classes and upstream acceptance | `internal/upstream/` |
| Immutable operation sources, envelopes, provenance and codec linking | `internal/oif/` |
| Ordered generation views and independent operation contracts | `internal/operations/` |
| Strict generation admission, continuation and semantic obligations | `internal/interaction/` |
| Registered non-generation strict operation contracts | `internal/operationplan/`, `internal/operationregistry/` |
| Strict media, batch, realtime and Gemini lifecycle contracts | `internal/mediacontract/`, `internal/durablecontract/`, `internal/realtimecontract/`, `internal/geminilifecycle/` |
| Transformed provider preparation and wire defaults | `internal/providerinvoke/` |
| Provider profiles, addressing, and upstream hosting, authentication and signing | `internal/connectors/` |
| Shared wire formats, request/result envelopes, usage and errors | `internal/protocols/wire/` |
| OpenAI, Anthropic, Gemini, Bedrock codecs and cross-dialect translation | `internal/protocols/` |
| Database publication, release installation, authority refresh and credential source | `internal/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `internal/limits/` |
| Admission token estimates: tokenizers, the request walker, per-family counters | `internal/operations/tokenization/estimate/` |
| Cost reservation at admission; rate-limit and gateway metadata response headers | `internal/gateway/cost_reservation.go`, `response_headers.go` |
| Accounting, pricing, request history, ingestion, retention, notification delivery | `internal/usage/` and `console/src/lib/features/usage/` |
| Provider billing configuration, invoice imports and reconciliation | `internal/billing/` and billing files in `console/src/lib/features/providers/` |
| Export sinks, queued metering delivery, FOCUS datasets and payload capture | `internal/export/` and `console/src/lib/features/observability/` |
| Playground execution state, request composition, routing inspection | `console/src/lib/features/inference/playground/` |
| Uploads, durable media jobs, reconciliation | `internal/media/` and `console/src/lib/features/media/` |
| Configuration, connections, startup, shutdown | `internal/config/`, `internal/process/`, `internal/database/` |
| HTTP middleware, development origin, body limits | `internal/process/` |
| Public path surfaces: admission pools, console-reserved prefixes, edge routes | `internal/surface/` |
| Outbound networking, credential redaction, secret purposes | `internal/egress/`, `internal/secrets/` |
| Management authorization policy, contract requirements, route admission | `internal/access/policy.go`, `contract.go`, `route.go`, `scope.go` |
| Gateway key locations and retained-resource admission | `internal/gateway/credentials.go`, `retained.go` |
| Public response security headers | `internal/process/perimeter.go` |
| Metrics, traces, readiness, worker health | `internal/observability/` |

## Contracts and feature ownership

Feature packages own SQL, transactions, validation and audit provenance. Prefer
concrete types and explicit dependencies; introduce interfaces at actual
substitution boundaries. Package context supplies names instead of redundant
provider- or protocol-specific aliases.
`openapi/management.json` is the management source; `openapi/document.go` embeds
it. `make api` generates Go/TypeScript types, console authorization requirements
and safe SDK/CLI/MCP consumer metadata. Unsupported serializer shapes fail
generation; integration tests enforce contract/handler parity.

Callers own logical mutation identity. Console drafts retain submitted payload,
ETag and Idempotency-Key through ambiguous retries, retire secrets on abandonment/
resource/principal changes and fence stale callbacks. A confirmed write is not
repeated because refresh failed.

Configuration promotion owns portable intent and destination bindings, not feature
validation. Direct and promoted provider edits share credential-boundary,
certification-invalidation and staged-inventory operations. Unchanged slots retain
enrollment lifetime; parent/collection ETags advance coherently. Installation
serialization protects reauthorization, replay, publication and atomic audit.

## Runtime and immutable sources

Published releases freeze configuration and credential bytes; serving borrows
immutable state without copying complete snapshots per request. Live authority
refresh progresses independently with monotonic freshness and generation fences.
Historical remote-secret lookup occurs outside short authorized database
transactions, followed by revocation revalidation.

Routing has no runtime-manager or database dependency. `Snapshot.Validate` checks
configuration without replacing compiled contracts; `Snapshot.Compile` publishes
a complete set of derived contracts only after every target succeeds. Runtime
owns installation and live credentials. Management boundaries translate routing
validation failures to HTTP problems.

`Snapshot.Clone`, `connectors.Config.Clone` and `contentpolicy.Policy.Clone` give
new owners independent mutable configuration. Immutable plugin profiles may be
shared. Connector configuration constructs serving identities for every operation;
provider management reads health statistics from observability, which the gateway
can report without depending on provider management.

`routing/revision.go` owns pure revision decoding. `routing/release_pin.go` verifies
exact historical provider/route/target/slot/credential identity for retained/media
work. Ambiguous migration backfills remain null and refuse, never substitute current
configuration. Runtime credential source owns current and historical resolution;
grant eligibility remains live.

OIF sources retain immutable JSON spans, presence and numeric provenance without
provider/storage dependencies. `wire` owns neutral envelopes and format names;
codecs own parsing, validation and encoding. Request transformations return a new
request or an immediate error and preserve the original source. Reject decoded
duplicate keys, invalid UTF-8 and surrogates; accessors copy or expose explicit
immutable views. Protocol owners
validate bounded native events and terminal proof. Local projection cannot invent
provider completion.

## Ordinary execution

`routing.Select(..., routing.Options)` owns selection policy, constraints, stable
ordering and slot budget. Execution and simulation consume its `routing.Plan`;
request preparation is supplied explicitly by each operation. Gateway attempt state
separates request transmission, upstream acceptance, validated native completion,
durable mapping and client visibility; typed upstream evidence applies uniformly
to native and plugin traffic.

Strict contracts are per-target and operation-owned; transformed preparation owns
declared defaults/translation. A gateway `call` owns authority, deadlines, financial
state and the shared attempt loop. Its `routeLeg` owns selection and preparation;
`generationCall` and `unaryCall` own their respective source and operation state.
Media and code-mode lifetimes remain with their existing owners.

Each preparation record belongs to one immutable source and route leg, keyed by
target ID, provider ID, provider revision and model. Template lookup uses the exact
target ID even when several targets share a provider/model pair. Estimates and
dispatch read the same prepared document. Retain at most four heavyweight plans,
release unused plans before streaming, and deterministically rebuild evicted plans
from the pinned source and release. A route transition replaces the entire leg.

Observe native usage before projection/persistence/delivery. Validated terminal
consumption survives failed writes/flushes; partial/missing usage stays uncertain,
not zero. No retries after ambiguous accepted work or client commitment. Join
multipart writers and both duplex tasks before terminal accounting.

## Code-mode forwarding boundary

[Code mode](features/code-mode.md) separately forwards `/code/{slug}` native
models/bodies without ordinary translation, rewriting or retries.
`Snapshot.CodeRoutes` and `CodeConnection` pin immutable route/provider pairs;
missing frozen configuration refuses until republish. Path adapters select only
matching accounts. Pool/key/grant eligibility remains live authority.

`CodeTransport` forwards raw HTTP/SSE/WebSockets, `CodeAuthorizer` resolves pinned
account credentials and principal, and `CodeLedger`/`resources.CodeStore` atomically
admit every generation, bind its conversation tree and reserve proven hard-token
bounds. Upgrade is not generation admission. Match observed principal before one
`MarkDispatched`; prepared aborts can refund, dispatched unknown usage cannot.
Equal settlement is idempotent; conflicting totals refuse and proven overruns record
`bound_violation`. Rate estimates and upstream allowances are separate ledgers.
No bodies/tools/auth headers persist. See [qualification](qualification/code-mode.md).

## Accounting and process lifetime

PostgreSQL owns durable events/prices/receipts; Valkey holds coordinated counters,
reservations, health and delivery hints. Aggregate attempt ownership is key project,
then provider project, then route project, with exact decimals/calendar windows.
Dispatched rate allowance is never refunded for smaller reported usage.

Retained generation mappings settle the original content-free event once.
Pending subject projections keep unresolved spend visible even to newly enabled
budgets; unsupported templates stay retained. Maintenance uses bounded passes,
savepoints and progress cursors. Original HTTP observations may remain null until
a cancellation-independent finalizer completes; it cannot change financial identity.

Process composition owns one startup/shutdown cleanup path and shared deadline.
Drain requests/shadows/probes while authority remains, then close/flush metadata,
stop workers and close dependencies. Expiry explicitly unblocks and reports
incomplete joins rather than waiting without bound.

See [security](security.md), [execution](gateway.md), [access](access.md),
[pricing](pricing.md), [operations](operations.md) and the
[historical repair review](architecture-review.md).
