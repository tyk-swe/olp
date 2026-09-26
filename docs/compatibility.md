# Provider compatibility

OpenLLMProxy accepts OpenAI, Anthropic, Gemini, and Bedrock client protocols. A
route can select a provider using the same protocol or, when the route is
[transformed](provider-routing.md#route-fidelity), translate to another
supported protocol. The tables below show supported combinations; translation
limits follow. See [concepts](concepts.md) for routes and certification.

Compatible-vendor profiles can narrow the general connector matrix. See
[provider routing](provider-routing.md) for the qualified DeepSeek, Fireworks,
DeepInfra, Hugging Face, Perplexity, Cohere, and Voyage contracts and OLP
routing preferences. Custom native endpoints require live certification; they do
not inherit the official OpenAI media discovery contract.

The [gateway](gateway.md) applies shared admission and response bounds to these
surfaces. Current protocol, connector, SDK, and media tests are described in
[tests/README.md](../tests/README.md).

## Legend

| Cell | Meaning |
| --- | --- |
| `native` | The provider speaks this surface's protocol. Protocol-specific fields are preserved subject to gateway validation and model rewriting. |
| `translated` | The request is decoded into the canonical model and re-encoded for the provider; the route must be transformed. Some fields are dropped and some are refused — see the notes below. |
| `—` | Refused. The tuple can never be certified, so it can never be activated on a route, and the gateway rejects the request. |
| `gateway` | Answered from visible published routes; model listing is not proxied upstream. |
| `qualified` | Restricted to the provider, model family, or resource policy described below; exact capabilities still require certification. |
| `reviewed` | Available only through the named compatible-vendor profiles and certified models. |

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
| `POST /v1/chat/completions` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses/input_tokens` | token_count | native | translated | translated | translated | translated | native | native |
| `POST /v1/embeddings` | embeddings | native | — | translated | translated | translated | native | native |
| `POST /v1/rerank` | rerank | — | — | — | — | — | — | reviewed |
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
| `POST /v1/images/generations` | image_generation | native | — | — | qualified | qualified | — | — |
| `POST /v1/images/edits` | image_edit | native | — | — | — | — | — | — |
| `POST /v1/images/variations` | image_variation | native | — | — | — | — | — | — |
| `POST /v1/audio/speech` | speech | native | — | — | — | — | — | — |
| `POST /v1/audio/transcriptions` | transcription | native | — | — | — | — | — | — |
| `POST /v1/videos` | video_create | native | — | — | — | — | — | — |
| `GET /v1/videos` | video_list | native | — | — | — | — | — | — |
| `GET /v1/videos/{video_id}` | video_get | native | — | — | — | — | — | — |
| `DELETE /v1/videos/{video_id}` | video_delete | native | — | — | — | — | — | — |
| `GET /v1/videos/{video_id}/content` | video_content | native | — | — | — | — | — | — |
| `GET /v1/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /v1/models/{id}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

`rerank` is certified only for the reviewed OpenAI-compatible vendors Cohere and
Voyage; other vendors are refused. Vertex `image_generation` qualifies
`imagen-*` models and Bedrock qualifies `amazon.titan-image-generator-*` models;
other models and every edit/variation/audio/video operation remain refused.
Native embeddings accept only the canonical input shapes described per provider
below.

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
plus rerank for the reviewed Cohere and Voyage vendors, so the other surfaces
and every media operation are refused. Cohere rerank resolves to the official
`/v2/rerank` endpoint for the reviewed preset; Voyage rerank posts to the
configured base plus `/rerank`. Azure OpenAI supports generation and token
counting on OpenAI, Anthropic, and Gemini surfaces, plus OpenAI-surface
embeddings and moderation. It also supports the qualified file, batch, realtime,
and stored-response paths above; image, audio, and video operations remain
uncertifiable. See [certification eligibility](../internal/providers/kinds.go).

See the [Azure connector guide](providers/azure.md) for API-key and Microsoft
Entra authentication, token caching, and identity egress.

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
