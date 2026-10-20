# Provider compatibility

OpenLLMProxy accepts OpenAI, Anthropic, Gemini, and Bedrock client protocols. A
route can select a provider using the same protocol or, when the route is
[transformed](provider-routing.md#route-fidelity), translate to another
supported protocol. The tables below show supported combinations; translation
limits follow. See [concepts](concepts.md) for routes and certification.

Reviewed vendor contracts narrow the general connector matrix to the operations
each vendor documents. See [provider routing](provider-routing.md#reviewed-vendor-contracts)
for how contracts shape requests, and the [preset table](configuration.md#openai-compatible-provider-presets)
for every reviewed vendor. Custom native endpoints require live certification; they do
not inherit the official OpenAI media discovery contract.

The [gateway](gateway.md) applies shared admission and response bounds to these
surfaces. Current protocol, connector, SDK, and media tests are described in
[tests/README.md](../tests/README.md).

## Guardrail compatibility

[Guardrails](guardrails.md) inspect effective provider input and qualified unary
output, including tool arguments and results. Enforcing streaming policies use
a byte-bounded holdback; rules needing a complete result return
`422 guardrail_surface_unavailable`. Strict routes accept block, annotation and
monitoring. Redaction and request-local masking require transformed routes.
Opaque media and retained surfaces without a complete inspection contract refuse
enforcement; monitoring records the unavailable surface and continues.

## Legend

| Cell | Meaning |
| --- | --- |
| `native` | The provider speaks this surface's protocol. Protocol-specific fields are preserved subject to gateway validation and model rewriting. |
| `translated` | The request is decoded into the canonical model and re-encoded for the provider; the route must be transformed. Some fields are dropped and some are refused — see the notes below. |
| `—` | Refused. The tuple can never be certified, so it can never be activated on a route, and the gateway rejects the request. |
| `gateway` | Answered from visible published routes; model listing is not proxied upstream. |
| `qualified` | Restricted to the provider, model family, or resource policy described below; exact capabilities still require certification. |
| `reviewed` | Available only through the named compatible-vendor profiles and certified models. |

Reviewed vendors serve image, speech and transcription operations on the
OpenAI surface through their media codecs; see [vendor media](providers/media.md).

Native generation dialects, such as Mistral fill-in-the-middle and Cohere
Chat v2, serve their own clients at `/native/{dialect}/models/{route}` and are
never translated; see [native generation dialects](provider-profiles.md#native-generation-dialects).

The `sagemaker` kind serves only generation on the OpenAI surface, as the
`openai_compatible` column shows; see [SageMaker AI](providers/sagemaker.md).
The `watsonx` kind serves only generation, translated on every surface but
Bedrock; see [IBM watsonx.ai](providers/watsonx.md).

`native` means the upstream speaks the incoming wire protocol. Bedrock Converse
is native on the Bedrock surface and translated on OpenAI, Anthropic, and Gemini
surfaces. Native traffic still undergoes gateway validation and rewriting.

Cells combine transport modes. Generation supports unary and streaming; token
counting is unary, video creation is asynchronous, and realtime uses WebSockets.
`—` means no mode of that operation can be certified for the provider.

Endpoint registration lives in
[`internal/gateway/server.go`](../internal/gateway/server.go); certification
policy lives in [`internal/providers/kinds.go`](../internal/providers/kinds.go).
The conformance corpus and SDK suites exercise these behaviors.

## Surfaces and operations

### OpenAI surface

Use a base URL ending in `/v1` for native OpenAI SDK requests.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /v1/completions` | generation | qualified | — | — | — | — | translated | reviewed |
| `POST /v1/decisions` | decision | qualified | — | — | — | — | — | qualified |
| `POST /v1/systemone` | decision | — | — | — | — | — | — | qualified |
| `POST /v1/chat/completions` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses/input_tokens` | token_count | native | translated | translated | translated | translated | native | native |
| `POST /v1/embeddings` | embeddings | native | — | translated | translated | translated | native | native |
| `POST /v1/rerank` | rerank | — | — | — | — | qualified | — | reviewed |
| `POST /v1/moderations` | moderation | native | — | — | — | — | native | native |
| `POST /v1/files` | file | qualified | — | — | — | — | qualified | — |
| `GET /v1/files` | file | qualified | — | — | — | — | qualified | — |
| `GET /v1/files/{id}` | file | qualified | — | — | — | — | qualified | — |
| `DELETE /v1/files/{id}` | file | qualified | — | — | — | — | qualified | — |
| `GET /v1/files/{id}/content` | file | qualified | — | — | — | — | qualified | — |
| `POST /v1/batches` | batch | qualified | — | — | — | — | qualified | — |
| `GET /v1/batches` | batch | qualified | — | — | — | — | qualified | — |
| `GET /v1/batches/{id}` | batch | qualified | — | — | — | — | qualified | — |
| `POST /v1/batches/{id}/cancel` | batch | qualified | — | — | — | — | qualified | — |
| `GET /v1/responses/{id}` | generation | qualified | — | — | — | — | qualified | — |
| `DELETE /v1/responses/{id}` | generation | qualified | — | — | — | — | qualified | — |
| `POST /v1/responses/{id}/cancel` | generation | qualified | — | — | — | — | qualified | — |
| `GET /v1/responses/{id}/input_items` | generation | qualified | — | — | — | — | qualified | — |
| `GET /v1/realtime` | realtime | qualified | — | — | — | — | qualified | — |
| `POST /v1/images/generations` | image_generation | native | — | — | qualified | qualified | qualified | — |
| `POST /v1/images/edits` | image_edit | native | — | — | — | — | — | — |
| `POST /v1/images/variations` | image_variation | native | — | — | — | — | — | — |
| `POST /v1/audio/speech` | speech | native | — | — | — | — | qualified | — |
| `POST /v1/audio/transcriptions` | transcription | native | — | — | — | — | qualified | — |
| `POST /v1/audio/translations` | translation | native | — | — | — | — | — | — |
| `POST /v1/videos` | video_create | native | — | — | — | — | — | — |
| `GET /v1/videos` | video_list | native | — | — | — | — | — | — |
| `GET /v1/videos/{video_id}` | video_get | native | — | — | — | — | — | — |
| `DELETE /v1/videos/{video_id}` | video_delete | native | — | — | — | — | — | — |
| `GET /v1/videos/{video_id}/content` | video_content | native | — | — | — | — | — | — |
| `GET /v1/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /v1/models/{id}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

`rerank` is certified only for the reviewed OpenAI-compatible vendors Cohere,
Voyage, Jina, Together and Infinity, and for Bedrock's rerank models; other
vendors are refused. Echoed documents must be the request's own, as text or
as an object holding it. Vertex `image_generation` qualifies
`imagen-*` and Gemini image models, Gemini and Vertex serve speech and
transcription through Gemini models, and Bedrock qualifies `amazon.titan-image-generator-*` and Stability text-to-image models
and speech through Amazon Polly's engines; other models and every other
media operation of these kinds remain refused.
Native embeddings accept only the canonical input shapes described per provider
below.

### Legacy text completions and Decisions

`POST /v1/completions` uses `generation` authorization. Native text completions
require an explicit `openai-completions`, `vllm-completions`, `tgi-completions`,
`fireworks-completions`, or `together-completions` profile and a certified model.
Generic compatible hosting does not qualify the native endpoint. Unary and SSE
results retain the native text-completion grammar, finish reasons and usage.
The first native contract excludes `suffix`.

The `tgi-completions` profile accepts text prompts and unary text batches.
Streaming is limited to one text prompt because TGI emits per-prompt usage
instead of a request aggregate. Token-ID prompts and unsupported controls such
as `n`, `best_of`, `echo`, `logprobs`, `presence_penalty`, `logit_bias`, and `user`
are refused before dispatch. Its stream adapter accepts TGI's empty nonterminal
finish reason and still requires a terminal finish reason and `[DONE]`.

Transformed Chat Completions targets accept one text prompt as one user message.
Prompt arrays and token IDs, `echo`, `best_of`, `logprobs`, `suffix`, unknown
fields, and defaults without an equivalent mapping are refused before dispatch.
Every result candidate retains its index; tool calls and refusals without a
text-completion representation fail projection. These endpoints do not use the
response cache. Text prompts and choice text are
[guardrail](guardrails.md#execution) and
[payload-capture](payload-capture.md#capture-behavior-and-bounds) surfaces for
unary and SSE results. Unsupported inspection surfaces, such as token-ID prompts
or `logprobs` under an output policy, refuse enforcing guardrails.

Usage keeps cached and cache-write prompt token counts. On the text-completion
target, `total_tokens` must equal prompt plus completion tokens; an inconsistent
total is a protocol error rather than accounted usage. Admission estimates sum
a prompt batch under one exact-tokenization budget, and the context-window check
uses its largest prompt. When a connector's generation target is a native text
completion profile, [active health probes](provider-routing.md#credential-pools-and-limits)
send a short text completion instead of a chat request.

`decision` is a separate operation with native `openai-decisions` and `systemone`
profiles. The text-only contracts accept predicate/noul, choice and score
questions, preserving names, order, typed choice values, native probabilities,
score scales and OpenAI refusals. Images, box questions, streaming and unmodeled
fields are excluded. The OpenAI shape is pinned to `openai-go/v3` 3.78.0;
System One is pinned to TypeSafe OpenAPI 0.2.0. Exact tuple certification is
required before publishing either profile.

OpenAI Decisions can map to System One for a string input, choices whose typed
values do not collide as string keys, and score levels without separate
descriptions. Safety identifiers and other unmappable controls are refused.
System One requests cannot map to OpenAI: its frozen response schema has no
refusal variant. That direction returns `422 target_capability` before dispatch.
Native System One remains available. No chat-generated probabilities are used.
Native answers must carry string choice and score labels that match the
request; other answers fail validation.
Because these contracts have no provider-enforced output bound, token-capped
keys, budget groups, end users, routes, providers and slots, and USD-capped
keys, routes, providers, slots and budget hierarchies, are refused with
`unbounded_work_budget` before dispatch. A
[caller-paid](gateway.md#caller-paid-budget-policy) route waives only the USD
caps, and only for caller-credential targets; token caps still apply. Route
simulation and key inspection report the same `unbounded_work_budget` diagnostic,
including inherited hierarchy budgets and named-route limits. Observed usage is
still priced from the target's input/output token prices, including for the
`price` routing strategy, and accounted on unlimited calls.

Local evidence is the [M9 endpoint ledger](../tests/clients/m9-endpoints.json)
and the focused `CLIENTS=m9-go tests/clients/run.sh` suite. Live-provider
qualification remains required; this evidence does not qualify the rest of M9.

### OCR and Web search

`ocr` and `search` are separate unary operations. `POST /v1/ocr` binds its
route from the request body's `model`; `POST /v1/search` carries no model
field and binds its route from the required `X-OLP-Route` header — a missing
header is refused `400 missing_required_parameter` before dispatch. Both
operations also serve `/native/{dialect}/models/{route}` on strict routes, and
serve transformed routes through their reviewed mappings. Neither operation
uses the response cache.

The `mistral-ocr` profile (vendor Mistral) admits `model`, `document`,
`id`, `pages`, `include_image_base64`, `image_limit`, `image_min_size`,
`table_format`, `extract_header` and `extract_footer`. Documents are inline
`document_url` or `image_url` values over `https:` or `data:` only; file
documents, annotations, `confidence_scores_granularity`, `include_blocks` and
any other field are refused `400` before dispatch. Results require `model`,
`pages[].index`, `pages[].markdown` and `usage_info.pages_processed`, which
meters as media units. OCR follows the
[unbounded-work-budget](gateway.md) refusal rules of Decisions: token and
non-exempt cost caps refuse `422 unbounded_work_budget` before dispatch,
waived only for caller-paid cost-exempt routes.

The `perplexity-search` profile (vendor Perplexity) admits the frozen
ApiSearchRequest: a string `query` (arrays refused), `max_results` 1–20,
`search_type`, `max_tokens`, `max_tokens_per_page`, `country`,
`search_language_filter`, an all-allow-or-all-deny `search_domain_filter` of
at most 20 entries, and the date/recency filters. Transformed serving maps it
to `tavily-search`, `exa-search` and `parallel-search` (vendors Tavily, Exa,
Parallel; Parallel credentials arrive as `X-Api-Key`). The mappings always
send `query` and `max_results` (default 10); `search_type` is admitted only
absent or `web`; token, locale and freshness controls are refused `400`
rather than approximated. Domain filters reach Exa as root domains expanded
with `*.d` subdomains and Parallel verbatim (`.tld` and domain/path entries
allowed, `-` stripped on deny); Tavily carries none. Projections answer with
the Perplexity shape, a gateway-minted `search_<hex>` id — upstream ids never
cross — `""` for absent titles, joined highlight/excerpt snippets, and
per-target dates. A result page beyond `max_results` is a protocol violation.
Search results carry no usage and settle like moderation: a validated result
completes its evidence, and the attempt is charged at the target's
`per_request` price.

Local evidence is the [M9 endpoint ledger](../tests/clients/m9-endpoints.json),
the focused `CLIENTS=m9-go tests/clients/run.sh` suite and
`tests/gateway/ocr-search.spec.ts`. Live-provider qualification remains
required; this evidence does not qualify the rest of M9. Brave (GET-query
transport), Azure Document Intelligence (202/poll transport) and Vertex
document processing (GCP-authenticated profile) stay pending; Google's Custom
Search JSON API is excluded — the provider discontinued new enrollment and
the API sunsets on 2027-01-01.

### Files, batches, realtime, and provider-retained state

The file, batch, and realtime rows are qualified to official OpenAI and Azure
OpenAI targets only. `POST /v1/files` carries no model field, so it requires an
`X-OLP-Route` header naming a batch-enabled allowed route; every other endpoint
resolves the route through the resource mapping or the `model` field. Uploads
stream multipart bodies to the provider under the shared request and spool
bounds; file bytes are never stored in the gateway. Every file, batch, and
stored response gets a gateway-owned `file_`, `batch_`, or `resp_` identifier
backed by a metadata-only `provider_resources` row pinned to the selected
provider revision, slot, and credential. List, retrieve, content, delete, and
cancel calls resolve that mapping, dispatch to the pinned credential, and
rewrite identifiers between local and upstream forms; another key's identifier
is indistinguishable from a missing one. No cross-provider fallback applies: a
revoked pinned credential fails with `provider_resource_credential_unavailable`
rather than rerouting.

Stateful Responses fields (`store`, `background`, `previous_response_id`)
additionally require the key policy `allow_provider_state`, because provider
state may retain user content upstream. `previous_response_id` must reference a
response stored under the same key and route and is rewritten to the pinned
upstream identifier. Streaming terminal events are rewritten so callers only
ever see gateway-owned response IDs.

`GET /v1/realtime` is a WebSocket endpoint; WebRTC is not supported. The `model`
query parameter is an OLP route slug, the route must allow `realtime`, and the
target must hold a certified `openai`/`realtime` capability. Target and slot are
selected and reserved once before the upgrade, with no failover afterwards. The
relay enforces the configured maximum event size and a one-hour session bound,
propagates ping/close frames, and rechecks the presented API key every five
seconds, closing with a policy violation on revocation or expiry. Terminal usage
events feed accounting when the provider sends them; sessions that end without
usage are recorded as billing-uncertain.

### Anthropic surface

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /anthropic/v1/messages` | generation | translated | native | translated | translated | translated | translated | — |
| `POST /anthropic/v1/messages/count_tokens` | token_count | translated | native | translated | translated | translated | translated | — |
| `GET /anthropic/v1/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /anthropic/v1/models/{id}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

A Messages request may carry `system` messages between turns, as Claude Code
sends them. The fields of an Anthropic beta travel with the `Anthropic-Beta`
header, so an Anthropic upstream receives the caller's header when the route
hands it the caller's own request: on a strict route through the
`anthropic-messages` profile, and on a transformed route to a provider that
takes it, an Anthropic provider or one whose profile declares the header, as the
Vertex AI hosting of Claude does. A request translated from another dialect
carries none, and neither does Bedrock InvokeModel. A provider whose profile
publishes `Anthropic-Beta` among its semantic headers is sent its own betas and
the caller's, each once, since the caller's fields need theirs; a strict route
refuses a caller's list that differs from the published one with
`semantic_header_conflict`, because its profile binds one value. A header not a
valid header value, longer than 2048 bytes, or longer than that once joined with
a provider's published betas, is refused wherever it could be forwarded. The
Anthropic SDKs add `?beta=true` to the beta namespace's calls; a strict route
consumes it, since it selects no behavior, and refuses any other query setting
the profile does not declare. See
[Client compatibility](clients.md#claude-code).

### Gemini surface

Gemini endpoints are served under both `/gemini/v1` and `/gemini/v1beta`.
Replace `{version}` below with `v1` or `v1beta`.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /gemini/{version}/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /gemini/{version}/models/{model}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `POST /gemini/{version}/models/{model}:generateContent` | generation | translated | translated | native | native | translated | translated | — |
| `POST /gemini/{version}/models/{model}:streamGenerateContent` | generation | translated | translated | native | native | translated | translated | — |
| `POST /gemini/{version}/models/{model}:countTokens` | token_count | translated | translated | native | native | translated | translated | — |
| `POST /gemini/{version}/models/{model}:embedContent` | embeddings | — | — | qualified | — | — | — | — |
| `POST /gemini/{version}/models/{model}:batchEmbedContents` | embeddings | — | — | qualified | — | — | — | — |

The two Gemini embedding endpoints are served only on
[strict routes](provider-routing.md#route-fidelity) whose targets hold the
certified `embeddings` capability and use a Gemini API profile:
`gemini-generation` serves `embedContent`, and `gemini-batch-embeddings` serves
`batchEmbedContents`. The request reaches the provider unchanged apart from
model rewriting. No qualified mapping links Gemini embedding requests to another
provider's embedding dialect, so other targets are refused with
`target_capability`, and transformed routes refuse both endpoints with HTTP 400.
To reach Gemini, Vertex AI, or Bedrock embeddings from another dialect, use
`POST /v1/embeddings` on a transformed route.

### Bedrock surface

The Bedrock surface accepts requests signed for the AWS Bedrock SDK shape at
`{gateway}/bedrock`, but `{model}` in each path is always an OLP route slug,
never a provider model ID. Incoming AWS SigV4 is not gateway authentication:
clients must present `X-OLP-API-Key` (or an ordinary `Authorization: Bearer`
key), and inbound `Authorization`/`X-Amz-*` headers are stripped before the
gateway re-signs with the configured Bedrock credential.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /bedrock/model/{model}/converse` | generation | — | — | — | — | native | — | — |
| `POST /bedrock/model/{model}/converse-stream` | generation | — | — | — | — | native | — | — |
| `POST /bedrock/model/{model}/invoke` | bedrock_invoke | — | — | — | — | qualified | — | — |
| `POST /bedrock/model/{model}/invoke-with-response-stream` | bedrock_invoke | — | — | — | — | qualified | — | — |

Only native Bedrock targets holding the exact certified ingress mode qualify.
Unary Converse responses are validated through the same decoder as translated
traffic before their bytes are returned to the caller. InvokeModel requires a
transformed route and supports only the model families OLP qualifies —
Anthropic Claude generation, Titan embeddings, and Titan image generation — each
with model-specific response validation; other model IDs fail with a clear
protocol error. Streaming responses validate AWS event-stream CRC and frame size
and re-encode each frame; a malformed or oversized frame terminates the stream
rather than forwarding unchecked bytes. Usage is normalized from Bedrock
metadata and provider terminal events into standard accounting.

## What translation drops or refuses

The gateway prefers a clear refusal over a silent success with different
semantics: when a request carries something the target protocol cannot express,
the attempt fails with a protocol error rather than being quietly downgraded.
The exceptions are noted as drops.

Unknown vendor fields are preserved as source extensions and replayed for native
providers. Cross-protocol requests carrying unsupported extensions are refused;
unsupported response and stream extensions are dropped. The Anthropic and Gemini
request fixtures in `tests/fixtures/protocols/` cover preservation of fields
such as `cache_control`, `metadata`, `topK`, and `safetySettings`.

A system message between turns, which Claude Code sends, is kept in place where
the target can place it and refused where it cannot: Anthropic, Gemini, and
Bedrock Converse have one system field ahead of the conversation, so a request
translated to them with a late system message fails with `unsupported_parameter`
instead of moving the message to the front.

`tests/fixtures/protocols/selected-operation-families.json` covers every
operation family and surface. Keep these tables aligned with the
[protocol suites](../internal/protocols/translation_test.go) when semantics
change.

### Anthropic providers

Translation to Anthropic maps a `json_schema` `response_format` — schema object
required, `strict` honored only when set true — onto Anthropic's
`output_config`, and treats `text` as no structured output. `json_object` and
unknown formats are still refused, and whether a given model accepts the
translated field is provider/model dependent: unsupported combinations fail
clearly rather than silently degrading. The
[Anthropic encoder](../internal/protocols/canonical_anthropic.go) enforces
these rules. Cached-input usage, provider request IDs, media parts, and
oversized-response bounds remain part of the shared contract.

Beyond that, translating into Anthropic Messages refuses a request that uses a
participant `name` on a message, a deterministic `seed`, more than one
candidate, a tool-result message without its tool-call ID, an image with an
explicit detail level, or an input audio, input file, or refusal content part. A
missing maximum output token count is also refused, because Anthropic requires
one. These checks live in the
[Anthropic encoder](../internal/protocols/canonical_anthropic.go). The
[connector capability rules](../internal/connectors/capabilities.go) keep token
counting unary and bind the selected transport mode.

### Gemini providers

Gemini holds the shared contract with no exemptions. Translation refuses
parallel tool-call selection, a participant `name`, a system message that
appears after the conversation has started, a tool result whose content is not
text, and — as with Anthropic — input audio, input file, and refusal parts, or
an image with an explicit detail level. An image without a MIME type is refused
rather than guessed. These checks live in the
[Gemini encoder](../internal/protocols/canonical_gemini.go); the
[connector capability rules](../internal/connectors/capabilities.go) enforce
unary token counting and refuse asynchronous generation. Preserved counting
requests are validated after model rewriting as well.

Gemini takes a schema in two kinds of field. `parameters` and `responseSchema`
take an OpenAPI subset and refuse any member outside it, which OpenAI and
Anthropic clients routinely send (`$schema`, `additionalProperties`, `$ref`,
`const`, `oneOf`, a `type` that is a list, an `enum` of anything but strings);
`parametersJsonSchema` and `responseJsonSchema` take JSON Schema as it is, as
Google's own SDKs send it. A translated request puts a tool or response schema
in the subset field when every member of it is in the subset, so such a schema
reaches Gemini as it always did, and in the JSON Schema field when any is not,
so the schema reaches the model whole and is never rewritten or trimmed. The other direction rewrites only what
the two dialects spell differently: a Gemini schema's capital type names
(`OBJECT`, `STRING`) become the lower-case names of JSON Schema.

Gemini reports a `STOP` finish reason even when a candidate contains only
function calls. The gateway corrects that to a tool-call finish reason so agent
loops keyed on tool calls do not stop early; this is a deliberate rewrite, not a
pass-through.

Gemini also serves embeddings natively: a single string input becomes an
`embedContent` call and an array of strings becomes `batchEmbedContents`, with
`dimensions` mapped to `outputDimensionality`. Token-array input, more than 100
inputs, and unsupported encodings are refused. Vertex AI embeddings use the
publisher `predict` endpoint with at most five text inputs and report the summed
`statistics.token_count` as input usage when the upstream returns it. Both
reject provider responses that do not carry a numeric embedding vector for every
requested input.

### Bedrock providers

For translation into Bedrock, a `json_schema` `response_format` maps onto
Converse `outputConfig` — the schema as compact JSON text plus the required
schema name — for both Converse and ConverseStream, while `json_object` and
unknown formats remain refused. Whether a given model honors the field is
provider/model dependent; unsupported combinations still fail clearly. Reported
cache-read and cache-write input tokens are normalized into usage; canonical
provider response IDs remain unsupported. Unary responses have the shared
response-byte limit, and event lengths are checked before AWS event-stream
decoding allocates the advertised body.

Generation and token-count inputs accept inline base64 PNG, JPEG, GIF, and WebP
images. Remote image URLs, explicit image-detail controls, unsupported formats,
invalid base64, and images in system instructions or tool results are refused.
The shared inline-media admission limits also apply. Image parts inside
generation inputs do not grant separate media capabilities.

Bedrock also serves two qualified native operations beyond Converse.
`amazon.titan-embed-text-*` models accept a single string input via InvokeModel
and must return `embedding` plus a nonnegative `inputTextTokenCount`.
`amazon.titan-image-generator-*` models accept `image_generation` via
`taskType: TEXT_IMAGE` with one to four images and the `1024x1024`, `768x768`,
or `512x512` sizes only; `b64_json` is the only supported response format, and
provider error fields, malformed base64, or an image-count mismatch fail the
attempt. Other Bedrock models, and every edit/variation/audio/video operation,
remain refused.

The [image-input tests](../internal/protocols/bedrock_test.go) and
[stream bounds tests](../internal/protocols/stream_translation_test.go) cover these
rules.

The [Go Converse encoder](../internal/protocols/bedrock.go) refuses, with an
explicit protocol error, a request that asks for more than one candidate, sets a
deterministic seed, sets parallel tool-call selection, asks for a structured
response format other than `text` or a well-formed `json_schema`, puts a name or
tool-call metadata on a system instruction, or gives a maximum output token
count that does not fit Bedrock's limits. Tool results have their own rules: a
tool result must carry a tool-call ID and non-empty text content, must not carry
tool calls of its own, and only a tool-result message may carry a tool-call ID.
Tool names must be unique across the request and must be short, ASCII, and free
of punctuation other than `_` and `-`; a named tool choice must exist in the
tool list, and a tool choice of "none" cannot be combined with a non-empty tool
list. Non-finite temperature or `top_p` values and non-finite JSON numbers
inside tool arguments are refused too. The
[capability rules](../internal/connectors/capabilities.go) keep token counting
unary and refuse asynchronous generation; the
[request builder](../internal/connectors/config.go) validates the model ID or
ARN.

See the [Bedrock connector guide](providers/bedrock.md) for authentication, SDK
retry policy, deadlines, and live tests.

### OpenAI-compatible and Azure OpenAI providers

Both are native on the OpenAI surface, but their certification path is narrower
than OpenAI's own. An OpenAI-compatible provider can only be certified for
generation, embeddings, token counting, and moderation on the OpenAI surface,
plus rerank for the reviewed rerank vendors and the media operations of the
[reviewed media vendors](providers/media.md); the other surfaces are refused.
Cohere rerank resolves to the official `/v2/rerank` endpoint for the reviewed
preset; Voyage, Jina, Together and Infinity rerank post to the configured base
plus `/rerank`, and refuse `truncation` except at Voyage. Jina receives the
OpenAI `encoding_format` as its `embedding_type`. Azure OpenAI supports generation and token
counting on OpenAI, Anthropic, and Gemini surfaces, plus OpenAI-surface
embeddings, moderation, and unary image generation, speech and transcription
through deployment profiles; the Azure v1 profiles do not serve media. See
[Azure media](providers/azure.md#media). It also supports the qualified file,
batch, realtime, and stored-response paths above; image edits and variations,
audio translation, and video operations remain uncertifiable. See
[certification eligibility](../internal/providers/kinds.go).

See the [Azure connector guide](providers/azure.md) for API-key and Microsoft
Entra authentication, token caching, and identity egress.

## Files and batches

| Provider/surface | Supported scope |
| --- | --- |
| OpenAI and Azure OpenAI | Native and strict OpenAI-shaped `/v1/files` and `/v1/batches`. |
| Groq | Native file/batch endpoints under the reviewed Groq contract. |
| Together, Mistral and Gemini | Managed OpenAI-shaped file uploads and batches on transformed routes. |
| Anthropic | Native `/anthropic/v1/files` and Messages batch jobs on transformed routes. |
| Gemini | Native resumable upload, Files list/get/delete/download and `batchGenerateContent`. |
| Vertex AI and Bedrock | Transformed-only `/native/{dialect}/batch-jobs` create/list/get/cancel, with operator-configured `batch_storage`. |
| Fireworks | Declined: result retrieval uses provider-returned signed URLs, which the egress contract forbids. |

Managed Anthropic, Vertex and Bedrock OpenAI-shaped batches are refused with
`422 target_capability`; only the listed managed adapters translate through the
OpenAI file/batch shape.
Strict resource and batch routes are available only to OpenAI and Azure OpenAI;
other families return `422 target_capability`. Batch creation that cannot be
bounded by the applicable enforced budget returns `422 unbounded_work_budget`.
Unknown fields, unsupported output configuration, unsafe file references,
out-of-prefix cloud URIs and unsupported timeout settings are refused rather
than silently ignored.

OpenAI-shaped batch input must be JSONL with unique `custom_id` values and one
supported endpoint. Each line's `body.model` must name the route or its pinned
provider model; transformed OpenAI and Groq uploads bind route aliases to that
provider model before upload. Files uploaded before model qualification must be
uploaded again before creating new batches. Native Gemini inline batches refuse
file and cached-content references until owner and affinity binding is available.

Creation requires `X-OLP-Route`; follow-up requests derive route and affinity
from the owner-scoped gateway ID. Ambiguous creates are retained as
`submission_unknown`: the create returns `502`, a matching reconciliation can
adopt the job, while conflicting evidence returns `409` and is never blindly
redispatched. Result
settlement records observed per-result usage against the pinned line-operation
price; incomplete evidence remains uncertain and is never zero-cost.

## SDK and live-provider checks

The deterministic JavaScript smoke suite pins OpenAI `7.4.0`, Anthropic
`0.116.0` and Google GenAI `2.16.0` in `tests/sdk-smoke/package.json`. These
are mock protocol checks, not a claim that every upstream model or every SDK
version has live certification. Live-provider checks are dispatched separately
with selected-provider credentials; use their result for the actual provider,
model and capability revision. Nothing is certified live when that workflow has
not run.

Provider-owned response/conversation/file IDs are not universally portable.
Referencing a resource across independently selected providers is unsupported
unless the implementation pins the owning provider and credential revision, as
the media and provider-resource paths do. Unsupported stateful fields are
rejected by the surface's capability/translation policy; route failover cannot
turn an upstream identifier into a gateway-owned resource. The native/translated
tables above and the endpoint registry are the maintained support records;
update them with the conformance fixtures when semantics change, rather than
introducing a second independently maintained matrix.

The ordinary console SDK examples are executed verbatim by the pinned official
JavaScript and Python SDK smoke harness, including typed authentication refusal
and disabled automatic retries. This qualifies controlled gateway fixture
behavior; the browser's separate connection check and these fixtures do not
establish live-provider or production-performance qualification.
