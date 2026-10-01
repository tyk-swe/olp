# M9: API surface completion

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | [M2](m02-provider-catalog.md) | [M10](m10-agent-gateway.md) retrieval tools, full official-SDK coverage |

OLP serves the generation, counting, embedding, rerank, moderation, media,
file, batch, realtime and stored-response operations in the
[compatibility matrix](../compatibility.md). LiteLLM additionally serves legacy
completions, fine-tuning, vector stores, OCR, search, containers, skills, evals,
WebRTC realtime, cross-provider batches and vendor pass-through. This milestone
completes the surface while keeping OLP's rule for API shapes: serve official
shapes natively, translate only between official shapes on transformed routes,
and expose vendor-specific operations under `/native/{dialect}` rather than
inventing new cross-vendor shapes.

## Outcome

- Every endpoint family the official OpenAI, Anthropic and Gemini SDKs expose
  for current, supported APIs is served or explicitly excluded.
- Files and batches work across providers, and batch results settle into
  accounting exactly once.
- Fine-tuned models flow into discovery and certification.
- Realtime sessions work over WebRTC and with more providers.
- Operators have a governed escape hatch for vendor endpoints OLP does not
  model.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Retained resources | Files, batches and stored responses for OpenAI and Azure, with gateway-owned identifiers pinned to provider revision, slot and credential ([compatibility](../compatibility.md#files-batches-realtime-and-provider-retained-state)) | Files and batches across providers, [managed files](https://docs.litellm.ai/docs/proxy/litellm_managed_files) and [managed batches](https://docs.litellm.ai/docs/proxy/managed_batches) |
| Durable jobs | Video jobs pinned to their revisions and reconciled by workers ([media](../gateway.md#media-and-durable-video-jobs)) | Background batch cost tracking (Enterprise) |
| Realtime | WebSocket for OpenAI and Azure; Gemini Live | [WebSocket](https://docs.litellm.ai/docs/realtime) for more providers and [WebRTC](https://docs.litellm.ai/docs/proxy/realtime_webrtc) |
| Native operations | `/native/{dialect}/models/{route}` for TEI and Cohere v2 operations | [Pass-through endpoints](https://docs.litellm.ai/docs/pass_through/intro) |

## Scope

### M9.1 Legacy completions

`POST /v1/completions` serves the OpenAI text completion API natively for
OpenAI and for compatible servers that implement it (vLLM, TGI, Fireworks,
Together and others), including streaming and fill-in-the-middle `suffix`. On
transformed routes it translates to Chat Completions as a single user message;
`echo`, `best_of`, `logprobs` and `suffix` are refused when the target cannot
honor them.

### M9.2 Files and batches across providers

- **Native surfaces.** Anthropic Message Batches
  (`/anthropic/v1/messages/batches` and results) and the Anthropic Files API;
  Gemini Files (resumable upload) and Gemini batch mode; OpenAI-compatible batch
  APIs from reviewed vendors (Groq, Together, Fireworks, Mistral). Each reuses
  the provider-resource mapping: gateway-owned identifiers, pinned credentials
  and owner-only access.
- **Cloud batch jobs.** Vertex AI batch prediction and Bedrock batch inference
  read inputs from and write outputs to operator-configured Cloud Storage or S3
  locations, declared on the connection and validated by egress policy.
- **Managed batches.** On transformed routes, an OpenAI-format batch whose
  target speaks another dialect is translated line by line through the
  canonical codecs and submitted in the target's batch format, then its results
  are translated back. Lines that cannot be represented fail validation before
  submission.
- **Settlement.** A worker task, `batch_settlement`, polls batches to their
  terminal state, reads each result line's usage, prices it with the
  revision pinned at creation and the batch multiplier from
  [M6.1](m06-cost-management.md#m61-pricing-dimensions), and settles it to the
  creating key exactly once, including across concurrent workers.

### M9.3 Fine-tuning

`/v1/fine_tuning/jobs` with events, checkpoints, cancel, pause and resume, for
OpenAI and Azure first, as a provider-resource family with gateway-owned
identifiers. Training files reuse M9.2 files. A completed job's model appears in
its connection's discovery as a declared model; it serves traffic only after
certification and publication on a route, so fine-tuning cannot bypass the
certification gate. Vertex AI tuning follows.

### M9.4 Vector stores and retrieval

- `/v1/vector_stores`, vector store files and file batches, and
  `/v1/vector_stores/{id}/search`, for OpenAI and Azure, as provider resources.
- Native routes rewrite gateway vector store identifiers inside `file_search`
  tool configurations, as they already rewrite `previous_response_id`.
- Retrieval connectors for Amazon Bedrock Knowledge Bases, Vertex AI RAG Engine
  and Azure AI Search serve `/v1/vector_stores/{id}/search` on transformed
  routes, so retrieval can move between platforms.

### M9.5 Responses completion

- The OpenAI Conversations API (`/v1/conversations` and its items), with
  conversation identifiers owned by the gateway and subject to
  `allow_provider_state`.
- `/v1/responses/compact`, served natively where the provider supports it.

### M9.6 Realtime expansion

- **WebRTC.** `POST /v1/realtime/client_secrets` mints a short-lived provider
  client secret bound to one route, key and session configuration, so browsers
  and mobile clients connect over WebRTC without holding a provider key. The
  key's limits are reserved when the secret is minted. Usage is accounted
  through the provider's server-side session channel where the provider offers
  one; otherwise the session is recorded as billing-uncertain, as WebSocket
  sessions without usage events are today.
- **Providers.** xAI realtime, Vertex AI Live, and Amazon Nova Sonic through
  Bedrock bidirectional streaming.
- **Transcription sessions.** Realtime transcription-only sessions where the
  provider supports them.

### M9.7 OCR and search

- **OCR.** `POST /v1/ocr` accepts the Mistral OCR request shape, as LiteLLM
  does, natively for Mistral and translated on transformed routes for Azure AI
  Document Intelligence and Vertex AI. Documents arrive inline or by URL; URLs
  are fetched by the provider, never by the gateway.
- **Search.** `POST /v1/search` accepts the Perplexity Search request shape, as
  LiteLLM does, natively for Perplexity and translated for Tavily, Exa, Brave
  Search, Google Programmable Search and Parallel. Searches are accounted per
  request with the `per_request` price component.
- Each vendor is also reachable natively under `/native/{dialect}`.

### M9.8 Provider-retained resource families

Each family below reuses the provider-resource mapping, fidelity rules and
owner-only access:

| Family | Surface |
| --- | --- |
| Code interpreter containers and container files | OpenAI and Azure `/v1/containers` |
| Evals and eval runs | OpenAI `/v1/evals` |
| Skills | Anthropic `/anthropic/v1/skills` |
| Context caches | Gemini `cachedContents` |
| Managed agents | Gemini `/gemini/v1beta/agents` |

### M9.9 Governed pass-through

A pass-through route binds one connection and an explicit allowlist of methods
and path patterns, such as `POST /v1/agents/*/invoke`. The gateway
authenticates the key, applies limits, injects the connection's credential,
enforces body and response bounds and egress policy, and records an attempt
with operation `passthrough`, priced by `per_request` when a price exists.

Pass-through routes carry no interaction contract, so they are a distinct route
kind rather than strict or transformed: guardrails, caching, translation and
failover are refused on them, and the console marks them as uncertified. They
serve vendor APIs OLP does not model, never as a substitute for a modeled
operation.

## Change map

| Change | Start here |
| --- | --- |
| New endpoints and codecs | `internal/gateway/`, `internal/protocols/`, `internal/operations/` |
| Provider-resource families | `internal/resources/`, `internal/durablecontract/` |
| Batch settlement | `internal/resources/`, `internal/process/workers.go` |
| Realtime | `internal/gateway/realtime.go`, `internal/realtimecontract/` |
| Pass-through routes | `internal/routes/`, `internal/gateway/` |

## Decisions to settle

1. Cloud batch storage: operator-provided buckets only, or gateway-managed
   staging (recommended: operator-provided only, so OLP never holds batch
   content).
2. Whether managed batches may split one batch across several targets
   (recommended: no; one batch, one pinned target).
3. The first WebRTC provider set (recommended: OpenAI and Azure).

## Exit criteria

- [ ] Every new endpoint passes the official SDK suites at their pinned
      versions, in unary and streaming modes where the API defines both.
- [ ] Batch results settle exactly once under concurrent workers and restarts,
      priced with the batch multiplier.
- [ ] A fine-tuned model cannot serve traffic before certification.
- [ ] Gateway identifiers never leak upstream identifiers for any new resource
      family, and another key's identifier is indistinguishable from a missing
      one.
- [ ] WebRTC sessions honor key revocation and limits; sessions without usage
      are recorded as billing-uncertain.
- [ ] Pass-through routes refuse paths outside their allowlist before dispatch.
- [ ] The [compatibility matrix](../compatibility.md) lists every new endpoint,
      and the [parity matrix](parity.md) endpoint rows are `Parity`, `Ahead` or
      `Excluded`.
