# Provider profiles and connection configuration

A versioned provider profile selects the API dialect, hosting wrapper, authentication combination, and operation schemas for a provider connection. The profile is separate from both the model binding and the credential slot. Selecting a profile declares the intended contract, though it does not certify a model interaction or establish quality parity with another provider.

A provider that omits `profile_id` and `profile_revision` is an Automatic provider. Its endpoints follow from its provider kind, including the Responses-to-Chat fallback for chat-only vendors, and only [transformed routes](provider-routing.md#route-fidelity) can use it. Set both fields to select a profile, which strict routes require. Unknown revisions, incompatible connector and authentication combinations, a wrong Vertex publisher path, and undeclared operations all fail validation. OpenAI Chat and Responses are separate generation dialects, so a Responses profile cannot fall back to the Chat endpoint.

`GET /api/v1/provider-profiles` returns profile revisions, operation dialects, allowed semantic headers and query settings, and JSON schemas for operation defaults. Those schemas also describe the controls the console presents. Each entry's `strict` field reports whether the profile may serve [strict routes](provider-routing.md#route-fidelity). Model certification and credential-slot validation remain explicit activation gates, and connectivity or certification does not establish empirical model intelligence parity.

The catalogue also lists the profiles of approved [provider plugins](plugins.md) under the `plugin` provider kind. A plugin profile's revision is the digest of the plugin module, or of an unconfined plugin's executable, that supplies it, so a provider pins that build. It serves one built-in dialect's generation operation at the address its plugin declares, shares that dialect's revision and semantic headers and query settings, and authenticates with a static credential placed in its declared headers and query parameters, signed by its [signing hook](plugins.md#signing-hooks), or enrolled through a [grant](plugins.md#grant-enrollment) when it declares one. Its `options_schema` describes the options a provider using it sets, which the hosting adaptation also applies. A plugin profile that envelopes or rewrites the dialect's bodies, forces upstream streaming, or relies on an unconfined plugin that [carries its traffic](plugins.md#carrying-traffic) is not `strict`. Its entry reports `model_discovery` as `true` when the plugin declares the upstream's model listing, and otherwise operators declare the provider's models.

## Built-in profiles

Each built-in profile composition currently has OLP profile revision `1`. That revision identifies OLP's component composition rather than an immutable provider or model release, so undated provider APIs and moving model aliases remain mutable.

### Generation and completions profiles

| Profile | Kind | Native generation address and contract |
| --- | --- | --- |
| `openai-chat`, `compatible-chat` | `openai`, `openai_compatible` | `/chat/completions` under the configured base |
| `openai-responses`, `compatible-responses` | `openai`, `openai_compatible` | `/responses` under the configured base |
| `anthropic-messages` | `anthropic` | `/messages`, Anthropic version `2023-06-01`, also serving `token_count`, `skill`, and `batch` |
| `gemini-generation` | `gemini` | `/models/{model}:generateContent` or `:streamGenerateContent`, revision `v1beta`, also serving `token_count`, `embeddings`, `context_cache`, and `batch` |
| `azure-legacy-chat` | `azure_openai` | `/openai/deployments/{deployment}/chat/completions` with a dated `api_version` |
| `azure-legacy-responses` | `azure_openai` | `/openai/responses` with a dated `api_version` and deployment in the body |
| `azure-v1-chat`, `azure-v1-responses` | `azure_openai` | `/openai/v1/chat/completions` or `/openai/v1/responses`, with no dated `api_version` |
| `vertex-gemini` | `vertex_ai` | Configured project and location, Google publisher, and Gemini `v1` dialect, also serving `token_count`, `embeddings`, `fine_tuning`, and `image_generation` |
| `vertex-anthropic` | `vertex_ai` | Anthropic publisher, `:rawPredict` and `:streamRawPredict`, body version `vertex-2023-10-16`, model selected in URL |
| `vertex-openai` | `vertex_ai` | `/endpoints/openapi/chat/completions` under the configured project and location, with models named `publisher/model`, such as `google/gemini-3.8-flash` or `meta/llama-4-scout-17b-16e-instruct-maas` |
| `bedrock-converse` | `bedrock` | `/model/{model}/converse` or `/converse-stream`, also serving `token_count`, `embeddings`, and `image_generation` |
| `bedrock-anthropic-invoke` | `bedrock` | `/model/{model}/invoke` or `/invoke-with-response-stream`, body version `bedrock-2023-05-31`, serving `bedrock_invoke` and `generation` |
| `bedrock-invoke` | `bedrock` | Qualified model-specific native Invoke surface (`bedrock_invoke`), with no generation or Chat fallback |
| `sagemaker-openai-chat` | `sagemaker` | `/endpoints/{endpoint}[/inference-components/{component}]/openai/v1/chat/completions`, see [SageMaker AI](providers/sagemaker.md) |
| `openai-completions`, `together-completions`, `tgi-completions`, `vllm-completions`, `fireworks-completions` | `openai`, `openai_compatible` | Legacy text completions at `/completions`, each pinned to its reviewed vendor |

Azure v1 profiles (`azure-v1-chat` and `azure-v1-responses`) accept a resource origin or a base ending in `/openai/v1`. Their Microsoft Entra scope is `https://ai.azure.com/.default`, whereas the `azure-legacy-*` profiles and Automatic Azure OpenAI connections use `https://cognitiveservices.azure.com/.default`. Azure AI Foundry Models such as DeepSeek, Grok, and Llama deployments use `azure-v1-chat` and `azure-v1-responses` at `https://{resource}.services.ai.azure.com` with the deployment name as the model. Because Microsoft deprecated the separate Model Inference API under `/models`, OLP provides no profile for it.

Vertex AI endpoints must match the configured project, location, and profile publisher, or the project's `endpoints/openapi` path for `vertex-openai`. When a Vertex provider omits an endpoint, OLP derives the address from the profile. See [Vertex AI](providers/vertex.md) for Google Cloud publisher addressing, authentication, and operation rules.

Bedrock signing runs only after the final URL, body, and semantic headers have been constructed. Anthropic Invoke streaming unwraps bounded AWS event envelopes into native Anthropic events rather than decoding the stream as Converse or manufacturing terminal events.

### Gemini hosted lifecycle and realtime profiles

Gemini Interactions, Gemini managed agents, and Gemini Live each ship as dedicated strict profiles on the `gemini` kind (API key authentication only, `v1beta` endpoint) rather than sharing `gemini-generation`. Native duplex realtime sessions for xAI, Vertex AI Live, and Bedrock Nova Sonic likewise ship as dedicated strict profiles.

| Profile | Kind | Transport | Operation and surface | Upstream contract |
| --- | --- | --- | --- | --- |
| `gemini-interactions` | `gemini` | `http` | `generation` on `gemini` | `/v1beta/interactions` via `/gemini/v1beta/interactions` |
| `gemini-agents` | `gemini` | `http` | `agent_resource` on `gemini` | Managed agent resources via `/gemini/v1beta/agents` |
| `gemini-live` | `gemini` | `websocket` | `realtime` on `gemini` | `BidiGenerateContent` WebSocket via `/gemini/ws/...` and `/ws/...` |
| `xai-realtime` | `openai_compatible` | `websocket` | `realtime` on `native` | `/realtime?model={model}` via `/native/xai-realtime/sessions/{route}` |
| `vertex-realtime` | `vertex_ai` | `websocket` | `realtime` on `native` | `LlmBidiService/BidiGenerateContent` WebSocket via `/native/vertex-realtime/sessions/{route}` |
| `bedrock-realtime` | `bedrock` | `bedrock-bidi` | `realtime` on `native` | Bedrock Nova Sonic `InvokeModelWithBidirectionalStream` via `/native/bedrock-realtime/sessions/{route}` |

Neither the Gemini lifecycle profiles (`gemini-interactions`, `gemini-agents`, `gemini-live`) nor the native duplex profiles (`xai-realtime`, `vertex-realtime`, `bedrock-realtime`) admit `operation_defaults` or model binding defaults.

### Retrieval profiles and Azure AI Search

Operator-managed retrieval indexes use three dedicated non-strict profiles that serve the `retrieval` operation and take resource and filter configuration from explicit retrieval bindings rather than `operation_defaults` or `bindings`.

| Profile | Kind | Authentication | Upstream contract |
| --- | --- | --- | --- |
| `bedrock-knowledge-bases` | `bedrock` | `static`, `default_chain` | `bedrock-agent-runtime.{region}/knowledgebases/{id}/retrieve` |
| `vertex-rag-engine` | `vertex_ai` | `adc`, `service_account` | Regional Vertex AI `:retrieveContexts` |
| `azure-ai-search` | `azure_ai_search` | `api_key`, `azure_default`, `azure_client_secret` | `/indexes/{index}/docs/search?api-version=2025-09-01` |

The `azure_ai_search` connector kind is dedicated to query-only Azure AI Search indexes. It requires the `azure-ai-search` profile, a search service origin without a path, `api_version` set to `2025-09-01`, and index names declared as models. API keys travel in `Api-Key`, and Microsoft Entra modes (`azure_default` and `azure_client_secret`) request the `https://search.azure.com/.default` scope. See [retrieval](retrieval.md) for setup, bindings, and query semantics across all three backends.

### OCR, search, and decision profiles

| Profile | Kind | Operation | Upstream contract |
| --- | --- | --- | --- |
| `vertex-document-processing-ocr` | `vertex_ai` | `ocr` | Google Cloud Document AI `/processors/{model}:process` |
| `mistral-ocr` | `openai_compatible` | `ocr` | Mistral `/ocr` (`api_key` only) |
| `azure-document-intelligence-ocr` | `openai_compatible` | `ocr` | Azure Document Intelligence `/documentintelligence/documentModels/{model}:analyze` (`api_key` only) |
| `perplexity-search` | `openai_compatible` | `search` | Perplexity `/search` (`api_key` only) |
| `tavily-search` | `openai_compatible` | `search` | Tavily `/search` (`api_key` only) |
| `exa-search` | `openai_compatible` | `search` | Exa `/search` (`api_key` only) |
| `parallel-search` | `openai_compatible` | `search` | Parallel `/search` (`api_key` only) |
| `brave-search` | `openai_compatible` | `search` | Brave `GET /res/v1/web/search` (`api_key` only) |
| `openai-decisions` | `openai` | `decision` | OpenAI `/decisions` |
| `gemini-decisions` | `gemini` | `decision` | Gemini `/v1beta/decisions` (`api_key` only, endpoint ending in `/v1beta`) |
| `systemone` | `openai_compatible` | `decision` | TypeSafe Jev `/systemone` |

All OCR, search, and decision profiles are strict (`strict: true`) and bind to their reviewed vendor contracts.

### Embeddings, rerank, classification, scoring, and tokenization profiles

| Profile | Kind | Operation | Upstream contract |
| --- | --- | --- | --- |
| `openai-embeddings` | `openai` | `embeddings` | `/embeddings` |
| `gemini-batch-embeddings` | `gemini` | `embeddings` | `/models/{model}:batchEmbedContents` |
| `voyage-embeddings` | `openai_compatible` | `embeddings` | `/embeddings` |
| `cohere-embed-v2` | `openai_compatible` | `embeddings` | Cohere v2 `/embed` (`api_key` only) |
| `tei-embeddings`, `tei-sparse-embeddings`, `tei-multivector-embeddings` | `openai_compatible` | `embeddings` | Hugging Face TEI `/embed`, `/embed_sparse`, and `/embed_all` |
| `rerank` | `openai_compatible` | `rerank` | `/rerank` |
| `voyage-rerank` | `openai_compatible` | `rerank` | Voyage `/rerank` |
| `cohere-rerank-v2` | `openai_compatible` | `rerank` | Cohere v2 `/rerank` (`api_key` only) |
| `tei-rerank` | `openai_compatible` | `rerank` | TEI `/rerank` |
| `openai-moderation` | `openai` | `moderation` | `/moderations` |
| `tei-classification` | `openai_compatible` | `classification` | TEI `/predict` |
| `tei-scoring` | `openai_compatible` | `scoring` | TEI `/similarity` |
| `openai-input-tokens` | `openai` | `token_count` | `/responses/input_tokens` |
| `tei-tokenize` | `openai_compatible` | `token_count` | TEI `/tokenize` |
| `cohere-v2` | `openai_compatible` | `embeddings`, `rerank`, `generation` | Combined Cohere native v2 profile composing `cohere-embed-v2`, `cohere-rerank-v2`, and `cohere-chat-v2` (`api_key` only) |

The `cohere-embed-v2`, `cohere-rerank-v2`, and `cohere-v2` profiles compose `openai_compatible` direct HTTP hosting and API-key authentication with Cohere's native v2 codecs for [Embed v2](https://docs.cohere.com/reference/embed), [Rerank v2](https://docs.cohere.com/reference/rerank), and Chat v2. Choose the `cohere-native-v2` preset at `https://api.cohere.ai/v2`, or a trusted custom v2-compatible base. Native requests use `/native/cohere-embed-v2/models/{route}` or `/native/cohere-rerank-v2/models/{route}`. The separate `cohere` preset targets `/compatibility/v1`, so selecting any Cohere native v2 profile with `https://api.cohere.ai/compatibility/v1` fails configuration validation instead of calling `/compatibility/v1/embed` or `/rerank`.

Embed v2 retains original `texts`, image data URIs or ordered multimodal `inputs`, task type, `truncate`, dimension, and requested `float`, `int8`, `uint8`, `binary`, `ubinary`, or `base64` storage. Multi-type or non-float storage requires the explicit `raw-vector-storage/1` client contract. Rerank v2 retains original query and documents, `top_n`, per-document token cap, priority, returned indices, native score spelling, order, ties, and provider search-unit billing. Unknown native extensions remain source-exact where policy coverage permits them, while foreign aliases and uninspectable content under restrictive policies are refused before dispatch. Cohere's v2 API does not define sparse or token-multivector output, so TEI remains the scoped native contract for those layouts.

## Cloud batch profiles

`vertex-batch-prediction` and `bedrock-batch-inference` are strict batch-only control-plane profiles on the `native` surface and do not enable live generation. Both require the nonsecret operator `batch_storage` object with input and output URI prefixes. Vertex AI uses `gs://` prefixes, while Bedrock uses `s3://` prefixes and also requires `role_arn`. Validation constrains candidate and job output URIs to those configured scopes.

For OpenAI-compatible third-party vendors with dedicated file and batch APIs, OLP ships three non-strict files-and-batches profiles on `openai_compatible` (`api_key` authentication only).

| Profile | Vendor | Operations |
| --- | --- | --- |
| `groq-files-batches` | `groq` | `generation`, `batch` |
| `mistral-files-batches` | `mistral` | `generation`, `embeddings`, `batch` |
| `together-files-batches` | `together` | `generation`, `batch` |

## Native generation dialects

Some vendor generation APIs have no counterpart in another dialect. Clients speak them directly at `POST /native/{dialect}/models/{route}` on strict routes whose targets are certified for `generation` on the `native` surface.

| Dialect | Profile | Upstream |
| --- | --- | --- |
| `mistral-fim` | `mistral-fim`, with the `mistral` preset | [`/v1/fim/completions`](https://docs.mistral.ai/api/endpoint/fim) |
| `cohere-chat-v2` | `cohere-v2`, the `cohere-native-v2` preset's profile | [`/v2/chat`](https://docs.cohere.com/reference/chat) |

OLP validates the request envelope, binds the route's model in place of the body's `model` (which may be absent or name the route), and forwards everything else unchanged. Cohere's documents, citation options, tools, and thinking reach Cohere intact, and its citations, tool plans, and documents return byte for byte. Results and every streamed event are validated against the dialect's grammar and metered. Fill-in-the-middle streams meter as chat completion chunks ending in `[DONE]`, while Cohere meters by the billed units reported in its `message-end` event, which exclude its own preamble. A stream that ends before its terminal event is marked truncated.

A native generation dialect is never translated. Its profile serves no other generation surface, so an OpenAI, Anthropic, or Gemini request to its route finds no eligible target. An Automatic `cohere-native-v2` provider has no Chat Completions endpoint to call and therefore does not generate without the `cohere-v2` profile.

## Defaults, bindings and semantic configuration

An example configuration fragment for native Anthropic generation looks like this.

```json
{
  "kind": "anthropic",
  "profile_id": "anthropic-messages",
  "profile_revision": "1",
  "auth_mode": "api_key",
  "endpoint": "https://api.anthropic.com/v1",
  "options": {
    "semantic_headers": { "Anthropic-Version": "2023-06-01" },
    "operation_defaults": {
      "generation": {
        "dialect": "anthropic-messages",
        "values": { "temperature": 0.5 }
      }
    },
    "bindings": {
      "logical-model": {
        "model": "configured-native-model-id",
        "principal_id": "operator-declared-upstream-account",
        "defaults": {
          "generation": {
            "dialect": "anthropic-messages",
            "values": { "temperature": 0.25 }
          }
        }
      }
    }
  }
}
```

The provider default is inherited first, followed by the selected binding's member replacement. An explicit caller member always wins, including native `null`, zero, `false`, or an empty array. Arrays, tools, and schema definitions replace atomically rather than merging recursively across layers. To clear a configured default, remove that member in the replacement configuration. A raw `null` inside `values` is sent as an actual native `null` rather than acting as a reset instruction. Null default definitions, ambiguous duplicate JSON members, and case-aliased configuration fields are rejected before typed decoding can erase the distinction.

`values` is validated against the operation and dialect control schema. Explicit native extensions belong in `native_options`, where they remain data and cannot replace model, input envelope, destination, authentication, routing, or durable-resource authority. Collisions between shared and native members are errors, including collisions across provider and binding layers. `parameter_defaults` belongs to Automatic providers and cannot be combined with explicit profiles. Prepared invocation provenance records which omitted fields were filled and which layer supplied each default. Shared token admission uses the prepared bounds sent upstream without shrinking caller controls.

For Gemini embeddings, native defaults such as `taskType` and `outputDimensionality` apply to each native batch member while preserving input order. Vertex embedding defaults use its atomic `parameters` member. Media defaults retain original JSON presence and exact native extension numbers, while multipart values require a qualified representation and delivery-mode changes are rejected.

Bindings can declare model or deployment, principal, snapshot, region, and resource scope. Identity facts without independent observation remain operator declarations, and a secret version does not prove an upstream account, whereas a grant's [observed principal](plugins.md#re-enrolling-and-the-observed-principal) replaces a declared principal. Binding, semantic, and profile changes appear separately in revision diffs. Credential rotation preserves semantic configuration, while changes to a declared principal or resource scope count as serving-identity changes rather than a secret refresh.

Semantic headers are independent from encrypted API credentials. Profile allowlists reserve authorization, host, hop-by-hop, tenant, tracing, and security-sensitive routing controls, and invalid HTTP header values are rejected without echoing them. Google profiles allow the schema-owned `$xgafv` query setting (`1` or `2`), and a legacy Azure `api-version` query setting must match its configured API revision. No query setting can change destination or credential scope.

## Secure connection options

`options.network` is optional and accepts the following fields.

| Field | Contract |
| --- | --- |
| `proxy_url` | HTTP/HTTPS CONNECT or SOCKS5 proxy origin with no userinfo, path, query, or fragment |
| `trust_roots_pem` | Public certificate PEM augmenting system trust, rejecting private-key blocks |
| `credential_id` | Provider-owned encrypted network credential version UUID |
| `connect_timeout_ms` | DNS, TCP, and proxy establishment budget, 1 to 120000 ms |
| `tls_handshake_timeout_ms` | TLS handshake budget, 1 to 120000 ms |
| `response_header_timeout_ms` | Response-header deadline, 1 to 600000 ms |
| `idle_conn_timeout_ms` | Idle pooled-connection lifetime, 1 to 3600000 ms |
| `max_idle_conns`, `max_idle_conns_per_host`, `max_conns_per_host` | Positive bounded pool settings, at most 4096. Defaults when unset are 128, 16, and 64 |

A provider uses one of two connection pool modes depending on whether `options.network` or a provider profile is set. A provider with neither `options.network` nor a profile shares the gateway's default transport, keeping at most 16 idle connections per host with no cap on total connections opened to one host. Setting any `options.network` field, even `connect_timeout_ms` alone, or selecting a provider profile gives the provider a dedicated pool per credential slot capped at 64 connections per host, 16 idle per host, and 128 idle in total unless overridden above. When that cap is full, a request waits inside the transport with no error, no admission rejection, and no open circuit, so over HTTP/1.1 every request past the 64th in flight on one slot queues behind active work. A provider carrying more concurrent streams should raise `max_conns_per_host` and `max_idle_conns_per_host` (up to 4,096) or spread traffic across additional credential slots or providers, whose pools are separate. See [provider connection capacity](../operate/deployment.md#provider-connection-capacity).

Both the proxy and every resolved destination IP pass the existing egress policy. CONNECT and SOCKS requests carry locally validated destination IPs while preserving target Host and SNI. Remote DNS (`socks5h`) is refused because local DNS pinning cannot govern a proxy's resolver. HTTP proxies must support CONNECT even for plain HTTP targets. Environment proxy variables are ignored, and plain HTTP proxy origins require the operator host exception. TLS verification, TLS 1.2 or newer, redirect refusal, bounded headers, bodies, and events, and cancellation remain enforced.

Custom trust roots apply independently to HTTPS proxy TLS and target TLS. A client certificate is offered only to the target, never to the proxy, and proxy authentication stays confined to tunnel establishment. Reusable pools are bounded and isolated by provider, serving revision, and credential scope even when network settings match, so a cached connection never substitutes for current credential authority.

Create network credential material with `POST /api/v1/providers/{id}/network-credentials` using the current provider `If-Match` and an `Idempotency-Key`. Its `credential` string contains a JSON object with `proxy_username` and `proxy_password`, `client_certificate_pem` and `client_key_pem`, or both pairs. The endpoint encrypts the material through the secret authority and returns only a reference. Patch that reference into `options.network.credential_id`, certify and validate, and activate normally. A network credential cannot be assigned to an API credential slot or borrowed from another provider. Listing credentials returns metadata only, and revocation advances the same current authority used by inference and retained resource recovery.

Configuration export replaces the environment-specific UUID with `network_credential_ref` (the provider name followed by `/network`). Import binds that reference through `secret_bindings`, creates or reuses a destination-owned network credential, and keeps imported providers and routes as drafts. Network and API credential references must be distinct, and ambiguous references are rejected. Exports never contain private keys, proxy passwords, or source-installation UUIDs. Export, plan, and apply perform no upstream provider calls, while probes and certifications remain separate operator actions.

The same network options apply to discovery, certification, generation, media, multipart resources, retained resource retrieval, and WebSocket realtime connections. Pinned revisions keep their network and profile settings, while current credential revocation still takes precedence. Multi-turn continuation admission remains its own interaction and lifecycle contract rather than something implied by network configuration.

## Qualification and extension

Controlled unit and integration tests cover every registered composition's final path, headers, wrappers, and signing, as well as mTLS, credential rotation and revocation, portable configuration round trips, HTTP/HTTPS/SOCKS egress protection and pool isolation, media, resource, and realtime TLS transport, and preserve-or-reject defaults. Those suites provide deterministic fixture verification rather than live-provider or model quality claims.

Providers beyond the built-in profiles come from [provider plugins](plugins.md), which are WASM modules an installation owner approves for confined execution, or executables permitted under the experimental [unconfined tier](plugins.md#unconfined-plugins-experimental). Plugin profiles always target a built-in dialect, so introducing a new dialect or lifecycle requires its own codec, runner, and behavioral verification.

First-party contract references consulted for these compositions include [OpenAI API migration](https://developers.openai.com/api/docs/guides/migrate-to-responses), [Azure API lifecycle](https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle), [Anthropic versions](https://platform.claude.com/docs/en/api/versioning), [Claude on Vertex](https://platform.claude.com/docs/en/build-with-claude/claude-on-vertex-ai), [Bedrock Converse](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html), and [Bedrock Invoke](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html).

A gateway using `OLP_CONNECTOR_CONFIG_FILE` without database decryption keys may supply `network_credential_file` beside `credential_file` in each mounted provider entry. A plugin provider mounts its static credential the same way, taking its profile from the plugin pinned by its published revision. The network file uses the same private JSON shape as the creation API, and its configured credential UUID must match the published provider's network reference because mounted files cannot create authority. For an explicit profile, the mounted configuration must match the published model-significant and network settings so that only secret material comes from the local file. Current revocation still applies, and mounted secrets are excluded from serialization and diagnostic formatting.

### Credential destination changes

Stored static credentials belong to a provider's authentication and destination boundary. While a provider owns any unrevoked credential version, editing or restoring its kind, authentication mode, endpoint, cloud context, profile, credential headers, proxy, or TLS trust roots requires either creating a new provider with explicitly supplied credentials or revoking all existing versions before supplying new ones. Detaching a slot does not lift this restriction because old versions can still be reselected. Model, quota, and connection timeout edits remain allowed. Vertex AI `service_account` and `adc` credentials always use Google's endpoint for the configured location and the system TLS trust roots.

Grant enrollment records both the plugin digest and the profile ID. Changing either requires re-enrollment before the draft can be probed, validated, or activated, while existing published revisions keep their historical serving configuration. This transition preserves the remaining credential boundary, including the endpoint and plugin options, so changing those still requires revoking old credentials or creating a new provider even when changing the profile at the same time. Unexpired retained resources keep their grants refreshable under the original provider revision until those resources expire or are deleted, and explicit credential revocation still takes effect. Grants created before profile provenance was recorded require re-enrollment because the migration does not infer their original profile from mutable drafts.
