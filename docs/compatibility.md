# Provider compatibility

OpenLLMProxy accepts OpenAI, Anthropic, and Gemini client protocols. A route
can select a provider using the same protocol or translate to another supported
protocol. The tables below show supported combinations; translation limits
follow. See [concepts](concepts.md) for routes and certification.

Compatible-vendor profiles can narrow the general connector matrix. See
[provider routing](provider-routing.md) for the qualified DeepSeek, Fireworks,
DeepInfra, Hugging Face, Perplexity, Cohere, and Voyage contracts and OLP routing
preferences. Custom native endpoints require live certification; they do not
inherit the official OpenAI media discovery contract.

The [gateway](gateway.md) serves this matrix through one bounded executor.
Current protocol, connector, SDK, and media tests are described in
[tests/README.md](../tests/README.md). The [dated completion record](roadmap/README.md)
links historical qualification; it does not qualify newer source.

## Legend

| Cell | Meaning |
|---|---|
| `native` | The provider speaks this surface's protocol. Protocol-specific fields are preserved subject to gateway validation and model rewriting. |
| `translated` | The request is decoded into the canonical model and re-encoded for the provider. Some fields are dropped and some are refused — see the notes below. |
| `—` | Refused. The tuple can never be certified, so it can never be activated on a route, and the gateway rejects the request. |
| `gateway` | Answered by the gateway from the configured routes. Model listing is never proxied to a provider, so it does not depend on the provider kind. |

A cell is `native` when the provider family's own wire protocol is the surface
the request arrived on. Bedrock is never native: its wire protocol is Converse,
which the gateway does not expose as a client surface, so every Bedrock request
is translated regardless of which SDK sent it.

Transport modes are folded into the cells. Generation is available both unary
and streaming wherever it is available at all; token counting is unary only.
Video creation is asynchronous. A cell shows `—` only when no transport mode
of that operation can be certified for that provider kind.

Endpoint registration lives in [`internal/gateway/server.go`](../internal/gateway/server.go);
certification policy lives in [`internal/providers/kinds.go`](../internal/providers/kinds.go).
The conformance corpus and SDK suites exercise these behaviors.

## Surfaces and operations

### OpenAI surface

Use a base URL ending in `/v1` for native OpenAI SDK requests.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
|---|---|---|---|---|---|---|---|---|
| `POST /v1/chat/completions` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses/input_tokens` | token_count | native | translated | translated | translated | translated | native | native |
| `POST /v1/embeddings` | embeddings | native | — | — | — | — | native | native |
| `POST /v1/moderations` | moderation | native | — | — | — | — | native | native |
| `POST /v1/images/generations` | image_generation | native | — | — | — | — | — | — |
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

### Anthropic surface

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
|---|---|---|---|---|---|---|---|---|
| `POST /anthropic/v1/messages` | generation | translated | native | translated | translated | translated | translated | — |
| `POST /anthropic/v1/messages/count_tokens` | token_count | translated | native | translated | translated | translated | translated | — |
| `GET /anthropic/v1/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /anthropic/v1/models/{id}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

### Gemini surface

Gemini endpoints are served under both `/gemini/v1` and `/gemini/v1beta`.
Replace `{version}` below with `v1` or `v1beta`.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
|---|---|---|---|---|---|---|---|---|
| `GET /gemini/{version}/models` | model_list | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /gemini/{version}/models/{model}` | model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `POST /gemini/{version}/models/{model}:generateContent` | generation | translated | translated | native | native | translated | translated | — |
| `POST /gemini/{version}/models/{model}:streamGenerateContent` | generation | translated | translated | native | native | translated | translated | — |
| `POST /gemini/{version}/models/{model}:countTokens` | token_count | translated | translated | native | native | translated | translated | — |

## What translation drops or refuses

The gateway prefers a clear refusal over a silent success with different
semantics: when a request carries something the target protocol cannot express,
the attempt fails with a protocol error rather than being quietly downgraded.
The exceptions are noted as drops.

Unknown vendor fields are preserved as source extensions and replayed for
native providers. Cross-protocol requests carrying unsupported extensions are
refused; unsupported response and stream extensions are dropped. The Anthropic
and Gemini request fixtures in `tests/fixtures/protocols/` cover preservation
of fields such as `cache_control`, `metadata`, `topK`, and `safetySettings`.

`tests/fixtures/protocols/selected-operation-families.json` covers every operation
family and surface. Keep these tables aligned with the
[Go protocol suites](../internal/protocols/parity_test.go) and the
[frozen certification check](../internal/providers/frozen_capabilities_test.go)
when semantics change.

### Anthropic providers

Translation to Anthropic refuses `response_format` rather than advertising
structured-output support it cannot express. This retains the frozen
`NO_ANTHROPIC_STRUCTURED_OUTPUT` exception; current enforcement is in the
[Anthropic encoder](../internal/protocols/canonical_anthropic.go). Cached-input
usage, provider request IDs, media parts, and oversized-response bounds remain
part of the shared contract.

Beyond that, translating into Anthropic Messages refuses a request that uses a
participant `name` on a message, a deterministic `seed`, more than one
candidate, a tool-result message without its tool-call ID, an image with an
explicit detail level, or an input audio, input file, or refusal content part.
A missing maximum output token count is also refused, because Anthropic
requires one. These checks live in the
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
loops keyed on tool calls do not stop early; this is a deliberate rewrite, not
a pass-through.

### Bedrock providers

Bedrock translates on every surface. Non-text structured response formats,
cached-input token accounting, and canonical provider response IDs remain
unsupported. The Go connector uses OLP's HTTP transport for inference: unary
responses have the shared response-byte limit, and stream event lengths are
checked before AWS event-stream decoding allocates the advertised body.

Generation and token-count inputs accept inline base64 PNG, JPEG, GIF, and WebP
images. Remote image URLs, explicit image-detail controls, unsupported formats,
invalid base64, and images in system instructions or tool results are refused.
The shared inline-media admission limits also apply. Image/audio/video operation
endpoints remain unavailable for Bedrock; image parts inside generation inputs
do not grant those separate capabilities.

These are explicit changes from the frozen Rust reference: its
`NO_BEDROCK_RESPONSE_BOUND` and `NO_BEDROCK_MEDIA` exemptions described unbounded
SDK-owned response bodies and refusal of image input parts, respectively.
The remaining frozen exceptions—`NO_BEDROCK_STRUCTURED_OUTPUT`,
`NO_BEDROCK_CACHED_USAGE`, and `NO_BEDROCK_REQUEST_ID`—remain explicit above.
See [image-input tests](../internal/protocols/bedrock_test.go) and
[stream bounds tests](../internal/protocols/stream_parity_test.go).

The [Go Converse encoder](../internal/protocols/bedrock.go) refuses, with an
explicit protocol error, a request that asks for more than one candidate, sets
a deterministic seed, sets parallel tool-call selection, asks for a structured
response format other than text, puts a name or tool-call metadata on a system
instruction, or gives a maximum output token count that does not fit Bedrock's
limits. Tool results have their own rules: a tool result must carry a tool-call
ID and non-empty text content, must not carry tool calls of its own, and only a
tool-result message may carry a tool-call ID. Tool names must be unique across
the request and must be short, ASCII, and free of punctuation other than `_`
and `-`; a named tool choice must exist in the tool list, and a tool choice of
"none" cannot be combined with a non-empty tool list. Non-finite temperature or
`top_p` values and non-finite JSON numbers inside tool arguments are refused
too. The [capability rules](../internal/connectors/capabilities.go) keep token
counting unary and refuse asynchronous generation; the
[request builder](../internal/connectors/config.go) validates the model ID or ARN.

See the [Bedrock connector guide](providers/bedrock.md) for authentication,
SDK retry policy, deadlines, and live tests.

### OpenAI-compatible and Azure OpenAI providers

Both are native on the OpenAI surface, but their certification path is
narrower than OpenAI's own. An OpenAI-compatible provider can only be certified
for generation, embeddings, token counting, and moderation on the OpenAI
surface, so the other surfaces and every media operation are refused. Azure
OpenAI can be certified for the same four operations, but on any surface, so it
appears as `translated` on the Anthropic and Gemini surfaces and `—` for the
media, image, audio, and video operations. Certification eligibility is defined in
[`internal/providers/kinds.go`](../internal/providers/kinds.go); update this table when it changes.

## Qualification records

The deterministic JavaScript smoke suite currently pins OpenAI `7.4.0`,
Anthropic `0.116.0` and Google GenAI `2.16.0` in
`tests/sdk-smoke/package.json`. These are mock protocol checks, not a claim
that every upstream model or every SDK version has live certification. Each
CI run records its source commit, test counts and SDK lockfile. Live-provider
qualification is separately dispatched with selected-provider credentials;
use its dated result for the actual provider/model/capability revision. There
is no inferred live certification date when that workflow has not run.

Provider-owned response/conversation/file IDs are not universally portable.
Referencing a resource across independently selected providers is unsupported
unless the operation's implementation explicitly pins the owning provider and
retained credential revision (as the media job paths do). Unsupported stateful
fields are rejected by the surface's capability/translation policy; route
failover cannot turn an upstream identifier into a gateway-owned resource.
The existing native/translated tables and endpoint registry are the maintained
support records; update them with the conformance fixtures when semantics
change, rather than introducing a second independently maintained matrix.
