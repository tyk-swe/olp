# Architecture and gateway execution

This guide maps OLP's feature ownership and explains how a request moves through
the gateway. Production Go code lives under `internal/`, public packages live
under capability-named top-level folders such as `plugin/`, `management/`,
`inference/`, and `guardrails/`, and `cmd/olp` starts the binary. Folders are
named for the module that owns the behavior rather than for a layer. The console
mirrors the same feature boundaries under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/platform/database/migrations/`.

| Area | Backend and console owner |
| --- | --- |
| Connections, certification, catalog and grants | `internal/providers/`, `internal/connectors/`, `internal/grants/`, `internal/catalog/`, `internal/signing/`, and console `providers/` |
| Routes, publication, caching and selection | `internal/routes/`, `internal/routing/`, `internal/release/`, `internal/cache/`, and console `routes/` |
| Users, keys, identity and consumer auth | `internal/identity/` (sign-in, sessions, MFA, OIDC, SAML, SCIM, members and tokens), `internal/apikeys/` (keys, end users and workload issuers), `internal/workload/`, `internal/consumerauth/`, `internal/keyauthority/` (the key authority policy a release publishes), `internal/access/authz/` (callers, roles and operations), `internal/auditlog/`, and console `identity/` and `access/` |
| Management transport, console shell and CLI | `internal/access/` (authentication, route registration, transactions, replay and audit), `internal/access/httpkit/` (problems, replies, decoding and pagination), `internal/management/` (generated contract types, serving and overview), `internal/console/`, `internal/operatorcli/`, and console `overview/` |
| Projects, installation settings and policy | `internal/projects/`, `internal/settings/` (settings and branding), `internal/budgets/` (budget groups, increases and execution debits), `internal/limits/`, and console `access/`, `settings/` and `budgets/` |
| Notifications | `internal/notifications/` and console `notifications/` |
| Admission, execution, media and retained resources | `internal/gateway/` (assembly, execution kernel in `kernel/`, extracted surfaces in `surface/`, plus `anthropicbeta/`, `codeforward/`, `delegation/`, `duplexrelay/`, `events/`, `fault/`, `generationrefs/`, `guardstream/`, `health/`, `healthprobes/`, `ingress/`, `managedbatch/`, `models/`, `publishedprompt/`, `quota/`, `realtimeresponses/` and `traceingest/`), `internal/resources/` (with `manage/` and `retainedworker/`), `internal/retainedapi/`, `internal/batches/`, `internal/retrieval/`, `internal/media/` (with `jobs/` and `transport/`), and console `inference/`, `media/`, `resources/` and `health/` |
| Code mode | `internal/codemode/` (vocabulary, with `codeadapter/`, `codeclient/`, `codewire/`, `codeadmin/`, the plan profiles and plugins in `codeplans/`, and the subscription auth plugins in `claudeauth/`, `codexauth/` and `antigravityauth/`), `internal/codesessions/` (account selection and bindings, with `codeallowance/` and `codeaccounts/`), plus research and Codex wire handling under `internal/gateway/codeforward/` (`coderesearch/` and `codexwire/`), and console `code-mode/` |
| Source, operation, protocol and lifecycle contracts | `internal/domain/` (`oif/`, `operations/`, `operationregistry/`, `protocols/`, `geminilifecycle/`, `agentcontract/`, `grantconfig/`, `vendors/`, and other pure vocabulary packages) and `internal/contracts/` (the strict contract compilers `interaction/`, `unary/`, `duplex/` and `media/`) |
| Preparation and failure evidence | `internal/providerinvoke/` and `internal/upstream/` |
| Accounting, pricing, billing and exports | `internal/usage/` (event contract, with `completeness/`, `delivery/`, `ingest/`, `param/`, `pricebook/`, `rates/`, `reports/`, `retention/` and `routinginputs/`), `internal/billing/`, `internal/export/`, `internal/sinks/`, and console `usage/` and `exports/` |
| Plugins and guardrails | `internal/plugins/`, `plugin/`, `internal/guardrails/`, and console `plugins/` and `guardrails/` |
| Agents, MCP registration, tool execution and personal grants | `internal/agents/` (`a2a/`, `investigations/`, `publications/`, `runs/`, `tasks/`), `internal/mcp/`, `internal/grants/personal/`, and console `agents/` and `plugins/` |
| Promotion and process lifetime | `internal/promotion/` kernel, one `internal/promotion/<section>/` package per document section, `internal/promotion/serve/`, `cmd/olp/`, and `internal/platform/database/` |
| Egress, secrets and telemetry | `internal/platform/egress/`, `internal/platform/secrets/`, `internal/platform/staging/`, `internal/platform/telemetry/`, `internal/platform/workerhealth/`, `internal/observability/`, and console `health/` |

## Contracts and feature ownership

Feature packages own SQL, transactions, validation and audit provenance. Prefer
concrete types and explicit dependencies, and introduce interfaces only at
actual substitution boundaries. Package context supplies names instead of
redundant provider- or protocol-specific aliases. `management/management.json` is
the management source and `management/document.go` embeds it. `make api`
generates Go and TypeScript types, console authorization requirements and safe
SDK, CLI and MCP consumer metadata. Unsupported serializer shapes fail
generation, and integration tests enforce contract and handler parity.

Callers own logical mutation identity. Console drafts retain submitted payload,
ETag and Idempotency-Key through ambiguous retries, retire secrets on
abandonment or resource and principal changes, and fence stale callbacks. A
confirmed write is not repeated because refresh failed. Provider detail state
owns draft reconciliation and semantic commands, while the wizard retains its
onboarding phases. Personal grant state owns enrollment lifetime and
stale-response rejection. API key and publication form modules compile typed
commands. Playground rendering, inspection and submission consume the same
resolved execution plan.

Configuration promotion owns portable intent and destination bindings rather
than feature validation. Direct and promoted provider edits share
credential-boundary, certification-invalidation and staged-inventory operations.
Unchanged slots retain enrollment lifetime, and parent and collection ETags
advance coherently. Installation serialization protects reauthorization,
replay, publication and atomic audit. Every `access` mutation helper acquires
that lock and refreshes authorization before feature commands run.
Project-policy batches and billing-import writes share their feature-owned
persistence commands with promotion.

Each promotion section parses its part of the generated contract document once
at the request boundary, reads and renders destination state, and contributes a
planner and ordered appliers. Plan, apply and export run the same section code,
and the document digest is computed over the rendered canonical document. Every
credential binding must be claimed by a section reference, and unclaimed
bindings are refused before any planner runs.

`providers.Server` owns provider draft, slot, credential, certification,
activation and grant enrollment changes. Promotion writes drafts through the
narrower `provider.Drafts` interface it declares. The stored configuration
document and the kind registry live in `providers/connection`, upstream probes
in `providers/probe`, and code accounts in `codesessions/codeaccounts`. Network
steps run outside mutation locks, with authority and slot identity checked again
before commit. Personal grant enrollment remains a separate protocol.

`internal/agents/publications` keeps two publication commands. `Publish` validates
current references even when the draft digest already exists. Promotion uses
`EnsureRetainedRevision` to reuse retained definitions without revalidating
changed dependencies. Revision retention, labels, final selection, authority and
runtime publication remain in one transaction.

Provider configuration identity and connector projection live in
`connectors/providerconfig`. Provider mutations accept a prepared configuration
whose constructor owns normalization, plugin resolution and validation. Stored
management and serving representations retain their original serialization.

MCP follows the same feature ownership. `mcp/registration` owns registration
validation, revisions, executable permissions and publication. Direct writes and
configuration promotion share preparation and persistence operations that return
new values. `mcp/upstream` owns connections and catalog certification, using the
same tool-schema compiler as routing snapshots.

The `mcp` executor owns authorization, guardrails, reservation, dispatch and
settlement through constructor-injected dependencies. JSON, Streamable HTTP,
legacy SSE and workflow adapters translate explicit execution outcomes into
their response contracts. `mcp/accounting` owns durable accounting and recovery,
while `mcp/sessions` owns session identity, capacity, expiry and Valkey
persistence. `mcp/messages` supplies bounded JSON-RPC handling with raw request
IDs to consumer, upstream and management adapters. Management keeps its own
authorization and administrative dispatch.

`grants/personal` owns personal OAuth, Entra exchange, renewal and delegated
signing for MCP and agent calls. Configure-time validation remains with
registration over the shared types in `domain/grantconfig`. Registration hands
`grants/personal` its server lookup through the constructor in `cmd/olp`.

## Runtime and immutable sources

Published releases freeze configuration and credential bytes. A `routing.View`
keeps published maps private and returns detached selected values while reusing
compiled contracts. Serving never copies complete snapshots per request. Live
authority refresh progresses independently with monotonic freshness and
generation fences. Historical remote-secret lookup occurs outside short
authorized database transactions, followed by revocation revalidation.

Worker health belongs to `platform/workerhealth`. Reporters receive their task
and region explicitly and can write through a caller-owned transaction.
Application shutdown waits for every registered worker, including workflow
recovery, before closing its dependencies.

Routing has no runtime-manager or database dependency. `Snapshot.Validate`
checks configuration without replacing compiled contracts, and
`Snapshot.Compile` publishes a complete set of derived contracts only after
every target succeeds. Runtime owns installation and live credentials.
Management boundaries translate routing validation failures to HTTP problems.

`Snapshot.Clone` and `connectors.Config.Clone` give new owners independent
mutable configuration, while immutable plugin profiles may be shared. Connector
configuration constructs serving identities for every operation. Provider
management reads health statistics from observability, which the gateway can
report without depending on provider management.

`routing/revision.go` owns pure revision decoding. `routing/release_pin.go`
verifies exact historical provider, route, target, slot and credential identity
for retained and media work. Ambiguous migration backfills remain null and
refuse rather than substituting current configuration. The runtime credential
source owns current and historical resolution, while grant eligibility remains
live. `resources.Resolver` returns the proved historical target as one value.
`providerinvoke` binds that target to current credentials, network secrets and
its connection scope. Resource revision and release-snapshot decoders remain
separate, as do consumer-specific eligibility and timeout rules. Caller
credentials use ephemeral connections.

OIF sources retain immutable JSON spans, presence and numeric provenance without
provider or storage dependencies. `wire` owns neutral envelopes and format
names, and codecs own parsing, validation and encoding. Request transformations
return a new request or an immediate error and preserve the original source.
Decoders reject duplicate keys, invalid UTF-8 and surrogates, and accessors copy
or expose explicit immutable views. Protocol owners validate bounded native
events and terminal proof, and local projection cannot invent provider
completion.

## Gateway packages

`internal/gateway/kernel` is the execution kernel. It owns the `Call`, the route
leg, the attempt loop, settlement, the generation routes and the retained steps
that read call or pin state, which surfaces call without reaching into internal
fields. A package under `internal/gateway/surface/` owns the routes of one
surface and calls the kernel through its exported API. `internal/gateway` itself
is the assembly, whose `Register` mounts the kernel and each surface. The other
packages hold what the kernel uses and never import it back.

| Package | Owns | May import |
| --- | --- | --- |
| `gateway/fault` | client-visible error values, per-surface error bodies, rate-limit headers and committed stream writing | none |
| `gateway/events` | the terminal request envelope, attempt facts and the log and accounting sinks | none |
| `gateway/quota` | limiter admission, typed key, route, target and aggregate reservations, cost reservations and supply | `events`, `fault` |
| `gateway/ingress` | concurrency slots, API key authentication, attribution, end-user identity and bounded body reading | `fault` |
| `gateway/health` | circuit state, credential cooldowns and fleet health | `events` |
| `gateway/codeforward/coderesearch` | bounding, method filtering and event-ID sealing for the subscription research MCP surface | none |
| `gateway/codeforward/codexwire` | Codex wire request classification, response observation and passive allowance header parsing without rewriting payloads | none |
| `gateway/codeforward` | `/code/{slug}` forwarding and the research proxy | `events`, `fault`, `ingress`, `quota`, `codeforward/coderesearch`, `codeforward/codexwire` |
| `gateway/traceingest` | OTLP trace ingest on `POST /v1/traces` | none |
| `gateway/delegation` | delegation grant creation on `POST /v1/delegations` | `fault`, `ingress` |
| `gateway/models` | the model catalog on the OpenAI, Anthropic and Gemini surfaces | `fault`, `ingress` |
| `gateway/guardstream` | streamed output guardrails, with the tool call and text framing of each wire family in its own file | none |
| `gateway/realtimeresponses` | which responses a realtime session still owes usage for, and the usage each terminal frame reports | `ingress` |
| `gateway/publishedprompt` | replacing a published prompt reference with the rendered messages and defaults in each wire family's form | `fault`, `ingress` |
| `gateway/generationrefs` | where a generation request names provider resources, and rewriting the ones a result or a stored input item list returns to the caller's identifiers | none |
| `gateway/anthropicbeta` | validating and forwarding `Anthropic-Beta` headers across compatible route targets | `fault` |
| `gateway/duplexrelay` | shared bidirectional WebSocket relay loop, bounded upstream writer, delegation reauth and abnormal teardown | `fault`, `ingress` |
| `gateway/managedbatch` | managed batch JSONL line translation, manifest metadata and Gemini batch record and result codecs | `fault` |
| `gateway/kernel` | the `Call`, admission, the attempt loop, pinned exchanges, settlement, native and generation dispatch, and retained steps such as retained dispatch, page admission and result projection | the 15 top-level gateway packages above (excluding `codeforward/coderesearch` and `codeforward/codexwire`) |
| `gateway/healthprobes` | the health probe worker, which picks opted-in targets, claims each once per interval across the fleet, sends one small request through `kernel.ProbeGeneration`, and shares the result and the fleet circuit | `health`, `kernel`, `quota` |
| `gateway/surface/anthropicstate` | Anthropic files and message batches | `fault`, `kernel` |
| `gateway/surface/bedrock` | Bedrock Converse and Invoke under `/bedrock/model/{model}` | `fault`, `kernel` |
| `gateway/surface/geminilive` | Gemini Live sessions under `/gemini/ws/` and `/ws/` | `duplexrelay`, `fault`, `ingress`, `kernel`, `quota` |
| `gateway/surface/media` | OpenAI images, audio and video jobs under `/v1/images`, `/v1/audio` and `/v1/videos`, including media execution, delivery and video job lifecycles | `events`, `fault`, `kernel`, `quota` |
| `gateway/surface/nativeduplex` | Native duplex sessions under `/native/{dialect}/sessions` | `duplexrelay`, `fault`, `ingress`, `kernel` |
| `gateway/surface/openaistate` | OpenAI files, batches, stored responses and continuations | `fault`, `generationrefs`, `ingress`, `kernel` |
| `gateway/surface/realtime` | OpenAI realtime WebSocket sessions and WebRTC calls under `/v1/realtime` | `duplexrelay`, `fault`, `ingress`, `kernel`, `quota`, `realtimeresponses` |
| `gateway/surface/retrieval` | Cloud retrieval queries under `/v1/retrieval/{binding_id}/query` | `fault`, `kernel` |
| `gateway/surface/cloudbatches` | Vertex and Bedrock batch jobs under `/native/{dialect}/batch-jobs` | `fault`, `ingress`, `kernel` |
| `gateway/surface/geminiinteractions` | Gemini Interactions under `/gemini/v1beta/interactions`, whole and streamed | `fault`, `kernel` |
| `gateway/surface/geministate` | Native Gemini files, resumable uploads and batches, including `:batchGenerateContent` | `fault`, `kernel` |
| `gateway/surface/guardrails` | Standalone guardrail evaluation under `/v1/guardrails/apply` | `fault`, `kernel` |
| `gateway/surface/passthrough` | Pass-through routes under `/native/{dialect}/routes/{route}/` | `fault`, `ingress`, `kernel` |
| `gateway/surface/retained` | Retained provider resources from the `retainedapi` catalog, such as fine-tuning jobs, vector stores, conversations, containers, evals, Anthropic skills, Gemini cached contents and agents, and Vertex tuning jobs, plus `/v1/vector_stores/{id}/search` and `/v1/responses/compact` | `fault`, `ingress`, `kernel`, `quota` |
| `gateway/surface/playground` | The console playground, which runs a route for a signed-in member through `kernel.MemberGeneration` and exposes `Handle` and `Stream` to the management API | `fault`, `kernel`, `publishedprompt` |

`scripts/check-lint.sh` enforces the last column and fails on any gateway
package without an entry, so a new gateway package declares its imports when it
is added. Most surface packages follow `surface/anthropicstate`, exporting one
`Register(k *kernel.Server, mux *http.ServeMux)`, keeping handlers on an
unexported `surface` type that holds the kernel, and registering in
`internal/gateway/gateway.go`. Three surfaces take extra collaborators or wire
into management instead. `retained.Register(k *kernel.Server, mux *http.ServeMux, retrievalQuery http.HandlerFunc)`
takes the retrieval query handler for vector store search.
`media.Register(k *kernel.Server, mux *http.ServeMux, deps *Deps)` takes its
`*media.Deps` bundle through `gateway.WithMedia`.
`playground.NewAPI(k *kernel.Server) *API` builds the playground API whose
`Handle` and `Stream` handlers `cmd/olp` mounts on the management API mux.

The kernel never names a surface or a vendor flag for one. A surface that needs
something from a shared step sets a general field on the `Call`, as
`PinnedHeaders` carries the Anthropic file header. A kernel step that differs by
provider asks the provider's `connectors.Config` or its `domain/vendors`
contract, as `ResponsesPath` and `BatchWindowFor` do, and
`scripts/check-lint.sh` refuses a kernel comparison of a provider kind or vendor
with a literal. The refusal matrix in `internal/gateway/surfaces_test.go`
registers the same assembly, so it covers a surface's routes after it moves.

The kernel owns the `/gemini/{version}/models/{model}:{action}` route because
generation lives there. A surface that serves one of those actions returns it
from an `Actions` function, as `surface/geministate` does for
`batchGenerateContent`, and the assembly passes the map to `RegisterKernel`.

A handler in a surface reads as a list of steps. `surface/geminiinteractions` is
the example for a request that admits, pins, sends and answers. Its handler
calls one named kernel step per stage, such as `ReserveGeneration`,
`GatePinnedGeneration` and `SettleGeneration` in `kernel/pinned_generation.go`,
and a surface calls kernel steps without reading or writing the `Call`'s
admission or settlement fields. When a surface needs a stage the kernel does not
name yet, add the step to the kernel and call it.

A step takes what it needs as arguments and returns what it found, so its
dependencies appear in its signature. The request value is built once and never
written again, as `surface/retained/request.go` shows. The only state a handler
writes is what its deferred settlement must still read when a step panics, such
as the pin and the status, and that state lives in one small type that says so,
like `retainedEnd`.

Put new code in the kernel only when every surface needs it. Code that reads a
few values from the `Call` takes them as a config struct and moves beneath the
kernel, as `guardstream.Config` does. A handler that can be built from its own
collaborators belongs in its own package, as `codeforward.Forwarder`,
`traceingest.Ingester`, `delegation.Handler` and `models.Handler` are. The
delegation handler takes the ingress gate, the key authority and the kernel
header preamble through its constructor, so it tests without a `Server` while
its responses keep the request identity and CORS headers every inference
response carries. Session reauthentication for delegated calls stays in the
kernel because it reads the `Call` it reauthenticates.

## Where new code goes

Start from the owner in the table at the top of this guide. Feature behavior
lives in the feature package with its SQL, transactions, validation and audit
provenance, and the console screen for it lives in the matching feature under
`console/src/lib/features/`. A new gateway surface is a package under
`internal/gateway/surface/`, work every surface shares belongs in the kernel,
and a gateway handler that can be built from its own collaborators belongs in
its own package under `internal/gateway/`. Each declares its allowed imports in
`scripts/check-lint.sh`. Process wiring belongs in `cmd/olp`, where each plane
is assembled by one constructor and `Run` owns mode selection, listeners and
shutdown ordering. A new small package is justified only when it owns behavior
of its own or breaks an import cycle with stable vocabulary, as
`internal/domain/routeidentity` and `internal/keyauthority` do. A wrapper whose
only job is forwarding to another package belongs in that package instead.

## Structure rules

`make lint` enforces the Go layout through `.golangci.yml` and
`scripts/check-lint.sh`. Read the failure message and fix the code. Frontend
boundaries are enforced separately by ESLint and documented in
[`console/README.md`](../../console/README.md#boundaries).

| Rule | Scope | Constraint |
| --- | --- | --- |
| `serving` | `internal/gateway/**` (non-test) | Denies the root `internal/access$` management kit package so the gateway never takes management transactions or server state. `gateway/delegation` and `gateway/surface/guardrails` still import `internal/access/httpkit`, and `gateway/surface/playground` imports both `internal/access/httpkit` and `internal/access/authz`. |
| `management` | `internal/access/**` (non-test) | Denies all imports of `internal/gateway`. Only `cmd/olp` wires serving and management planes together. |
| `kernel` | `internal/gateway/kernel/**` (non-test) | Denies `github.com/jackc/pgx`, `internal/access/httpkit` and `internal/access/authz`. Persistence stays in owning store packages, faults convert in `cmd/olp`, and member authorization passes through a callback on `kernel.Member`. |
| `gateway_isolation` | `internal/**` outside `internal/gateway/**` (non-test) | Denies all imports of `internal/gateway`. Features receive narrow collaborators from `cmd/olp` instead of importing gateway packages directly. |
| `resources_isolation` | `internal/resources/**` outside `internal/resources/manage/**` (non-test) | Denies `internal/access` and `internal/management/contract` so resource persistence and background workers stay free of management HTTP handlers. |
| `media_isolation` | `internal/media/**` outside `internal/media/jobs/manage/**` (non-test) | Denies `internal/access` and `internal/management/contract` so media codecs, transport, persistence and workers stay free of management HTTP handlers. |
| `domain` | `internal/domain/**` (non-test) | Allows only `internal/domain` and `internal/catalog$` inside `internal/`, and denies `net/http`, `database/sql`, `github.com/jackc/pgx` and `github.com/valkey-io`. Take values in as arguments and do I/O in the caller. |
| `platform` | `internal/platform/**` (non-test) | Allows only `internal/platform` and `internal/domain` inside `internal/` so shared infrastructure never imports a feature. |
| `library` | `internal/**` | Denies all imports of `cmd/`. |

Beyond those `depguard` rules, `scripts/check-lint.sh` verifies that top-level
modules under `internal/` never form an import cycle, with `internal/domain`
importing `internal/catalog` as the one allowed edge. When two modules need each
other, move the shared vocabulary into `internal/domain` or inject the
collaborator from `cmd/olp`. Every other directory under `internal/` is named
for the module that owns a behavior, including its SQL, validation and routes,
while `internal/access` remains the management kit with no feature logic inside
it. `funlen` caps functions at 100 lines and `revive` caps files at 500 lines of
code outside tests and generated `.gen.go` files. Split code by behavior rather
than by line count.

## Adding a management endpoint

Copy `internal/guardrails/credential/api.go`. It is the reference for every
management endpoint and shows the complete shape in one file.

1. Describe the operation in `management/management.json` and run `make api`.
   `Route` refuses to start when a registered pattern is missing from the
   contract, and it reads the permission the route needs from there.
2. Register the handler in the feature's `Register(mux)` with
   `server.Route(mux, "METHOD /api/v1/...", handler)`.
3. A read handler queries the pool, checks the caller's scope with
   `p.Project(&projectID, authz.View)` and returns `httpkit.OK`,
   `httpkit.Detail` or a list reply.
4. A write handler decodes and validates the body, then runs one of the six
   helpers in `internal/access/mutation.go` listed below.
5. For `Mutate`, `Remove`, `Transact` and `Preflight`, provide a `lock` step
   that loads the current row `FOR UPDATE` and authorizes the caller against it,
   or calls `RequireProject` for a create (use `access.Unlocked` when there is
   nothing to load). The `apply` step checks `httpkit.Match`, writes the change,
   and returns the `httpkit.Reply` together with the `access.Event` to audit.

| Helper | Steps | Behavior |
| --- | --- | --- |
| `access.Mutate` | `lock` and `apply` | Runs a change carrying `Idempotency-Key`. Locks and authorizes current state first, replays the stored reply if the same key and input already completed, and otherwise runs `apply`, records the audit row, stores the replay and commits. |
| `access.Remove` | `lock` and `apply` | Runs an idempotent delete, checking the stored replay before `lock` because a completed deletion leaves no row to lock. |
| `access.Transact` | `lock` and `apply` | Runs a change without `Idempotency-Key`, such as an edit guarded only by `If-Match`. |
| `access.Preflight` | `lock` only | Precedes `access.Mutate` when the change must call an upstream first. Authorizes the caller and serves any stored replay, then releases the installation lock so network calls never hold it, as `internal/providers/credentials.go` does for credential rotation. |
| `access.Run` | single `step` returning `(T, Event, error)` | Commits a step whose result is a value rather than an HTTP reply, such as claiming a grant enrollment or media-job deletion before an upstream call. |
| `access.Serialize` | single `step` taking `pgx.Tx` and returning `(T, bool, error)` | Acquires the installation `FOR UPDATE` lock without caller reauthorization or automatic audit, committing only when the step returns `true`. Used for unauthenticated or setup-stage serialized flows in `internal/identity/` such as initial setup, sign-in, MFA, OIDC and SAML transitions. |

The mutation helpers own the transaction, the replay record, the audit row and
the commit, so a handler never calls `tx.Commit`. The functions that open the
management transaction and read the replay record are unexported, so only
`internal/access` can call them. `Mutate`, `Remove`, `Transact` and `Run` record
their returned `access.Event` through `access.AuditForProject`. Call
`access.Audit` or `access.AuditForProject` directly only to record an extra
audit row in the same change or inside `access.Serialize`.

## Ordinary execution

`routing.Select(..., routing.Options)` owns selection policy, constraints,
stable ordering and slot budget. Execution and simulation consume its
`routing.Plan`, while request preparation is supplied explicitly by each
operation. Gateway attempt state separates request transmission, upstream
acceptance, validated native completion, durable mapping and client visibility.
Typed upstream evidence applies uniformly to native and plugin traffic.

Strict contracts are per-target and operation-owned, while transformed
preparation owns declared defaults and translation. A gateway `call` owns
authority, deadlines, financial state and the shared attempt loop. Its
`routeLeg` owns selection and preparation, and `generationCall` and `unaryCall`
own their respective source and operation state. Media lifetimes remain with
their existing owner, and code mode runs outside the call in
`gateway/codeforward`. A planning session owns selection, delegation and plan
adoption across operation adapters. The admitted-attempt lease owns final
authority checks, reservations and typed completion. The attempt loop owns
retries and fallback.

The bound exchange owns endpoint validation, authentication, signing, redaction
and transmission evidence. A prepared protocol response codec uses the exact
dispatched request. The provider response reader adds hosting envelopes, media
types and forced-stream aggregation. Guardrails, durable persistence and client
delivery remain gateway responsibilities.

Each preparation record belongs to one immutable source and route leg, keyed by
target ID, provider ID, provider revision and model. Template lookup uses the
exact target ID even when several targets share a provider and model pair.
Estimates and dispatch read the same prepared document. The gateway retains at
most four heavyweight plans, releases unused plans before streaming, and
deterministically rebuilds evicted plans from the pinned source and release. A
route transition replaces the entire leg.

Native usage is observed before projection, persistence or delivery. Validated
terminal consumption survives failed writes or flushes, and partial or missing
usage stays uncertain rather than zero. No retries run after ambiguous accepted
work or client commitment. Multipart writers and both duplex tasks join before
terminal accounting.

## Code-mode forwarding boundary

[Code mode](../use/code-mode.md) forwards `/code/{slug}` native models and
bodies without ordinary translation, rewriting or inference retries by default.
An explicit published initial recovery policy permits bounded additional
attempts only after complete supported HTTP rejection and positive
portable-state proof. `Snapshot.CodeRoutes` and `CodeConnection` freeze route
and provider pairs. Missing configuration refuses until republish, while pool,
key and grant eligibility remains live. `codewire.PortableInitialRequest` owns
the shared complete-request proof for initial recovery. Unknown or account-owned
state remains on its original account, and the native first dispatch remains
unchanged.

| Owner | Contract in | Invariant |
| --- | --- | --- |
| `codemode` | shared account, route, binding, usage and allowance types | observations never replace forwarded bytes |
| `codeadapter`, fed by `codeplans`, `codexauth`, `claudeauth` and `antigravityauth` | subscription profiles, client and upstream paths, operations, transports, conversation identity, credentials and observation rules | families sharing a client path agree on semantics, checked by `codeadapter.Validate` during snapshot validation |
| `routing`, installed by `release` | endpoint and model eligibility compiled from the frozen revision | a failed compile leaves the previous release serving |
| `codewire`, `codexwire` | native request classification and response observation on copies | parsing never reserializes the forwarded payload |
| `gateway/codeforward`, `codesessions.Store`, `limits` | dispatch, durable conversation pinning, admission and settlement, hard token budgets | one `MarkDispatched` per attributed attempt, atomic initial rebinding only after proven nonexecution, no replay of accepted or ambiguous work |
| `codeclient`, `routes`, `codeaccounts.Authorizer` | generated client configuration, project authority, upstream credential construction | generated configuration never contains an OLP or vendor key |

`codeforward/attempt.go` shares dispatch, observations, settlement and workload
binding across HTTP, SSE and WebSockets. Transport files own copying and
handshakes, `transport.go` shares endpoint credentials, and `refusal.go` records
refusals. `codeaccounts.Authorizer` implements the `codeforward.Authorizer`
interface, checking frozen plugin configuration plus current grant and
credential eligibility before delegating credential construction to the adapter.
The forwarder validates the principal, credential header and extra single-valued
headers before dispatch. Upgrade alone does not admit a generation, so the
gateway reauthorizes each generation and retains the credential generation used
at handshake for upstream refusals.

`codeclient` owns client and model reachability and renderers using the
published revision. `routes` retains project authority, account offers and HTTP
responses. Console editors own typed payloads and validation and use shared
mutation intent, cancellation and stale-callback fences. Secrets clear on
abandonment.

Research uses a separate `/code/{slug}/mcp/{service}` proxy. The published
`codemode.ResearchTools` binding selects one global Z.ai account without model
or conversation admission. `coderesearch` filters methods and discovery and
preserves native tool definitions and results, while the gateway reuses bounded
SSE decoding and raw egress with no replay. `codesessions.Store` rechecks live
key, pool and account authority and stores only opaque session metadata bound to
key, revision, service, account, credential and grant generation. Research
errors do not update inference health. This proxy is separate from the
management MCP endpoint and project-owned MCP certification.

The forwarder matches the observed principal before one `MarkDispatched`.
Prepared aborts can refund, while dispatched unknown usage cannot. Equal
settlement is idempotent, conflicting totals refuse and proven overruns record
`bound_violation`. Rate estimates, provider allowance and hard token
reservations remain independent. No bodies, tools or authentication headers
persist.

To add a subscription family, declare its profiles, endpoints, credential and
observation contract in `codeadapter`, add an enrollment plugin, and reuse a
family for profiles in the same subscription system. New protocols require a
native parser and observer rather than translation, and new clients need
`codeclient` renderers. Extend `codemode.Adapter` and the public
`CodeAccount.adapter`, `CodeRoute.adapters` and
`CodeClientConfiguration.adapters` enums, then regenerate contracts and console
types. Preserve controlled latest-client continuation, refusals, usage,
subagents, model selection and mixed-subscription behavior. Verify live
entitlement, allowance, cross-vendor history and any hard bound separately by
hand. See [Qualification](qualification.md#code-mode-qualification).

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials, though enabled named pools require it. `worker` publishes no public
listener and runs only the accounting, media reconciliation and
[recovery plane](../operate/operations.md#replicated-worker-health), which `all`
also runs. An installation that serves traffic with `gateway` and `control`
needs at least one `worker` replica for accounting, budget reconciliation,
retention, media job polling and plugin grant refresh to run. Gateway readiness
on the private `GET /health/ready` listener fails while the key-authority
snapshot is missing or stale.

`delivery.Delivery` (`internal/usage/delivery`) owns one producer identity,
bounded intake, durable registration, writer, loss reporting and accounting
readiness. Startup registers it before spending producers run. Shutdown drains
requests and probes while runtime authority remains available, then closes and
flushes delivery before stopping consumers. Failed producer drains mark the
epoch unclean.

## Runtime publication and authority

Activation validates and records one immutable numbered runtime release with a
SHA-256 digest. Gateways install only complete valid releases with decryptable
credentials, and a failed installation keeps the previous release.

Each route pins strict or transformed fidelity. Strict targets require admitted
operation contracts and refuse redaction, while transformed routes translate and
may redact. New revisions switch fidelity, while retained resources keep
creation contracts. See
[route fidelity](../use/provider-routing.md#route-fidelity).

Key and credential authority refreshes independently every five seconds and is
stale after 60 seconds from the start of its last successful read in monotonic
time. New work then receives `503 authority_unavailable`. Admitted ordinary
streams may finish, while realtime rechecks. Revoked or lapsed credential
versions remain denied even under historical releases, with no attempt spent for
skipped slots. Plan skip reasons use `credential_revoked`, `credential_lapsed`,
`credential_stale_authority` or `credential_external_secret_unavailable`, with
network, session and media-specific equivalents.

## Request path

Clients authenticate using bearer tokens, Anthropic `X-Api-Key`, Gemini
`X-Goog-Api-Key` or `key` query parameter, or Bedrock `X-OLP-API-Key` or
non-SigV4 bearer tokens. Inference needs `inference`, and discovery needs
`models_read`. The requested model is a published route slug inside the key's
project and route or group allowlist.

Responses carry a safe `X-Request-Id` (and Anthropic `request-id`), `no-store`
and configured gateway CORS. JSON requires `application/json` and must fit
installation and route bounds before and after gzip inflation. Media also
accepts bounded raw and multipart bodies. Uploads have a 15-second read deadline
separate from the inference deadline, and timeout releases admission.

`X-OLP-Routing` is one local JSON object, for example
`{"strategy":"price","allow_fallbacks":false,"max_attempts":1,"max_cost":"0.25"}`.
It may narrow published constraints or budgets, never increase authority or
deadlines. `max_cost` caps the worst-case attempt cost the reservation would
hold. A request that every non-shadow candidate excludes for `max_cost` fails
with `400` `cost_limit_exceeded` and `param: max_cost` before dispatch.
`priority` is bounded by the key ceiling. Raw controls are neither persisted nor
forwarded. `X-OLP-Attribution` accepts one object of at most 4096 bytes and four
labels with bounded machine-token values and key allowlist permission. Project
and key requirements and pins apply, and
[end-user policy](../use/access.md#end-user-identity) stores only HMAC identity.

Attempts use priority and preferred-order tiers, selected strategy and
key-stable weighted ordering. Each consumes the shared attempt and deadline
budget and selected slot credential. Native calls preserve unknown fields, while
translation refuses unrepresentable extensions. Eligible pre-commit connection,
timeout, rate, credential and server failures may retry or fail over. Typed
context-window errors may move to a larger target, and raw message text never
triggers that decision. Typed content filtering is terminal without health
penalty. Other client, protocol and cancellation failures are terminal.
Fallbacks share the original budget, and no committed stream restarts. Retained
resources, realtime and native Bedrock pin one provider and credential.

Upstream rejections preserve 400, 404, 405, 409, 413, 415 and 422 or return 502
with credential-redacted messages. Committed failures end in-band without
success. Event bounds cover retained tool and text state, native Chat choices
and Bedrock advertised lengths, while Bedrock SDK CRC checks still apply.

Five counted failures in 30 seconds open a circuit for 30 seconds. Connection,
timeout, protocol, server and strict or plugin ambiguous failures count. One
half-open probe is allowed, and credential-only failure does not penalize
siblings. Credential rejection cools its version for 60 seconds, while rate
limiting cools the logical slot across rotation for `Retry-After` (default ten,
maximum 60 seconds). Fleet circuits refresh through Valkey every two seconds,
and local evidence remains available during outages.

| Status | Typical refusal |
| --- | --- |
| 400 | Invalid controls, oversized token estimate, local content block |
| 401/403 | Invalid key, denied scope, project, network or identity |
| 413/422 | Body or response bound, unsupported semantics or surface |
| 429 | Key rate, concurrency or known cost exhaustion |
| 502 | Upstream or protocol failure |
| 503 | Stale authority, overload, unavailable limits or budget evidence, or no target |
| 504 | Route deadline before commitment |

### Priority admission

`OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` bounds active work. A full pool
returns `503 request_admission_overloaded` with `Retry-After: 1` unless a
bounded `OLP_HTTP_ADMISSION_QUEUE_DEPTH` queue is configured. Critical, high,
normal and low classes share slots in an 8:4:2:1 ratio. Queue waits end at the
lower of the route deadline or `OLP_HTTP_ADMISSION_QUEUE_TIMEOUT` (default two
seconds).

Keys set `priority` (default normal) and `max_priority` (default the chosen
class). A higher caller class returns `priority_increase_forbidden`. System work
runs at low priority. Metrics expose queue depth, wait and rejections by class.

### Per-route request sizes

Published `max_body_bytes` accepts 1 to 1,073,741,824 or null and can only lower
installation limits. Bounds cover decompression, multipart overhead and
epilogues, chunked uploads, fallbacks and selectors, and retained requests
before dispatch. WebSockets apply the lower bound per message (oversize closes
with code 1009), and sessions pin their starting revision. Responses have
separate limits. Ordinary route promotion preserves the bound, and subscription
refusals use `code_` errors.

## Response headers

Headers are produced from existing admission and attempt evidence before
commitment without additional Valkey reads. Key rate allowances are automatic,
while upstream metadata requires key opt-in.

### Rate-limit headers

| Surface | Requests | Tokens |
| --- | --- | --- |
| OpenAI | `x-ratelimit-limit-requests`, `-remaining-requests`, `-reset-requests` | `x-ratelimit-limit-tokens`, `-remaining-tokens`, `-reset-tokens` |
| Anthropic | `anthropic-ratelimit-requests-limit`, `-remaining`, `-reset` | `anthropic-ratelimit-tokens-limit`, `-remaining`, `-reset` |

Only key-limited dimensions appear. Concurrency-only and cost-only keys,
failed-open reservations, and Gemini, Bedrock and native endpoints receive none.
Values describe the admission's Valkey UTC-minute window after reservation
rather than a fresh read at delivery. Dispatched token allowance is never
refunded for smaller usage, and larger usage increases it. OpenAI reset is a
duration rounded up to milliseconds, while Anthropic reset is UTC RFC 3339. Key
rate and concurrency refusals include allowance and `Retry-After`, whereas
upstream, supply, cost and overload refusals do not.

### Gateway metadata

Key `response_metadata` defaults off. When enabled, successful attempted
operations and cache hits can include the following headers.

| Header | Meaning |
| --- | --- |
| `X-OLP-Attempts` | Attempts before commitment, including local connection or slot quota refusal, or `0` on a cache hit |
| `X-OLP-Route-Revision` | Serving route revision, with retained reads reporting the current ingress revision and job-record operations omitting it |
| `X-OLP-Provider` | Configured serving vendor, only when known and unambiguous |
| `X-OLP-Cost` | Unary pinned-price cost in installation currency, only with complete priced usage, with explicit zero and unary cache hits reported as `0` |

Streaming headers describe the serving attempt but cannot report final cost.
Cache hits emit `X-OLP-Attempts: 0`, `X-OLP-Route-Revision`, and for unary
responses `X-OLP-Cost: 0`. Video lists count refresh polls and name a vendor
only if all items agree. Opt-in does not scrub vendor or model names from
credential-redacted upstream errors.

### Which responses carry them

Successful inference, cache-hit, media, native and retained operations are
eligible. No-attempt local reads (other than cache hits), model discovery,
continuation replay, WebSocket upgrades and session-authenticated playground
calls carry no key attempt metadata. Errors omit both header families except
key-limit refusal allowances. Configured gateway CORS exposes `X-OLP-Cache`,
`Age`, `X-OLP-Guardrails`, `X-Request-Id`, `Retry-After`, `X-Should-Retry`,
`X-OLP-Delivery-Replay`, `X-OLP-Resource-ID`, `X-OLP-Mutation-Status`,
`X-Goog-Upload-URL`, `X-Goog-Upload-Status`, `X-Goog-Upload-Size-Received`, `X-OLP-Attempts`,
`X-OLP-Route-Revision`, `X-OLP-Provider`, `X-OLP-Cost`, the six OpenAI
`x-ratelimit-*` headers, the six Anthropic `anthropic-ratelimit-*` headers, and
`Request-Id` on Anthropic responses.

## Media and durable video jobs

OpenAI image, audio and video endpoints share key, route, admission and
accounting contracts. Raw and multipart uploads reserve bounded spool capacity.
Cancellation releases files, and restart cleanup never removes live work.
Content stays outside persistent diagnostics. See
[media providers](../use/providers/media.md).

Video creation durably pins route, provider, slot, credential and prices before
work can outlive a client. Ambiguous creates never blindly retry. Worker polling
resolves historical credentials with current revocation guards and settles
original usage. Jobs remain key-owned. `/v1/videos` supports create, list, read,
content and delete. Management `/api/v1/media-jobs` (`internal/media/jobs/manage`)
and the console Media Jobs feature (`console/src/lib/features/media/`) expose
job list and detail along with operator `POST /api/v1/media-jobs/{job_id}/refresh`
for polling, `GET /api/v1/media-jobs/{job_id}/content` for downloading `video`,
`thumbnail` and `spritesheet` variants, and ETag-fenced
`DELETE /api/v1/media-jobs/{job_id}` for deleting content and reconciling
upstream removal. Video jobs created with caller-supplied credentials still
require the caller's own `/v1/videos` requests for refresh, content and delete.
Video generation remains absent from the console Playground, while
`AudioTranslationPlayground.svelte` handles audio translation uploads.

## Limits and budgets

Admission requires Valkey for configured distributed limits. Without it,
unlimited keys can serve, limited keys return `distributed_limits_unavailable`
and quota-bound targets are skipped. Rate- and concurrency-only outage policy
can fail open, while cost and provider quotas always fail closed.

After authentication, parsing and routing, the gateway reserves key, group and
end-user rate, tokens, concurrency and cost before dispatch. Token reservation
includes prompt plus largest allowed reply (default 4096, times `n`) across
permitted attempts. Estimates exceeding TPM return `request_exceeds_token_limit`.
Leases cover the overall deadline, and first-byte and idle timeouts do not bound
stream lifetime.

Full strict, profile and unary plans retain at most four candidates, unused
documents are released before streaming, and evicted targets rebuild
deterministically. Native usage is observed before projection, mapping or
delivery can fail. Headers, validated completion, durable commit and client
visibility are distinct, and all multipart and duplex tasks join before terminal
accounting.

### How the prompt is estimated

The effective request walker counts supported message, tool, schema and
reasoning text, adding 1000 tokens per inline image and 2000 per audio, file or
document part. Encrypted content and some vendor tool renderings are unknown.
Counters follow the upstream model family rather than the ingress dialect, and
equivalent families are counted once.

| Family | Exact names and prefixes | Method |
| --- | --- | --- |
| `openai-o200k` | Exact `o1`, `o3`, `o4-mini`, `gpt-5`, `gpt-4.1`, `gpt-4o`, and prefixes `o1-`, `o3-`, `o4-mini-`, `gpt-5`, `gpt-4.5-`, `gpt-4.1-`, `chatgpt-4o-`, `gpt-4o-`, `gpt-oss-` | Embedded `o200k_base` BPE |
| `openai-cl100k` | Exact `gpt-4`, `gpt-3.5-turbo`, `gpt-3.5`, `gpt-35-turbo`, `davinci-002`, `babbage-002`, `text-embedding-ada-002`, `text-embedding-3-small`, `text-embedding-3-large`, and prefixes `gpt-4-`, `gpt-3.5-turbo-`, `gpt-35-turbo-` | Embedded `cl100k_base` BPE |
| `anthropic`, `gemini`, `other` | Models containing a `claude` or `gemini` name segment, plus all remaining recognized or unknown models | Four characters per token, scaled by catalog calibration when measured |

Opaque deployment names are not guessed as OpenAI. OpenAI counts documented
message framing. Text token fixtures match tiktoken, with possible
Unicode-version splitting differences. The walker retains only the first 36 KiB
of text and the tail size. Estimate provenance is recorded as `tokenizer` for
exact supported text and framing only, `calibrated` for prompts with media,
tools, schemas, encrypted or unknown fields, a catalog family factor other than
1, or a long tail past 32 KiB scaled by the prefix's measured density (where
mixed-script tails can differ substantially), and `heuristic` for plain
four-character counting, four-bytes-per-token native document estimates, or long
tails with under 4 KiB of exact prefix. Lifecycle calls, realtime, job polls,
multipart uploads and video creation produce no prompt estimate, though
model-family evidence remains.

Prepared-target rewrites use the larger of the source and prepared estimates and
the less-certain provenance. Context selection uses the same target-family
evidence. Attempts retain input estimate, provenance and family, and reports
compare only pairs with both estimated and reported input.

Each dispatched attempt also reserves connection and slot quotas. A refused slot
refunds its connection hold, and undispatched work refunds fully. Dispatched
token allowance can only grow, and settlement always releases concurrency
independently of client cancellation. Shared version and slot cooldowns survive
rotation as applicable.

Every applicable exact-decimal cost boundary checks calendar spend plus holds.
Known exhaustion returns 429, while unknown, malformed or wrong-window balances
return 503 until
[reconciliation](../operate/operations.md#spend-budget-reconciliation). No
synthetic zero initializes a budget.

### Cost reservation

Cost estimates use the pinned price list and planned token or media consumption.
Holds lapse at route deadline plus five minutes and are retired in bounded
pages, so stalled accounting can cause temporary refusals below displayed spend.
Accrued balances are authoritative. `cost:pending` and `cost:expiry` are
advisory, so damaged holds cannot block accounting, though losing them removes
in-flight protection.

Unpriced attempts, attempts missing a rate, and price lists older than one
minute reserve nothing. Most audio, media and lifecycle operations have no
pre-dispatch estimate. Video create reserves requested seconds (default four),
and realtime and Interaction create reserves initial input plus 4096 output.
Later turns accrue when observed. Background creation releases its initial hold
on return while durable original pending ownership remains visible to newly
enabled budgets.

These controls are admission checks rather than invoice caps. Heuristic counts,
multiple billed attempts, lag, unpriced work and unreserved operations can still
overspend. Known pending native work can return `503 cost_accounting_incomplete`
even at zero displayed spend. A hold-based refusal uses `Retry-After: 1`, while
a request too large even alone needs a smaller reply bound or larger budget.

`limits.valkey_unavailable=fail_open` bypasses only configured rate and
concurrency limits when Valkey is unreachable, incrementing
`olp_limits_fail_open_total`. Unconfigured Valkey never fails open. Settings
refresh every 15 seconds, and unreadable target quotas skip dispatch.

### Durable provisional exposure

Finite asynchronous and agent work shares `debits.Store` in `limits/debits`,
while feature owners retain their lifecycle and provider evidence. Each intent
records every applicable accounting boundary, including currently uncapped
boundaries, in both cost and charge bases. Dispatch is fenced once after
admission. Ordinary lease expiry, request or resource retention and calendar
resets cannot release it. Unknown acceptance is reconciled from evidence and
never automatically resent. API intents preserve the exact project or
installation namespace, while tools and agents require an owning project. An
unresolved bound violation blocks monetary admission even when observed spend
has consumed the original finite bound.

The PostgreSQL ledger is authoritative. Valkey holds a separate nonexpiring,
versioned exposure projection checked in ordinary monetary admission and supply
selection. The existing pending-accounting read also checks the ledger
generation, so a region refreshes stale exposure without an additional database
round trip. A missing projection fails closed and is rebuilt. Projection updates
install accrued spend and exposure atomically, and stale versions cannot reduce
holds. `internal/usage/ingest` joins existing observed-fact persistence with
idempotent result settlement. Run totals and reservations are never additional
usage charges.

Pure `domain/agentcontract` and `prompts` definitions are distinct from
execution and management. Published contracts join immutable runtime snapshots.
Snapshot copies own their OAuth configuration and task bounds. Live authority
independently checks keys, project and resource allowlists, retirement and
pinned MCP eligibility. Runtime arguments, rendered prompts, transcripts and
results are never persisted by these foundations. Prompt templates remain
explicit operator configuration. New execution families stay disabled pending
their own implementation and qualification.

## Agent execution

`internal/agents` owns the typed execution API and shared send lifecycle. Unary
and streaming sends share authorization, credential resolution, admission,
dispatch fencing and settlement. A task is settled before its public identifier
is returned. Failures before dispatch refund admission. Uncertain acceptance
after dispatch remains unknown and is never automatically resent.

`agents/a2a` owns HTTP, JSON-RPC, SSE, pinned cards and the official SDK
transports for A2A 0.3 and 1.0. It converts each incoming request once into
canonical SDK types. Card validation and dispatch use the same resolved
credential. Protocol adapters map execution failures to client responses and
replace upstream task identifiers with public identifiers.

`agents/tasks` owns task admission, persistence, settlement and cancellation
claims. `release` reads and counts callback capabilities, authenticates live
owners and authorizes pinned callback resources. Consumer requests, operator
requests and the bounded background monitor share remote task operations while
retaining their own authorization rules.

`agents/publications` owns draft changes, immutable revisions, credentials and
trust changes. Saving and publishing return new values. `agents/runs` owns run
and step accounting, approvals and sealed workflow continuations shared by
agents, MCP and gateway execution. `agents/investigations` owns investigation
evidence and management. Delegation views remain with delegation management in
`release`.

## Diagnostics

One content-free terminal envelope records request, actor, route, release,
commitment, outcome, status, timing, observed usage and attempt identities and
classes. Prompts, outputs, tools, headers and credentials never enter it.
Management serves provider health, readiness, request and usage history, and
media job metadata together with operator refresh, content download and delete
actions.

Deferred financial events capture original submission separately from actual
HTTP delivery, and delayed usage does not rewrite the immediate terminal log.
See [accounting delivery](../operate/operations.md#accounting-delivery-and-shutdown)
for durable completeness and shutdown. Keep health and metrics private, and note
that `olp health-probe` fails unless ready. Deterministic
[tests](../../tests/README.md) do not constitute live-provider proof.

## Retained generation accounting

`resources.Lifecycle` owns creation replay, dispatch fences, uncertain
completion, paid mutations and atomic creation of returned identities.
`resources.SettlementStore` owns retained-settlement claiming, provider-verified
and operator-verified usage evidence persistence, coverage closure and
reconciliation requests. `internal/resources/retainedworker` drives background
polling against `SettlementStore`, while `internal/resources/manage` isolates
the `/api/v1/provider-resources` and `/api/v1/provider-resource-contracts`
management endpoints from the persistence package. HTTP adapters validate native
requests and responses before passing typed results to `resources.Lifecycle`.
Batch recovery passes a resource-owned recovery value into settlement adoption
so both records commit together without a persistence callback.

`usage.PendingGeneration` owns deferred envelope validation, independent
evidence snapshots and HTTP completion. It chooses whole provider snapshots
instead of combining independent maxima, preserves missing values and explicit
zero, and keeps the original financial identity. Retained resource storage
retains its existing encoded representation.

Response, strict Response and Interaction mappings own content-free original
accounting with creator identity, labels and prices. Retrieval, cancel, delete
and input-item listing create no new generation charge. Native final usage
settles once across replicas, and clients must poll background work after
disconnected creation.

Partial observations retain one coherent snapshot and never sum incomparable
views. Validated terminal counters replace provisional evidence, including
explicit zero or corrected cache classification. Event kind, status and retained
identity must validate before effects, and nonterminal snapshots cannot settle.
Unknown or null counts are not zero. Batch `pending_usage` extensions remain
opaque metadata.

Gemini pending, queued and in-progress states retain generation ownership, while
`requires_action` ends that generation and continuation starts another.
Lifecycle reads may settle original owners despite newly enabled monetary
budgets, while authentication, revocation, rate and concurrency checks still
apply. Accepted native POSTs set `X-Should-Retry: false` before commitment,
independent of metadata opt-in.

Usage is observed before projection, persistence or delivery so failed writes
and flushes cannot erase validated consumption. Partial interruption remains
uncertain, and provider failure, route timeout and caller cancellation remain
distinct. Delete and expiry conservatively persist original partial usage or
uncertainty before removing mappings and secrets. Unsupported templates remain
retained, and bounded batches and savepoints prevent one owner from starving
cleanup. Submission and later observation times remain separate, and
non-generating resource failures are not unknown consumption.

## Caller-supplied provider credentials

Connections may use `credential_source: caller`. Operator-controlled endpoint,
profile, models, network, slots and separately stored probe credentials remain.
Each serving or lifecycle request supplies one nonempty
`X-OLP-Provider-Credential` of at most 16,384 bytes containing the connector's
normal API key, header JSON or static AWS JSON. Ambient, token-exchange and
plugin modes refuse. The header is removed before forwarding or retained
storage.

Secrets exist only for the request or live realtime connection, bound to its
provider and model. Same-target retries may reuse them, while another target
cannot. Isolated HTTP transports disable keep-alive and shared auth caches.
Error redaction includes submitted and derived signing and header values. Caller
rejection never revokes or cools the operator probe credential. Attempts record
caller source without secret, digest or operator credential version.

Caller video jobs require resupplied credentials for poll, content and delete
and are excluded from autonomous polling and backlog. Workers cannot substitute
probes.

### Caller-paid budget policy

Route `caller_cost_exempt: true` requires all foreground targets to use caller
credentials and all fallback and selector destinations to match. Classifiers
admit separately, while shadows and probes stay operator-paid and budgeted.
Eligible attempts bypass all applicable USD boundaries, while rate, token,
concurrency, network, body, identity and authorization controls still apply.
Historical operator-paid targets cannot serve through exempt ingress.

Prices, usage and unpriced evidence remain in reports. Immutable per-attempt
`routing.budget_exempt` survives facts, hourly rollups, delayed settlement and
cap changes, so budgets exclude it without hiding spend estimates. Source and
exemption promote, while secrets do not. Multi-step media retains isolated
transport and same-origin credential boundaries, and fetched products receive no
credential.

### Unbounded work and incomplete accounting

Batch creation, Gemini Live, native duplex and Decisions refuse with
`unbounded_work_budget` when applicable token or USD ceilings cannot bound
future consumption, while rate- and concurrency-only policy works. Live sessions
recheck newly enabled budgets on authority refresh. For Decisions, a caller-paid
exempt route waives USD ceilings on caller-credential targets but never token
ceilings, and route simulation reports the same refusal.

Cost admission checks durable ingestion losses across every active calendar
window and stale unclosed producer epochs older than 60 seconds. Ownership-free
gaps conservatively affect all cost budgets, including system and supply.
PostgreSQL unavailability fails closed, while no-cost admissions skip this
check. Local loss checkpoints immediately where possible, with periodic
recovery. Startup registers producer epochs before any work, and shutdown drains
producers before metadata closes. Unsupported event versions stay queued for
compatible consumers, and malformed supported records become explicit loss
evidence.

This stops additional admission based on incomplete evidence, though it cannot
restore lost spend, prevent races with checkpoints or promise a fixed recovery
time.
