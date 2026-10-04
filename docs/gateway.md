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

Every provider or route activation compiles the complete set of active providers
and latest route revisions into one snapshot, validates it, records its SHA-256,
and stores it as a numbered runtime release. Gateways install the newest release
only when it validates and every referenced credential can be decrypted; a
release that fails either check is skipped and the previous one stays active.
Repeated publication is harmless.

Every route revision and runtime snapshot carries an explicit
[fidelity](provider-routing.md#route-fidelity): strict or transformed. Strict
drafts reject redaction, and strict publication compiles each target's admitted
interaction contract during draft validation, activation and release
installation; a target without an admitted contract fails closed. Transformed
routes translate between dialects, apply redaction and use Automatic providers.
A published slug switches between strict and transformed through a new
revision. A stored resource is served only under the fidelity it was created
with, and its owner can always list, delete or cancel it; see
[route fidelity](provider-routing.md#route-fidelity).

Key authority (API keys, expiry, revocation, and credential versions that are
revoked or whose [grant lapsed](plugins.md#lapsed-grants)) is polled every five
seconds independently of release installation. Authority older
than 60 seconds, measured from the start of the last successful read with a
monotonic clock, is stale: new requests are rejected with
`503 authority_unavailable` while requests
already admitted keep the snapshot and policy they were pinned to. Ordinary
streams may finish; realtime sessions recheck key authority every five seconds.
Credential-version revocation and grant lapse apply to retained releases too:
selection refuses such a version even when the request already pins it, and
skipping its slot spends no attempt. Records of a refused credential version
name why, `revoked`, `lapsed` or `stale_authority`: `credential_<reason>` in
the plan decisions of skipped credential slots (and of a target whose every slot
was skipped for that reason), `network_credential_<reason>` for a network
credential, `provider_credential_<reason>` when a realtime session ends, and
`media_job_credential_<reason>` on media jobs.
See [authority and replica tests](../tests/integration/replica_fleet_test.go).

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
is a safe token; the Anthropic surface sends the same value as `request-id`,
which the Anthropic SDKs read), a no-store cache policy, and CORS headers for explicitly
allowed browser origins (`OLP_GATEWAY_CORS_ALLOWED_ORIGINS`). Responses also carry
the key's [rate-limit headers](#response-headers) and, for a key that opted in,
gateway metadata. JSON endpoint
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
upstream server failures. Generation, token-counting, embedding, rerank, and
moderation attempts also fail over on a typed context-window rejection, an
upstream error code or type of `context_length_exceeded`,
`context_window_exceeded`, `max_context_length_exceeded`, or `prompt_too_long`,
because a target with a larger context window may serve the request; message
text never triggers it. Other upstream client errors, protocol errors, and
cancellations are terminal. A request that ends on an upstream rejection
returns `upstream_rejected` with the redacted upstream message, keeping an
upstream 400, 404, 405, 409, 413, 415, or 422 status and otherwise answering
502. A committed stream never restarts on another provider: a later failure
is reported in-band as an error event and the stream ends without a success
marker. Retained resources, realtime, and native Bedrock ingress pin one
provider/credential without cross-target failover; see
[compatibility](compatibility.md). Translation and native tool validation bound
retained text/tool state by the event-size limit. Distinct native Chat choice
tracking is bounded to `max(1, OLP_PROVIDER_MAX_EVENT_BYTES / 16)` entries.
Bedrock advertised event lengths are checked before SDK allocation, and the SDK
still verifies event CRCs.

Provider health is tracked per gateway: five counted failures within 30 seconds
open a provider's circuit for 30 seconds. Connection, timeout, protocol and
upstream server failures count; so do ambiguous ones of a strict interaction
or of traffic a plugin carries. One half-open probe may proceed;
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
| 429 | `rate_limit_exceeded`, `budget_exhausted`, `upstream_rate_limit` | The key's requests, tokens, or concurrency limit was exceeded; the key's daily or monthly cost budget is exhausted or cannot hold the request's [estimated cost](#cost-reservation); or every attempt was rate limited upstream. `Retry-After` carries whole seconds, and a limit of a key that has a request or token limit adds that key's [rate-limit headers](#rate-limit-headers). |
| 502 | `upstream_unavailable`, `upstream_rejected`, `upstream_authentication_failed`, `upstream_permission_denied`, `provider_protocol_error`, `upstream_response_too_large` | Upstream or transport failures after the budget is spent, or a stream from an upstream that serves only streams whose aggregated non-streaming result exceeds the response size limit. |
| 503 | `authority_unavailable`, `request_admission_overloaded`, `distributed_limits_unavailable`, `upstream_unavailable` | Stale authority, admission limit, limits that cannot be enforced, or no eligible target. |
| 504 | `gateway_timeout` | Route deadline reached before commitment. |

## Response headers

Beside `X-Request-Id`, a response can carry two more sets of headers: the
caller key's remaining allowance, which every key with a request or token limit
receives, and metadata about how the gateway served the request, which only a key
that opts in receives. Both are written before the response is committed, from
what admission and the attempt loop already hold, so they cost no Valkey round
trip, and a request that needs neither allocates nothing for them. The allowance
comes with the answer of the reservation that admits the key: the rate script
states it only to a key with a request or token limit, so a key bound by
concurrency alone, and a provider's connection or credential quota, which is
reserved on every attempt, are answered with the decision and no more.

### Rate-limit headers

Each surface gets the family its own SDKs already read.

| Surface | Requests | Tokens |
| --- | --- | --- |
| OpenAI (`/v1/...`) | `x-ratelimit-limit-requests`, `x-ratelimit-remaining-requests`, `x-ratelimit-reset-requests` | `x-ratelimit-limit-tokens`, `x-ratelimit-remaining-tokens`, `x-ratelimit-reset-tokens` |
| Anthropic (`/anthropic/...`) | `anthropic-ratelimit-requests-limit`, `-remaining`, `-reset` | `anthropic-ratelimit-tokens-limit`, `-remaining`, `-reset` |

Gemini, Bedrock and native-operation endpoints send none: their clients read no
such headers.

- **Only what the key limits.** A key limited to requests per minute gets the
  request headers alone, and one limited to tokens the token headers alone. A key
  with neither limit gets none, including a key bound only by concurrency or by a
  cost budget. So does a request admitted without a reservation, which is one that
  failed open while Valkey was unavailable.
- **The key's own window.** The values are what the admission reservation
  measured in the fixed UTC minute the request was counted in. `limit` is the
  per-minute limit and `remaining` what the limit leaves once this request's
  reservation is counted, never below zero. The token reservation is the
  admission estimate, a prompt plus the largest reply for every attempt the request
  may dispatch, and not usage: settlement replaces it with the tokens the provider
  reported, so the next response can show more remaining than this one implied.
  The counts are not read again when the response is written, so a stream that
  commits after a long wait states the window as it was admitted.
- **Reset.** Both dimensions reset when the minute ends. OpenAI's headers give the
  time left in the notation of a Go duration, such as `20ms`, `1s`, `8.64s` or
  `1m0s`, counted down from the reservation to the moment the headers are written
  and stated in whole milliseconds, rounded up.
  Anthropic's give the instant the minute ends as an RFC 3339 time in UTC, such as
  `2026-10-02T09:31:00Z`. The minute is Valkey's, so every replica and client
  agrees on it whatever its own clock says.
- **Rejections.** A request refused by the key's requests, tokens or concurrency
  limit gets the same headers beside `Retry-After`, describing the window that
  refused it. The refused request reserved nothing, so `remaining` is what the
  window still holds. A concurrency refusal's `Retry-After` is the short wait for
  a slot, while the reset is still the end of the minute. A refusal by a cost
  budget, a provider's connection or credential quota, an upstream's rate limit or
  admission overload states no allowance, because none of them is the key's
  requests or tokens.

### Gateway metadata

The key policy `response_metadata` (off by default; see
[access](access.md#key-response-metadata)) adds these headers to its successful
responses.

| Header | Value |
| --- | --- |
| `X-OLP-Attempts` | The attempts the request had made when the response was committed, counting one a connection or credential quota refused locally. `2` after a failover. |
| `X-OLP-Route-Revision` | The `revision_id` of the route revision that served the request, the identifier `GET /api/v1/routes/{route_id}/revisions/{revision_id}` takes. A call on a retained resource reports the route's current revision, not the one that created the resource, and a video job read, download, delete or list, which is answered from the jobs' own records, reports none. |
| `X-OLP-Provider` | The vendor of the provider that served the request, as its configuration names it. A provider with no vendor, as a plugin provider has none, is not named. Callers address routes, not upstreams, so this is the one place the gateway itself names an upstream, and only for a key that opts in. The message of an upstream rejection is relayed as the upstream wrote it, with credential values redacted, and can name its vendor or model whatever the key's policy. |
| `X-OLP-Cost` | The cost of a unary response in the installation currency, as a plain decimal such as `0.000064`. |

A stream records its serving attempt before its first frame is committed, so
`X-OLP-Attempts` and `X-OLP-Provider` describe the attempt that is streaming, and a
failover before the first byte is counted. A stream cannot carry a cost, since its
usage is not known until it ends; the request history API has it afterwards.

A call on one video job is its one attempt. A video list refreshes every job that
is still queued or running with a poll of its provider, and those polls are its
attempts: `X-OLP-Attempts` counts them all, and `X-OLP-Provider` names a provider
only when every poll went to the same one. A list with no job to poll made no
attempt and carries no metadata.

`X-OLP-Cost` is the cost accounting will record for the request, priced from the
gateway's pinned price list and the usage the provider reported. It is left out
when the request would be recorded as unpriced: an attempt that was billed or may
have been billed has no price or no rate for something it used, or a provider
answered successfully and reported no usage. A price of zero is a cost of `0`.
Failed attempts that reported nothing add nothing.

### Which responses carry them

Every success response of an inference endpoint is covered: Chat Completions,
Responses, Messages, Gemini generation, embeddings, rerank, moderation, token
counting, the media endpoints, Bedrock, the native operations, and calls on
retained provider resources (files, batches, stored responses, interactions and
video jobs), whose one pinned attempt is counted, and a video list, whose polls
are. Error responses carry neither set, except the rate-limit headers of a limit
rejection, and a response that made no attempt, such as a file list answered from
the gateway's own records, has no metadata to give. These do not carry either
set:

- WebSocket upgrades (Realtime and Gemini Live). The provider is dialled only
  after the session is accepted, so there is no attempt to describe, and a
  WebSocket client does not read an HTTP API's rate-limit headers.
- A replayed or recovered continuation delivery, which is answered from stored
  state before admission and makes no attempt.
- Model listings, which are served without admission, and the console playground,
  which answers a signed-in member and not a key.

For browser clients, the headers above, `Retry-After`, `X-Should-Retry` and
`X-OLP-Delivery-Replay` are listed in `Access-Control-Expose-Headers` for the
origins in `OLP_GATEWAY_CORS_ALLOWED_ORIGINS`.

## Content policy

A route draft may carry an optional `content_policy`: an ordered list of local
[RE2](https://github.com/google/re2/wiki/Syntax) rules validated and compiled
inside the gateway — no external service, model, or timeout is involved. Each
rule names a phase (`input` or `output`), an action (`block` or `redact`), a
pattern, and an optional literal replacement (redact only; `$`-sequences are
never expanded, so a replacement of `$1` inserts the text `$1`). Validation
bounds the policy at 64 rules, 512 bytes per pattern, 16 KiB of combined pattern
bytes, and rejects patterns that match the empty string. `redact` rules require
a transformed route; strict routes accept only `block` rules. Policies publish
with the route revision, restore with it, and travel through configuration
export and import.

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
`/v1/images/variations`, `/v1/audio/speech`, `/v1/audio/transcriptions`,
`/v1/audio/translations`, and the
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
`/api/v1/media-jobs` reads mirror for operators. The console provides
metadata-only list/detail views, filters, and manual refresh. It has no
automatic polling, content download/delete controls, video cancellation
workflow, or video playground controls. The Advanced playground provides a
separate public audio translation upload form. Content and deletion remain
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
its prompt, plus the largest reply the caller allowed (`max_completion_tokens`,
`max_tokens`, or `max_output_tokens`, 4096 by default, times `n`) — for every
attempt the request permits, and reserves the key's requests-per-minute,
tokens-per-minute, and concurrency windows and its cost budgets together. The
lease is sized by the route's overall deadline; it is the backstop for a replica
that dies mid-request, not the request deadline. An estimate larger than the
key's tokens-per-minute limit is refused immediately with
`400 request_exceeds_token_limit` rather than sent to retry into a window it can
never fit. The windows each response leaves the key with are reported in its
[rate-limit headers](#rate-limit-headers).

### How the prompt is estimated

The request is walked once into its text and its media parts: message content,
tool calls and their results, the names and arguments a call travels with, every
tool schema, the schema a structured output must follow (a chat
`response_format`, a Responses `text.format`, Anthropic's
`output_config.format`, Gemini's response schema, Bedrock's `outputConfig`), the
tool catalogue of a Bedrock request, a Responses reasoning summary and Anthropic
thinking text, in any of the client dialects. Each inline image costs a flat
1,000 tokens and each audio, file or document part 2,000, whatever the bytes the
part carries; a document given as text is counted by its text. Some of a prompt
is not read as text: encrypted reasoning (a Responses `encrypted_content`,
Anthropic `redacted_thinking`) and, in the Anthropic and Gemini dialects, the
arguments of a tool call and the response of a function response, which are
counted only where they hold text under a key the walker knows (`text`,
`content`, `parts`, `input`, `output`). Those leave a prompt under-counted,
which the reservation's reconciliation against the reported usage corrects after
the attempt, and which the estimate says by being `calibrated`. The same walk
serves every target. It keeps the first 36 KiB of the text and only the size of
the rest, which is all a count reads, so a request of megabytes is not held
twice while its upstream answers. What the text costs depends on the model that
will read it, and the gateway counts it for the upstream model of each target,
once for each family, however many attempts, credential slots or translated
targets use that family. A route that fails over from an OpenAI model to a
Claude model counts the prompt twice, once for each family, and a request in the
Anthropic dialect sent to an OpenAI target is counted by OpenAI's tokenizer,
because the family follows the target model and not the client's dialect.

| Family | Models | Counted by | Provenance |
| --- | --- | --- | --- |
| `openai-o200k` | GPT-4o, GPT-4.1, GPT-5, o-series, gpt-oss | OpenAI's `o200k_base` byte-pair encoding | `tokenizer` |
| `openai-cl100k` | GPT-4, GPT-3.5 Turbo, `text-embedding-3` | OpenAI's `cl100k_base` encoding | `tokenizer` |
| `anthropic` | Claude, including Bedrock and Vertex names | four characters per token | `heuristic` |
| `gemini` | Gemini | four characters per token | `heuristic` |
| `other` | everything the registry does not recognize | four characters per token | `heuristic` |

The OpenAI encodings are in the binary, and their counts match OpenAI's own
`tiktoken` token for token on the text of the checked-in fixtures, which are
generated by `tiktoken` itself. The encoder reads the letter, number and space
classes of its split patterns from Go's Unicode tables, which are newer than the
ones the engine behind `tiktoken` was built with, so a character assigned since
(the newest CJK ideographs, for one) can split a piece differently and cost a
token more or less; a Go upgrade moves the tables with it. A model name decides
the family by OpenAI's own rules (an exact name or a prefix, with a provider
path, a `ft:` prefix or a `:` variant removed); a name that does not say, such
as an Azure deployment name, is never assumed to be an OpenAI model, because a
wrong tokenizer miscounts without saying so. OpenAI chat models also read each
message's role, three tokens of framing around each message, one more for a
name, and three that prime the reply; these are the figures of the OpenAI
Cookbook, and an OpenAI count includes them. A request in another dialect is
framed by what that dialect calls a message: each entry of the Anthropic,
Bedrock or Gemini conversation, and each system prompt, and the instructions of
a Responses request. The other families charge text only, four characters per
token with every field rounded up, and that charge is scaled by a per-family
factor that is 1 until the [reference catalog](roadmap/m02-provider-catalog.md)
carries measured ones.

Each estimate carries its provenance. `tokenizer` is an exact count of the text
and of the message framing OpenAI documents, for a prompt that is nothing else:
a prompt with any member the count cannot read as the model does, or leaves out,
is never `tokenizer`. `calibrated` is a count that is partly a ratio or a guess.
A prompt past 32 KiB of text is counted exactly up to that point and the rest is
charged at the tokens per byte the exact part measured, which keeps a
100,000-token prompt from costing milliseconds of CPU on every request; the
ratio is within a quarter of a percent for a prompt of one kind of text, and can
be tens of percent off when the first 32 KiB is unlike the rest, such as a short
instruction ahead of a long document in another script. A count that charged an
image, document or media part at a flat rate is calibrated, because the flat
rate is a guess next to an exact count of the text, and so is the count of a
request with a tool catalogue, tool calls, a structured-output schema or
reasoning, which a model reads in a rendering of its own that no provider
documents, or with encrypted content that no count reads. `heuristic` is the
four-characters rule. It is also the count of a long prompt whose tail came
after less than 4 KiB of exact text, too little to measure a ratio from, and is
charged at four bytes to a token. Requests that reach an upstream through a
native operation contract (the embeddings, rerank, classification and
token-counting endpoints of a strict route), and Bedrock invoke, Gemini
interaction creation and JSON media requests (speech and image generation), are
estimated from the size of their documents, four bytes to a token, and are
always `heuristic`. Stored-response lifecycle calls, realtime sessions, job
polls, multipart media uploads (image edits and variations, transcription and
translation) and video creation read no prompt, or reserve a flat charge, and
record no estimate; their attempts still record the family of their model.

Planning uses the same counts. A target whose context window cannot hold the
estimate is excluded before any provider is called, and each target is weighed
by the count of its own model: a prompt that is a hundred tokens to the
four-characters rule may be three hundred to an OpenAI tokenizer, and the target
that serves it is chosen by the second figure. Where a target is sent a request
of its own, because a provider profile, a strict contract or a content policy
rewrote it, the estimate is the larger of the caller's request and that one, each
counted for the target's family, and the input the attempt records is the larger
of the two with the less trustworthy provenance of the two counts. A request that
reads the same as the caller's is not counted again.

Every attempt records the estimate it was admitted under: the input alone, not
the reply the reservation also holds, its provenance, and the model family. The
[usage reports](operations.md#accounting-delivery-and-shutdown) compare the estimate with the
input the provider reported, by route and by model family, and
[route simulation](provider-routing.md#explain-and-observe) shows it for each
target.

Each attempt then reserves the provider connection quota and the credential slot
quota, with concurrency leases covering the remaining overall route deadline. An
attempt's first-byte or idle timeout does not bound a stream's total lifetime. A
rejected slot refunds the connection reservation it already took, the attempt is
recorded as a rate-limit failure with its `Retry-After`, and failover continues
to the next target. When a request ends, a reservation that dispatched nothing
is refunded in full; otherwise the token reservation is a conservative floor.
Reported usage can increase it but cannot refund it, because upstream metering
is untrusted. This also means legitimate completions shorter than the reserved
output allowance retain that allowance for the current minute. The concurrency
lease is released, and the cost
reservation becomes the cost incurred. Settlement ignores client cancellation, so
a caller that hangs up still returns its slot.

Credential failures record a version-scoped cooldown in Valkey; rate limits
record a logical-slot cooldown that survives rotation. Shared cooldown state is
authoritative when available, and successful credential validation clears it.
Without shared coordination, the gateway uses its local cooldowns. An unreadable
shared cooldown is treated as absent.

Cost budgets compare attributed spend with daily/monthly thresholds, using UTC
windows and exact decimals. A request must satisfy both its key and any assigned
budget group. Exhaustion returns `429 budget_exhausted`; so does a budget with room
left that cannot hold the request's estimate beside what is spent and in flight,
and its message says which it was. Missing, malformed, or
wrong-window snapshots return `503 distributed_limits_unavailable` until
[authoritative initialization](operations.md#spend-budget-reconciliation) completes.

That wait is by design: a budget is enforced against the spend PostgreSQL has
confirmed, and the gateway never invents a zero for a key whose spend it does not
know. Only the worker plane's reconciliation pass, which runs every minute, and the
accounting of a request that has finished, install the snapshot of a window. So a
key created with a cost budget, a budget added to a key that had none, and a
budgeted key whose UTC day or month has just rolled over, refuse requests with that
503 until the next pass, which is up to a minute and which the gateway cannot
shorten. A client sees an ordinary retryable 503. A provisioning script or a
benchmark that creates a key and sends traffic at once waits for the first pass,
which the worker logs as `reconciled cost budgets`. A deployment without a running
worker never installs one.

### Cost reservation

A budget is measured against the spend already accrued and also against what
requests in flight may still spend, so a burst cannot all be admitted against
the same unspent balance. A priced request reserves an estimate of its cost in the
same Valkey call that checks the balance, and is admitted only if the accrued
spend, plus what other requests hold, plus its own estimate, fits every window
the key and its group have. A request that exactly fills a window is admitted.
A rejection reserves nothing. It keeps the code `budget_exhausted` in both
cases, but only a budget whose accrued spend has reached its limit is called
exhausted. When the budget has room and the request's estimate does not fit
beside the spend and the requests in flight, the message says so and names what
helps (a lower `max_tokens`, waiting for requests in flight, or a larger
budget); `Retry-After` is one second when waiting can admit the request, and the
end of the window when nothing short of a larger budget can.

The estimate is the most one request could cost across the attempts it may
dispatch, priced from the gateway's pinned price list, the revision accounting
pins the request to. Input is charged at the highest input-side rate the price
has, whether input, cached input or any cache write, because nothing says
beforehand whether the provider will read or write its cache. The reply is
charged at the output rate for the tokens the request allows, which is
4,096 when it names no bound, times the candidates it asks for. Each division by a
million rounds up. Settlement bills every dispatch that reports usage, so when a
route can fail over or retry through another credential slot the estimate is the
sum of the dispatches the request may make, not just the dearest single attempt.

A request that ends replaces its estimate with the cost of the attempts that
reported usage, priced exactly as accounting will price them; one that was never
dispatched, or whose upstream reported nothing, releases it. When accounting
records the request, it installs the spend and removes the reservation in one
step, so the budget never counts a request twice, and delivering the same event
again removes nothing more. Settlement ignores client cancellation, as the other
reservations do. Replacing the estimate with a cost is a single attempt that
gives up after 100 milliseconds, because losing it only leaves the estimate for
accounting or the lapse to remove; giving an estimate back, for a request that
was never dispatched or whose cost is nothing, is retried like every other
release, since no spend is coming to remove it. A reservation that nothing
removes, because its event was lost or accounting stalled, lapses at the route
deadline plus five minutes, after which the budget counts accrued spend alone
again. Lapsed reservations are retired as later reservations and settlements
find them, a bounded page at a time, so that a backlog left by a long stall
cannot hold Valkey while it is cleared: until a backlog has been worked through,
what is left of it still counts, which can hold a budget back for a short while
and never lets it overspend.

Reservations are advisory and derived. The accrued balance is the authority and
fails closed; a damaged reservation, including one whose two keys no longer
agree because only one of them was deleted or evicted, is discarded rather than
allowed to block accounting, and deleting the reservation keys loses only the
protection for requests in flight. They live beside the balances, in `cost:pending` and
`cost:expiry` ([Valkey keys](operations.md#shared-state-in-valkey)).

What is not reserved, and is judged on accrued spend alone: an attempt whose
model has no price, or a price without a rate the request needs; any request
while the gateway's price list is more than a minute old; and media, audio and
video requests, realtime sessions, Gemini Live and Interactions, Bedrock invoke
and stored-response lifecycle calls, whose cost is not known before they run. A
background response is reserved while its creating request runs and released when
it returns; its spend counts when its final usage arrives. The reservation is
therefore not an invoice cap. Overspend remains possible from token counts that
are heuristic for a family without a public tokenizer, failover that bills more
than once, accounting that lags beyond the lapse above, unpriced spend, and every
request that reserves nothing.

Because reserved cost counts, a key can be refused while its spend reads below its
limit, and a small budget on an expensive model can be refused at zero spend
when the request names no `max_tokens`: the default reply alone may cost more than
the budget. A refusal for in-flight reservations carries `Retry-After: 1`; one
for a request that could not fit even alone carries the time until the window
ends, though no wait will admit it, so bound the reply or raise the budget.

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
are never included. `GET /api/v1/provider-health` summarizes the same facts per
provider for the requested window, `GET /api/v1/health/ready` serves the cached
readiness snapshot, and `GET /api/v1/media-jobs` lists and reads durable video
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

### Unbounded work and incomplete accounting

Batch submissions and Gemini Live sessions are rejected with `unbounded_work_budget`
when the key or its budget group has a token or cost budget, or the selected
provider or credential slot has a token budget. Their future consumption cannot
be bounded at admission. Request-rate and concurrency controls remain supported.
Live sessions recheck key budgets during their normal authority refresh.

Cost-budget admission checks PostgreSQL for ingestion losses overlapping the
current UTC month and returns `cost_accounting_incomplete` while any remain.
Because a lost event cannot identify its owner reliably, the guard conservatively
applies to every cost-budgeted key and group. Unbudgeted traffic remains available.
This adds one database lookup per cost-budgeted admission and fails closed when
that lookup is unavailable. The local emitter blocks admission immediately after
a drop; a bounded synchronous gap write shares the loss with other replicas, with
normal epoch checkpoints as recovery if that write fails. An in-flight request or
a replica racing that gap write can still finish; a failed write followed by a
process crash relies on unclean-epoch detection. This guard does not reconstruct
lost usage. It expires naturally when the budget month no longer overlaps a gap.
