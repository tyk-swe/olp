# Architecture and gateway execution

The Go module owns production code under `internal/` and the public provider
plugin SDK under `sdk/`; `cmd/olp` starts the CLI.
The console mirrors feature ownership under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/database/migrations/`.

| Area | Backend and console owner |
| --- | --- |
| Connections, certification and grants | `internal/providers/`, `internal/connectors/`, `internal/grants/`; console `providers/` |
| Routes, publication and selection | `internal/routes/`, `internal/routing/`, `internal/runtime/`; console `routes/` |
| Users, keys, identity, settings and policy | `internal/access/`, `internal/limits/`; console `access/` |
| Admission, execution and retained resources | `internal/gateway/`, `internal/resources/`, `internal/media/`; console inference/media |
| Code mode | `internal/codemode/`, `internal/codeadapter/`, `internal/codeclient/`, `internal/coderesearch/`, `resources/code_*.go`; console code mode |
| Source and operation contracts | `internal/oif/`, `internal/operations/`, `internal/interaction/`, `internal/operationplan/`, `internal/operationregistry/` |
| Protocol and lifecycle contracts | `internal/protocols/`, `internal/mediacontract/`, `internal/durablecontract/`, `internal/realtimecontract/`, `internal/geminilifecycle/` |
| Preparation and failure evidence | `internal/providerinvoke/`, `internal/upstream/` |
| Accounting, pricing, billing and exports | `internal/usage/`, `internal/billing/`, `internal/export/`; console usage/observability |
| Plugins and guardrails | `internal/plugins/`, `sdk/plugin/`, `internal/guardrails/`, `internal/contentpolicy/`; matching console features |
| Promotion and process lifetime | `internal/configuration/`, `internal/config/`, `internal/process/`, `internal/database/` |
| Egress, secrets and telemetry | `internal/egress/`, `internal/secrets/`, `internal/observability/`, `internal/telemetry/` |

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

[Code mode](../use/code-mode.md) forwards `/code/{slug}` native models/bodies
without ordinary translation, rewriting or inference retries. `Snapshot.CodeRoutes`
and `CodeConnection` freeze route/provider pairs; missing configuration refuses
until republish, while pool/key/grant eligibility remains live.

| Owner | Contract in | Invariant |
| --- | --- | --- |
| `codemode` | shared account, route, binding, usage and allowance types | observations never replace forwarded bytes |
| `codeadapter`, fed by `codeplans` and `codexauth` | subscription profiles, client/upstream paths, operations, transports, conversation identity, credentials and observation rules | families sharing a client path agree on semantics, checked by `codeadapter.Validate` during snapshot validation |
| `routing`, installed by `runtime` | endpoint and model eligibility compiled from the frozen revision | a failed compile leaves the previous release serving |
| `codewire`, `codexwire` | native request classification and response observation on copies | parsing never reserializes the forwarded payload |
| `gateway/code_attempt.go`, `resources.CodeStore`, `limits` | dispatch, durable conversation pinning, admission and settlement, hard token budgets | one `MarkDispatched`, no silent account switching, no replay after dispatch |
| `codeclient`, `routes`, `providers.CodeAuthorizer` | generated client configuration, project authority, upstream credential construction | generated configuration never contains an OLP or vendor key |

`gateway/code_attempt.go` shares dispatch, observations, settlement and workload
binding across HTTP/SSE and WebSockets. Transport files own copying/handshakes;
`code_transport.go` shares endpoint credentials, and `code_refusal.go` records
refusals. `CodeAuthorizer` checks frozen plugin plus current grant/credential
eligibility, then delegates credential construction to the adapter. Validate
principal, credential header and extra single-valued headers before dispatch.
Upgrade alone does not admit a generation: reauthorize each generation and retain
the credential generation used at handshake for upstream refusals.

`codeclient` owns client/model reachability and renderers, using the published
revision. `routes` retains project authority, account offers and HTTP responses.
Console editors own typed payloads/validation and use shared mutation intent,
cancellation and stale-callback fences. Secrets clear on abandonment.

Research uses a separate `/code/{slug}/mcp/{service}` proxy. The published
`codemode.ResearchTools` binding selects one global Z.ai account without model
or conversation admission. `coderesearch` filters methods/discovery and preserves
native tool definitions/results; the gateway reuses bounded SSE decoding and
raw egress with no replay. `resources.CodeStore` rechecks live key/pool/account
authority and stores only opaque session metadata, bound to key, revision,
service, account, credential and grant generation. Research errors do not update
inference health. This proxy is separate from the management MCP endpoint and
project-owned MCP certification.

Match the observed principal before one `MarkDispatched`. Prepared aborts can
refund; dispatched unknown usage cannot. Equal settlement is idempotent, conflicting
totals refuse and proven overruns record `bound_violation`. Rate estimates,
provider allowance and hard token reservations remain independent. No bodies,
tools or authentication headers persist.

To add a subscription family, declare its profiles/endpoints/credential and
observation contract in `codeadapter`, add an enrollment plugin, and reuse a
family for profiles in the same subscription system. New protocols require a
native parser/observer, not translation; new clients need `codeclient` renderers.
Extend `codemode.Adapter` and public `CodeAccount.adapter`, `CodeRoute.adapters`
and `CodeClientConfiguration.adapters` enums, then regenerate contracts and console
types. Preserve controlled latest-client continuation, refusals, usage, subagents,
model selection and mixed-subscription tests; separately qualify live entitlement,
allowance, cross-vendor history and any hard bound. See
[Qualification](qualification.md#code-mode-qualification).

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials; enabled named pools require it. `worker` publishes no public
listener and runs only the accounting, media reconciliation, and
[recovery plane](../operate/operations.md#replicated-worker-health), which `all` also runs;
an installation that serves traffic with `gateway` and `control` needs at least
one `worker` replica for accounting, budget reconciliation, retention, media job
polling, and plugin grant refresh to happen at all. Gateway readiness on the
private `GET /health/ready` listener fails while the key-authority snapshot is
missing or stale.

## Runtime publication and authority

Activation validates and records one immutable numbered runtime release with a
SHA-256. Gateways install only complete valid releases with decryptable
credentials; failed installation keeps the previous release.

Each route pins strict or transformed fidelity. Strict targets require admitted
operation contracts and refuse redaction; transformed routes translate and may
redact. New revisions switch fidelity, while retained resources keep creation
contracts. See [route fidelity](../use/provider-routing.md#route-fidelity).

Key/credential authority refreshes independently every five seconds and is stale
after 60 seconds from the start of its last successful read (monotonic time).
New work then receives `503 authority_unavailable`. Admitted ordinary streams may
finish; realtime rechecks. Revoked/lapsed credential versions remain denied even
under historical releases, with no attempt spent for skipped slots.
Plan reasons use `credential_revoked`, `credential_lapsed` or
`credential_stale_authority`, with network/session/media-specific equivalents.

## Request path

Authenticate using bearer, Anthropic `X-Api-Key`, Gemini `X-Goog-Api-Key`/query
key, or Bedrock `X-OLP-API-Key`/non-SigV4 bearer. Inference needs `inference`;
discovery needs `models_read`. The requested model is a published route slug
inside the key's project and route/group allowlist.

Responses carry safe `X-Request-Id` (also Anthropic `request-id`), no-store and
configured gateway CORS. JSON requires `application/json` and fits installation/
route bounds before and after gzip inflation. Media also accepts bounded raw/
multipart bodies. Uploads have a 15-second read deadline, separate from the
inference deadline; timeout releases admission.

`X-OLP-Routing` is one local JSON object, for example
`{"strategy":"price","allow_fallbacks":false,"max_attempts":1,"max_cost":"0.25"}`.
It may narrow published constraints/budgets, never increase authority or
deadlines. `max_cost` caps the worst-case attempt cost the reservation would
hold; a request every non-shadow candidate excludes for it fails `400`
`cost_limit_exceeded` with `param: max_cost` before dispatch.
`priority` is bounded by the key ceiling. Raw controls are not persisted/forwarded.
`X-OLP-Attribution` accepts one object, at most 4096 bytes/four labels with bounded
machine-token values and key allowlist permission. Project/key requirements and
pins apply; [end-user policy](../use/access.md#end-user-identity) stores only HMAC identity.

Attempts use priority/preferred-order tiers, selected strategy and key-stable
weighted ordering. Each consumes the shared attempt/deadline budget and selected
slot credential. Native calls preserve unknown fields; translation refuses
unrepresentable extensions. Eligible pre-commit connect/timeout/rate/credential/
server failures may retry or fail over. Typed context-window errors may use a
larger target; message text never triggers that decision. Typed content filtering
is terminal without health penalty. Other client/protocol/cancellation failures
are terminal. Fallbacks share the original budget; no committed stream restarts.
Retained resources, realtime and native Bedrock pin one provider/credential.

Upstream rejections preserve 400/404/405/409/413/415/422 or return 502, with
credential-redacted messages. Committed failures end in-band without success.
Event bounds cover retained tool/text state, native Chat choices and Bedrock
advertised lengths; Bedrock SDK CRC checks still apply.

Five counted failures in 30 seconds open a circuit for 30 seconds. Connection,
timeout, protocol/server and strict/plugin ambiguous failures count. One half-open
probe is allowed; credential-only failure does not penalize siblings. Credential
rejection cools its version for 60 seconds; rate limiting cools the logical slot
across rotation for Retry-After (default ten, maximum 60 seconds). Fleet circuits
refresh through Valkey every two seconds; local evidence remains available.

| Status | Typical refusal |
| --- | --- |
| 400 | Invalid controls, oversized token estimate, local content block |
| 401/403 | Invalid key, denied scope/project/network/identity |
| 413/422 | Body/response bound, unsupported semantics or surface |
| 429 | Key rate/concurrency or known cost exhaustion |
| 502 | Upstream/protocol failure |
| 503 | Stale authority, overload, unavailable limits/budget evidence or no target |
| 504 | Route deadline before commitment |

### Priority admission

`OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` bounds active work. A full pool returns
`503 request_admission_overloaded`, Retry-After 1, unless a bounded
`OLP_HTTP_ADMISSION_QUEUE_DEPTH` queue is configured. Critical/high/normal/low
classes share slots 8:4:2:1. Queue waits end at the lower of route deadline or
`OLP_HTTP_ADMISSION_QUEUE_TIMEOUT` (default two seconds).

Keys set `priority` (default normal) and `max_priority` (default the chosen class).
A higher caller class returns `priority_increase_forbidden`. System work runs low.
Metrics expose queue depth/wait/rejections by class.

### Per-route request sizes

Published `max_body_bytes` accepts 1–1,073,741,824 or null and can only lower
installation limits. Bounds cover decompression, multipart overhead/epilogues,
chunked uploads, fallbacks/selectors and retained requests before dispatch.
WebSockets apply the lower bound per message (oversize: 1009); sessions pin their
starting revision. Responses have separate limits. Ordinary route promotion
preserves the bound; subscription refusals use `code_` errors.

## Response headers

Headers are produced from existing admission/attempt evidence before commitment,
without additional Valkey reads. Key rate allowances are automatic; upstream
metadata requires key opt-in.

### Rate-limit headers

| Surface | Requests | Tokens |
| --- | --- | --- |
| OpenAI | `x-ratelimit-limit-requests`, `-remaining-requests`, `-reset-requests` | `x-ratelimit-limit-tokens`, `-remaining-tokens`, `-reset-tokens` |
| Anthropic | `anthropic-ratelimit-requests-limit`, `-remaining`, `-reset` | `anthropic-ratelimit-tokens-limit`, `-remaining`, `-reset` |

Only key-limited dimensions appear; concurrency/cost-only keys, failed-open
reservations, Gemini/Bedrock/native endpoints receive none. Values describe the
admission's Valkey UTC-minute window after reservation, not a fresh read at delivery.
Dispatched token allowance is never refunded for smaller usage; larger usage
increases it. OpenAI reset is a duration rounded up to milliseconds; Anthropic
reset is UTC RFC 3339. Key rate/concurrency refusals include allowance and
Retry-After; upstream/supply/cost/overload refusals do not.

### Gateway metadata

Key `response_metadata` defaults off. Successful attempted operations can include:

| Header | Meaning |
| --- | --- |
| `X-OLP-Attempts` | Attempts before commitment, including local connection/slot quota refusal |
| `X-OLP-Route-Revision` | Serving route revision; retained reads report current ingress revision, job-record operations omit it |
| `X-OLP-Provider` | Configured serving vendor, only when known/unambiguous |
| `X-OLP-Cost` | Unary pinned-price cost in installation currency, only with complete priced usage; explicit zero remains `0` |

Streaming headers describe the serving attempt but cannot report final cost.
Video lists count refresh polls and name a vendor only if all agree.
Opt-in does not scrub vendor/model names from credential-redacted upstream errors.

### Which responses carry them

Successful inference/media/native/retained operations are eligible. No-attempt
local reads, model discovery, continuation replay, WebSocket upgrades and
session-authenticated playground calls have no key attempt metadata. Errors
omit both families except key-limit refusal allowances. Configured CORS exposes
these headers, Retry-After, X-Should-Retry and X-OLP-Delivery-Replay.

## Media and durable video jobs

OpenAI image/audio/video endpoints share key, route, admission and accounting
contracts. Raw/multipart uploads reserve bounded spool capacity; cancellation
releases files, and restart cleanup never removes live work. Content stays outside
persistent diagnostics. See [media providers](../use/providers/media.md).

Video creation durably pins route/provider/slot/credential/prices before work can
outlive a client. Ambiguous creates never blindly retry. Worker polling resolves
historical credentials with current revocation guards and settles original usage.
Jobs remain key-owned. `/v1/videos` supports create/list/read/content/delete;
management `/api/v1/media-jobs` and console expose metadata list/detail only.
The console does not poll, download/delete/cancel videos or generate them in the
playground; audio translation has a separate upload form.

## Limits and budgets

Admission requires Valkey for configured distributed limits. Without it,
unlimited keys can serve, limited keys return `distributed_limits_unavailable`
and quota-bound targets are skipped. Rate/concurrency-only outage policy can
fail open; cost and provider quotas always fail closed.

After auth/parsing/routing, reserve key/group/end-user rate, tokens, concurrency
and cost before dispatch. Token reservation includes prompt plus largest allowed
reply (default 4096, times `n`) across permitted attempts. Estimates exceeding
TPM return `request_exceeds_token_limit`. Leases cover the overall deadline;
first-byte/idle timeouts do not bound stream lifetime.

Full strict/profile/unary plans retain at most four candidates; unused documents
are released before streaming and evicted targets rebuild deterministically.
Native usage is observed before projection, mapping or delivery can fail.
Headers, validated completion, durable commit and client visibility are distinct;
all multipart/duplex tasks join before terminal accounting.

### How the prompt is estimated

The effective request walker counts supported message/tool/schema/reasoning text,
with 1000 tokens per inline image and 2000 per audio/file/document part. Encrypted
content and some vendor tool renderings are unknown. Counters follow the upstream
model family, not ingress dialect; equivalent families are counted once.

| Family | Models | Method |
| --- | --- | --- |
| `openai-o200k` | GPT-4o/4.1/5, o-series, gpt-oss | Embedded `o200k_base` BPE |
| `openai-cl100k` | GPT-4/3.5, text-embedding-3 | Embedded `cl100k_base` BPE |
| `anthropic`, `gemini`, `other` | Remaining recognized/unknown models | Four characters/token, with catalog calibration when measured |

Opaque deployment names are not guessed as OpenAI. OpenAI counts documented
message framing; fixtures match tiktoken, with possible Unicode-version splitting
differences. The walker retains only the first 36 KiB of text and tail size.

- `tokenizer`: exact supported text/framing only.
- `calibrated`: media/tool/schema/encrypted/unknown fields or long-tail ratio.
  After 32 KiB, tail bytes use the prefix's measured density; mixed-script tails
  can differ substantially from estimates.
- `heuristic`: four-character counting, or four bytes/token for document-size
  native operation estimates and long tails with under 4 KiB exact prefix.
- No estimate: lifecycle calls, realtime, job polls, multipart uploads and video
  creation, though model-family evidence remains.

Prepared-target rewrites use the larger source/prepared estimate and less-certain
provenance. Context selection uses the same target-family evidence. Attempts
retain input estimate/provenance/family; reports compare only pairs with both
estimated and reported input.

Each dispatched attempt also reserves connection/slot quotas. A refused slot
refunds its connection hold; undispatched work refunds fully. Dispatched token
allowance can only grow; settlement always releases concurrency, independently
of client cancellation. Shared version/slot cooldowns survive rotation as applicable.

Every applicable exact-decimal cost boundary checks calendar spend plus holds.
Known exhaustion returns 429; unknown/malformed/wrong-window balances return 503
until [reconciliation](../operate/operations.md#spend-budget-reconciliation). No synthetic
zero initializes a budget.

### Cost reservation

Cost estimates use the pinned price list and planned token/media consumption.
Holds lapse at route deadline plus five minutes and are retired in bounded pages;
stalled accounting can therefore cause temporary refusals below displayed spend.
Accrued balances are authoritative. `cost:pending`/`cost:expiry` are advisory;
damaged holds cannot block accounting, but losing them removes in-flight protection.

Unpriced/missing-rate attempts and price lists older than one minute reserve
nothing. Most audio/media and lifecycle operations have no pre-dispatch estimate.
Video create reserves requested seconds (default four); realtime/Interaction
create reserves initial input plus 4096 output. Later turns accrue when observed.
Background creation releases its initial hold on return while durable original
pending ownership remains visible to newly enabled budgets.

These are admission controls, not invoice caps: heuristic counts, multiple billed
attempts, lag, unpriced work and unreserved operations can overspend. Known pending
native work can return `503 cost_accounting_incomplete` even at zero displayed
spend. A hold-based refusal uses Retry-After 1; a request too large even alone
needs a smaller reply bound or larger budget.

`limits.valkey_unavailable=fail_open` bypasses only configured rate/concurrency
when Valkey is unreachable, incrementing `olp_limits_fail_open_total`.
Unconfigured Valkey never fails open. Settings refresh every 15 seconds;
unreadable target quotas skip dispatch.

### Durable provisional exposure

Finite asynchronous and agent work shares `limits.DebitStore`; feature owners
retain their lifecycle and provider evidence. Each intent records every
applicable accounting boundary, including currently uncapped boundaries, in
both cost and charge bases. Dispatch is fenced once after admission. Ordinary
lease expiry, request/resource retention and calendar resets cannot release it.
Unknown acceptance is reconciled from evidence and never automatically resent.
API intents preserve the exact project or installation namespace; tools and
agents require an owning project. An unresolved bound violation blocks monetary
admission even when observed spend has consumed the original finite bound.

The PostgreSQL ledger is authoritative. Valkey holds a separate nonexpiring,
versioned exposure projection, checked in ordinary monetary admission and supply
selection. The existing pending-accounting read also checks the ledger generation,
so a region refreshes stale exposure without an additional database round trip.
A missing projection fails closed and is rebuilt. Projection updates install accrued spend and exposure atomically, and
stale versions cannot reduce holds. `usage.PersistDebitResultTx` joins existing
observed-fact persistence with idempotent result settlement. Run totals and
reservations are never additional usage charges.

Pure `agentcontract` and `prompts` definitions are distinct from management in
`agents` and executable operation codecs. Published contracts join immutable
runtime snapshots; live authority independently checks keys, project/resource
allowlists, retirement and pinned MCP eligibility. Runtime arguments, rendered
prompts, transcripts and results are never persisted by these foundations.
Prompt templates remain explicit operator configuration. New execution families
stay disabled pending their own implementation and qualification.

## Diagnostics

One content-free terminal envelope records request/actor/route/release,
commitment/outcome/status/timing, observed usage and attempt identities/classes.
Never include prompts, outputs, tools, headers or credentials. Management serves
provider health, readiness, request/usage history and metadata-only media jobs.

Deferred financial events capture original submission separately from actual HTTP
delivery; delayed usage does not rewrite the immediate terminal log. See
[accounting delivery](../operate/operations.md#accounting-delivery-and-shutdown) for durable
completeness and shutdown. Keep health/metrics private; `olp health-probe` fails
unless ready. Deterministic [tests](../../tests/README.md) are not live-provider proof.

## Retained generation accounting

Response/strict Response/Interaction mappings own content-free original
accounting with creator identity/labels/prices. Retrieval/cancel/delete/input-item
listing creates no new generation charge. Native final usage settles once across
replicas; clients must poll background work after disconnected creation.

Partial observations retain one coherent snapshot, never sum incomparable views.
Validated terminal counters replace provisional evidence, including explicit zero
or corrected cache classification. Event kind/status/retained identity must
validate before effects; nonterminal snapshots cannot settle. Unknown/null counts
are not zero. Batch `pending_usage` extensions remain opaque metadata.

Gemini pending/queued/in-progress retain generation ownership; `requires_action`
ends that generation and continuation starts another. Lifecycle reads may settle
original owners despite newly enabled monetary budgets, while auth/revocation/
rate/concurrency still apply. Accepted native POSTs set `X-Should-Retry: false`
before commitment, independent of metadata opt-in.

Usage is observed before projection/persistence/delivery: failed writes/flushes
cannot erase validated consumption. Partial interruption remains uncertain;
provider failure, route timeout and caller cancellation remain distinct.
Delete/expiry conservatively persists original partial usage/uncertainty before
removing mappings/secrets. Unsupported templates remain retained; bounded batches
and savepoints prevent one owner starving cleanup. Submission and later observation
times remain separate. Non-generating resource failures are not unknown consumption.

## Caller-supplied provider credentials

Connections may use `credential_source: caller`. Operator-controlled endpoint,
profile, models/network/slots and separately stored probe credentials remain.
Each serving/lifecycle request supplies one nonempty
`X-OLP-Provider-Credential`, at most 16,384 bytes, containing the connector's normal
API key, header JSON or static AWS JSON. Ambient/token-exchange/plugin modes refuse.
The header is removed before forwarding or retained storage.

Secrets exist only for the request (or live realtime connection), bound to its
provider/model. Same-target retries may reuse; another target cannot. Isolated
HTTP transports disable keep-alive and shared auth caches. Error redaction includes
submitted and derived signing/header values. Caller rejection never revokes or
cools the operator probe credential. Attempts record caller source without secret,
digest or operator credential version.

Caller video jobs require resupplied credentials for poll/content/delete and are
excluded from autonomous polling/backlog. Workers cannot substitute probes.

### Caller-paid budget policy

Route `caller_cost_exempt: true` requires all foreground targets to use caller
credentials and all fallback/selector destinations to match. Classifiers admit
separately; shadows/probes stay operator-paid and budgeted. Eligible attempts
bypass all applicable USD boundaries, not rate/token/concurrency/network/body/
identity/authorization controls. Historical operator-paid targets cannot serve
through exempt ingress.

Prices/usage/unpriced evidence remain in reports. Immutable per-attempt
`routing.budget_exempt` survives facts/hourly rollups, delayed settlement and cap
changes; budgets exclude it without hiding spend estimates. Source/exemption
promote, secrets do not. Multi-step media retains isolated transport and same-origin
credential boundaries; fetched products receive no credential.

### Unbounded work and incomplete accounting

Batch creation/Gemini Live/Decisions refuse `unbounded_work_budget` when
applicable token/USD ceilings cannot bound future consumption; rate/concurrency-only
policy works. Live rechecks newly enabled budgets on authority refresh. For
Decisions, a caller-paid exempt route waives USD ceilings on caller-credential
targets but never token ceilings; route simulation reports the same refusal.

Cost admission checks durable ingestion losses across every active calendar
window and stale (>60 seconds) unclosed producer epochs. Ownership-free gaps
conservatively affect all cost budgets, including system/supply. PostgreSQL
unavailability fails closed; no-cost admissions skip this check. Local loss
checkpoints immediately where possible, with periodic recovery. Startup registers
producer epochs before any work; shutdown drains producers before metadata closes.
Unsupported event versions stay queued for compatible consumers; malformed
supported records become explicit loss evidence.

This stops additional admission based on incomplete evidence; it cannot restore
lost spend, prevent races with checkpoints or promise fixed recovery time.
