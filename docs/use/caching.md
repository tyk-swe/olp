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

## Identity, ordering and bounds

TTL is 1–604,800 seconds and maximum entry size is 1–16,777,216 bytes;
`streaming` accepts `replay` or `bypass`. Cache identity is an HMAC under
`cache_key`, binding project/scope, named and delegated routes, provider revision,
selected target, ordered effective guardrail revisions, dialect, transport,
semantic headers/query and response-projection controls. It uses the effective
request after defaults, rewriting, prompt-cache insertion and input guardrails.
Canonicalization sorts object keys, preserves array order and numeric spelling,
and refuses duplicate JSON members without floating-point conversion.

Parallel input guards join before a hit becomes usable. Token/budget admission
uses the resolved hit/miss policy; output guards run before storage. Stateful
Responses must explicitly request `store: false` to become eligible. Fallback
responses are never stored under another target's lookup identity. A disconnected
cache replay remains a `cache_hit` attempt, not provider work.

Lookup/store have 100 ms bounds. Corrupt or mismatched sealed values bypass.
Versioned installation keys bind cache identity through authenticated associated
data; key rotation retains historical keys under the normal ring policy. Purge
failure is reported, not acknowledged as success. Purge is logical invalidation;
physical erasure belongs to the dedicated store's operator.

## Per-request controls

Send comma-separated directives in `X-OLP-Cache`:

| Directive | Effect |
| --- | --- |
| `no-cache` | Skip lookup; a successful response may replace the entry. |
| `no-store` | Skip both lookup and storage. |
| `max-age=30` | Accept entries no older than 30 seconds. |
| `ttl=60` | Store for at most 60 seconds, capped by the route lifetime. |

Directive input is at most 512 bytes. Unknown/duplicate directives and values
outside 0–604,800 seconds return `400 invalid_cache_control`. `no-store` wins;
`ttl=0` disables storage. `requests` is mandatory in `count_hits_against`.

Successful responses on configured routes report `X-OLP-Cache: hit`,
`semantic-hit`, `miss`, or `bypass`. Hits include `Age` in seconds. HTTP
`Cache-Control: no-store` remains in place; response caching does not authorize a
browser or intermediary to retain content.

## Semantic matching

A transformed generation route can use `mode: semantic`. Set `embedding_route`
to a published embeddings route the caller may use and explicitly acknowledge
`allow_unsealed_vectors: true`. Responses remain sealed, but content-derived
vectors are visible in the Valkey Search index. The default similarity threshold
is 0.95 (greater than zero through 1) and at most five candidates are examined
(1–32), ordered by descending score and key for ties. Sealed values authenticate
scope, namespace and embedding vector space; changed route/revision/dimensions
cannot match old vectors. Embedding failure bypasses approximate lookup while
ordinary model admission still applies.

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

Anthropic Messages on Anthropic, Vertex and Bedrock use at most four breakpoints. `ttl`
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

Reference-hardware release qualification remains open: paired cache-hit/bypass S1
workloads must show at most 1 ms incremental p95 latency. See
[Performance](../develop/performance.md#performance-budget).
