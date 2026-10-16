# Response caching

Routes can opt into an exact response cache or semantic matching. Cache entries
contain response content, so they are an explicit exception to OLP's otherwise
content-free storage. Entries are encrypted with the installation key ring and
expire in a separate Valkey instance. PostgreSQL retains configuration and
accounting metadata, never cached response bodies.

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

Omitting `cache` disables it. The example shows the defaults. `scope: key`
isolates API keys; `project` and `route` explicitly permit sharing between
authorized keys in the same project. Effective guardrails, provider configuration,
route revisions, semantic headers and native request values remain part of cache
identity. Changing a route or guardrail revision makes old entries unreachable.

Only successful, completely metered responses are stored. Streams are stored
after their success terminal event and replay the original event sequence without
its timing. Unary and streaming entries are separate. Retained Responses,
previous-response references, files, uploads, batches, realtime, multiple
candidates, caller-supplied credentials and request-local masking bypass the
cache. Keys can independently set `forbid_response_cache: true`.

Authentication, request/concurrency admission and input guardrails still apply.
By default a hit counts against request limits, but consumes no provider tokens
or cost budget. Add `tokens` or `budgets` to `count_hits_against` to debit cached
usage or its equivalent current price. Those budget debits remain separate from
actual provider billing. Embedding calls used by semantic matching are ordinary
authorized, billed requests linked to their parent. Their token and cost limits
apply independently; the parent owns the caller request/concurrency reservation.

## Per-request controls

Send comma-separated directives in `X-OLP-Cache`:

| Directive | Effect |
| --- | --- |
| `no-cache` | Skip lookup; a successful response may replace the entry. |
| `no-store` | Skip both lookup and storage. |
| `max-age=30` | Accept entries no older than 30 seconds. |
| `ttl=60` | Store for at most 60 seconds, capped by the route lifetime. |

Successful responses on configured routes report `X-OLP-Cache: hit`,
`semantic-hit`, `miss`, or `bypass`. Hits include `Age` in seconds. HTTP
`Cache-Control: no-store` remains in place; response caching does not authorize a
browser or intermediary to retain content.

## Semantic matching

A transformed generation route can use `mode: semantic`. Set `embedding_route`
to a published embeddings route the caller may use and explicitly acknowledge
`allow_unsealed_vectors: true`. Responses remain sealed, but content-derived
vectors are visible in the Valkey Search index. The default similarity threshold
is 0.95 and at most five candidates are examined; both are configurable.

Single-turn text is eligible by default. `allow_multiturn: true` permits matching
the final user question within an otherwise identical conversation. Other
request settings and preceding conversation content must match exactly. Tools
are excluded unless this option is enabled; their definitions still match
exactly. Neither mode caches provider-retained resources or executes tools.

## Provider prompt caching

An independent `prompt_cache` policy on a transformed route inserts omitted
provider controls. For example:

```json
{
  "prompt_cache": {
    "positions": ["system", "tools", "last_user"],
    "ttl": "5m",
    "key_source": "key"
  }
}
```

Anthropic Messages use explicit breakpoints within the provider's limit. `ttl`
may be `5m` or `1h`; select a provider/model supporting that duration. OpenAI
requests receive a derived `prompt_cache_key`; `key_source: session` additionally
requires `session_label`, naming an attribution label. Existing caller controls
take precedence; a caller's different breakpoint TTL disables automatic
breakpoints for that invocation. Strict routes reject insertion. Simulation shows the inserted
fields, and existing usage reports include provider cache-read and cache-write
token dimensions.

## Deploy and operate

Set `OLP_CACHE_VALKEY_URL` or `OLP_CACHE_VALKEY_URL_FILE`. For TLS use a `rediss`
URL and, where needed, `OLP_CACHE_VALKEY_TLS_CA_FILE`. The cache must be a different
Valkey instance from `OLP_VALKEY_URL`, not another logical database. OLP rejects
identical endpoints and checks server identity before using a separately named
endpoint. A cache outage bypasses caching; it never disables the main limiter.
Authentication capabilities expose `response_cache_configured` and
`semantic_cache_vectors_unsealed`; configuration does not imply live connectivity.

The optional Compose `cache` profile provides a pinned Valkey Bundle with Search,
a 256 MiB memory cap, `allkeys-lru` eviction, and persistence disabled. Set
`OLP_CACHE_VALKEY_URL=redis://cache:6379` when enabling it. Local development and
integration environments provision their own cache automatically. In Helm, use
`config.cacheValkeySecretName`, `cacheValkeySecretKey`, optional
`cacheValkeyTlsCaFile`, and the dedicated `networkPolicy.egress.cacheValkey` peer.

Route Studio can purge a published route. API operators can call
`POST /api/v1/cache/purge` with exactly one `route_id` or `project_id`; this requires
`configure` and authorization for that project. Purge advances a random namespace
generation in constant time. Old entries expire naturally and cannot reappear
when an eviction removes an invalidation marker.

`GET /api/v1/cache/statistics` reports retained request activity over the last
30 days, optionally filtered by route, with avoided costs separated by currency.
Request history identifies cache results and cache attempts. Metrics are
`olp_cache_requests_total{route,result}`, `olp_cache_bytes_stored_total`, and
`olp_cache_errors_total`. Avoided cost is an estimate at current prices; it is not
an invoice credit or a guarantee of provider-side prompt-cache savings.
