# M8: Response caching

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | [M7](m07-guardrails.md) | Lower cost and latency for repeated work |

LiteLLM caches responses in Redis, Valkey, object storage, memory or disk, with
semantic variants and per-request controls. At the planning baseline, OLP had no response cache: it
forwards provider prompt-caching controls natively and prices cache reads and
writes, but every request reaches a provider. A response cache stores content,
which OLP's ordinary accounting records never hold, so this milestone builds caching as an
explicit, sealed, scoped and bounded exception. Implementation and focused
qualification are complete; the M1 reference-hardware release gate remains
open. [Response caching](../caching.md) is the operator contract.

## Outcome

- Routes opt into exact-match caching, with sealed entries in a dedicated
  Valkey, isolated per key by default; explicitly shared scopes remain within
  one project.
- Streamed responses replay from cache with their original event sequence.
- Semantic caching serves single-turn traffic that tolerates approximate
  matches.
- Transformed routes insert provider prompt-cache breakpoints automatically.
- Cache hits are accounted, rate-limited and reported like any other request.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| | Planning baseline | LiteLLM reference |
| --- | --- | --- |
| Response cache | None | [Caching](https://docs.litellm.ai/docs/proxy/caching) with exact and [semantic](https://docs.litellm.ai/docs/proxy/caching_semantic) backends |
| Controls | None | [Per-request controls](https://docs.litellm.ai/docs/proxy/caching_controls): TTL, maximum age, no-cache, no-store, namespace |
| Provider prompt caching | Native controls forwarded; cache reads and writes priced, including 5-minute and 1-hour writes ([pricing](../../internal/usage/pricing.go)) | Pass-through and automatic `cache_control` insertion |

## Scope

### M8.1 Exact response cache

**Configuration.** A route revision may carry:

```json
{
  "cache": {
    "mode": "exact",
    "ttl_seconds": 3600,
    "scope": "key",
    "operations": ["generation", "embeddings", "rerank", "moderation"],
    "streaming": "replay",
    "max_entry_bytes": 1048576,
    "count_hits_against": ["requests"]
  }
}
```

`scope` is `key` (the default), `project` or `route`; both wider scopes share
only among authorized callers within the project. A key policy may forbid
caching. Omission disables caching. TTL is 1–604,800 seconds, entry size is
1–16,777,216 bytes, and `streaming` is `replay` or `bypass`. The example gives
the exact-cache defaults; `requests` is mandatory in `count_hits_against`.

**Storage.** Entries live in a Valkey deployment named by
`OLP_CACHE_VALKEY_URL`, never in the Valkey that holds limits, leases and
accounting streams: a cache needs an eviction policy, and limit state must never
be evicted. Each value is sealed with AES-256-GCM under a new `response_cache`
purpose, with the cache key and installation as associated data. No cache
payload is written to PostgreSQL.

**Keys.** The cache key is an HMAC (`cache_key` purpose) over the canonical
effective provider request and a fingerprint of the route and named route,
provider revision, selected target, effective ordered guardrail revisions,
client surface/dialect, unary/streaming mode, semantic headers/query and response
projection controls such as embedding encoding and requested stream usage.
Scope and project are always bound. Defaults, model rewriting, prompt-cache
insertion and input guardrails precede key construction. Canonicalization sorts
object members, preserves array order and numeric spelling, and uses validated
JSON with no duplicate members or floating-point conversion. Changes in these
identities miss; unrelated projects' guardrails do not invalidate this cache.

**Eligibility.** Requests using stateful Responses fields (`store`,
`background`, `previous_response_id`), files, batches, realtime, media uploads,
images/audio/video, caller-provided credentials, request-local masking, or more
than one candidate bypass caching. Only successful, completely metered responses
are stored, and a stream only after its success terminal event. Responses that
permit provider state must explicitly request `store: false`; defaults cannot
accidentally make retained work cacheable. Fallback results from a target other
than the lookup target are not stored under that target's identity.

**Order of operations.** Authentication, admission and input guardrails run
before lookup, with request/concurrency reservations acquired first. Parallel
input guards are joined before lookup returns a usable hit. Token and budget
admission then use the resolved hit/miss policy before delivery or dispatch.
Output guardrails run before storage, so entries already satisfy the exact
effective policy in their identity. Hits replay the stored response in the same
client dialect and transport mode; streams preserve event sequence without
original timing. They never initiate provider fallback or execute tools.

**Controls.** Callers send `X-OLP-Cache` with Cache-Control-style directives:
`no-cache` (skip lookup, still store), `no-store` (neither), `max-age=<s>`
(accept only entries younger than that) and `ttl=<s>` (store for at most that
long, bounded by the route). Responses carry `X-OLP-Cache: hit | miss | bypass`
and, on hits, `Age`; semantic matches use `semantic-hit`. Directive input is
limited to 512 bytes; unknown or duplicate directives and values outside
0–604,800 seconds return `400 invalid_cache_control`. `no-store` takes
precedence and `ttl=0` disables storage. HTTP `Cache-Control: no-store` stays in
place for browsers and intermediaries.

**Accounting.** A hit records an attempt of class `cache_hit` with zero provider
cost and the original usage marked as cached. By default hits count against the
key's request rate but not its token rate or budgets; `count_hits_against`
may additionally include `tokens` and/or `budgets`. A budget debit uses the
current equivalent cost/charge and is recorded separately from actual provider
cost. Hits retain original normalized usage; a client-disconnected replay does
not become a provider call. Reports separate currencies and estimated avoided
cost from invoices and provider prompt-cache savings.

**Purge.** `POST /api/v1/cache/purge` invalidates a route's or project's
entries by advancing a random namespace generation in the cache Valkey, in
constant time. It accepts exactly one `route_id` or `project_id`, requires
`configure` and project authority, and invalidates named/delegated route keys.
In-flight writes remain bound to the old generation. Evicting a generation
marker cannot resurrect old entries; old payloads expire naturally. Purge is
logical invalidation, not synchronous physical erasure.

**Metrics.** `olp_cache_requests_total{route,result}`,
`olp_cache_bytes_stored_total` and `olp_cache_errors_total`. A cache Valkey
outage degrades inference to `bypass`, never disables admission, and never
replays unauthenticated content. Lookup/store use 100 ms bounds. Corrupt,
unreadable or mismatched sealed metadata also bypasses. Oversized, incomplete
and failed responses are not stored. The management purge operation must
report failure if it cannot invalidate; it cannot claim success during outage.

### M8.2 Semantic cache

`mode: semantic` adds approximate matching:

- Requires a transformed generation route. `embedding_route` names an
  embeddings route authorized for the caller. Exact lookup is tried first; on
  a miss the embedding is a child request linked to the parent, with independent
  token/cost admission and ordinary accounting, including failed attempts. The
  parent owns the caller's request/concurrency reservation. Embedding failure
  bypasses approximate caching and does not bypass the model call's limits.
- The vector index uses the Valkey Search module on the cache Valkey. Vectors
  are derived from content and cannot be sealed while indexed, so enabling a
  semantic cache requires `allow_unsealed_vectors: true`, recorded in audit
  and reported in capabilities. Sealed responses authenticate scope, namespace
  generation and embedding vector-space identity before any candidate is used;
  search metadata alone is never trusted.
- Only single-turn requests are eligible by default: one user message, no
  tools, no images. Multi-turn and agentic traffic, where approximate matches
  return wrong answers, require `allow_multiturn: true`. Only the final user
  text varies approximately; preceding conversation, tool definitions and all
  non-query controls still match exactly. Media and retained resources remain
  ineligible under either setting.
- Cosine similarity defaults to 0.95 (valid range greater than zero through 1);
  at most five candidates are examined by default (valid range 1–32), ordered
  by descending score and key for ties. Changed embedding routes/revisions or
  vector dimensions cannot match an old vector space.
- A semantic hit carries `X-OLP-Cache: semantic-hit` and records its similarity
  score in the attempt.

### M8.3 Provider prompt-cache automation

On transformed routes, a `prompt_cache` policy inserts provider controls that
the caller omitted:

- Anthropic Messages on Anthropic, Vertex and Bedrock: `cache_control`
  breakpoints at configured positions (system prompt, tool definitions, the last
  user turn), with a 5-minute or 1-hour TTL, within the provider's breakpoint
  limit.
- OpenAI/Azure Chat or Responses: a `prompt_cache_key` derived from the key or
  session. Session derivation requires an explicit attribution `session_label`.
  It is a cache hint, not a guarantee of provider savings.

Defaults are system/tools/last-user positions, a 5-minute TTL and key-based
derivation. Caller controls win; conflicting caller breakpoint TTLs disable
automatic insertion, and Anthropic insertion stays within four breakpoints.
Provider/model TTL support still requires qualification. Strict routes reject
insertion, because it changes the invocation.
Route simulation shows inserted controls in the effective request, and usage
reports show cache read and write tokens by route.

## Change map

| Change | Start here |
| --- | --- |
| Cache keys, sealing, storage, purge | new `internal/cache/`, `internal/secrets/purpose.go` |
| Lookup, replay and accounting | `internal/gateway/executor.go`, `internal/gateway/accounting.go` |
| Prompt-cache insertion | `internal/providerinvoke/` |
| Configuration and Helm | `internal/configuration/`, `internal/routing/cache.go`, `deploy/helm/` |
| Console | `console/src/lib/features/routes/` |

## Binding deployment and completion order

Separate Valkey **instances** are required; another logical database is not
sufficient because eviction is instance-wide. Configuration rejects identical
endpoints and runtime checks reject aliases of the coordination server. Cache
values use versioned installation keys and authenticated associated data; key
rotation follows the existing key-ring retention policy. No request/response
bodies enter PostgreSQL, traces or logs. Semantic vectors remain the explicit
unsealed exception. Storage encryption does not replace instance access control.

| Order | Owner and deliverable | Gate |
| --- | --- | --- |
| 1 | Cache/configuration maintainers: preserve identity, sealing, bounds and dedicated-instance checks | Unit/integration fixtures and activation/plan rejection cases |
| 2 | Gateway/accounting maintainers: preserve guarded replay, quota separation, child embedding calls and no-store eligibility | Protocol, isolation, cancellation and accounting regression fixtures |
| 3 | Operations/console maintainers: verify publish, outage, purge, eviction and rotation procedures | Cache journey, generated-contract determinism and deployment checks |
| 4 | Performance/release maintainers: repeat paired bypass/hit workload at M1 S1 load on reference hardware | p95 incremental latency at most 1 ms, with environment and raw results retained |

Steps 1–3 have focused evidence below. Step 4 is open and does not become
complete through a microbenchmark. Deployment can disable caching by removing
the route policy; outstanding entries then expire. Emergency invalidation uses
purge, while physical erasure is an operator task on the dedicated cache store.
M7's implemented guardrail revision and execution contracts are prerequisites;
M7 live-vendor and performance qualification can proceed independently.

## Exit criteria

- [x] Hits and misses are correct for unary and streamed responses on each
      qualified eligible surface; replayed streams validate against the
      protocol corpus and ineligible surfaces bypass caching.
- [x] No entry is ever served across a key or project boundary in an isolation
      test with `key` and `project` scopes.
- [x] A new route or guardrail revision misses every older entry.
- [x] Inspecting the cache Valkey reveals no plaintext response for exact
      entries.
- [x] Losing the cache Valkey degrades to `bypass` without request errors.
- [ ] A cache hit adds at most 1 ms at p95 over scenario S1's added latency on
      the same hardware.
- [x] Prompt-cache insertion preserves caller controls and reports fixture
      cache-read tokens for repeated multi-turn traffic; strict routes refuse
      insertion. This is not evidence of live-provider savings.
- [x] The [parity matrix](parity.md) caching rows are `Parity` or better.

## Implementation evidence (2026-10-16)

These are historical focused results, not tests rerun during roadmap editing.
Code anchors include `internal/cache/{policy,canonical,store,search}.go`,
`internal/gateway/cache*.go`, `internal/providerinvoke/prompt_cache.go` and
their regression/integration fixtures.

`make check` passed with race detection, including the Go protocol corpus,
1,316 console tests and the script suites. The focused PostgreSQL/Valkey run
passed management authorization and project-isolation sweeps, cache activation,
sealed storage, invalidation, quotas, semantic embedding children, audit
acknowledgement and accounting. Native unary/stream replay covers Chat
Completions, Responses, Anthropic, Gemini and Bedrock Converse. The packaged
Chromium cache journey passed publication, replay, statistics, purge and axe.
Generated contracts reproduced byte-for-byte, and Helm lint/render passed.

The focused regression gate compared 15 gateway/preparation benchmarks with
the pre-M8 tree, using six 100 ms samples, and found no significant regression
above 10%. The dedicated cache benchmark used Go 1.27.2, `GOMAXPROCS=4`, an
8-vCPU Intel Haswell host and the pinned Valkey Bundle 9.1.0. With competing
builds stopped, six paired one-second samples measured:

| Warm request path | Median p95 |
| --- | --- |
| Cache bypass | 0.286 ms |
| Exact cache hit | 0.990 ms |
| Additional latency | 0.704 ms |

This development measurement satisfies the 1 ms overhead budget. The full-rate
S1 reference-hardware run remains an open release qualification step alongside
M1, so its exit checkbox remains open.

The broad `make integration` attempt encountered an eight-second process
shutdown timeout in `TestCodeCLIFollowupAutomaticRetriesAndRecovery/websocket-reconnect`.
That case passed when rerun alone. The broad run was not completed; the focused
M8 integration and browser results above are the completed evidence.
