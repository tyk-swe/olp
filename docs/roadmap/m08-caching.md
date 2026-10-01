# M8: Response caching

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M7](m07-guardrails.md) | [M3](m03-routing-resilience.md) (sessions for prompt-cache keys) | None |

LiteLLM caches responses in Redis, Valkey, object storage, memory or disk, with
semantic variants and per-request controls. OLP has no response cache: it
forwards provider prompt-caching controls natively and prices cache reads and
writes, but a repeated request reaches a provider again. OLP's request records
never hold content, but two bounded stores already do: strict files and batches
keep a sealed native payload for seven days, and continuations keep sealed
history for 24 hours and replay a repeated submission without a provider call.
This milestone adds response caching on the same terms: explicit, sealed,
scoped and bounded.

## Outcome

- Routes opt into exact-match caching, with sealed entries in a dedicated
  Valkey, scoped so that no response crosses a key or project boundary unless
  the operator says so.
- Streamed responses replay from cache with their original event sequence.
- Semantic caching serves single-turn traffic that tolerates approximate
  matches.
- Transformed routes insert provider prompt-cache breakpoints automatically.
- Cache hits are accounted, rate-limited and reported like any other request.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Response cache | None | [Caching](https://docs.litellm.ai/docs/proxy/caching) with exact and [semantic](https://docs.litellm.ai/docs/proxy/caching_semantic) backends |
| Sealed content stores | Strict file and batch payloads, and continuation history and delivery, sealed in PostgreSQL under the `provider_continuation` purpose ([`continuation.go`](../../internal/resources/continuation.go)) | Not comparable |
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

`scope` is `key` (the default), `project` or `route`. A key policy may forbid
caching for that key.

**Storage.** Entries live in a Valkey deployment named by
`OLP_CACHE_VALKEY_URL`, never in the Valkey that holds limits, leases and
accounting streams: a cache needs an eviction policy, and limit state must never
be evicted. Each value is sealed with AES-256-GCM under a new `response_cache`
purpose, with the cache key and installation as associated data, following the
sealing pattern of the continuation store. Nothing is written to PostgreSQL.

**Keys.** The cache key is an HMAC (new digest purpose `cache_key`) over the
route revision, every guardrail policy revision in effect for the request, the
surface and dialect, the transport mode, the scope identifier, and the
canonical request after input guardrails and model rewriting. The canonical
form uses the decoded source that ingress already validates (no duplicate
members, exact numbers), with members sorted. A new route or guardrail revision
at any attachment scope therefore never serves an older entry. Masked requests
still share entries, because
[`mask`](m07-guardrails.md#m71-guardrail-engine) placeholders are deterministic
per request.

**Eligibility.** These requests are never cached: those using stateful
Responses fields (`store`, `background`, `previous_response_id`), files,
batches, realtime or media uploads, and those asking for more than one
alternative (`n` or `candidateCount` above 1). Only successful responses are
stored, and a stream only after its success terminal event.

**Order of operations.** Authentication, admission and input guardrails run
before lookup, so blocked or over-limit requests never read the cache. Output
guardrails run before storage, so entries are already guarded. A hit replays
the stored response; for streams, the original events in order, without
original timing.

**Controls.** Callers send `X-OLP-Cache` with Cache-Control-style directives:
`no-cache` (skip lookup, still store), `no-store` (neither), `max-age=<s>`
(accept only entries younger than that) and `ttl=<s>` (store for at most that
long, bounded by the route). Responses carry `X-OLP-Cache: hit | miss | bypass`
and, on hits, `Age`.

**Accounting.** A hit records an attempt of class `cache_hit` with zero provider
cost and the original usage marked as cached. By default hits count against the
key's request rate but not its token rate or budgets; `count_hits_against`
changes that. Usage reports show hits, misses and the provider cost avoided at
current prices.

**Purge.** `POST /api/v1/cache/purge` invalidates a route's or project's
entries by advancing a namespace generation in the cache Valkey, in constant
time. It requires the `configure` operation.

**Metrics.** `olp_cache_requests_total{route,result}`,
`olp_cache_bytes_stored_total` and `olp_cache_errors_total`. A cache Valkey
outage degrades to `bypass`, never to an error.

### M8.2 Semantic cache

`mode: semantic` adds approximate matching:

- `embedding_route` names an OLP embeddings route, whose calls are accounted
  attempts; `similarity_threshold` and `max_candidates` bound matching.
- **Isolation.** An approximate lookup cannot rely on the exact cache key, so
  every vector is indexed with its scope identifier, route revision, guardrail
  policy revisions and namespace generation, and a query filters on all of
  them before similarity is compared. A semantic hit therefore never crosses a
  boundary that an exact entry could not, and a purge or a new revision
  invalidates semantic entries too.
- The vector index uses the Valkey Search module on the cache Valkey. Vectors
  are derived from content and cannot be sealed while indexed, so enabling a
  semantic cache requires an explicit acknowledgement, recorded in audit and
  reported in capabilities.
- Only single-turn requests are eligible by default: one user message, no
  tools, no images. Multi-turn and agentic traffic, where approximate matches
  return wrong answers, require an explicit opt-in.
- A semantic hit carries `X-OLP-Cache: semantic-hit` and records its similarity
  score in the attempt.

### M8.3 Provider prompt-cache automation

On transformed routes, a `prompt_cache` policy inserts provider controls that
the caller omitted:

- Anthropic Messages on Anthropic, Vertex and Bedrock: `cache_control`
  breakpoints at configured positions (system prompt, tool definitions, the last
  user turn), with a 5-minute or 1-hour TTL, within the provider's breakpoint
  limit.
- OpenAI: a `prompt_cache_key` derived from the key or from the session, as
  [M3.2](m03-routing-resilience.md#m32-capacity-aware-selection-and-session-affinity)
  defines it, which improves hit rates for shared prefixes. Until M3.2 ships,
  the key alone is used.

Strict routes never insert controls, because doing so changes the invocation.
Route simulation shows inserted controls in the effective request, and usage
reports show cache read and write tokens by route.

## Non-goals

- Serving an entry across a key or project boundary by default.
- Caching stateful, realtime or content-retaining operations.
- Storing cached content in PostgreSQL, or in the Valkey that holds limits.
- Retrieval pipelines. The cache stores responses; it does not ingest
  documents.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Cached responses | The cache Valkey, sealed | The entry TTL, at most the route's `ttl_seconds`, or until evicted or purged | New seal purpose `response_cache` |
| Cache keys | The cache Valkey, as HMAC digests | With the entry | New digest purpose `cache_key` |
| Semantic vectors, tagged with scope, revisions and namespace | The cache Valkey search index, unsealed | With the entry | None; requires the semantic-cache acknowledgement |
| Cache configuration | Route revisions in PostgreSQL; the runtime snapshot | As route revisions today | None |
| Cache-hit attempts | Attempt records in PostgreSQL | Request retention | None; metadata only |

## Change map

| Change | Start here |
| --- | --- |
| Cache keys, sealing, storage, purge | new `internal/cache/`, `internal/secrets/purpose.go` |
| Lookup and replay | `internal/gateway/server.go`, `internal/gateway/unary.go`, `internal/gateway/executor.go` |
| Hit accounting | `internal/gateway/accounting.go` |
| Route `cache` configuration | `internal/routes/`, `internal/configuration/` |
| Prompt-cache insertion | `internal/providerinvoke/` |
| Process configuration and Helm | `internal/config/`, `deploy/helm/` |
| Console | `console/src/lib/features/routes/` |

## Decisions to settle

1. Whether a cache may be shared across keys within a project by default
   (recommended: no; `key` scope is the default, and wider scopes are explicit).
2. Whether cross-mode replay (serving a cached stream to a unary request) is
   allowed on transformed routes (recommended: not in the first release).
3. Whether `OLP_CACHE_VALKEY_URL` may point at the main Valkey with a separate
   logical database (recommended: no, because eviction policies are
   per-instance).

## Exit criteria

- [ ] **M8.1** Hits and misses are correct for unary and streamed responses on
      every surface, and replayed streams validate against the protocol corpus.
- [ ] **M8.1, M8.2** No entry, exact or semantic, is ever served across a key
      or project boundary in an isolation test with `key` and `project` scopes,
      including for a near-identical prompt from another key.
- [ ] **M8.1, M8.2** A new route revision, or a new guardrail policy revision
      at any attachment scope, misses every older entry, exact or semantic.
- [ ] **M8.1** Inspecting the cache Valkey reveals no plaintext response for
      exact entries.
- [ ] **M8.1** Losing the cache Valkey degrades to `bypass` without request
      errors, and a purge makes every entry of its route or project miss.
- [ ] **M8.1** A hit is recorded as a `cache_hit` attempt with zero provider
      cost and counts against exactly the limits `count_hits_against` names.
- [ ] **M8.1** A cache hit adds at most 1 ms at p95 over scenario S1's added
      latency on the same hardware.
- [ ] **M8.2** A semantic cache cannot be enabled without its acknowledgement,
      serves only requests above the similarity threshold, and never serves a
      multi-turn request unless the route opts in.
- [ ] **M8.3** Prompt-cache insertion raises measured cache-read tokens on a
      fixture replaying multi-turn traffic, and never applies on strict routes.
- [ ] The [parity matrix](parity.md) caching rows are `Parity` or better.
