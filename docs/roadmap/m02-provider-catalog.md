# M2: Provider and catalog breadth

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | None | [M6](m06-cost-management.md), [M9](m09-api-surface.md) |

The planning comparison counted about 170 LiteLLM provider pages; that is a
documentation inventory, not a count of independent adapters or an executable
coverage target. M2 implements the reviewed provider set below, with fifty
compatible presets, first-party compositions and an operator plugin path.
Every served operation retains exact-tuple certification, and a signed reference
catalog supplies reviewed model facts and prices. `Implemented` records code
and fixture coverage; external and release qualification remain open.

## Outcome

- Every candidate named in this milestone has a reviewed implementation or an
  explicit decline. Operators can add other hosting/authentication compositions
  through plugins for existing dialects. Neither a plugin path nor a provider
  page establishes universal provider coverage.
- Supported media vendors serve the operation set in M2.3; declined vendors and
  operations remain explicit rather than implied by a provider-wide claim.
- A signed catalog of model facts and list prices ships with every release and
  feeds discovery, routing facts, token calibration and pricing.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| | Planning baseline | LiteLLM reference |
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

The reviewed preset inventory covers vendors that document an OpenAI-compatible
Chat Completions, Responses or embeddings API. Each preset ships only after its contract test
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

The initial candidate set below records the intended compositions. The
[settled outcomes](#settled-during-implementation) supersede it where a vendor
contract required a different profile, kind or decline.

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
| Cohere Chat v2 | A native generation dialect at `/native/cohere-chat-v2/models/{route}`, served by the `cohere-v2` profile, that forwards documents, citations and tools unchanged and meters billed units. OpenAI clients keep the `cohere` compatibility preset. | [Chat v2](https://docs.cohere.com/reference/chat) |
| Mistral fill-in-the-middle | A native generation dialect at `/native/mistral-fim/models/{route}`, served by the `mistral-fim` profile, unary and streaming. | [FIM API](https://docs.mistral.ai/api/endpoint/fim) |
| Bedrock rerank | Rerank models serve `/v1/rerank` through the Agent Runtime's Rerank API, metered in the queries AWS bills, a query per hundred documents. | [Rerank API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Rerank.html), [rerank pricing](https://docs.aws.amazon.com/bedrock/latest/userguide/rerank-pricing.html) |
| Bedrock Cohere embeddings | Declined. Cohere Embed on Bedrock reports no usage in its response, and the `X-Amzn-Bedrock-Input-Token-Count` header is absent from the InvokeModel reference, so OLP could not meter it. | [InvokeModel](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html), [Cohere Embed v4](https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-embed-v4.html) |
| Bedrock Stability images | Stable Diffusion 3.5 Large, Stable Image Core and Stable Image Ultra serve `image_generation` through InvokeModel, one image per request. | [SD3.5 Large on Bedrock](https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-diffusion-3-5-large.html) |
| Bedrock Nova Canvas | Declined. `amazon.nova-canvas-v1:0` reached end of life on 2026-09-30. | [Bedrock legacy models](https://docs.aws.amazon.com/bedrock/latest/userguide/model-lifecycle-legacy.html) |
| Azure AI Foundry models | Served by the `azure-v1-chat` and `azure-v1-responses` profiles at the resource's `services.ai.azure.com` origin. Microsoft deprecated the `/models` Model Inference API in favour of `/openai/v1`, so no `azure-ai-inference` profile ships. | [Model Inference API specification](https://github.com/Azure/azure-rest-api-specs/blob/main/specification/ai/data-plane/ModelInference/main.tsp), [migration guide](https://learn.microsoft.com/en-us/azure/foundry/how-to/model-inference-to-openai-migration) |

### M2.3 Media, embedding and rerank providers

At the planning baseline, `certifiable` in
[`kinds.go`](../../internal/providers/kinds.go) limited media operations to
`openai`, `vertex_ai` and `bedrock`. This workstream adds reviewed media
contracts and transformed-route codecs for the candidate set below. The
[settled outcomes](#settled-during-implementation-1) define which combinations
are served and which were declined.

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

#### Settled during implementation

Reviewed on 2026-10-05 against each vendor's own documentation.

| Target | Outcome | Source |
| --- | --- | --- |
| Azure OpenAI media | Image generation, speech and transcription through deployments, certified by the smallest real call, as Vertex and Bedrock images are; Azure offers no costless proof. Azure Sora video is declined: its only remaining version retires on 2026-10-15. | [Azure OpenAI preview reference](https://learn.microsoft.com/en-us/azure/foundry/openai/reference-preview), [retirement schedule](https://learn.microsoft.com/en-us/azure/foundry/openai/concepts/model-retirement-schedule) |
| Google speech | Gemini speech models on the Gemini API and Vertex. Google Cloud Text-to-Speech is declined: it sits on its own host, which a Vertex provider's address does not reach, and its voices need a language OpenAI's speech request does not carry. | [Gemini speech generation](https://ai.google.dev/gemini-api/docs/speech-generation), [Cloud TTS](https://docs.cloud.google.com/text-to-speech/docs/reference/rest/v1/text/synthesize) |
| Amazon Polly | Speech through a Bedrock provider, at `polly.{region}` and signed for Polly. | [SynthesizeSpeech](https://docs.aws.amazon.com/polly/latest/dg/API_SynthesizeSpeech.html) |
| Fireworks audio | Declined. Fireworks deprecated audio inference on 2026-06-10 and removed its documentation. | [Fireworks changelog](https://docs.fireworks.ai/updates/changelog) |
| Runway | A `runway` preset whose video tasks are durable video jobs, polled, settled and deleted like OpenAI's. Video job calls now encode in the wire of the vendor their pinned provider revision names. | [Runway API](https://docs.dev.runwayml.com/api) |
| Video vendors declined | Azure OpenAI Sora retires on 2026-10-15; Veo on the Gemini API ends on 2026-10-22; Amazon Nova Reel reached end of life on 2026-09-30. Veo on Vertex AI: Google's references disagree on its operation's result, the video arrives inside the operation unless written to Cloud Storage, and it has no deletion; Runway serves Veo 3.1. | [Azure retirements](https://learn.microsoft.com/en-us/azure/foundry/openai/concepts/model-retirement-schedule), [Gemini deprecations](https://ai.google.dev/gemini-api/docs/deprecations), [Bedrock legacy models](https://docs.aws.amazon.com/bedrock/latest/userguide/model-lifecycle-legacy.html), [Veo on Vertex](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/video/generate-videos-from-text) |
| fal | Declined: no costless authenticated request for ordinary keys, so certification would bill an image. | [fal billing API](https://fal.ai/docs/platform-apis/v1/account/billing) |
| Jina AI | A `jina` preset for embeddings and rerank; `encoding_format` reaches Jina as `embedding_type`. | [Jina API](https://api.jina.ai/openapi.json) |
| Together AI rerank | The `together` preset serves rerank on dedicated endpoints, metered by its token usage. | [Together rerank](https://docs.together.ai/reference/rerank-1) |
| Infinity | An `infinity` self-hosted preset for embeddings and Cohere-protocol rerank. | [Infinity API](https://github.com/michaelfeil/infinity/blob/main/docs/assets/openapi.json) |
| Mistral embeddings | Served by the `mistral` preset since M2.1. Mistral has no rerank API, so its rerank is declined. | [Mistral API](https://docs.mistral.ai/api/) |
| NVIDIA NIM rerank | Declined. NVIDIA's hosted rerankers sit at a model-specific address each and score with raw logits, which OLP's rerank result, a relevance score from 0 to 1, cannot carry without rescaling. NIM embeddings are served by the `nvidia-nim` preset. | [NVIDIA reranking API](https://docs.api.nvidia.com/nim/reference/nvidia-llama-3_2-nv-rerankqa-1b-v2-infer) |
| Bedrock Cohere embeddings | Declined; see M2.2. | |

### M2.4 Reference catalog

A first-party catalog, `openllmproxy.dev/catalog/v2`, describes models by
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

## Decisions settled

1. **Signing uses Ed25519 with offline verification.** Release builds trust
   compiled release public keys only; development builds may trust the committed
   development key. The release workflow supplies the private seed and signs
   both catalog and plugin index. Rotation overlaps keys and signatures as
   specified in [catalog signing](../catalog.md#signing-keys). A development
   signature is never release-signing evidence.
2. **Maintenance reports drift and requires reviewed edits.** The weekly
   [catalog drift workflow](../../.github/workflows/catalog-drift.yml) opens or
   updates an issue from vendor model listings. It does not scrape prices into
   publication or auto-merge changes. A maintainer verifies vendor source pages,
   updates provenance, runs catalog checks and submits a re-signed change.
3. **Azure media scope is image generation, speech and transcription.**
   Certification uses the smallest real operation when no costless proof exists.
   The declined video and other candidates above remain outside delivered scope;
   provider counts must not silently include them.
4. **Catalog facts and prices remain advisory.** Facts require explicit operator
   acceptance and re-certification; prices require source/snapshot review and
   publication. Unknown model facts or missing list prices do not authorize a
   route or manufacture zero cost. Strict v2 decoding refuses older consumers.
5. **Calibration is evidence, not a vendor default.** A family has factor `1`
   until `tests/fixtures/tokens/calibrate.py` records a vendor count over the
   checked-in corpus. Publish the measured model, endpoint, time, totals and
   upward-rounded factor with catalog provenance. A factor does not claim
   per-request exactness or generalize a live certification to other models.

## Delivered

### M2.1 Reviewed presets

`internal/vendors` owns reviewed wire facts; fifty compatible presets have
contract fixtures. Defaults/certification/discovery read that table.
[Declined candidates](#declined-candidates) remain explicit.

### M2.2 First-party profiles and codecs

Vertex OpenAI, SageMaker, watsonx, Bedrock rerank/Stability and strict native
Mistral FIM/Cohere v2 ship with fixtures and live-test entry points.
[Settled outcomes](#settled-during-implementation) preserve actual scope;
compile/fixture evidence is not a paid-provider pass.

### M2.3 Media, embedding and rerank providers

The media registry supports bounded asynchronous steps, cleanup and uncredentialed
product fetches; each served wire has conformance fixtures and gateway tests.
See [media](../providers/media.md) and
[settled outcomes](#settled-during-implementation-1) for declines.

### M2.4 Reference catalog

[Signed catalog](../catalog.md), `internal/signing`, startup/doctor verification,
anti-rollback pricing snapshots, provenance-tagged discovery facts and lifecycle
warnings ship. Release builds trust release keys; development keys stay separate.

### M2.5 Plugin ecosystem

Tested SDK [templates](../plugin-authoring.md#templates) and a signed
[reviewed index](../plugins.md#reviewed-plugin-index) ship. Installation/approval/
unconfined permission remain explicit owner actions.

## Open items

**Evidence still to produce**

- The live-provider tests of the M2.2 targets compile under the `liveproviders`
  tag but have not run: they need vendor credentials this work did not have.
- No token-estimation factor ships. `tests/fixtures/tokens/calibrate.py`
  measures one against the free Anthropic and Gemini counting endpoints; each
  family keeps the factor 1 until a measurement lands in the catalog.

**Later work**

- Veo on Vertex AI, if Google settles how a finished operation returns its
  video; fal, if it offers a costless authenticated request; NVIDIA rerank,
  if OLP's rerank result gains a raw score.
- Translation on vendor speech-to-text APIs, streaming from vendor media
  codecs, and image variations beyond OpenAI.

## Remaining delivery order

M2 has no implementation dependency on another milestone. M6 consumes its
published v2 prices; M1 consumes measured estimation factors; M9 consumes
certified provider operations. Those consumers may proceed with fixtures and
explicitly missing prices/factors, but cannot claim live provider coverage from
M2's local tests.

1. **Qualify implemented tuples.** The provider maintainer runs the existing
   `liveproviders` cases for each supported M2.2 composition with a named
   model/deployment, region, operation and credential mode. Record the commit,
   vendor API version, pass/skip result, usage evidence and cleanup. A skipped
   case is pending; a successful probe covers only that exact tuple. Start with
   Azure/Vertex, then Bedrock/SageMaker, then watsonx and native Cohere/Mistral.
2. **Publish measured calibration.** Collect Anthropic and Gemini count results,
   review the corpus ratios and publish provenance-tagged factors through the
   signed catalog workflow. Until then the factor-1 fallback remains explicit.
3. **Qualify the signed release artifact.** The release maintainer verifies
   both embedded and downloadable catalog/index bytes with release trust,
   verifies tamper and rollback refusal, and records startup/doctor results for
   the candidate image. Do not infer availability of CI secrets from workflow
   declarations or committed public keys.
4. **Review deferred breadth separately.** Reopen a declined candidate only with
   a documented wire contract, representable usage/pricing and an executable
   certification path. Retain its previous decision as dated evidence; add
   fixtures, live-test entry points and catalog coverage before claiming the
   new operation. Catalog drift alone does not expand supported dialects.

## Exit criteria

Implementation and fixture evidence are complete. Paid-provider and release
signing qualification are separate; checked implementation items do not imply
unrun external results.

- [x] Every candidate in M2.1 ships as a preset with a passing contract test, or
      is recorded in this file with the reason it was declined.
      *Evidence:* `TestEveryPresetHasReviewedEvidence` checks every preset against its
      fixture; [declined candidates](#declined-candidates) records the rest.
- [ ] **Live qualification pending:** Every target in M2.2 is certifiable through its profile or codec and has a
      live-provider test.
      *Done:* each target is certifiable through the profile, kind or dialect
      its [settled outcome](#settled-during-implementation) names, and has a
      live-provider case. *Remaining:* the live cases compile but have not run
      against the vendors (see [open items](#open-items)).
- [x] Every served provider/operation in M2.3 can be certified on transformed
      routes, with conformance fixtures and pricing coverage; the rest have
      explicit declines.
      *Evidence:* each served vendor has fixtures in `tests/fixtures/vendors/`,
      codec tests, an integration test through the gateway and catalog prices;
      the providers and operations not served are declined with their reasons
      in [settled outcomes](#settled-during-implementation-1).
- [x] Releases embed a signed catalog; a tampered catalog fails verification at
      startup and at source refresh.
      *Evidence:* `TestTamperedCatalogsAreRefused` and the startup path in
      `internal/process`, which verifies the embedded catalog through the same
      `catalog.Load`, and the tampered-document and tampered-signature
      refresh tests in `internal/usage`. The release workflow re-signs with the
      release key `release-2026a`, whose public key is in
      `internal/signing/keys.go` and whose seed is the repository's
      `OLP_SIGNING_KEY` secret; release signing requires release-specific evidence.
- [x] The catalog covers every model of every catalog vendor with published list
      prices, each component either priced or marked `unrepresentable`.
      *Evidence:* 714 models of 32 vendors, transcribed on 2026-10-05; the
      vendors left to operator prices and why are listed under
      [coverage](../catalog.md#coverage).
- [x] Discovery suggests catalog facts, and accepting them stores
      provenance-tagged operator facts.
      *Evidence:* the catalog-suggestions API and console, storing
      `catalog@<sha256>` facts and invalidating certification.
- [ ] Measured Anthropic and Gemini estimation factors carry reproducible
      vendor-count evidence and signed catalog provenance; unmeasured families
      remain explicitly heuristic.
- [ ] The candidate release verifies embedded and published artifacts using
      release keys, starts successfully and passes tamper/rollback refusal.
      Checked-in signing code and development signatures do not close this gate.
- [x] The [parity matrix](parity.md) provider and media rows are `Parity` or
      better.
