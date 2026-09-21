# Gateway execution

The Go gateway serves OpenAI, Anthropic, Gemini, and Bedrock clients with shared
authentication, project boundaries, admission, routing policy, and accounting.
Canonical generation, media, retained resources, and realtime use the execution
paths appropriate to their lifecycle. See [Access](access.md) for installation
and identity.

The [compatibility matrix](compatibility.md) owns endpoint support and
translation limits. [Provider connections and routing](provider-routing.md)
covers onboarding, credential pools, publication, and selection policy.

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials; enabled named pools require it. Mounted releases must contain the
published default-slot ID; republish older Go releases before enabling this
mode. `worker` publishes no public listener and runs only the accounting, media
reconciliation, and [recovery plane](operations.md#replicated-worker-health),
which `all` also runs; an installation that serves traffic with `gateway` and
`control` needs at least one `worker` replica for accounting, budget
reconciliation, retention, and media job polling to happen at all. Gateway
readiness on the private `GET /health/ready` listener fails while the
key-authority snapshot is missing or stale.

## Configuration

Use the [configuration reference](configuration.md) for flags, defaults, bounds,
egress, and mounted connectors, and [access control](access.md) for secret files
and maintenance commands. Local mock providers need explicit loopback and HTTP
[egress exceptions](configuration.md#test-and-harness-variables).

## Runtime publication and authority

Every provider or route activation compiles the complete set of active providers
and latest route revisions into one snapshot, validates it, records its SHA-256,
and stores it as a numbered runtime release. Gateways install the newest release
only when it validates and every referenced credential can be decrypted; a
release that fails either check is skipped and the previous one stays active.
Repeated publication is harmless.

Key authority (API keys, expiry, revocation, and revoked credential versions) is
polled every five seconds independently of release installation. Authority older
than 60 seconds, measured from the start of the last successful read, is stale:
new requests are rejected with `503 authority_unavailable` while requests
already admitted keep the snapshot and policy they were pinned to.

## Request path

Requests authenticate with `Authorization: Bearer <key>`, Anthropic `X-Api-Key`,
or Gemini `X-Goog-Api-Key` (the Gemini query-key form is also accepted). Bedrock
uses `X-OLP-API-Key` or a non-SigV4 bearer key. Every inference operation needs
`inference`; model reads need `models_read`. A key may use only routes in its
own project; a key without a project may use only routes without a project. A
route allowlist restricts both scopes further. An empty allowlist permits every
route within that project boundary. The body model, or Gemini URL model, must be
a published route slug. Model list/get expose only those routes the key may use.

Each request receives an `X-Request-Id` (a client-supplied value is kept when it
is a safe token), a no-store cache policy, and CORS headers for explicitly
allowed browser origins (`OLP_GATEWAY_CORS_ALLOWED_ORIGINS`). JSON endpoint
bodies must be `application/json`, optionally gzip-compressed, and within the
JSON body limit before and after inflation; the media endpoints also accept raw
and `multipart/form-data` bodies bounded by `OLP_HTTP_MAX_MEDIA_BODY_BYTES` and
the spool's per-endpoint reservations. Uploads have a 15-second read deadline;
incomplete bodies receive `408 request_timeout` and release their admission
slot. The upload deadline ends when the body is read, before the route's
inference deadline starts. The optional `X-OLP-Routing` header accepts one JSON
object, for example
`{"strategy":"price","allow_fallbacks":false,"max_attempts":1}`. It can narrow
constraints and attempts within published policy, never expand access or
deadlines. Unknown controls, invalid selectors, and budget increases are
refused. Raw header values are never forwarded or persisted.

The optional `X-OLP-Attribution` header attaches caller-chosen labels to a
request's usage records, for example `{"team":"core","env":"prod"}`. Exactly one
header is accepted, at most 4096 bytes, decoding to a single JSON object with at
most four entries. Every key must appear in the API key's configured
`allowed_attribution_keys` allowlist, and every value must be a short machine
token (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`); nested values, numbers, nulls,
free text, and control characters are refused with `invalid_attribution` before
the request reaches a provider. Labels are metadata only: they never influence
authentication, authorization, or routing, and they travel with the request's
usage facts and hourly rollups so usage reports can filter and break down by
them.

Canonical attempts follow priority and preferred-order tiers, then the selected
strategy. Weighted ties are seeded by the key so the same key sees a stable
order. Each attempt consumes the budget, uses the target's timeout within the
route's overall deadline, injects the selected slot's credential, and rewrites
the model to the upstream identifier. Native calls preserve unknown request
fields; translation refuses semantic extensions it cannot represent. Failover to
the next eligible attempt happens only before any response bytes have been sent
to the client and only for connect, timeout, rate-limit, credential, and
upstream server failures; upstream client errors, protocol errors, and
cancellations are terminal. A committed stream never restarts on another
provider: a later failure is reported in-band as an error event and the stream
ends without a success marker. Retained resources, realtime, and native Bedrock
ingress pin one provider/credential without cross-target failover; see
[compatibility](compatibility.md). Translation and native tool validation bound
retained text/tool state by the event-size limit. Distinct native Chat choice
tracking is bounded to `max(1, OLP_PROVIDER_MAX_EVENT_BYTES / 16)` entries.
Bedrock advertised event lengths are checked before SDK allocation, and the SDK
still verifies event CRCs.

Provider health is tracked per gateway: five counted failures within 30 seconds
open a provider's circuit for 30 seconds. One half-open probe may proceed;
credential-only failure releases it without penalizing siblings. A credential
rejection cools that credential version for 60 seconds; a rate limit cools the
logical slot across rotation for the upstream `Retry-After` (10 seconds when
absent, at most 60 seconds). Client cancellation and disconnects close the
upstream request and release admission once. Unary response writes and
individual stream frames have a 30-second write deadline. Failed unary writes or
flushes are recorded as cancellation; successful delivery is recorded only after
the response has been flushed.

| Status | `error.code` | Meaning |
| --- | --- | --- |
| 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `unsupported_parameter`, `unsupported_stateful_reference`, `request_exceeds_token_limit`, `content_policy_blocked` | Request envelope problems, including unsupported Responses state references, an estimate larger than the key's tokens-per-minute limit, or text blocked by a route content-policy rule. |
| 401 | `invalid_api_key` | Missing, unknown, expired, or revoked key. |
| 403 | `permission_denied`, `route_forbidden` | Missing scope or route outside the key's project/allowlist. |
| 404 | `route_not_found`, `not_found` | Unknown route slug or endpoint. |
| 408 | `request_timeout` | Request body not received within 15 seconds. |
| 413 / 415 | `request_too_large`, `unsupported_media_type`, `unsupported_content_encoding` | Body limits and content negotiation. |
| 422 | `content_policy_surface_unavailable`, `content_policy_streaming_requires_unary` | The request surface cannot be inspected by the route's content policy, or output rules require a buffered unary response instead of streaming. |
| 429 | `rate_limit_exceeded`, `budget_exhausted`, `upstream_rate_limit` | The key's requests, tokens, or concurrency limit was exceeded; the key's daily or monthly cost budget is exhausted; or every attempt was rate limited upstream. `Retry-After` carries whole seconds. |
| 502 | `upstream_unavailable`, `upstream_rejected`, `upstream_authentication_failed`, `upstream_permission_denied`, `provider_protocol_error` | Upstream or transport failures after the budget is spent. |
| 503 | `authority_unavailable`, `request_admission_overloaded`, `distributed_limits_unavailable`, `upstream_unavailable` | Stale authority, admission limit, limits that cannot be enforced, or no eligible target. |
| 504 | `gateway_timeout` | Route deadline reached before commitment. |

## Content policy

A route draft may carry an optional `content_policy`: an ordered list of local
[RE2](https://github.com/google/re2/wiki/Syntax) rules validated and compiled
inside the gateway — no external service, model, or timeout is involved. Each
rule names a phase (`input` or `output`), an action (`block` or `redact`), a
pattern, and an optional literal replacement (redact only; `$`-sequences are
never expanded, so a replacement of `$1` inserts the text `$1`). Validation
bounds the policy at 64 rules, 512 bytes per pattern, 16 KiB of combined pattern
bytes, and rejects patterns that match the empty string. Policies publish with
the route revision, restore with it, and travel through configuration export and
import.

Input rules run before token estimation and before any provider call. The
gateway walks only the supported textual fields of the canonical request —
OpenAI Chat and Responses message content and instructions, Anthropic system and
message content, Gemini contents and system instructions, embeddings and
moderation inputs, rerank queries and documents, and the textual
`prompt`/`input` fields of media requests. URLs, binary or base64 payloads, tool
schemas and descriptions, and unrelated metadata are never inspected. Rules
apply in document order: `redact` rewrites the in-memory request dispatched
upstream, and `block` stops evaluation and answers `400 content_policy_blocked`
without a provider call. The original client body and the transformed body are
never logged or persisted.

Output rules apply only to unary generation responses, after the upstream body
is decoded and usage and cost are accounted, and before the response is written
to the client. Only visible assistant text and textual refusals are inspected —
reasoning, tool-call arguments, citations, and binary or media output are not.
`redact` rewrites the response in the caller's OpenAI, Anthropic, or Gemini
family format; `block` answers `400 content_policy_blocked` with no output
payload while the provider attempt, usage, and cost remain accounted. Because
output inspection needs the complete decoded response, a request with
`stream: true` on a route with any output rule is rejected before dispatch with
`422 content_policy_streaming_requires_unary`; input-only policies may stream
after input enforcement.

Surfaces where canonical text cannot be inspected refuse policy-equipped
requests rather than bypassing the policy: raw Bedrock `InvokeModel`,
Files/Batch JSONL, realtime events, stateful Responses, and other
provider-native or background surfaces answer
`422 content_policy_surface_unavailable` before dispatch. Bedrock Converse is
inspectable only through the canonical decode path.

Enforcement evidence is metadata only. Each matched rule records a
`{rule_id, phase, action, outcome}` decision — `blocked` or `redacted` — on the
durable request record (`requests.policy_decisions`) and the request history
API. Matched text, offsets, patterns, replacement strings, and the request or
response bodies are never recorded anywhere.

Content policy is a deterministic text filter, not a security guarantee: it
cannot promise universal safety, prompt-injection prevention, or coverage of
fields a provider interprets outside the inspected surfaces.

## Media and durable video jobs

The media endpoints — `/v1/images/generations`, `/v1/images/edits`,
`/v1/images/variations`, `/v1/audio/speech`, `/v1/audio/transcriptions`, and the
`/v1/videos` family — share the same authentication, route-slug, limits, and
accounting contracts as generation. Multipart and raw uploads reserve capacity
inside `OLP_HTTP_MAX_MEDIA_BODY_BYTES` and per-endpoint fractions of the spool;
oversized bodies, excessive parts, and insufficient capacity fail predictably,
and cancellation releases the reservation and its files. Restart cleanup never
removes live work, and uploaded content stays inside its bounded operational
lifetime — it never enters persistent request diagnostics.

Video creation is asynchronous: an admitted request writes a durable job record
pinning the route, provider, slot, credential, and price revisions, so a client
disconnect cannot erase upstream work and an ambiguous create never triggers a
blind retry or duplicate. Jobs cannot cross API-key ownership boundaries. The
worker plane's media reconciler polls claimed jobs to completion, resolves the
pinned historical credential through rotation and provider changes, obeys
explicit revocation guards, and finishes accounting. `GET /v1/videos`,
`GET /v1/videos/{video_id}`, `GET /v1/videos/{video_id}/content`, and
`DELETE /v1/videos/{video_id}` expose the durable record, which the management
`/api/v3/media-jobs` reads mirror for operators. The console provides
metadata-only list/detail views, filters, and manual refresh. It has no
automatic polling, content download/delete controls, video cancellation
workflow, or media playground controls. Content and deletion remain
API-key-owned inference operations; request cancellation and backend job
reconciliation are separate from these console controls.

## Limits and budgets

Enforcement needs `OLP_VALKEY_URL`. Every counter lives in Valkey and is
evaluated by a Lua script on the server's clock, so replicas share one decision
rather than each keeping its own. Without it there is no admission backend at
all: a key without individual or group limits can be served; a limited key is
refused with `503 distributed_limits_unavailable`, and a target with a
configured quota is skipped rather than used unmetered.

A request is admitted once it is authenticated, parsed, and routed, and before
any provider is called. The gateway estimates the tokens the request may use —
text at four characters per token across messages, tool calls, and tool schemas,
a flat charge per inline image or media part, plus the largest reply the caller
allowed (`max_completion_tokens`, `max_tokens`, or `max_output_tokens`, 4096 by
default, times `n`) — for every attempt the request permits, and reserves the
key's requests-per-minute, tokens-per-minute, and concurrency windows and its
cost budgets together. The lease is sized by the route's overall deadline; it is
the backstop for a replica that dies mid-request, not the request deadline. An
estimate larger than the key's tokens-per-minute limit is refused immediately
with `400 request_exceeds_token_limit` rather than sent to retry into a window
it can never fit.

Each attempt then reserves the provider connection quota and the credential slot
quota, with concurrency leases covering the remaining overall route deadline. An
attempt's first-byte or idle timeout does not bound a stream's total lifetime. A
rejected slot refunds the connection reservation it already took, the attempt is
recorded as a rate-limit failure with its `Retry-After`, and failover continues
to the next target. When a request ends, a reservation that dispatched nothing
is refunded in full; otherwise the token reservation is reconciled against the
usage the upstream reported and the concurrency lease is released. Settlement
ignores client cancellation, so a caller that hangs up still returns its slot.

Credential failures record a version-scoped cooldown in Valkey; rate limits
record a logical-slot cooldown that survives rotation. Shared cooldown state is
authoritative when available, and successful credential validation clears it.
Without shared coordination, the gateway uses its local cooldowns. An unreadable
shared cooldown is treated as absent.

Cost budgets compare attributed spend with daily/monthly thresholds, using UTC
windows and exact decimals. A request must satisfy both its key and any assigned
budget group. Concurrent accepted work can exceed a threshold; unpriced attempts
accrue zero. Exhaustion returns `429 budget_exhausted`. Missing, malformed, or
wrong-window snapshots return `503 distributed_limits_unavailable` until
[authoritative initialization](spend-budget-recovery.md) completes.

When Valkey is configured but unreachable, `limits.valkey_unavailable` controls
rate/concurrency-only keys: `fail_closed` returns
`503 distributed_limits_unavailable`; `fail_open` bypasses those dimensions,
logs a warning, and increments `olp_limits_fail_open_total`. Key or group cost
budgets always fail closed. Gateways load the setting before binding and poll
every 15 seconds, retaining the last value on read failure. Unconfigured Valkey
never fails open. Unreadable provider quotas skip the target; exhausting all
targets for that reason returns `503 distributed_limits_unavailable`.

## Diagnostics

Every request ends in exactly one terminal envelope logged as
`inference request`: request ID, actor (key or playground user), route and
revision, release sequence, family, mode, outcome, status, whether the response
was committed, timing, observed usage when the upstream reported it, and one
record per attempt with target, provider revision, slot, credential version,
status class, and timing. Prompts, outputs, tool data, headers, and credentials
are never included. `GET /api/v3/provider-health` summarizes the same facts per
provider for the requested window, `GET /api/v3/health/ready` serves the cached
readiness snapshot, and `GET /api/v3/media-jobs` lists and reads durable video
jobs. The same envelope is the source of the durable request, attempt, and
priced usage records described in
[accounting delivery](operations.md#accounting-delivery-and-shutdown).

Readiness, worker checkpoints, metadata delivery, tracing, and shutdown are
documented in the [operations runbook](operations.md). Keep health and metrics
on the private listener. `olp health-probe` exits nonzero unless readiness
passes.

See [tests/README.md](../tests/README.md) for deterministic protocol, SDK,
service, and browser checks. Paid provider qualification is separate.

## Stored response accounting

Retrieving, cancelling, deleting, or listing input items for a stored response
does not generate new usage. Background responses retain a content-free
accounting record with the original request identity, attribution, and pricing.
A terminal stream event, retrieval, or cancellation carrying final usage settles
that record once, including when several replicas poll concurrently. Until final
usage is observed, the record remains pending; clients must poll responses that
finish after their creation connection closes.

Stored mappings created before this accounting record was introduced cannot
safely recover the original generation identity. Retrieval does not rebill those
mappings.
