# Gateway execution

The gateway serves OpenAI, Anthropic, Gemini, and Bedrock clients with shared
authentication, project boundaries, admission, routing policy, and accounting.
Canonical generation, media, retained resources, and realtime use the execution
paths appropriate to their lifecycle. See [Access](access.md) for identity and
[Deployment](deployment.md) for installation.

The [compatibility matrix](compatibility.md) owns endpoint support and
translation limits. [Provider connections and routing](provider-routing.md)
covers onboarding, credential pools, publication, and selection policy.

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials; enabled named pools require it. `worker` publishes no public
listener and runs only the accounting, media reconciliation, and
[recovery plane](operations.md#replicated-worker-health), which `all` also runs;
an installation that serves traffic with `gateway` and `control` needs at least
one `worker` replica for accounting, budget reconciliation, retention, media job
polling, and plugin grant refresh to happen at all. Gateway readiness on the
private `GET /health/ready` listener fails while the key-authority snapshot is
missing or stale.

## Configuration

Use the [configuration reference](configuration.md) for flags, defaults, bounds,
egress, mounted connectors, and secret files. [Operations](operations.md) covers
maintenance commands. Local mock providers need explicit loopback and HTTP
[egress exceptions](configuration.md#test-and-harness-variables).

## Runtime publication and authority

Activation validates and records one immutable numbered runtime release with a
SHA-256. Gateways install only complete valid releases with decryptable
credentials; failed installation keeps the previous release.

Each route pins strict or transformed fidelity. Strict targets require admitted
operation contracts and refuse redaction; transformed routes translate and may
redact. New revisions switch fidelity, while retained resources keep creation
contracts. See [route fidelity](provider-routing.md#route-fidelity).

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
pins apply; [end-user policy](access.md#end-user-identity) stores only HMAC identity.

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

## Content policy

Published `content_policy` is an ordered local RE2 rule list: input/output phase,
block/redact action, pattern and optional literal replacement. Limits: 64 rules,
512 bytes/pattern, 16 KiB combined patterns, no empty-string matches. Replacement
`$` sequences are literal. Strict routes allow blocking only; redaction requires
transformed fidelity. [Named guardrails](guardrails.md) copy policy into revisions.

Input runs before estimation/dispatch over supported text: messages/instructions,
embedding/moderation inputs, rerank documents/queries and media prompts. URLs,
binary/base64, tool schemas and unrelated metadata are not inspected. Redaction
rewrites only memory; block returns `400 content_policy_blocked` without dispatch.

Output inspects visible unary assistant text/refusals after usage accounting,
not reasoning/tool arguments/citations/media. Blocking still accounts upstream
consumption. Any output rule refuses streaming with
`422 content_policy_streaming_requires_unary`; input-only policy may stream.
Uninspectable native/background/stateful/Files/Batch/realtime surfaces refuse
`content_policy_surface_unavailable`, rather than bypassing policy.

Evidence stores rule ID/phase/action/outcome only, never matched text, offsets,
patterns, replacements or bodies. This bounded text filter is not a universal
safety or prompt-injection guarantee.

## Media and durable video jobs

OpenAI image/audio/video endpoints share key, route, admission and accounting
contracts. Raw/multipart uploads reserve bounded spool capacity; cancellation
releases files, and restart cleanup never removes live work. Content stays outside
persistent diagnostics. See [media providers](providers/media.md).

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
until [reconciliation](operations.md#spend-budget-reconciliation). No synthetic
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

## Diagnostics

One content-free terminal envelope records request/actor/route/release,
commitment/outcome/status/timing, observed usage and attempt identities/classes.
Never include prompts, outputs, tools, headers or credentials. Management serves
provider health, readiness, request/usage history and metadata-only media jobs.

Deferred financial events capture original submission separately from actual HTTP
delivery; delayed usage does not rewrite the immediate terminal log. See
[accounting delivery](operations.md#accounting-delivery-and-shutdown) for durable
completeness and shutdown. Keep health/metrics private; `olp health-probe` fails
unless ready. Deterministic [tests](../tests/README.md) are not live-provider proof.

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
