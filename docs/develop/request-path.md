# Requests and responses

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
`X-OLP-Delivery-Replay`, `X-OLP-Resource-ID`, `X-Goog-Upload-URL`,
`X-Goog-Upload-Status`, `X-Goog-Upload-Size-Received`, `X-OLP-Attempts`,
`X-OLP-Route-Revision`, `X-OLP-Provider`, `X-OLP-Cost`, the six OpenAI
`x-ratelimit-*` headers, the six Anthropic `anthropic-ratelimit-*` headers, and
`Request-Id` on Anthropic responses.

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
