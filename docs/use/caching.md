# Response caching

A route can opt into exact response caching or semantic response matching. This
is an explicit content-storage choice rather than part of OLP's normal
content-free request history. Cached response bodies are encrypted with the
installation key ring and expire in a separate Valkey instance. PostgreSQL
retains cache configuration and accounting metadata, but never cached response
bodies.

## Configure a route

Use the response-caching section of Route Studio, or include a policy in a route
draft, published-route write, or configuration promotion document:

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

Omitting `cache` disables response caching for the route. The example above
shows the default values. Setting `scope` to `key` isolates API keys, while
`project` and `route` explicitly permit sharing between authorized keys in the
same project. Effective guardrails, provider configuration, route revisions,
semantic headers, and native request values remain part of cache identity.
Changing a route or guardrail revision makes old entries unreachable.

Only successful, completely metered responses are stored. Streams are stored
after their success terminal event and replay the original event sequence
without its timing. Unary and streaming entries are stored separately.

Several request kinds bypass the response cache so caching never skips side
effects, retained-state contracts, or non-text inputs. Gateway-executed tool
workflows, published prompts, legacy `/v1/completions`, negotiated tool
continuations, and delegated task capabilities bypass caching. Non-eligible
operations such as files, uploads, batches, realtime, decisions, OCR, search,
and retained resource management also bypass caching. Within eligible
operations, requests bypass caching when they request multiple candidates, use
caller-supplied credentials, trigger request-local input masking, carry an
internal non-caller origin, or come from a key with
`forbid_response_cache: true`. Stateful or reference-bearing fields also bypass
the cache, including `store` values other than `false`, `background`,
`previous_response_id`, `conversation`, `file_id`, `file_ids`, `batch_id`,
`file_uri`, `file_data`, `vector_store_ids`, `cachedContent`, and
`item_reference`, as well as multimodal and file inputs such as `image`,
`image_url`, `input_image`, `audio`, `input_audio`, `video`, `document`,
`file`, `input_file`, `inlineData`, and `fileData`.

Authentication, request and concurrency admission, and input guardrails still
apply on cached routes. By default a hit counts against request limits, while
consuming no provider tokens or cost budget. Add `tokens` or `budgets` to
`count_hits_against` to debit cached usage or its equivalent current price.
Those budget debits remain separate from actual provider billing. Embedding
calls used by semantic matching are ordinary authorized, billed requests linked
to their parent. Their token and cost limits apply independently, while the
parent owns the caller request and concurrency reservation.

## Identity, ordering, and bounds

Cache TTL must be between 1 and 604,800 seconds, and maximum entry size must be
between 1 and 16,777,216 bytes. The `streaming` field accepts `replay` or
`bypass`. Cache identity is an HMAC under `cache_key` that binds the project and
scope, named and delegated routes, provider revision, selected target, ordered
effective guardrail revisions, dialect, transport, semantic headers and query
settings, and response-projection controls. Identity is computed from the
effective request after defaults, rewriting, prompt-cache insertion, and input
guardrails. Canonicalization sorts object keys, preserves array order and
numeric spelling, and refuses duplicate JSON members without floating-point
conversion.

Parallel input guardrails join before a hit becomes usable. Token and budget
admission uses the resolved hit or miss policy, and output guardrails run before
storage. Stateful Responses requests from keys with `allow_provider_state` must
explicitly send `store: false` to become cache-eligible. Fallback responses are
never stored under another target's lookup identity. A disconnected cache replay
remains recorded as a `cache_hit` attempt rather than provider work.

Lookup and store operations have 100 ms bounds. Corrupt or mismatched sealed
values bypass cleanly. Versioned installation keys bind cache identity through
authenticated associated data, and key rotation retains historical keys under
the normal key-ring policy. Purge failures are reported rather than acknowledged
as success. Purge performs logical invalidation, while physical erasure belongs
to the operator of the dedicated cache store.

## Per-request controls

Send comma-separated directives in the `X-OLP-Cache` request header:

| Directive | Effect |
| --- | --- |
| `no-cache` | Skip lookup while allowing a successful response to replace the entry. |
| `no-store` | Skip both lookup and storage. |
| `max-age=30` | Accept entries no older than 30 seconds. |
| `ttl=60` | Store for at most 60 seconds, capped by the route lifetime. |

Directive input is limited to 512 bytes. Unknown or duplicate directives and
values outside 0 to 604,800 seconds return `400 invalid_cache_control`. The
`no-store` directive takes precedence over other directives, and `ttl=0`
disables storage. In route configuration, `requests` is mandatory in
`count_hits_against`.

Even on a route without a `cache` policy configured, sending `X-OLP-Cache` on a
cache-eligible operation still validates the header. An invalid directive
returns `400 invalid_cache_control`, and a valid header results in
`X-OLP-Cache: bypass` on the response. When the route has no `cache` policy and
the caller omits `X-OLP-Cache`, the gateway skips cache processing and omits the
`X-OLP-Cache` response header.

Successful responses on configured routes report `X-OLP-Cache` with value `hit`,
`semantic-hit`, `miss`, or `bypass`. Hits also include `Age` in seconds. HTTP
`Cache-Control: no-store` remains in place so response caching never authorizes
a browser or intermediary to retain content.

## Semantic matching

A transformed generation route can set `mode` to `semantic`. Set
`embedding_route` to a published embeddings route that the caller is authorized
to use, and explicitly acknowledge `allow_unsealed_vectors: true`. Cached
response bodies remain encrypted, while content-derived embedding vectors are
visible in the Valkey Search index. The default similarity threshold is 0.95
(greater than 0 through 1), and at most 5 candidates are examined by default
(configurable from 1 to 32), ordered by descending score and then by key for
ties. Sealed values authenticate scope, namespace, and embedding vector space,
so a changed route, revision, or vector dimension cannot match old vectors. An
embedding failure bypasses approximate lookup while ordinary model admission
still applies.

Single-turn text is eligible by default. Setting `allow_multiturn: true` permits
matching the final user question within an otherwise identical conversation.
Other request settings and preceding conversation content must still match
exactly. Requests with tool definitions are excluded unless `allow_multiturn` is
enabled, and their definitions must still match exactly. Neither exact nor
semantic caching caches provider-retained resources or executes tools.

## Provider prompt caching

An independent `prompt_cache` policy on a transformed route inserts omitted
provider prompt-caching controls before input inspection and token estimation:

```json
{
  "prompt_cache": {
    "positions": ["system", "tools", "last_user"],
    "ttl": "5m",
    "key_source": "key"
  }
}
```

OpenAI and Azure OpenAI targets (`openai` and `azure_openai`) on Chat
Completions and Responses receive a derived `prompt_cache_key` when the caller
omits it. Setting `key_source` to `session` additionally requires
`session_label`, which names an attribution label of at most 32 characters. If
the request carries no value for that attribution label, `prompt_cache_key` is
omitted.

Anthropic Messages wire targets insert at most four ephemeral cache breakpoints
across `tools`, `system`, and `last_user` with `ttl` set to `5m` (the default)
or `1h`. This applies to `anthropic` providers, `vertex_ai` providers using the
`vertex-anthropic` profile, and `bedrock` providers using the
`bedrock-anthropic-invoke` profile. Bedrock Converse does not use Anthropic
Messages wire encoding and does not receive automatic breakpoints. Select a
provider and model that supports the configured duration.

Existing caller controls always take precedence. A caller-supplied top-level
`cache_control` field disables automatic breakpoint insertion entirely. A caller
block-level `cache_control` whose TTL differs from the route's configured `ttl`
also disables automatic breakpoint insertion for that invocation so mixed-TTL
ordering is never broken. Strict routes reject `prompt_cache` policies. Route
simulation shows the inserted fields, and usage reports include provider
cache-read and cache-write token dimensions.

## Deploy and operate

Set `OLP_CACHE_VALKEY_URL` or `OLP_CACHE_VALKEY_URL_FILE`. For TLS use a
`rediss` URL and, where needed, `OLP_CACHE_VALKEY_TLS_CA_FILE`. The cache must
be a different Valkey instance from `OLP_VALKEY_URL`, not another logical
database on the same server. OLP rejects identical endpoints and checks server
identity before using a separately named endpoint. A cache outage bypasses
caching and never disables the main limiter.

Authentication capabilities expose `response_cache_configured` and
`semantic_cache_vectors_unsealed`. The `response_cache_configured` flag reflects
whether a cache Valkey endpoint is configured and does not imply live
connectivity. The `semantic_cache_vectors_unsealed` flag is always reported as
`true` to declare that semantic cache search vectors remain unsealed in the
Valkey index whenever semantic caching is enabled.

The optional Compose `cache` profile provides a pinned Valkey Bundle with
Search, a 256 MiB memory cap, `allkeys-lru` eviction, and persistence disabled.
Set `OLP_CACHE_VALKEY_URL=redis://cache:6379` when enabling it. Local
development and integration environments provision their own cache
automatically. In Helm, configure `config.cacheValkeySecretName`,
`config.cacheValkeySecretKey`, optional `config.cacheValkeyTlsCaFile`, and the
dedicated `networkPolicy.egress.cacheValkey` peer.

Route Studio can purge a published route. API operators can call
`POST /api/v1/cache/purge` with exactly one of `route_id` or `project_id`,
which requires `configure` permission and authorization for that project. Purge
advances a random namespace generation in constant time. Old entries expire
naturally and cannot reappear when an eviction removes an invalidation marker.

`GET /api/v1/cache/statistics` reports retained request activity over the last
30 days, optionally filtered by `route`, with avoided costs separated by
currency. Request history identifies cache results and cache attempts.
Prometheus metrics are `olp_cache_requests_total` (labeled by `route` and
`result`), `olp_cache_bytes_stored_total`, and `olp_cache_errors_total`. Avoided
cost is an estimate at current prices rather than an invoice credit or a
guarantee of provider-side prompt-cache savings.

Reference-hardware release qualification remains open. Paired cache-hit and
bypass S1 workloads must show at most 1 ms incremental p95 latency. See
[Performance](../develop/performance.md#performance-budget).
