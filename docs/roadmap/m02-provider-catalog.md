# M2: Provider and catalog breadth

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | None | [M1](m01-measured-advantage.md) (calibration factors), [M5](m05-observability.md) (retirement events), [M6](m06-cost-management.md) (price components) | [M6](m06-cost-management.md), [M9](m09-api-surface.md) |

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
| Media | Every media operation on OpenAI's official endpoint; unary image generation on Vertex Imagen and Bedrock Titan; media refused for Azure, compatible endpoints and custom OpenAI endpoints ([compatibility](../compatibility.md)) | Image, audio, video and OCR providers |
| Model metadata | Operator-declared model facts, which override the context and modality facts that discovery imports from upstream model lists ([provider routing](../provider-routing.md#private-endpoints-headers-defaults-and-model-facts)); pricing sources an operator registers ([operations](../operations.md#accounting-delivery-and-shutdown)) | A [model cost map](https://docs.litellm.ai/docs/proxy/sync_models_github) fetched from GitHub at startup |

## Qualification tiers

| Tier | What it adds | Evidence required |
| --- | --- | --- |
| Reviewed preset | A catalog entry over an existing dialect and hosting composition | A vendor contract test pinning documented endpoints, unsupported parameters, usage shape, stream terminal events and error envelopes |
| First-party profile or codec | A new profile, hosting composition, authentication mode or dialect codec in `internal/connectors`, `internal/protocols` or `internal/operations` | Conformance fixtures, connector composition tests, a live-provider test behind `liveproviders`, and pricing coverage |
| Plugin | Authentication and hosting around a built-in dialect, supplied by an operator ([plugins](../plugins.md)) | The plugin's own manifest review and origin approval |

No tier certifies a model: certification stays the server's exact-tuple check,
by live probe or, for operations a probe cannot exercise cheaply, by
authenticated discovery.

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

Each addition extends the capability rules
([`capabilities.go`](../../internal/connectors/capabilities.go),
[`profiles.go`](../../internal/connectors/profiles.go)) and certification
eligibility ([`kinds.go`](../../internal/providers/kinds.go),
[`operations.go`](../../internal/providers/operations.go)) explicitly; no
operation becomes certifiable by default.

### M2.3 Media, embedding and rerank providers

`certifiable` in [`kinds.go`](../../internal/providers/kinds.go) admits Vertex
and Bedrock for unary image generation only, and limits every other media
operation to OpenAI's official endpoint. This workstream adds reviewed media
contracts and the codecs that translate OpenAI media requests for them on
transformed routes:

| Operation | Providers |
| --- | --- |
| `image_generation`, `image_edit` | Azure OpenAI, Gemini API image models and Imagen, Vertex Gemini image models, Stability AI, Black Forest Labs, fal, Recraft, xAI |
| `speech` | Azure OpenAI, ElevenLabs, Google Cloud Text-to-Speech and Gemini speech models, Amazon Polly, Deepgram |
| `transcription`, `translation` | Azure OpenAI, Groq and Fireworks audio endpoints, Deepgram, AssemblyAI, ElevenLabs, Gemini |
| `video_create` and the video lifecycle | Google Veo through the Gemini API and Vertex, Azure OpenAI, Runway, Bedrock Nova Reel |
| `embeddings`, `rerank` | Jina AI, NVIDIA NIM, Bedrock Cohere, Infinity, and reviewed contracts for the Mistral and Together embeddings their presets can already certify |

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
  models. The `model.retirement` event ships with whichever of this workstream
  and [M5.4](m05-observability.md#m54-alert-channels-and-events) lands second.

### M2.5 Plugin ecosystem

- Authoring templates in [`sdk/plugin`](../../sdk/plugin) for OAuth 2.0
  client credentials, signed-request schemes and token-exchange
  authentication.
- A reviewed plugin index: a signed list of plugin digests, origins and
  source repositories that the console can browse. Installation, origin
  approval and permitting stay explicit owner actions.

## Non-goals

- Vendor-for-vendor parity with LiteLLM's provider list. The provider rows
  close under the [breadth rule](parity.md#how-to-read-the-matrix): the vendors
  named here ship, and the rest stay reachable through presets and plugins.
- Letting a plugin define a dialect. New dialects remain first-party codecs
  with conformance evidence.
- Treating the catalog as authority. It never certifies a model or prices an
  attempt on its own.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Reference catalog and its signature | Embedded in the binary; refreshed copies as pricing source snapshots in PostgreSQL | Per release; snapshots as today | None; public data, verified with a compiled-in public key |
| Accepted catalog facts | Operator model facts in PostgreSQL, tagged `catalog@<digest>` | Until edited | None |
| Catalog signing key | The release pipeline's secret store, never the repository or the binary | Rotated by the documented procedure | Not an OLP seal purpose |
| Plugin index | Fetched by the console through egress policy; not persisted | None | None |

## Change map

| Change | Start here |
| --- | --- |
| Presets and `profile_id` | `internal/providers/kinds.go` |
| Profiles, hosting and codecs | `internal/connectors/profiles.go`, `internal/protocols/`, `internal/operations/` |
| Certification eligibility | `internal/providers/kinds.go`, `internal/providers/operations.go`, `internal/connectors/capabilities.go` |
| Media contracts and jobs | `internal/media/`, `internal/mediacontract/` |
| Catalog | new `internal/catalog/`, `internal/usage/pricing_sources.go`, `internal/providers/connector.go` |
| Plugin templates and index | `sdk/plugin/`, `console/src/lib/features/providers/` |

## Decisions to settle

1. Catalog signing: an Ed25519 key pair whose private half lives only in the
   release pipeline, with a documented rotation procedure, or Sigstore keyless
   signing (recommended: Ed25519, because verification then needs no network
   access).
2. Catalog maintenance: a scheduled job that proposes changes from vendor
   pricing pages as pull requests, with human review before merge
   (recommended).
3. The minimum media set for Azure OpenAI that unblocks most deployments
   (recommended: image generation, speech and transcription first).

## Exit criteria

- [ ] **M2.1** Every candidate ships as a preset with a passing contract test,
      or is recorded in this file with the reason it was declined.
- [ ] **M2.2** Every target is certifiable through its profile or codec and has
      a live-provider test.
- [ ] **M2.3** Every provider in the table can be certified for its listed
      operations on transformed routes, with conformance fixtures and pricing
      coverage.
- [ ] **M2.4** Releases embed a signed catalog; a tampered catalog fails
      verification at startup and at source refresh.
- [ ] **M2.4** The catalog covers every model of every catalog vendor with
      published list prices, each component either priced or marked
      `unrepresentable`.
- [ ] **M2.4** Discovery suggests catalog facts, and accepting them stores
      provenance-tagged operator facts.
- [ ] **M2.5** A plugin built from each authoring template passes the SDK
      conformance suite, and the console refuses a plugin index whose signature
      does not verify.
- [ ] The [parity matrix](parity.md) provider and media rows are `Parity` or
      better, with the breadth rows closed under the
      [breadth rule](parity.md#how-to-read-the-matrix).
