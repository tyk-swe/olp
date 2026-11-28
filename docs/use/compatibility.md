# Provider compatibility

OpenLLMProxy accepts OpenAI, Anthropic, Gemini, Bedrock, and native dialect
client protocols. A route can use a provider that speaks the same protocol, or
a [transformed route](provider-routing.md#route-fidelity) can translate a
request to another supported protocol. The tables in this guide state the
supported combinations, and the sections after them define what translation
preserves, drops, or refuses. [Concepts](../concepts.md) explains routes,
certification, and fidelity before this reference goes into endpoint detail.

Reviewed vendor contracts narrow the general connector matrix to the operations
that each vendor documents.
[Provider routing](provider-routing.md#reviewed-vendor-contracts) explains how
those contracts shape requests, and the
[provider preset table](../operate/configuration.md#openai-compatible-provider-presets)
lists every reviewed vendor. A custom native endpoint still requires live
certification and does not inherit the official OpenAI media discovery contract.

The [gateway](../develop/request-path.md) applies shared admission and response
bounds to these surfaces. Current protocol, connector, SDK, and media tests are
described in [tests/README.md](../../tests/README.md).

## Published prompt compatibility

Prompts have project-scoped drafts, immutable published revisions, and
`production` and `staging` labels. A request selects one label or exact
revision for a prompt in the route's project. The gateway resolves the label
once, substitutes declared string variables in `{{name}}` placeholders, and
applies the route's input limit and input guardrails to the rendered messages.
Unknown or missing required variables are refused before dispatch. Strict routes
do not support registry prompt rendering. See the [agents guide](agents.md) for
prompt authoring and publication workflows.

Chat Completions, Anthropic Messages, and Gemini generation accept an
`olp_prompt` object in place of explicit messages:

```json
{"olp_prompt":{"id":"PROMPT_ID","label":"production","variables":{"name":"Ada"}}}
```

Responses uses the same selection inside `prompt`, with `id` prefixed by
`olpp_`. Other Responses prompt IDs retain their provider behavior. An exact
`revision_id` can be used instead of `label` on either form. The console
playground and inference render on the server using the same published
revision, including after configuration export and import. Variable values and
rendered messages are transient rather than configuration data.

Local gateway fixtures cover all four request formats in unary and streaming
mode, and the packaged-console journey covers publication, label movement, and
accessible playground rendering.

## Saved investigations

The management API and console save project-scoped investigations with time,
run, duration, error, and tool filters, an operator-authored summary, and up to
100 references to run, step, approval, span, task, or delegation evidence. The
record contains no source payloads. Reading an investigation requires current
project access, and opening referenced evidence requires its own current access
check. Retained step and span links identify the referenced record, including
spans beyond the first page. Evidence follows its own retention policy and can
expire before the investigation. Local HTTP regressions and the
packaged-console journey exercise these boundaries.

## Agent workflow evidence exports

Observability and managed sinks can export `runs` and `trace_spans` for
project-scoped or installation-wide configurations. Gateway-created runs carry
`evidence_origin` set to `gateway_observed`. Ingested spans carry
`evidence_origin` set to `client_asserted`, even if a client supplies a
different value. Gateway request and attempt spans set the
`olp.evidence_origin` attribute to `gateway_observed`. The existing request
`origin` continues to describe the invocation source.

Span exports retain the nullable `parent_span_id` as a hexadecimal identifier.
The management [OpenAPI contract](../../management/management.json) describes
the observability and managed `runs` and `trace_spans` webhook envelopes.

Run and span exports contain metadata only. They do not include prompt, tool,
message, or artifact contents, and they do not change billing. Before delivery,
the sink creator must still have access to the source project and the source
must remain within request-history retention. These checks apply independently
to each record sent through an installation-wide sink. Local gateway and export
fixtures exercise origin, privacy, project isolation, and retention.

## Gateway-executed tool workflows

On transformed routes with `tool_execution: gateway`, Chat Completions,
Responses, Anthropic Messages, and Gemini generation can execute published MCP
tools as part of a bounded run. Only targets certified for model-emitted tool
calls on the request surface and mode are eligible. The caller can narrow the
route's toolsets with `olp_toolsets` and tighten its bounds with
`olp_run_limits`. Read-only tools can execute without approval, while other
tools require approval. See the [agents guide](agents.md) for workflow
governance, delegation, and approval handling.

A Chat Completions reply sums token usage across the model turns in that HTTP
request when all turns report usage and the sum fits. Otherwise, it leaves the
final turn's usage unchanged. Streaming clients receive usage only when they
set `stream_options.include_usage`.

Default bounds are eight model iterations, 32 tool calls, and 120 seconds per
run, with hard ceilings of 32 iterations, 128 tool calls, and 600 seconds. The
effective bound is the smallest of the route, key, and caller limits. A distinct
optional `max_run_cost` bounds the whole run, and every next step must fit the
remaining worst-case allowance in the installation pricing currency, so an
unbounded output model or unpriced tool cannot satisfy a finite run ceiling.
The existing `max_cost` stays a per-attempt candidate filter and applies
independently to each model step. Operators classify each published tool as
`read_only` or `approval_required`, and an unclassified tool requires approval.
Upstream tool annotations do not change the classification, and a guardrail deny
overrides approval. An approval expires after five minutes and binds the
principal, project, run, tool and route revisions, argument and transcript
digests, deadline, and cost allowance. The caller resubmits the transcript and
arguments to resume, and resuming reauthorizes and verifies the exact digests.
Expiry, changed input, replay, or revoked authority refuses. Response caching
is disabled for gateway-executed runs so a cache hit cannot bypass tool effects,
approvals, or accounting.

Model list and detail responses on the OpenAI, Anthropic, and Gemini surfaces
include `capabilities.gateway_execution` when the route and calling key permit
inspection. It reports surface-specific certified modes for non-shadow targets,
workflow accounting availability, the effective route and key limits, whether
all default toolsets are usable together, and the caller's available tools with
pinned revisions, classifications, and unit prices. A null price is unpriced,
while zero is explicitly free. The capability describes current configuration
and does not reserve a run or guarantee admission.

To inspect a particular request, select the same API key in the published route
or draft route simulation and supply the native request, including any narrowed
toolsets and bounds. The simulation capability reports the effective request
bounds and caller-authorized tools and prices, while target decisions report
workflow refusals alongside normal routing decisions. Delegated route legs use
the named route's toolsets and bounds and their own serving targets. Simulation
does not dispatch a model or tool, reserve budget, or guarantee live admission.

Former local gateway fixtures exercised unary and streaming workflows for Chat
Completions, Responses, Anthropic, and Gemini, including bounds, target
certification, approval, authority changes, `typesafe` pruning and compression
recovery, and caller-filtered tool discovery. A former process test checked
same-key simulation authorization and request bounds for draft and published
routes, a run and its durable settlement, and rollback with local services. To
stop new gateway execution, republish the transformed route without
`tool_execution`, `toolsets`, `run_limits`, and `responses_mcp_items`. Pending
approvals then cannot resume, later requests do not dispatch gateway tools,
caller-managed tool calls still return to the client, and authorized project
viewers can still read the retained run, steps, and settlement.

Responses MCP output additionally requires the route's `responses_mcp_items`
declaration and the certified built-in `openai-responses` contract on every
successful model step. Other or mixed contracts return the final output or an
approval pause without MCP items. Former local fixtures confirmed that the
pinned official OpenAI Go SDK decodes the generated stream and covered the
final-only fallback for the tested dialect and target combinations.

## Consumer MCP qualification

Published tools support the JSON call endpoint and Streamable HTTP at MCP
2025-03-26, 2025-06-18, and 2025-11-25 through one authorized execution path.
Local fixtures cover schema and drift refusal, revocation, limits, pinned
pricing, guardrail arguments and results, durable reservation and settlement,
unknown outcomes, and call-identifier replay. The console exposes content-free
attempt history, and capability and simulation responses use the same
publication, price, and effective-limit inputs as execution.

The former minimal reference check used the official
[Go MCP SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/tree/v1.8.0),
pinned in `go.mod`, as both a Streamable HTTP server and client with the
protocol explicitly pinned to 2025-11-25. It registered and certified that
server, published its tool, listed it through the consumer boundary, invoked it
with the reference client, and checked the settled record. That was local
fixture qualification rather than a claim about every third-party MCP
deployment, and it no longer runs.

Explicit legacy HTTP+SSE was also qualified at MCP 2024-11-05 using the same
SDK pin. A former fixture registered and recertified a legacy server,
round-tripped its portable configuration, published the tool, and called it
through a legacy reference client, checking the resulting durable settlement.
Public-handler fixtures covered exact argument-byte identity, authority
revocation, modern and legacy session isolation, and refusal when a replica
lacked the live stream. Legacy deployments require connection affinity between
the SSE GET and its POST requests. Modern sessions never fall back to the
legacy transport.

A former process test exercised a delegated agent through the shared tool path
using a local workload issuer and signing fixture. It checked the downstream
audience-bound agent signature, refusal at another audience, one-use
credentials, consent, session isolation, and shared budget accounting.

Per-user OAuth was qualified separately. Experimental stdio was covered by a
locally built, owner-permitted executable fixture through the shared consumer
path. It preserves the unconfined native execution contract and does not provide
a sandbox. See [MCP servers](mcp-servers.md#experimental-stdio-executables) for
deployment, permission, and lifecycle requirements. Standalone modern consumer
event streams, sampling, elicitation, roots, and arbitrary resource or prompt
calls are not supported by this slice. See [MCP server use](mcp-servers.md) for
supported calls and rollback behavior.

## A2A evidence

Former local reference fixtures covered the pinned A2A 0.3 and 1.0 JSON-RPC
adapters. A former process test exercised delegated sends, callbacks, caller,
agent, and pair accounting, consent, renewal, project operator access, and
revocation. Another verified that task actions use their original grant and
endpoint after publication replacement and refuse revoked grants. OLP cannot
observe or stop hidden remote iterations.

## Guardrail compatibility

[Guardrails](guardrails.md) inspect effective provider input and qualified unary
output, including tool arguments and results. Enforcing streaming policies use a
byte-bounded holdback, and rules needing a complete result return
`422 guardrail_surface_unavailable`. Strict routes accept block, annotation, and
monitoring. Redaction and request-local masking require transformed routes.
Opaque media and retained surfaces without a complete inspection contract refuse
enforcement, while monitoring records the unavailable surface and continues.

## Legend

| Cell | Meaning |
| --- | --- |
| `native` | The provider speaks this surface's protocol. Protocol-specific fields are preserved subject to gateway validation and model rewriting. |
| `translated` | The request is decoded into the canonical model and re-encoded for the provider, and the route must be transformed. Some fields are dropped and some are refused. |
| `refused` | The tuple can never be certified, so it can never be activated on a route. The gateway rejects the request. |
| `gateway` | Answered directly by the gateway from visible configuration, policy, or ingestion endpoints rather than proxied upstream. |
| `qualified` | Restricted to the provider, profile, model family, or resource policy described below. Exact capabilities still require certification. |
| `reviewed` | Available only through the named compatible-vendor profiles and certified models. |

Reviewed vendors serve image, speech, transcription, translation, and video
operations on the OpenAI surface through their media codecs. See
[vendor media](providers/media.md).

Native generation dialects, such as Mistral fill-in-the-middle and Cohere Chat
v2, serve their own clients at `POST /native/{dialect}/models/{model}` and are
never translated. See
[native generation dialects](provider-profiles.md#native-generation-dialects).

Three specialized provider kinds do not have their own column in the tables
below because each serves a narrow scope. The `sagemaker` kind serves only
generation on the OpenAI surface, matching the OpenAI chat behavior in the
`openai_compatible` column. See [SageMaker AI](providers/sagemaker.md). The
`watsonx` kind serves only generation on transformed routes, translated across
the OpenAI, Anthropic, and Gemini surfaces and refused on Bedrock. See
[IBM watsonx.ai](providers/watsonx.md). The `azure_ai_search` kind serves only
cloud retrieval through the `azure-ai-search` profile and governed retrieval
bindings at `POST /v1/retrieval/{binding_id}/query` (and
`POST /v1/vector_stores/{id}/search` when given a binding UUID). See the
[retrieval guide](retrieval.md).

`native` means the upstream speaks the incoming wire protocol. Bedrock Converse
is native on the Bedrock surface and translated on OpenAI, Anthropic, and Gemini
surfaces, while Bedrock Anthropic Invoke (`bedrock-anthropic-invoke`) and Vertex
Anthropic (`vertex-anthropic`) host the Anthropic Messages dialect natively.
Native traffic still undergoes gateway validation and rewriting.

Cells combine transport modes. Generation supports unary and streaming, token
counting is unary, video creation is asynchronous, and realtime uses WebSockets
or WebRTC. `refused` means no mode of that operation can be certified for the
provider.

Endpoint registration is assembled in
[`internal/gateway/gateway.go`](../../internal/gateway/gateway.go) from the
execution kernel in [`internal/gateway/kernel/`](../../internal/gateway/kernel)
and the surface packages under
[`internal/gateway/surface/`](../../internal/gateway/surface). Certification
policy lives in
[`internal/providers/probe/operations.go`](../../internal/providers/probe/operations.go).
The conformance corpus and SDK suites exercise these behaviors.

## Surfaces and operations

### OpenAI surface

Use a base URL ending in `/v1` for native OpenAI SDK requests.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /v1/completions` | generation | qualified | refused | refused | refused | refused | translated | reviewed |
| `POST /v1/decisions` | decision | qualified | refused | refused | refused | refused | refused | qualified |
| `POST /v1/systemone` | decision | refused | refused | refused | refused | refused | refused | qualified |
| `POST /v1/chat/completions` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses` | generation | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses/input_tokens` | token_count | native | translated | translated | translated | translated | native | native |
| `POST /v1/responses/compact` | generation | qualified | refused | refused | refused | refused | qualified | refused |
| `POST /v1/embeddings` | embeddings | native | refused | translated | translated | translated | native | native |
| `POST /v1/rerank` | rerank | refused | refused | refused | refused | qualified | refused | reviewed |
| `POST /v1/moderations` | moderation | native | refused | refused | refused | refused | native | native |
| `POST /v1/ocr` | ocr | refused | refused | refused | qualified | refused | refused | reviewed |
| `POST /v1/search` | search | refused | refused | refused | refused | refused | refused | reviewed |
| `POST /v1/files`, `GET /v1/files`, `GET /v1/files/{id}`, `DELETE /v1/files/{id}`, `GET /v1/files/{id}/content` | batch / retained file | qualified | refused | qualified | refused | refused | qualified | reviewed |
| `POST /v1/batches`, `GET /v1/batches`, `GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel` | batch | qualified | refused | qualified | refused | refused | qualified | reviewed |
| `GET /v1/responses/{id}`, `DELETE /v1/responses/{id}`, `POST /v1/responses/{id}/cancel`, `GET /v1/responses/{id}/input_items` | generation | qualified | refused | refused | refused | refused | qualified | refused |
| `GET /v1/continuations/{id}`, `GET /v1/continuation-submissions/{submission}` | generation | refused | qualified | refused | refused | refused | refused | refused |
| `GET /v1/realtime` | realtime | qualified | refused | refused | refused | refused | qualified | refused |
| `POST /v1/realtime/calls` | realtime (webrtc) | qualified | refused | refused | refused | refused | refused | refused |
| `POST /v1/images/generations` | image_generation | native | refused | qualified | qualified | qualified | qualified | reviewed |
| `POST /v1/images/edits` | image_edit | native | refused | refused | refused | refused | refused | reviewed |
| `POST /v1/images/variations` | image_variation | native | refused | refused | refused | refused | refused | refused |
| `POST /v1/audio/speech` | speech | native | refused | qualified | qualified | qualified | qualified | reviewed |
| `POST /v1/audio/transcriptions` | transcription | native | refused | qualified | qualified | refused | qualified | reviewed |
| `POST /v1/audio/translations` | translation | native | refused | refused | refused | refused | refused | reviewed |
| `POST /v1/videos`, `GET /v1/videos`, `GET /v1/videos/{video_id}`, `DELETE /v1/videos/{video_id}`, `GET /v1/videos/{video_id}/content` | video lifecycle | native | refused | refused | refused | refused | refused | reviewed |
| `/v1/fine_tuning/jobs*` (create, list, get, cancel, events, checkpoints) | fine_tuning | qualified | refused | refused | refused | refused | qualified | refused |
| `/v1/vector_stores*` (create, list, get, update, delete, search, files, file_batches) | vector_store / retrieval | qualified | refused | refused | refused | refused | qualified | refused |
| `/v1/conversations*` (create, get, update, delete, items) | conversation | qualified | refused | refused | refused | refused | refused | refused |
| `/v1/containers*` (create, list, get, delete, files, content) | container | qualified | refused | refused | refused | refused | qualified | refused |
| `/v1/evals*` (create, list, get, update, delete, runs, output_items) | eval | qualified | refused | refused | refused | refused | refused | refused |
| `POST /v1/retrieval/{binding_id}/query` | retrieval | refused | refused | refused | qualified | qualified | refused | refused |
| `POST /v1/guardrails/apply` | guardrails | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `POST /v1/delegations` | delegation | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `POST /v1/traces` | trace ingest | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `GET /v1/models`, `GET /v1/models/{id}` | model_list, model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

`rerank` is certified only for the reviewed OpenAI-compatible vendors Cohere,
Voyage, Jina, Together, and Infinity, and for Bedrock's rerank models, while
other vendors are refused. Echoed documents must be the request's own, as text
or as an object holding it.

Media support spans both native cloud providers and reviewed vendors. Gemini
and Vertex AI serve `image_generation` (Vertex qualifies `imagen-*` and Gemini
image models, while Gemini serves Gemini image models), `speech`, and
`transcription` through Gemini models. Bedrock qualifies
`amazon.titan-image-generator-*` and Stability text-to-image models for
`image_generation` and Amazon Polly engines for `speech`. Reviewed
`openai_compatible` presets qualify for specific media operations, including
xAI, Stability AI, Recraft, and Black Forest Labs for `image_generation`,
Stability AI for `image_edit`, ElevenLabs and Deepgram for `speech`, Groq,
ElevenLabs, Deepgram, and AssemblyAI for `transcription`, Groq for
`translation`, and Runway for the `/v1/videos*` lifecycle. See
[vendor media](providers/media.md) for per-vendor wire rules and limits.

OpenAI-shaped `/v1/files*` and `/v1/batches*` serve official OpenAI, Azure
OpenAI, Gemini (through the managed adapter), and the reviewed
`openai_compatible` batch presets for Groq, Mistral, and Together. Non-batch
retained file uploads (`fine-tune`, `assistants`, `evals`, and `user_data`)
require `allow_provider_state`, an `Idempotency-Key` of at most 128 bytes, and
a compatible OpenAI or Azure OpenAI route permitting the matching operation.

`POST /v1/retrieval/{binding_id}/query` queries operator-configured cloud
retrieval bindings on transformed routes backed by `bedrock`
(`bedrock-knowledge-bases`), `vertex_ai` (`vertex-rag-engine`), or
`azure_ai_search` (`azure-ai-search`). `POST /v1/vector_stores/{id}/search`
queries a retained OpenAI or Azure OpenAI vector store when `{id}` is a gateway
vector store ID, and dispatches to the same cloud retrieval handler when `{id}`
is a retrieval binding UUID. See the [retrieval guide](retrieval.md).

`GET /v1/continuations/{id}` and
`GET /v1/continuation-submissions/{submission}` recover committed
`chat-anthropic-tools-v1` continuations targeting direct Anthropic profiles.
`POST /v1/guardrails/apply` evaluates a published guardrail policy revision
directly against caller-supplied text segments. `POST /v1/delegations`
exchanges a workload identity plus a delegating key or member consent for a
bounded delegation grant. `POST /v1/traces` ingests OTLP protobuf trace spans
(`application/x-protobuf`) for project observability under `traces_write`
scope. Native embeddings accept only the canonical input shapes described per
provider below.

### Legacy text completions and Decisions

`POST /v1/completions` uses `generation` authorization. Native text completions
require an explicit `openai-completions`, `vllm-completions`, `tgi-completions`,
`fireworks-completions`, or `together-completions` profile and a certified
model. Generic compatible hosting does not qualify the native endpoint. The
self-hosted `tgi` preset is registered in the vendor catalog with
`Discovery: false` and supports `openai-chat` and `openai-completions`. Unary
and SSE results retain the native text-completion grammar, finish reasons, and
usage. The first native contract excludes `suffix`.

The `tgi-completions` profile accepts text prompts and unary text batches.
Streaming is limited to one text prompt because TGI emits per-prompt usage
instead of a request aggregate. Token-ID prompts and unsupported controls such
as `n`, `best_of`, `echo`, `logprobs`, `presence_penalty`, `logit_bias`, and
`user` are refused before dispatch. Its stream adapter accepts TGI's empty
nonterminal finish reason and still requires a terminal finish reason and
`[DONE]`.

Transformed Chat Completions targets accept one text prompt as one user message.
Prompt arrays and token IDs, `echo`, `best_of`, `logprobs`, `suffix`, unknown
fields, and defaults without an equivalent mapping are refused before dispatch.
Every result candidate retains its index, and tool calls or refusals without a
text-completion representation fail projection. These endpoints do not use the
response cache. Text prompts and choice text are
[guardrail](guardrails.md#execution) and
[payload-capture](../operate/observability.md#capture-behavior-and-bounds)
surfaces for unary and SSE results. Unsupported inspection surfaces, such as
token-ID prompts or `logprobs` under an output policy, refuse enforcing
guardrails.

Usage keeps cached and cache-write prompt token counts. On the text-completion
target, `total_tokens` must equal prompt plus completion tokens, and an
inconsistent total is a protocol error rather than accounted usage. Admission
estimates sum a prompt batch under one exact-tokenization budget, and the
context-window check uses its largest prompt. When a connector's generation
target is a native text completion profile,
[active health probes](provider-routing.md#credential-pools-and-limits) send a
short text completion instead of a chat request.

`decision` is a separate operation. On the OpenAI surface, the native
`openai-decisions` and `systemone` profiles accept text input with predicate or
noul, choice, and score questions, preserving names, order, typed choice values,
native probabilities, score scales, and OpenAI refusals. Images, box questions,
streaming, and unmodeled fields are excluded. The OpenAI shape is pinned to
`openai-go/v3` 3.78.0, and System One is pinned to TypeSafe OpenAPI 0.2.0. Exact
tuple certification is required before publishing either profile.

OpenAI Decisions can map to System One for a string input, choices whose typed
values do not collide as string keys, and score levels without separate
descriptions. Safety identifiers and other unmappable controls are refused.
System One requests cannot map to OpenAI because the frozen System One response
schema has no refusal variant, so that direction returns
`422 target_capability` before dispatch. Native System One remains available. No
chat-generated probabilities are used. Native answers must carry string choice
and score labels that match the request, and other answers fail validation.
Because these contracts have no provider-enforced output bound, token-capped
keys, budget groups, end users, routes, providers, and slots, along with
USD-capped keys, routes, providers, slots, and budget hierarchies, are refused
with `400 unbounded_work_budget` before dispatch. A
[caller-paid](../develop/accounting.md#caller-paid-budget-policy) route waives
only the USD caps, and only for caller-credential targets, while token caps
still apply. Route simulation and key inspection report the same
`unbounded_work_budget` diagnostic, including inherited hierarchy budgets and
named-route limits. Observed usage is still priced from the target's input and
output token prices, including for the `price` routing strategy, and accounted
on unlimited calls.

Former local evidence came from an endpoint ledger and a focused client suite,
both subtracted. No local or live qualification runs today, and that historical
evidence does not qualify unimplemented API families.

### OCR and Web search

`ocr` and `search` are separate unary operations. `POST /v1/ocr` binds its
route from the request body's `model`, while `POST /v1/search` carries no model
field and binds its route from the required `X-OLP-Route` header. A missing
header is refused with `400 missing_required_parameter` before dispatch. Both
operations also serve `POST /native/{dialect}/models/{model}` on strict routes,
and serve transformed routes through their reviewed mappings. Neither operation
uses the response cache.

The `mistral-ocr` profile (vendor Mistral) admits `model`, `document`, `id`,
`pages`, `include_image_base64`, `image_limit`, `image_min_size`,
`table_format`, `extract_header`, and `extract_footer`. Documents are inline
`document_url` or `image_url` values over `https:` or `data:` only. File
documents, annotations, `confidence_scores_granularity`, `include_blocks`, and
any other field are refused with `400` before dispatch. Results require `model`,
`pages[].index`, `pages[].markdown`, and `usage_info.pages_processed`, which
meters as media units. OCR follows the
[unbounded-work-budget](../develop/accounting.md#unbounded-work-and-incomplete-accounting) refusal rules of Decisions,
so token and non-exempt cost caps refuse with `400 unbounded_work_budget` before
dispatch, waived only for caller-paid cost-exempt routes.

The `azure-document-intelligence-ocr` profile (vendor Azure Document
Intelligence, an account-scoped endpoint the operator replaces with their
resource host, with credentials sent as `Ocp-Apim-Subscription-Key`) serves the
same frozen OCR contract through Azure Document Intelligence REST API
`2024-11-30`. Its native document admits `model`, exactly one of `urlSource`
(an `https:` URL the provider fetches) or `base64Source` (inline base64), and a
one-based `pages` selector such as `1-2,4`. The analyze call answers `202` and
the gateway polls the returned `Operation-Location` inside the same attempt and
route deadline, so the caller still sends one request and reads one response.
Polling retries are read-only and a document is never submitted twice. The
polling URL must stay on the bound provider origin or the attempt fails before
any credential is sent there. Cancelling the request stops the wait and makes a
best-effort delete of the upstream result, which only marks the result for
deletion because the provider offers no cancel operation, so a cancelled or
timed-out attempt stays billing-uncertain. Transformed serving maps the frozen
OCR document onto this dialect, converting zero-based `pages` to the one-based
selector, `https:` document and image URLs to `urlSource`, and base64 `data:`
documents to `base64Source`, and projects the provider markdown back into
`pages[].index` and `pages[].markdown`.

Provider-fetched documents are a route-level egress choice that applies to every
OCR target, Mistral included. An `https:` document or image URL is forwarded for
the provider to fetch only on a route whose revision sets
`provider_fetched_documents`. Without that choice the gateway refuses the
request before dispatch, while inline `data:` documents are always admitted on
their own contract terms. Routes that served `https:` documents before this gate
existed must set the flag when they next publish, keeping egress control with
the operator rather than the caller. The gateway never fetches document bytes
itself. `table_format` is refused because
[the upstream returns HTML tables](https://learn.microsoft.com/en-us/azure/ai-services/document-intelligence/concept/markdown-elements#table)
that do not map to the frozen table contract. The image-extraction and header or
footer controls accept only explicit `false`, and `document_name`, the caller
`id`, and every other unmappable field are refused with `400` before dispatch.
Usage meters the observed result pages as media units, and a result carrying no
page evidence stays billing-uncertain instead of free.

Transformed OCR serving also maps the frozen Mistral request to
`vertex-document-processing-ocr`, the Google Cloud Document AI profile on the
`vertex_ai` connector with vendor `google-vertex` (see the
[Vertex AI connector guide](providers/vertex.md)). Document AI is a separate
Google API from the Vertex AI publisher API. Its endpoint is
`https://LOCATION-documentai.googleapis.com/v1/projects/PROJECT/locations/LOCATION`,
where the connection's cloud region is the Document AI location, such as `us`
or `eu`, and each provider model names a processor ID. Calls go to
`processors/{processor}:process` with the bound connection's Google credential
(`adc` or `service_account`) and no other. The mapping carries only inline
`data:` documents with a base64 payload and a supported MIME type, plus `pages`,
which become a provider page selector. `https:` documents are refused before
dispatch because Document AI cannot fetch them and the gateway never fetches
document bytes. `document_name`, `id`, `include_image_base64`, `image_limit`,
`image_min_size`, `table_format`, `extract_header`, and `extract_footer` have no
Document AI equivalent and are refused before dispatch. The native dialect
additionally admits `gcsDocument` locations. A location is served only under one
of the connection's operator-configured `document_uri_prefixes`, and an empty
list grants no locations. Results project into the frozen page shape from the
document text and its page anchors. Observed pages meter as media units at the
target's `unit_price`, and a result with no observed pages stays
billing-uncertain rather than free.

The `perplexity-search` profile (vendor Perplexity) admits the frozen
`ApiSearchRequest` fields, namely a string `query` (arrays refused),
`max_results` from 1 to 20, `search_type`, `max_tokens`, `max_tokens_per_page`,
`country`, `search_language_filter`, an all-allow or all-deny
`search_domain_filter` of at most 20 entries, and the date and recency filters.
Transformed serving maps it to `tavily-search`, `exa-search`, `parallel-search`,
and `brave-search` (vendors Tavily, Exa, Parallel, and Brave, where Parallel
credentials arrive in `X-Api-Key` and Brave credentials in
`X-Subscription-Token`). The mappings always send `query` and `max_results`
(default 10), admit `search_type` only when absent or `web`, and refuse token
and locale controls with `400` rather than approximating them. Recency and
last-updated filters are refused on every target. Brave alone also carries a
paired `search_after_date_filter` and `search_before_date_filter` as a date
range, and a single-sided date filter is refused there too. Domain filters reach
Exa as root domains expanded with `*.d` subdomains and reach Parallel verbatim
(`.tld` and domain/path entries allowed, `-` stripped on deny), while Tavily
and Brave carry none. Projections answer with the Perplexity shape, a
gateway-minted `search_<hex>` ID (upstream IDs never cross), `""` for absent
titles, joined highlight or excerpt snippets or Brave descriptions, and
per-target dates. A result page beyond `max_results` is a protocol violation.
Search results carry no usage and settle like moderation, so a validated result
completes its evidence and the attempt is charged at the target's `per_request`
price.

The `brave-search` profile serves its upstream over a GET query transport. Its
native document admits `q`, `count`, `offset`, `freshness`, and `result_filter`
(only `web`). The gateway lowers the document to bounded query parameters with
decorations off and sends no body. Strict native ingress still takes the JSON
document at `POST /native/{dialect}/models/{model}`, and only the wire to Brave
uses query parameters. Offset pagination is native-only because the frozen
source contract has no offset field, so transformed Brave calls always read the
first page.

Former local evidence came from an endpoint ledger, a focused client suite, and
a browser journey, all subtracted. No local or live qualification runs today,
and that historical evidence does not qualify unimplemented API families.
Google's Custom Search JSON API is excluded because the provider discontinued
new enrollment and the API sunsets on 2027-01-01.

### Files, realtime, and provider-retained state

OpenAI-shaped `/v1/files*` and `/v1/batches*` serve official OpenAI, Azure
OpenAI, Gemini (through the managed adapter), and the reviewed compatible batch
vendors Groq, Mistral, and Together (see [Files and batches](#files-and-batches)
below for the complete batch and file contract across OpenAI, Anthropic, Gemini,
Vertex AI, and Bedrock). `POST /v1/files` carries no model field, so it requires
an `X-OLP-Route` header naming a route allowed for the file purpose (`batch`,
`fine-tune`, `assistants`, `evals`, or `user_data`). Every other endpoint
resolves the route through the resource mapping or the `model` field. Uploads
stream multipart bodies to the provider under the shared request and spool
bounds, and file bytes are never stored in the gateway.

Every retained provider resource gets a gateway-owned identifier backed by a
metadata-only `provider_resources` row pinned to the selected provider revision,
slot, and credential. That includes OpenAI and Azure OpenAI files (`file_`),
batches (`batch_`), stored Responses (`resp_`), fine-tuning jobs, vector stores
and vector store files or batches, conversations and items, containers and
container files, and evals and eval runs, as well as Anthropic files, batches,
and skills, Gemini files, batches, context caches, and managed agents, and
Vertex AI tuning jobs. List, retrieve, update, content, delete, and cancel calls
resolve that mapping, dispatch to the pinned credential, and rewrite identifiers
between local and upstream forms, so another key's identifier is
indistinguishable from a missing one. No cross-provider fallback applies. A
revoked pinned credential fails with `provider_resource_credential_unavailable`
rather than rerouting.

Stateful Responses fields (`store`, `background`, `previous_response_id`),
`POST /v1/responses/compact`, all retained resource families, and native
Anthropic, Gemini, and cloud batch resource APIs require the key policy
`allow_provider_state`, because provider state may retain user content upstream.
`previous_response_id` must reference a response stored under the same key and
route and is rewritten to the pinned upstream identifier. Streaming terminal
events are rewritten so callers only ever see gateway-owned response IDs.

Conversations, owned files, vector stores, and prior Responses must share one
compatible historical owner, route, target, connection, model, and credential
contract. Native Responses `file_search` inputs, result references, and
streaming citations use gateway IDs. History stays provider-side, and compaction
via `POST /v1/responses/compact` (requiring `X-OLP-Route` and an OpenAI or Azure
OpenAI route) is separately priced. Gemini `cachedContent` likewise requires a
gateway cache ID with a matching immutable model, token quantity, credential,
and authoritative expiry. Raw native cache IDs and unqualified provider file
URIs refuse before dispatch. Cached tool declarations and configuration are
unsupported, so declare supported client tools on each generation request.
Caches without a verified tool-free contract must be recreated before generation
can use them. State-bearing requests bypass response-cache replay. Known cached
tokens count in context, token, and monetary admission, while unknown history,
file, and retrieval input quantities refuse applicable consumption caps.

`GET /v1/realtime` is a WebSocket endpoint qualified for official OpenAI and
Azure OpenAI targets. The `model` query parameter is an OLP route slug, the
route must allow `realtime`, and the target must hold a certified
`openai`/`realtime` capability. Target and slot are selected and reserved once
before the upgrade, with no failover afterwards. The relay enforces the
configured maximum event size and a one-hour session bound, propagates ping and
close frames, and rechecks the presented API key every five seconds, closing
with a policy violation on revocation or expiry. Terminal usage events feed
accounting when the provider sends them, while sessions that end without usage
are recorded as billing-uncertain.

`POST /v1/realtime/calls` opens a governed WebRTC session on official OpenAI
only. The caller posts the OpenAI calls form (an SDP offer plus an optional
session document whose `model` names a strict route slug) or a raw SDP body with
a `model` query parameter. A session whose consumption no token or cost budget
can bound refuses applicable caps with `400 unbounded_work_budget`, matching the
other unbounded session paths. Request, concurrency, and bounded session cost
are reserved before the provider session exists, and a server control channel to
the provider session is established before the SDP answer returns. The answer
carries a gateway session identity in `Location`, never the provider call
identity and never a raw provider client secret. The gateway rechecks the key
every five seconds and closes the provider session on revocation, on provider
credential loss, and at the one-hour bound. Observed provider usage settles
exactly once. Sessions that end without trustworthy usage stay
billing-uncertain, and losing the control channel triggers a close attempt,
holds the exposure, and refuses new WebRTC work for that key while durable
exposure remains unresolved. Without a durable ledger, the process-local hold
lasts until the session bound. Bounded session cost is reserved through the
durable debit ledger before the provider session exists and settled exactly once
by the usage pipeline, while request and concurrency capacity ride the shared
realtime lease path. A gateway crash after dispatch leaves the reserved exposure
in the ledger and inside the key budgets until it is reconciled, so a lost
session never reads as free. Installations without shared state settle on the
lease path alone. SDP and media payloads are never logged or stored. The target
must hold a certified `realtime`/`openai`/`webrtc` capability, which is distinct
from the WebSocket `realtime` mode. Azure WebRTC is not supported yet and its
ledger tuple is excluded until its own transport contract is certified.

Native duplex profiles serve at `GET /native/{dialect}/sessions/{route}` on
strict routes, with dialect `xai-realtime` for xAI, `vertex-realtime` for
Vertex AI Live, and `bedrock-realtime` for Bedrock Nova Sonic. The caller always
speaks WebSocket to the gateway and sends that provider's native setup frame
first. xAI and Vertex AI Live continue over WebSocket to the provider, while
Nova Sonic continues over its SigV4-signed bidirectional event stream, with each
WebSocket text frame carrying one native JSON event in either direction. The
route must allow `realtime` and the target must hold a certified
`native`/`realtime` capability for the profile. Frames stay opaque apart from
the setup fields the profile gates and the usage evidence it extracts, so
guardrails that cannot inspect them refuse the route, and no response caching
ever applies. None of the three profiles declares transcription-only sessions,
and a setup asking for one is refused before any provider dispatch. Vertex AI
Live setup names the route as its model and is bound to the configured project,
location, and publisher model, and session resumption handles are refused
because sessions are connection-scoped. The gateway mints no session identifier
and exposes no call that can address one later, so a session belongs only to the
key and route that opened its connection. Admissions, the one-hour bound,
five-second authority rechecks, and exactly-once terminal settlement match
`GET /v1/realtime`. xAI settles from per-response usage including its cumulative
billable audio seconds, Vertex AI Live settles from per-turn usage metadata, and
Nova Sonic settles from the cumulative token totals carried by its usage events.
A session whose stream ends without that terminal evidence settles as
billing-uncertain and never as zero cost.

Rolling any OCR or search transport, realtime profile, or pass-through family
back is an ordinary publication change, for example retiring its routes or
disabling its providers in a new release. New work then refuses at admission
with nothing dispatched, and sessions that already settled stay in the usage
history unchanged, debit records included.

### Anthropic surface

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /anthropic/v1/messages` | generation | translated | native | translated | translated / native | translated / native | translated | refused |
| `POST /anthropic/v1/messages/count_tokens` | token_count | translated | native | translated | translated | translated | translated | refused |
| `POST /anthropic/v1/files`, `GET /anthropic/v1/files`, `GET /anthropic/v1/files/{id}`, `DELETE /anthropic/v1/files/{id}`, `GET /anthropic/v1/files/{id}/content` | batch / native file | refused | qualified | refused | refused | refused | refused | refused |
| `POST /anthropic/v1/messages/batches`, `GET /anthropic/v1/messages/batches`, `GET /anthropic/v1/messages/batches/{id}`, `DELETE /anthropic/v1/messages/batches/{id}`, `POST /anthropic/v1/messages/batches/{id}/cancel`, `GET /anthropic/v1/messages/batches/{id}/results` | batch | refused | qualified | refused | refused | refused | refused | refused |
| `/anthropic/v1/skills*` and `/anthropic/v1/skills/{skill_id}/versions*` (create, list, get, delete) | skill | refused | qualified | refused | refused | refused | refused | refused |
| `GET /anthropic/v1/models`, `GET /anthropic/v1/models/{id}` | model_list, model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |

On `POST /anthropic/v1/messages`, `vertex_ai` hosts Anthropic Messages natively
through the `vertex-anthropic` profile (`vertex-2023-10-16`) and translates
through `vertex-gemini` or automatic Vertex, while `bedrock` hosts Anthropic
Messages natively through the `bedrock-anthropic-invoke` profile
(`bedrock-2023-05-31`) and translates through Bedrock Converse. Neither
`vertex-anthropic` nor `bedrock-anthropic-invoke` serves `token_count`, so
`POST /anthropic/v1/messages/count_tokens` on `vertex_ai` and `bedrock` uses
translated targets. Anthropic files require `Anthropic-Beta: files-api-2025-04-14`
when the header is supplied, and Anthropic files, batches, and skills all
require `allow_provider_state` on the calling key.

A Messages request may carry `system` messages between turns, as Claude Code
sends them. The fields of an Anthropic beta travel with the `Anthropic-Beta`
header, so an Anthropic upstream receives the caller's header when the route
hands it the caller's own request on a strict route through the
`anthropic-messages` profile, or on a transformed route to an automatic
Anthropic provider or a profiled provider whose profile declares
`Anthropic-Beta` as a semantic header, such as `anthropic-messages` or
`vertex-anthropic`. A request translated from another dialect carries no
`Anthropic-Beta` header, and neither does `bedrock-anthropic-invoke`. A provider
whose profile publishes `Anthropic-Beta` among its semantic headers receives its
own published betas and the caller's betas deduplicated into one list. A strict
route refuses a caller's list that differs from the published value with
`semantic_header_conflict` because its profile binds one value. A header that is
not a valid header value, is longer than 2048 bytes, or exceeds 2048 bytes once
joined with a provider's published betas is refused wherever it could be
forwarded. The Anthropic SDKs add `?beta=true` to beta namespace calls, and a
strict route consumes it since it selects no behavior, while refusing any other
query setting that the profile does not declare. See
[Client compatibility](clients.md#claude-code).

### Gemini surface

Replace `{version}` on the Gemini model and batch endpoints with `v1` or
`v1beta`. Decisions, Interactions, Live, managed agents, context caches, and
files use `v1beta`.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /gemini/{version}/models`, `GET /gemini/{version}/models/{model}` | model_list, model_get | gateway | gateway | gateway | gateway | gateway | gateway | gateway |
| `POST /gemini/{version}/models/{model}:generateContent` | generation | translated | translated | native | native | translated | translated | refused |
| `POST /gemini/{version}/models/{model}:streamGenerateContent` | generation | translated | translated | native | native | translated | translated | refused |
| `POST /gemini/{version}/models/{model}:countTokens` | token_count | translated | translated | native | native | translated | translated | refused |
| `POST /gemini/{version}/models/{model}:embedContent` | embeddings | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/{version}/models/{model}:batchEmbedContents` | embeddings | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/{version}/models/{model}:batchGenerateContent` | batch | refused | refused | qualified | refused | refused | refused | refused |
| `GET /gemini/{version}/batches`, `GET /gemini/{version}/batches/{name}`, `POST /gemini/{version}/batches/{name}:cancel`, `DELETE /gemini/{version}/batches/{name}` | batch | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/upload/v1beta/files`, `POST /gemini/upload/{session}`, `GET /gemini/v1beta/files`, `GET /gemini/v1beta/files/{name}`, `DELETE /gemini/v1beta/files/{name}`, `GET /gemini/download/v1beta/files/{name}:download` | batch / native file | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/v1beta/decisions` | decision | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/v1beta/interactions`, `GET /gemini/v1beta/interactions/{id}`, `DELETE /gemini/v1beta/interactions/{id}`, `POST /gemini/v1beta/interactions/{id}/cancel` | generation | refused | refused | qualified | refused | refused | refused | refused |
| `GET /gemini/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent`, `GET /ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent` | realtime | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/v1beta/agents`, `GET /gemini/v1beta/agents`, `GET /gemini/v1beta/agents/{id}`, `DELETE /gemini/v1beta/agents/{id}` | agent_resource | refused | refused | qualified | refused | refused | refused | refused |
| `POST /gemini/v1beta/cachedContents`, `GET /gemini/v1beta/cachedContents`, `GET /gemini/v1beta/cachedContents/{id}`, `PATCH /gemini/v1beta/cachedContents/{id}`, `DELETE /gemini/v1beta/cachedContents/{id}` | context_cache | refused | refused | qualified | refused | refused | refused | refused |

Gemini Interactions requires the `gemini-interactions` profile and serves unary
and streaming generation plus retained interaction get, delete, and cancel.
Gemini Live requires the `gemini-live` profile and serves bidirectional
WebSocket realtime sessions under both `/gemini/ws/...BidiGenerateContent` and
`/ws/...BidiGenerateContent` with the same one-hour session bound and
unbounded-budget checks as other realtime surfaces. Gemini managed agents
require a strict route with the `gemini-agents` profile, `allow_provider_state`,
and an `Idempotency-Key` of at most 128 bytes. Gemini context caches require
`gemini-generation` (or automatic `gemini`) and `allow_provider_state`, and
native Gemini files, resumable uploads, downloads, and batches require
`allow_provider_state` on transformed routes.

To publish a Decisions route, use the Google Gemini provider with the
`gemini-decisions` profile and a Gemini API key. Certify `gemini-decision-1`
for the `decision` operation on the `gemini` surface in `unary` mode. Use the
published route slug as `model` in the request and authenticate with your
OLP inference key:

```sh
curl "$OLP_URL/gemini/v1beta/decisions" \
  -H "X-Goog-Api-Key: $OLP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "support-decisions",
    "input": "The user cannot sign in.",
    "questions": {
      "account_access": {
        "type": "noul",
        "instructions": "Is the user reporting a sign-in problem?"
      }
    }
  }'
```

The profile accepts Gemini's keyed `noul`, `choice`, and `score` questions
with text, image, audio, video, or PDF input. Inline media is subject to the
gateway's media limits. Enable `provider_fetched_documents` on the route for
HTTPS inputs, including Gemini Files API URIs. The native response preserves
answer keys, probabilities, score legends, safety feedback, and provider usage.
Gemini Decisions uses its own wire format and follows the same budget
admission and usage accounting as other `decision` operations. See the
[Gemini API reference](https://aistudio.google.com/api-reference/decisions/create-decision).

The two Gemini embedding endpoints are served only on
[strict routes](provider-routing.md#route-fidelity) whose targets hold the
certified `embeddings` capability and use a Gemini API profile, where
`gemini-generation` serves `embedContent` and `gemini-batch-embeddings` serves
`batchEmbedContents`. The request reaches the provider unchanged apart from
model rewriting. No qualified mapping links Gemini embedding requests to another
provider's embedding dialect, so other targets are refused with
`target_capability`, and transformed routes refuse both endpoints with HTTP 400.
To reach Gemini, Vertex AI, or Bedrock embeddings from another dialect, use
`POST /v1/embeddings` on a transformed route.

### Bedrock surface

The Bedrock surface accepts requests shaped for the AWS Bedrock Runtime SDK at
`{gateway}/bedrock`, where `{model}` in each path is always an OLP route slug
rather than a provider model ID. Incoming AWS SigV4 is never gateway
authentication, so clients must present `X-OLP-API-Key` (or an ordinary
non-SigV4 `Authorization: Bearer` key), and inbound `Authorization` and
`X-Amz-*` headers are stripped before the gateway re-signs with the configured
Bedrock credential.

| Endpoint | Operation | openai | anthropic | gemini | vertex_ai | bedrock | azure_openai | openai_compatible |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `POST /bedrock/model/{model}/converse` | generation | refused | refused | refused | refused | native | refused | refused |
| `POST /bedrock/model/{model}/converse-stream` | generation | refused | refused | refused | refused | native | refused | refused |
| `POST /bedrock/model/{model}/invoke` | bedrock_invoke | refused | refused | refused | refused | qualified | refused | refused |
| `POST /bedrock/model/{model}/invoke-with-response-stream` | bedrock_invoke | refused | refused | refused | refused | qualified | refused | refused |

Only native Bedrock targets holding the exact certified ingress mode qualify.
Unary Converse responses are validated through the same decoder as translated
traffic before their bytes are returned to the caller. InvokeModel requires a
transformed route and supports only the model families OLP qualifies, which are
Anthropic Claude generation, Titan embeddings, and Titan image generation, each
with model-specific response validation. Other model IDs fail with a clear
protocol error. Streaming responses validate AWS event-stream CRC and frame size
and re-encode each frame, so a malformed or oversized frame terminates the
stream rather than forwarding unchecked bytes. Usage is normalized from Bedrock
metadata and provider terminal events into standard accounting.

### Native surface

Provider dialects without a cross-vendor counterpart, duplex realtime protocols,
cloud batch control planes, Vertex supervised tuning, and governed pass-through
routes live under `/native/`.

| Endpoint | Operation | Route kind | Qualified providers and profiles |
| --- | --- | --- | --- |
| `POST /native/{dialect}/models/{model}` | generation or unary operation | strict | Native generation dialects (`mistral-fim`, `cohere-chat-v2`, plugin generation dialects) and registered unary operation dialects (Cohere Embed and Rerank v2, OCR, Search, Decisions) |
| `GET /native/{dialect}/sessions/{route}` | realtime | strict | Native duplex WebSockets on `xai-realtime` (`openai_compatible` xAI), `vertex-realtime` (`vertex_ai`), and `bedrock-realtime` (`bedrock`) |
| `POST /native/{dialect}/batch-jobs`, `GET /native/{dialect}/batch-jobs`, `GET /native/{dialect}/batch-jobs/{id}`, `POST /native/{dialect}/batch-jobs/{id}/cancel` | batch | transformed | Cloud batch jobs on `vertex_ai` (`vertex-batch-prediction`) and `bedrock` (`bedrock-batch-inference`) with operator-configured `batch_storage` |
| `{METHOD} /native/{dialect}/routes/{route}/{path...}` | passthrough | passthrough | Single bound connection with explicit method, path, query, and header allowlists (uncertified) |
| `POST /native/vertex-tuning/jobs`, `GET /native/vertex-tuning/jobs`, `GET /native/vertex-tuning/jobs/{id}`, `POST /native/vertex-tuning/jobs/{id}/cancel` | fine_tuning | transformed or strict | Vertex AI supervised fine-tuning jobs on `vertex_ai` (`vertex-gemini` profile) with `allow_provider_state` |

## What translation drops or refuses

The gateway prefers a clear refusal over a silent success with different
semantics. When a request carries something the target protocol cannot express,
the attempt fails with a protocol error rather than being quietly downgraded.
The exceptions are noted as drops.

Unknown vendor fields are preserved as source extensions and replayed for native
providers. Cross-protocol requests carrying unsupported extensions are refused,
and unsupported response and stream extensions are dropped.

A system message between turns, which Claude Code sends, is kept in place where
the target can place it and refused where it cannot. Anthropic, Gemini, and
Bedrock Converse each have one system field ahead of the conversation, so a
request translated to them with a late system message fails with
`unsupported_parameter` instead of moving the message to the front.

Keep these rules aligned with the
[protocol suites](../../internal/domain/protocols/canonical_test.go) when
semantics change.

### Anthropic providers

Translation to Anthropic maps a `json_schema` `response_format` onto Anthropic's
`output_config`. The schema object is required, and `strict` is honored only
when it is set to true. Translation treats `text` as no structured output.
`json_object` and unknown formats are still refused, and whether a given model
accepts the translated field is provider and model dependent, so unsupported
combinations fail clearly rather than silently degrading. The
[Anthropic encoder](../../internal/domain/protocols/canonical_anthropic.go)
enforces these rules. Cached-input usage, provider request IDs, media parts, and
oversized-response bounds remain part of the shared contract.

Beyond that, translating into Anthropic Messages refuses a request that uses a
participant `name` on a message, a deterministic `seed`, more than one
candidate, a tool-result message without its tool-call ID, an image with an
explicit detail level, or an input audio, input file, or refusal content part. A
missing maximum output token count is also refused because Anthropic requires
one. These checks live in the
[Anthropic encoder](../../internal/domain/protocols/canonical_anthropic.go). The
[connector capability rules](../../internal/connectors/capability.go) keep token
counting unary and bind the selected transport mode.

### Gemini and Vertex AI providers

Gemini holds the shared contract with no exemptions. Translation refuses
parallel tool-call selection, a participant `name`, a system message that
appears after the conversation has started, a tool result whose content is not
text, and, as with Anthropic, input audio, input file, and refusal parts, or an
image with an explicit detail level. An image without a MIME type is refused
rather than guessed. These checks live in the
[Gemini encoder](../../internal/domain/protocols/canonical_gemini.go), while the
[connector capability rules](../../internal/connectors/capability.go) enforce
unary token counting and refuse asynchronous generation. Preserved counting
requests are validated after model rewriting as well.

Gemini takes a schema in two kinds of field. `parameters` and `responseSchema`
take an OpenAPI subset and refuse any member outside it that OpenAI and
Anthropic clients routinely send (`$schema`, `additionalProperties`, `$ref`,
`const`, `oneOf`, a `type` that is a list, or an `enum` of anything other than
strings). `parametersJsonSchema` and `responseJsonSchema` take JSON Schema as it
is, as Google's own SDKs send it. A translated request puts a tool or response
schema in the subset field when every member fits the subset, so such a schema
reaches Gemini as it always did, and in the JSON Schema field when any member
does not, so the schema reaches the model whole without being rewritten or
trimmed. The other direction rewrites only what the two dialects spell
differently, turning Gemini's uppercase type names (`OBJECT`, `STRING`) into
lowercase JSON Schema names.

Gemini reports a `STOP` finish reason even when a candidate contains only
function calls. The gateway corrects that to a tool-call finish reason so agent
loops keyed on tool calls do not stop early. This is a deliberate rewrite rather
than pass-through behavior.

Gemini also serves embeddings natively, turning a single string input into
`embedContent` and an array of strings into `batchEmbedContents`, with
`dimensions` mapped to `outputDimensionality`. Token-array input, more than 100
inputs, and unsupported encodings are refused. Vertex AI embeddings use the
publisher `predict` endpoint with at most five text inputs and report the summed
`statistics.token_count` as input usage when the upstream returns it. Both
reject provider responses that do not carry a numeric embedding vector for every
requested input. See the [Vertex AI connector guide](providers/vertex.md) for
Vertex AI profiles (`vertex-gemini`, `vertex-anthropic`, `vertex-openai`,
`vertex-realtime`, `vertex-batch-prediction`, `vertex-document-processing-ocr`,
and `vertex-rag-engine`), authentication, and tuning details.

### Bedrock providers

For translation into Bedrock Converse, a `json_schema` `response_format` maps
onto Converse `outputConfig`, using the schema as compact JSON text plus the
required schema name, for both Converse and ConverseStream. The `json_object`
format and unknown formats remain refused. Whether a given model honors the
field is provider and model dependent, and unsupported combinations still fail
clearly. Reported cache-read and cache-write input tokens are normalized into
usage, while canonical provider response IDs remain unsupported. Unary responses
have the shared response-byte limit, and event lengths are checked before AWS
event-stream decoding allocates the advertised body.

Generation and token-count inputs accept inline base64 PNG, JPEG, GIF, and WebP
images. Remote image URLs, explicit image-detail controls, unsupported formats,
invalid base64, and images in system instructions or tool results are refused.
The shared inline-media admission limits also apply. Image parts inside
generation inputs do not grant separate media capabilities.

Bedrock also serves several qualified operations beyond Converse:

| Operation | Profile or model family | Contract |
| --- | --- | --- |
| `embeddings` | `amazon.titan-embed-text-*` via InvokeModel | Accepts a single string input and requires `embedding` plus a nonnegative `inputTextTokenCount`. |
| `image_generation` | `amazon.titan-image-generator-*` via InvokeModel | Uses `taskType: TEXT_IMAGE` with one to four images in `1024x1024`, `768x768`, or `512x512` sizes and `b64_json` output. |
| `image_generation` | `stability.sd3-5-large-*`, `stability.stable-image-core-*`, `stability.stable-image-ultra-*` | Returns one image per request with `1024x1024`, `1536x1024`, or `1024x1536` mapped to aspect ratios and `png`, `jpeg`, or `webp` output, mapping moderation refusals to `content_filter`. |
| `speech` | Amazon Polly engines (`standard`, `neural`, `long-form`, `generative`) on `POST /v1/audio/speech` | Calls Amazon Polly in the provider's AWS region with a Polly voice ID, returning `mp3` or `opus` audio and metering billed characters from `x-amzn-RequestCharacters` (see [vendor media](providers/media.md)). |
| `rerank` | Bedrock rerank models (such as `amazon.rerank-v1:0` and `cohere.rerank-v3-5:0`) on `POST /v1/rerank` | Uses the regional Bedrock Agent Runtime Rerank API, refuses `truncation`, and meters one query per hundred documents. |
| `retrieval` | `bedrock-knowledge-bases` profile | Serves governed retrieval bindings at `POST /v1/retrieval/{binding_id}/query` (see the [retrieval guide](retrieval.md)). |
| `generation` (Anthropic) | `bedrock-anthropic-invoke` profile (`bedrock-2023-05-31`) | Hosts the Anthropic Messages dialect natively over Bedrock InvokeModel. |
| `realtime` | `bedrock-realtime` profile (Amazon Nova Sonic) | Serves bidirectional WebSocket sessions at `GET /native/bedrock-realtime/sessions/{route}` over SigV4-signed event streams. |
| `batch` | `bedrock-batch-inference` profile | Runs asynchronous cloud batch jobs at `/native/bedrock-batch-inference/batch-jobs` with operator-configured `s3://` `batch_storage` and IAM `role_arn`. |

Other Bedrock models and unsupported media operations (image edits and
variations, transcription, translation, and video) remain refused. The
[image-input tests](../../internal/domain/protocols/bedrock_test.go) and
[stream bounds tests](../../internal/domain/protocols/stream_translation_test.go)
cover the Converse image and streaming bounds rules.

The [Go Converse encoder](../../internal/domain/protocols/bedrock.go) refuses,
with an explicit protocol error, a request that asks for more than one
candidate, sets a deterministic seed, sets parallel tool-call selection, asks
for a structured response format other than `text` or a well-formed
`json_schema`, puts a name or tool-call metadata on a system instruction, or
gives a maximum output token count that does not fit Bedrock's limits. Tool
results must carry a tool-call ID and non-empty text content, must not carry
tool calls of their own, and only a tool-result message may carry a tool-call
ID. Tool names must be unique across the request and must be short, ASCII, and
free of punctuation other than `_` and `-`. A named tool choice must exist in
the tool list, and a tool choice of `"none"` cannot be combined with a non-empty
tool list. Non-finite temperature or `top_p` values and non-finite JSON numbers
inside tool arguments are refused too. The
[capability rules](../../internal/connectors/capability.go) keep token counting
unary and refuse asynchronous generation, and the
[request builder](../../internal/connectors/addressing.go) validates the model
ID or ARN.

See the [Bedrock connector guide](providers/bedrock.md) for authentication, SDK
retry policy, deadlines, and live tests.

### OpenAI-compatible and Azure OpenAI providers

Both are native on the OpenAI surface, though their certification path is
narrower than OpenAI's own. An OpenAI-compatible provider can be certified for
generation, embeddings, token counting, and moderation on the OpenAI surface,
plus rerank for the reviewed rerank vendors, OCR and Search for the reviewed
operation vendors, OpenAI-shaped files and batches for Groq, Mistral, and
Together, native duplex realtime for xAI, and the media operations of the
[reviewed media vendors](providers/media.md), while the Anthropic, Gemini, and
Bedrock surfaces are refused. Cohere rerank resolves to the official
`/v2/rerank` endpoint for the reviewed preset, whereas Voyage, Jina, Together,
and Infinity rerank post to the configured base plus `/rerank` and refuse
`truncation` except at Voyage. Jina receives the OpenAI `encoding_format` as its
`embedding_type`.

Azure OpenAI supports generation and token counting on OpenAI, Anthropic, and
Gemini surfaces, plus OpenAI-surface embeddings, moderation, and unary image
generation, speech, and transcription through deployment profiles (the Azure v1
profiles do not serve media, see [Azure media](providers/azure.md#media)). It
also supports the qualified OpenAI file, batch, WebSocket realtime, stored
Responses, compaction, fine-tuning, vector-store, and container paths listed
above, while WebRTC realtime, conversations, evals, image edits and variations,
audio translation, and video operations remain uncertifiable. See
[certification eligibility](../../internal/providers/probe/operations.go) and
the [Azure connector guide](providers/azure.md) for API-key and Microsoft Entra
authentication, token caching, and identity egress.

## Files and batches

OLP supports three batch styles across providers, namely native OpenAI-shaped
files and batches, managed adapters that project OpenAI-shaped `/v1/files` and
`/v1/batches` onto vendor-specific batch APIs, and native vendor or cloud batch
APIs under `/anthropic/`, `/gemini/`, and `/native/`.

| Provider and surface | Mode | Supported scope |
| --- | --- | --- |
| OpenAI and Azure OpenAI (`/v1/files`, `/v1/batches`) | Native or strict | Native and strict OpenAI-shaped files and batches. |
| Groq (`/v1/files`, `/v1/batches`) | Reviewed native | Native OpenAI-shaped file and batch endpoints under the reviewed `groq-files-batches` contract. |
| Together, Mistral, and Gemini (`/v1/files`, `/v1/batches`) | Managed adapter | OpenAI-shaped file uploads and batches on transformed routes (`together-files-batches`, `mistral-files-batches`, and `gemini-generation` or automatic Gemini). |
| Anthropic (`/anthropic/v1/files`, `/anthropic/v1/messages/batches`) | Native | Native Anthropic files and Messages batch jobs on transformed routes. |
| Gemini (`/gemini/upload/v1beta/files`, `/gemini/v1beta/files`, `/gemini/{version}/batches`, `:batchGenerateContent`) | Native | Native resumable uploads, Files list, get, delete, and download, and `batchGenerateContent` on transformed routes. |
| Vertex AI and Bedrock (`/native/{dialect}/batch-jobs`) | Cloud batch | Transformed-only create, list, get, and cancel through `vertex-batch-prediction` (`gs://`) and `bedrock-batch-inference` (`s3://` plus `role_arn`) with operator-configured `batch_storage`. |
| Fireworks | Declined | Result retrieval uses provider-returned signed URLs, which the egress contract forbids. |

Native Anthropic and Gemini resource APIs, cloud batch jobs, and non-batch
retained file uploads require the key's `allow_provider_state` permission.
Existing OpenAI-shaped batch file and batch permissions on transformed routes
remain unchanged, while strict OpenAI-shaped file uploads also require
`allow_provider_state`.

Anthropic file lists, Gemini file and batch lists, and cloud batch lists can
return an empty page with a continuation token after checking a limited number
of resource bindings. Anthropic batch lists scan past empty pages until they
find a visible batch, reach the end of the list, or return a timeout error. Use
`nextPageToken` as `pageToken` for Gemini. When `has_more` is true, pass
`last_id` as `after` for cloud batch jobs or as `after_id` for Anthropic. For an
Anthropic request with `before_id`, pass `first_id` as `before_id` to continue
in the same direction.

Managed OpenAI-shaped batches for Anthropic, Vertex AI, and Bedrock are refused
with `422 target_capability` because only Together, Mistral, and Gemini have
managed OpenAI file and batch adapters. Strict resource and batch routes are
available only to OpenAI and Azure OpenAI, and other families return
`422 target_capability`. Batch creation that cannot be bounded by an applicable
enforced token or cost budget returns `400 unbounded_work_budget`. Unknown
fields, unsupported output configuration, unsafe file references, out-of-prefix
cloud URIs, and unsupported timeout settings are refused rather than silently
ignored.

OpenAI-shaped batch input must be JSONL with unique `custom_id` values and one
supported endpoint. Each line's `body.model` must name the route or its pinned
provider model, and transformed OpenAI and Groq uploads rewrite route aliases to
that provider model before upload. Files uploaded before model qualification
must be uploaded again before creating new batches. Native Gemini inline batches
refuse file and cached-content references until owner and affinity binding is
available.

Native Gemini file-backed execution requires a qualifying JSONL manifest from
one complete `upload, finalize` chunk. General resumable uploads remain
available, though segmented or unqualified files cannot start batch inference.
File and cached-content references are refused in native and managed Gemini
batch items. Batches using caller-supplied credentials are refused before
dispatch because autonomous settlement cannot retain the caller's credential.

Managed Gemini output and error files are independent views of one provider
result object. Deleting one view preserves its sibling, and deleting the final
view returns `409 batch_settlement_pending` while settlement still needs the
evidence. Batch objects and their native result files are likewise protected
until settlement closes.

Creation requires `X-OLP-Route`, and follow-up requests derive route and
affinity from the owner-scoped gateway ID. Ambiguous creates are retained as
`submission_unknown`, returning `502` on create. A matching reconciliation can
adopt the job, while conflicting evidence returns `409` and is never blindly
redispatched. Result settlement records observed per-result usage against the
pinned line-operation price, and incomplete evidence remains uncertain and is
never zero-cost.

Settlement uses `(resource_id, HMAC(custom_id), result_kind)` as its unique
ledger identity. Deterministic per-result usage events set `Batch: true`,
applying the pinned `batch_multiplier`, while missing multipliers remain
unpriced. Usage and settlement progress commit idempotently across pages,
workers, and restarts. Conflicting duplicates stop settlement visibly, and
missing lines or usage retain gap and uncertainty evidence. Deduplication
survives resource deletion or expiry for accounting retention, and reading
results again never charges inference again.

Submission intent precedes dispatch. Identical retries while `submitting` or
`submission_unknown` return `409`. Adoption requires provider evidence, namely
the owned input file, Gemini `file_name`, Vertex `olp_submission` label, or
Bedrock `clientRequestToken`. File adoption refuses competing unresolved intents
sharing a credential and input file and rechecks ownership transactionally.
Anthropic and Gemini inline submissions remain unknown until `close_after`, then
become uncertain. Ambiguous evidence retries only within retention, with no
guessed job or blind resubmission.

## SDK and live-provider checks

JavaScript SDK smoke suites and dispatched live-provider checks were subtracted
with the test system. Nothing in this section is certified live. Validate an SDK
or provider change by running it against a gateway and recording the exact SDK,
provider, model, and capability revision used.

Provider-owned response, conversation, and file IDs are not universally
portable. Referencing a resource across independently selected providers is
unsupported unless the implementation pins the owning provider and credential
revision, as the media and provider-resource paths do. Unsupported stateful
fields are rejected by the surface's capability or translation policy, and route
failover cannot turn an upstream identifier into a gateway-owned resource. The
native and translated tables above and the endpoint registry are the maintained
support records. Update them with the conformance fixtures when semantics change
rather than introducing a second independently maintained matrix.

The ordinary console SDK examples were formerly executed verbatim by a pinned
SDK smoke harness. That harness was subtracted, so treat the examples as
starting points to verify against your gateway. They do not establish
live-provider or production-performance qualification.

## Provider qualification and support boundaries

| Tier | What it adds | Evidence required |
| --- | --- | --- |
| Reviewed preset | A catalog entry over an existing dialect and hosting composition | Vendor contract fixtures in the unit tier pinning documented endpoints, unsupported parameters, usage shape, stream terminal events, and error envelopes |
| First-party profile or codec | A new profile, hosting composition, authentication mode, or dialect codec in `internal/connectors`, `internal/domain/protocols`, or `internal/domain/operations` | Conformance fixtures in the unit tier, pricing coverage, and a recorded manual run against a stub or real provider |
| Plugin | Authentication and hosting around a built-in dialect supplied by an operator ([plugins](plugins.md)) | The plugin's own manifest review and origin approval |

No tier certifies a model automatically, because certification remains a live,
exact-tuple probe run by the server.

### Declined presets

Reviewed on 2026-10-05 against each vendor's own documentation. Custom endpoints
and plugins remain subject to the same contract and certification rules.

| Candidate | Reason | Source |
| --- | --- | --- |
| Lambda | The Lambda Inference API was sunset on 2025-09-25. | [Lambda announcement](https://deeptalk.lambda.ai/t/sunsetting-chat-sunsetting-inference/4744) |
| Hyperbolic | The serverless inference API is retired, leaving only GPU rental. | [Hyperbolic FAQ](https://www.hyperbolic.ai/docs/faq/inference-models) |
| GitHub Models | Retired on 2026-07-30, including its inference API. | [GitHub changelog](https://github.blog/changelog/2026-07-30-github-models-is-now-retired/) |
| Xinference | Its documented chat result reports token usage as `-1`, which OLP refuses as unmeterable. | [Xinference guide](https://inference.readthedocs.io/en/latest/getting_started/using_xinference.html) |
| Perplexity (withdrawn chat preset) | Sonar Chat Completions ended on 2026-09-27, and its replacement router is in private preview. Perplexity Search ships separately under `perplexity-search`. | [Perplexity migration](https://docs.perplexity.ai/docs/agent-api/migrate-from-sonar/overview) |
| Codestral endpoint | `codestral.mistral.ai` is absent from current Mistral documentation, and fill-in-the-middle is served at `api.mistral.ai` through the `mistral-fim` profile. | [Mistral FIM API](https://docs.mistral.ai/api/endpoint/fim) |

### First-party and media limits

SageMaker supports its OpenAI-compatible SSE path (vLLM, SGLang, and conforming
custom containers), not LMI JSON-lines streaming. watsonx.ai is transformed-only
and excludes Cloud Pak for Data. Azure Foundry uses `azure-v1-chat` and
`azure-v1-responses` on `services.ai.azure.com`, and no `azure-ai-inference`
profile ships. Native Cohere v2 and Mistral FIM preserve their declared
dialects. See [provider profiles](provider-profiles.md) and the focused
[SageMaker](providers/sagemaker.md), [watsonx](providers/watsonx.md),
[Azure](providers/azure.md), [Bedrock](providers/bedrock.md),
[Vertex AI](providers/vertex.md), and [media](providers/media.md) guides.

The 2026-10-05 review declined Bedrock Cohere embeddings (no usable metering),
Bedrock Nova Canvas (2026-09-30 end of life), Fireworks audio (2026-06-10
retirement), Mistral rerank (no API), and NVIDIA hosted rerank (raw logits and
model-specific endpoints do not fit the normalized score contract). NIM
embeddings remain served. The [media guide](providers/media.md) owns fal, Google
Cloud TTS, Imagen, and video declines. Reopening a decline needs a stable wire
contract, representable pricing and usage, and executable certification, and
generic endpoint registration does not reopen it.

## Governed pass-through

A pass-through route is a separate route kind for provider endpoints that the
gateway does not model. It is neither strict nor transformed, it advertises as
uncertified in route detail and capability views, and it binds exactly one
connection through its single target. Callers reach it at
`/native/{dialect}/routes/{route}/...` with an inference key that names the
route, and the namespace cannot shadow a modeled endpoint because modeled native
work lives under the `models` segment rather than `routes`.

The route declares its methods and anchored path segment patterns, where `*`
matches one nonempty segment. The gateway normalizes the path once before
matching and refuses encoded separators, dot segments, absolute URLs, fragments,
and alternate authorities before any dispatch. Query names and forwarded headers
come from explicit allowlists, and an unlisted query name is refused. Hop-by-hop
headers and every caller credential are stripped, only the bound connection
credential is injected, and provider redirects are refused instead of followed.
The `Content-Type` of a request body is forwarded as body framing alongside the
allowlisted headers. WebSocket upgrades are refused. Request bodies are buffered
within the gateway body bound before dispatch so the bound credential can sign
them where the connector requires signing. Responses stream within the
response-byte limit and route deadline, and an oversized or interrupted response
terminates delivery without signaling successful completion.

Fallback, mirroring, translation, response caching, and guardrails cannot attach
to this route kind, and a configuration that asks for them fails at
configuration plan time and again at draft validation and publication.
Publishing requires the operator acknowledgement for opaque provider state and
unmetered work, recorded on the route itself. A successful attempt is charged
the per-request price configured for the `passthrough` operation when one
exists, and otherwise it is recorded as explicitly unpriced rather than as
metered work. The gateway mints no resource handles or background jobs for an
opaque response, and this escape hatch never qualifies the underlying endpoint
as a modeled family. Live qualification against a real provider account remains
a separate gate for each bound connection.

## Product boundaries

OLP is a gateway using official vendor SDK clients, with no embeddable Python
SDK. Assistants and application key-value `/memory` storage are outside scope.
Callers cannot choose arbitrary upstream base URLs or use uncertified wildcard
models. Authentication and hosting extensions use reviewed plugins, and plugins
cannot define new dialects. Custom guardrails run as confined WASM or signed
webhooks rather than arbitrary in-process code. Durable request history
excludes prompts and responses, while explicit external capture and sealed
response caching have their own storage contracts.

Entra on-behalf-of authorization is qualified against a local Entra-shaped
fixture. Both JSON and MCP calls require a live, verified delegated user and a
matching middle-tier audience, while application tokens, generic workload
identity, wrong issuers, and cross-user sessions refuse before exchange or
dispatch. Sealed grants reuse the existing refresh and revocation owner.
Redirect refusal and exclusion of assertions and credentials from operator
records are covered. This does not claim live Entra tenant qualification. See
[the configuration contract](mcp-servers.md#entra-on-behalf-of-grants).
