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
| Route drafts, target selection, publication, history | `internal/routes/` and `console/src/lib/features/routes/` |
| Users, sessions, OIDC, projects, API keys, budgets, tokens, audit | `internal/access/` and `console/src/lib/features/access/` |
| Installation settings and notification destinations/rules | `internal/access/` and console access/settings features |
| Configuration export, plan, and apply | `internal/configuration/` |
| Content-policy validation and matching | `internal/contentpolicy/` |
| Provider-resource mappings and stored-response accounting | `internal/resources/` and `internal/gateway/` |
| Admission, request execution, retries, cancellation | `internal/gateway/` |
| Code-mode contracts, conversation pins, hard-token reservations | `internal/codemode/`, `internal/resources/code_mode.go`, `internal/limits/` |
| Code-mode public management and immutable publication | Code-mode files in `internal/providers/`, `internal/access/`, `internal/routes/`, `internal/runtime/`, `internal/usage/` |
| Official coding-client qualification and controlled wire peers | `tests/codecli/`, `tests/fixtures/codex-qualified/`, `tests/integration/code_qualification*_test.go` |
| Upstream failure classes and upstream acceptance | `internal/upstream/` |
| Immutable operation sources, envelopes, provenance and codec linking | `internal/oif/` |
| Ordered generation views and independent operation contracts | `internal/operations/` |
| Strict generation admission, continuation and semantic obligations | `internal/interaction/` |
| Registered non-generation strict operation contracts | `internal/operationplan/`, `internal/operationregistry/` |
| Strict media, batch, realtime and Gemini lifecycle contracts | `internal/mediacontract/`, `internal/durablecontract/`, `internal/realtimecontract/`, `internal/geminilifecycle/` |
| Transformed provider preparation and wire defaults | `internal/providerinvoke/` |
| Provider profiles, addressing, and upstream hosting, authentication and signing | `internal/connectors/` |
| OpenAI, Anthropic, Gemini, Bedrock codecs and cross-dialect translation | `internal/protocols/` |
| Immutable runtime publication, activation, authority refresh, credential source, strict contract compilation | `internal/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `internal/limits/` |
| Accounting, pricing, request history, ingestion, retention, notification delivery | `internal/usage/` and `console/src/lib/features/usage/` |
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

Feature packages own their SQL, transactions, and workflows; mutations receive
explicit audit provenance. `openapi/management.json` defines the management
contract, including the security requirement of every operation. `make api`
generates Go transport types, ignored TypeScript declarations, and the console's
route requirements; `/api/v1/openapi.json` serves the embedded contract. Integration
tests check handler/contract parity.

## Code-mode forwarding boundary

[Code mode](features/code-mode.md) has a distinct `/code/{slug}` forwarding
contract. Its body retains native model names and does not enter ordinary
model-route translation, provider preparation, content rewriting or inference
retry/failover. The [qualification matrix](qualification/code-mode.md) separates
implemented management/ledger behavior from client and real-provider evidence.

`Snapshot.CodeRoutes` contains immutable published route documents.
`Snapshot.CodeConnection(route, providerID)` resolves connections frozen under
`revisionID:providerID`; a missing connection refuses until republish instead of
falling back to a mutable provider draft. Pool membership, API-key permission,
account/grant eligibility and retirement remain live admission authority.

`gateway.CodeTransport` owns raw HTTP/SSE and WebSocket forwarding.
`gateway.CodeAuthorizer` resolves encrypted account authorization and returns
headers plus an observed principal. `gateway.CodeLedger`, implemented by
`resources.CodeStore`, atomically authorizes each generation, creates/reuses a
whole-tree account pin, reserves all matching hard-token budgets and records an
attempt. WebSocket upgrade does not substitute for per-generation admission.
Process composition supplies `gateway.NewCodeForwarder` and
`providers.CodeAuthorizer` with the existing runtime and plugin host.
WebSocket handshakes bind and authorize before upstream connection and downstream
upgrade; every generation is admitted separately. Response IDs become durable,
route/key-scoped aliases to a binding; ambiguous references refuse.

`Admission.ReserveCodeRate` shares request/token-rate/concurrency accounting
without ordinary monetary limits. A rate estimate never becomes a hard bound.
The transport must match authorization's principal to the binding, call
`MarkDispatched` once before upstream dispatch, and never retry. Prepared failures
may abort; dispatched failures with no final usage settle as durable uncertainty.
Identical settlement is idempotent, conflicting totals refuse, and a proven-bound
overrun records `bound_violation`. Provider allowance observations are separate
from both this ledger and ordinary monetary accounting.

Management stays in the owning feature packages and uses the existing project,
scope, CSRF, ETag, idempotency and audit infrastructure. Observers may inspect
in-memory copies only: persisted attempts/refusals contain bounded metadata,
never request/response bodies, tool payloads or authorization headers.

`internal/runtime/revision.go` reconstructs providers and routes from scanned
revision metadata and stored JSON. Runtime publication and retained-resource
resolution share these pure decoders. A plugin provider's revision decodes with
the plugin it pins (`PluginColumn`: its manifest and whether it is unconfined),
so snapshots carry its plugin profile and gateways read no manifest; a
profile's signing hook runs on the code `plugins.Host` loads by digest: a
confined plugin's module on wazero, or an unconfined plugin's executable as a
subprocess speaking the ABI over stdio, where the deployment enables the
unconfined tier. Their callers still own SQL, transactions, authorization,
credential checks, and operation eligibility. Publication alone drops empty
provider limits; retained resources preserve the stored limits.

The console usage feature owns `PricingRevisionsPanel`, including its queries,
form, decimal validation, submission, and pagination. Settings keeps the panel
mounted for the page's lifetime and coordinates shared feedback and mutual
exclusion between setting saves and pricing creation through its `blocked`,
`onBusyChange`, and `onFeedback` props. The panel applies installation capabilities
and role permissions to its queries and rendering.

Each mounted playground page creates one `PlaygroundState` rune instance. It
owns queries, mutations, request construction, routing inspection, eligibility,
effects, and stream cancellation on unmount. The page owns presentation constants,
markup, and styles. Strict clients, native operations, audio translation, and
realtime traces keep their own operation-specific behavior.

Provider routing stays in `internal/runtime`: `PlanRequest` resolves policy
and budget, evaluates candidates, orders them stably, and assembles decisions.
Published eligibility and source constraints precede semantic preparation;
effective request constraints follow successful preparation. The plan retains
eligible target candidates, while preview attempt numbers and budget exhaustion
apply to credential slots. Excluded credentials do not spend the budget.
`internal/gateway` supplies common selection authority and measurements through
its explicit runtime interface, with operation-specific preparation callbacks.
`internal/routes` shares one inspection runner between draft and published
simulations; their handlers own loading, authorization and response formats.
Inspection never dispatches upstream or reserves provider state.

Inference pins an immutable runtime snapshot. `internal/gateway/attempts.go`
owns shared attempt progression, reservations, settlement, health and failover
for canonical inference and ordinary media. `executor.go` and `media.go` retain
their transport deadlines, streaming commitment and delivery; adjacent resource,
video, Bedrock, and realtime paths handle their specific lifecycles. Every
upstream call path, including media workers and provider probes, reports its
evidence to the `internal/upstream` classifier: whether the request reached the
upstream, the status and error it stated, and any interruption or transport
failure. The classifier derives the failure class and upstream acceptance, so a
transport other than net/http is classified the same way. A plugin profile's
declared classification rules, which the connector config carries, take
precedence over the built-in rules for the failures they match. Protocol
codecs live in `internal/protocols/`. Independent key-authority refresh prevents
a failed activation from retaining revoked access.

`internal/runtime/credentials.go` is the one credential source. Planning, slot
availability, dispatch, retained resources and continuations, and media
reconciliation ask it whether a credential version is eligible and for its
usable secret: from the pinned release, or from the secret authority for a
historical revision's version. A version with a grant serves the grant's
current access token: a worker's refresh advances the grant's generation, and
each poll reloads the access tokens whose generation changed. An ineligible
version carries its reason into plan and attempt records.

OIF is an in-process contract, not a public API or another request authority.
`internal/oif` owns immutable JSON source spans, exact presence and numeric
representations, request/result/event envelopes, bounded blob references, and
explicit provenance. It imports no provider, transport, storage, or generation
implementation. `internal/operations/generation` owns ordered messages, nodes,
tool dependencies, candidate branches, and native control scopes. Dialect
adapters in `internal/protocols` derive those views from source and link them
through a trusted build-time registry; a non-generation operation does not
extend generation types.

Ingress rejects duplicate decoded keys and malformed UTF-8 or surrogate escapes
before dispatch. Compatibility request accessors return copies; resource and
content-policy changes create overlays without replacing the caller source.
Unary results retain their native source before model rewriting. Stream codecs
lift bounded native events before projection, including framing metadata, and
keep no unbounded event history. Protocol validators own terminal grammar, while
the gateway owns cancellation and response commitment.

`protocols.PrepareTarget` records the transformed mapper's destination and the
defaults it actually applied. Explicit destination dialects cannot fall
back to another API. `protocols.PrepareIdentity` preserves native subtrees and
allows only registered model/resource/transport changes; it does not normalize
Responses input strings, rename token controls, or qualify an interaction by
itself. Strict contracts compile per published route target: `internal/interaction`
owns generation admission, `internal/operationplan` and the `internal/operationregistry`
composition root own the other unary operations, and `internal/mediacontract`,
`internal/durablecontract`, `internal/realtimecontract` and `internal/geminilifecycle`
own their surfaces. The planner still owns policy coverage, profile compatibility,
source requirements and client continuation; `internal/providerinvoke` owns the
transformed admission path.

PostgreSQL owns durable state. Valkey coordinates limits, hints, and accounting
events; retries and deduplication support recovery. Durable media jobs and
provider resources retain their admitted credential references through rotation
and restart. Workers remain with the feature they maintain.

Process startup composes gateway, control, worker, and all-in-one modes, owning
resource lifetimes and listeners. Draining, metadata delivery, worker shutdown,
and trace flushing share one deadline. Production serves static console assets;
development uses Vite with configured API proxies.

See [security architecture](security.md) for how these modules divide the
security rules. See [gateway execution](gateway.md),
[access control](access.md), and the
[worker runbook](operations.md#replicated-worker-health) for their operational
contracts. See [CONTRIBUTING.md](../CONTRIBUTING.md) for setup, checks,
integration, and releases.
