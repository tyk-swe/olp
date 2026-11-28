# Execution

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
