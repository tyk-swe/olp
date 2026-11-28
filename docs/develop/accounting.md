# Limits and accounting

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
