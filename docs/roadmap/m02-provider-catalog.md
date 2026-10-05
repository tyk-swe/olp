# M2: Provider and catalog breadth

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | None | [M6](m06-cost-management.md), [M9](m09-api-surface.md) |

LiteLLM's largest advantage is breadth: about 170 provider pages and a model
cost map covering thousands of models. OLP reaches seven native connector kinds,
13 OpenAI-compatible presets and operator-installed plugins. This milestone
closes the breadth gap without giving up certification: every new provider is
reachable through one of three qualification tiers, and a signed reference
catalog replaces hand-entered model facts and prices.

## Outcome

- Every provider family LiteLLM serves in production is reachable through a
  reviewed preset, a first-party profile or codec, or a plugin, and each is
  certified per exact tuple before it serves traffic.
- Azure OpenAI and the major media vendors serve image, speech, transcription
  and video operations.
- A signed catalog of model facts and list prices ships with every release and
  feeds discovery, routing facts, token calibration and pricing.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Connector kinds | `openai`, `openai_compatible`, `anthropic`, `gemini`, `vertex_ai`, `bedrock`, `azure_openai`, `plugin` ([`kinds.go`](../../internal/providers/kinds.go)) | [About 170 providers](https://docs.litellm.ai/docs/providers) |
| Presets | 13 OpenAI-compatible presets ([configuration](../configuration.md#openai-compatible-provider-presets)) | Provider prefixes such as `xai/`, `ollama/`, `watsonx/` |
| Media | OpenAI natively; Vertex Imagen and Bedrock Titan image generation; media refused for compatible endpoints ([compatibility](../compatibility.md)) | Image, audio, video and OCR providers |
| Model metadata | Operator-declared model facts ([provider routing](../provider-routing.md#private-endpoints-headers-defaults-and-model-facts)); pricing sources an operator registers ([operations](../operations.md#accounting-delivery-and-shutdown)) | A [model cost map](https://docs.litellm.ai/docs/proxy/sync_models_github) fetched from GitHub at startup |

## Qualification tiers

| Tier | What it adds | Evidence required |
| --- | --- | --- |
| Reviewed preset | A catalog entry over an existing dialect and hosting composition | A vendor contract test pinning documented endpoints, unsupported parameters, usage shape, stream terminal events and error envelopes |
| First-party profile or codec | A new profile, hosting composition, authentication mode or dialect codec in `internal/connectors`, `internal/protocols` or `internal/operations` | Conformance fixtures, connector composition tests, a live-provider test behind `liveproviders`, and pricing coverage |
| Plugin | Authentication and hosting around a built-in dialect, supplied by an operator ([plugins](../plugins.md)) | The plugin's own manifest review and origin approval |

No tier certifies a model: certification stays a live, exact-tuple probe run by
the server.

## Scope

### M2.1 Reviewed presets

Add presets for vendors that document an OpenAI-compatible Chat Completions,
Responses or embeddings API. Each preset ships only after its contract test
passes against the vendor's documentation, which the preset records as
`documentation_url`. Candidates, in priority order:

1. **Frontier and fast inference:** xAI, Cerebras, SambaNova, Nebius AI Studio,
   Novita AI, NVIDIA NIM, Hyperbolic, Featherless, Lambda, Baseten.
2. **Regional and open-model platforms:** Moonshot AI, Alibaba Cloud Model
   Studio (DashScope), Z.ai, MiniMax, Volcengine Ark, Scaleway, OVHcloud AI
   Endpoints, Nscale.
3. **Data platforms and aggregators:** Databricks Model Serving, Snowflake
   Cortex, Cloudflare Workers AI, GitHub Models, Vercel AI Gateway.
4. **Self-hosted runtimes:** Ollama, LM Studio, llama.cpp server, Docker Model
   Runner, Hugging Face TGI, Xinference. Like `vllm`, these presets start
   unauthenticated with a placeholder endpoint and rely on the
   [egress allowlists](../configuration.md#provider-egress-policy) for private
   addresses.

Presets also gain an optional `profile_id`, so a preset whose vendor contract
is exact can serve strict routes without the operator choosing a profile.

#### Declined candidates

Reviewed on 2026-10-05 against each vendor's own documentation. A declined
vendor stays reachable through **Custom endpoint** or a plugin.

| Candidate | Reason | Source |
| --- | --- | --- |
| Lambda | The Lambda Inference API was sunset on 2025-09-25. | [Lambda announcement](https://deeptalk.lambda.ai/t/sunsetting-chat-sunsetting-inference/4744) |
| Hyperbolic | The serverless inference API is retired; only GPU rental remains. | [Hyperbolic FAQ](https://www.hyperbolic.ai/docs/faq/inference-models) |
| GitHub Models | Retired on 2026-07-30, including its inference API. | [GitHub changelog](https://github.blog/changelog/2026-07-30-github-models-is-now-retired/) |
| Hugging Face TGI | The repository is archived and in maintenance mode; `GET /v1/models` is not a list. | [TGI repository](https://github.com/huggingface/text-generation-inference) |
| Xinference | Its documented chat result reports token usage as `-1`, which OLP refuses as unmeterable. | [Xinference guide](https://inference.readthedocs.io/en/latest/getting_started/using_xinference.html) |
| Perplexity (withdrawn preset) | Sonar Chat Completions ended on 2026-09-27; its replacement router is in private preview. | [Perplexity migration](https://docs.perplexity.ai/docs/agent-api/migrate-from-sonar/overview) |
| Codestral endpoint | `codestral.mistral.ai` is absent from current Mistral documentation; fill-in-the-middle is served at `api.mistral.ai` through the `mistral-fim` profile. | [Mistral FIM API](https://docs.mistral.ai/api/endpoint/fim) |

### M2.2 First-party profiles and codecs

Cloud platforms and APIs that no existing composition expresses get first-party
support:

| Target | Work |
| --- | --- |
| Azure AI Foundry models (non-OpenAI models) | An `azure-ai-inference` profile on the `azure_openai` kind with API-key and Entra authentication |
| Vertex AI partner and open models | A `vertex-openai` profile for Vertex's OpenAI-compatible Chat Completions endpoint, reusing Vertex project, location and token handling |
| Amazon SageMaker endpoints | A `sagemaker` kind using the existing SigV4 signing, with profiles for the OpenAI messages format served by the vLLM, TGI and LMI containers |
| IBM watsonx.ai | An IAM token-exchange authentication mode and a `watsonx-chat` codec |
| Cohere Chat v2 | A `cohere-chat-v2` codec that preserves documents and citations natively |
| Mistral fill-in-the-middle | A `mistral-fim` native dialect served at `/native/mistral-fim/models/{route}` |
| Bedrock | Rerank through the Bedrock Agent Runtime, Cohere embeddings, and Nova and Stability image models |

Each addition extends the [capability rules](../../internal/connectors/capabilities.go)
and [certification eligibility](../../internal/providers/kinds.go) explicitly;
no operation becomes certifiable by default.

#### Settled during implementation

Reviewed on 2026-10-05 against each vendor's own documentation.

| Target | Outcome | Source |
| --- | --- | --- |
| Amazon SageMaker endpoints | A `sagemaker` kind on SageMaker's [OpenAI-compatible path](https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html), which routes by endpoint and inference component and streams server-sent events, with bearer tokens signed from AWS credentials. It serves the vLLM and SGLang containers and custom containers implementing that path. The LMI container streams JSON lines, not server-sent events, and is not supported. | [SageMaker AI guide](https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html) |
| IBM watsonx.ai | A `watsonx` kind adapting Chat Completions to the watsonx chat API, with an `ibm_iam` mode. It serves transformed routes only; watsonx.ai software on Cloud Pak for Data is not supported. | [watsonx.ai chat API](https://dataplatform.cloud.ibm.com/docs/content/wsj/analyze-data/fm-api-chat.html?context=wx) |
| Bedrock rerank | Rerank models serve `/v1/rerank` through the Agent Runtime's Rerank API, metered in the queries AWS bills, a query per hundred documents. | [Rerank API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Rerank.html), [rerank pricing](https://docs.aws.amazon.com/bedrock/latest/userguide/rerank-pricing.html) |
| Bedrock Cohere embeddings | Declined. Cohere Embed on Bedrock reports no usage in its response, and the `X-Amzn-Bedrock-Input-Token-Count` header is absent from the InvokeModel reference, so OLP could not meter it. | [InvokeModel](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html), [Cohere Embed v4](https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-embed-v4.html) |
| Bedrock Stability images | Stable Diffusion 3.5 Large, Stable Image Core and Stable Image Ultra serve `image_generation` through InvokeModel, one image per request. | [SD3.5 Large on Bedrock](https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-diffusion-3-5-large.html) |
| Bedrock Nova Canvas | Declined. `amazon.nova-canvas-v1:0` reached end of life on 2026-09-30. | [Bedrock legacy models](https://docs.aws.amazon.com/bedrock/latest/userguide/model-lifecycle-legacy.html) |
| Azure AI Foundry models | Served by the `azure-v1-chat` and `azure-v1-responses` profiles at the resource's `services.ai.azure.com` origin. Microsoft deprecated the `/models` Model Inference API in favour of `/openai/v1`, so no `azure-ai-inference` profile ships. | [Model Inference API specification](https://github.com/Azure/azure-rest-api-specs/blob/main/specification/ai/data-plane/ModelInference/main.tsp), [migration guide](https://learn.microsoft.com/en-us/azure/foundry/how-to/model-inference-to-openai-migration) |

### M2.3 Media, embedding and rerank providers

`certifiable` in [`kinds.go`](../../internal/providers/kinds.go) limits media
operations to `openai`, `vertex_ai` and `bedrock`. This workstream adds reviewed
media contracts and the codecs that translate OpenAI media requests for them on
transformed routes:

| Operation | Providers |
| --- | --- |
| `image_generation`, `image_edit` | Azure OpenAI, Gemini API image models and Imagen, Vertex Gemini image models, Stability AI, Black Forest Labs, fal, Recraft, xAI |
| `speech` | Azure OpenAI, ElevenLabs, Google Cloud Text-to-Speech and Gemini speech models, Amazon Polly, Deepgram |
| `transcription`, `translation` | Azure OpenAI, Groq and Fireworks audio endpoints, Deepgram, AssemblyAI, ElevenLabs, Gemini |
| `video_create` and the video lifecycle | Google Veo through the Gemini API and Vertex, Azure OpenAI, Runway, Bedrock Nova Reel |
| `embeddings`, `rerank` | Jina AI, Mistral, NVIDIA NIM, Bedrock Cohere, Together AI, Infinity |

Asynchronous vendors (AssemblyAI transcription, every video vendor) use the
durable job model that [video creation](../gateway.md#media-and-durable-video-jobs)
already provides: the job pins its route, provider, slot, credential and price
revisions, and the media reconciler settles it.

### M2.4 Reference catalog

A first-party catalog, `openllmproxy.dev/catalog/v1`, describes models by
vendor:

- canonical model identity and aliases;
- context length, maximum output tokens, input and output modalities and
  supported parameters;
- capability hints (tools, structured outputs, reasoning, prompt caching);
- deprecation and retirement dates;
- list prices in every component the [pricing model](m06-cost-management.md#m61-pricing-dimensions)
  supports, and an explicit `unrepresentable` marker for a component it does
  not yet support;
- token-estimation factors for families without a public tokenizer
  ([M1.2](m01-measured-advantage.md#m12-accurate-admission-token-estimates));
- provenance for every entry: a source URL and an observation time.

The catalog source lives in `internal/catalog/` with its JSON Schema. CI
validates the schema, deterministic ordering, decimal price strings and
provenance. Each release embeds the catalog and a detached Ed25519 signature
over its canonical bytes; the verification key is compiled into the binary.
The same signed artifact is published with the release, so an installation can
register it as a [pricing source](../operations.md#accounting-delivery-and-shutdown)
and refresh between releases without trusting an unsigned document.

The catalog is advisory and never authoritative on its own:

- **Discovery** shows catalog facts beside discovered models. Accepting them
  stores operator model facts with source `catalog@<digest>`; certification
  is still required.
- **Pricing** flows through the existing source, snapshot, diff and publish
  workflow, mapped to OLP vendor identifiers. Accounting prices only against
  published revisions.
- **Retirement** dates raise console warnings on routes that target retiring
  models, and the `model.retirement` event in
  [M5.4](m05-observability.md#m54-alert-channels-and-events).

### M2.5 Plugin ecosystem

- Authoring templates in [`sdk/plugin`](../../sdk/plugin) for OAuth 2.0
  client credentials, signed-request schemes and token-exchange
  authentication.
- A reviewed plugin index: a signed list of plugin digests, origins and
  source repositories that the console can browse. Installation, origin
  approval and permitting stay explicit owner actions.

## Non-goals

- Guaranteeing parity with every provider page LiteLLM lists. Vendors without
  production demand or a stable documented API stay with the plugin tier.
- Letting a plugin define a dialect. New dialects remain first-party codecs
  with conformance evidence.

## Decisions to settle

1. Catalog signing: an in-repository Ed25519 key with a documented rotation
   procedure, or Sigstore keyless signing (recommended: Ed25519, because
   verification then needs no network access).
2. Catalog maintenance: a scheduled job that proposes changes from vendor
   pricing pages as pull requests, with human review before merge
   (recommended).
3. The minimum media set for Azure OpenAI that unblocks most deployments
   (recommended: image generation, speech and transcription first).

## Exit criteria

- [ ] Every candidate in M2.1 ships as a preset with a passing contract test, or
      is recorded in this file with the reason it was declined.
- [ ] Every target in M2.2 is certifiable through its profile or codec and has a
      live-provider test.
- [ ] Every provider in the M2.3 table can be certified for its listed
      operations on transformed routes, with conformance fixtures and pricing
      coverage.
- [ ] Releases embed a signed catalog; a tampered catalog fails verification at
      startup and at source refresh.
- [ ] The catalog covers every model of every catalog vendor with published list
      prices, each component either priced or marked `unrepresentable`.
- [ ] Discovery suggests catalog facts, and accepting them stores
      provenance-tagged operator facts.
- [ ] The [parity matrix](parity.md) provider and media rows are `Parity` or
      better.
