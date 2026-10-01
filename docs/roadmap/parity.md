# Parity matrix

This matrix compares the released capabilities of the LiteLLM AI Gateway with
OLP and is the authoritative parity scope of the [roadmap](README.md). The
LiteLLM column links its documentation as reviewed on 2026-10-01; the OLP
column describes this repository at 0.1.0.

| Status | Meaning |
| --- | --- |
| `Ahead` | OLP offers the capability with a stronger guarantee, or in its core product where LiteLLM requires an Enterprise license. |
| `Parity` | OLP offers an equivalent capability. |
| `Partial` | OLP covers part of the capability; the milestone completes it. |
| `Gap` | OLP lacks the capability; the milestone delivers it. |
| `Excluded` | Out of scope by design; see [out of scope](README.md#out-of-scope-by-design). |

Milestone references name a workstream, such as `M3.1` for the first workstream
of [M3](m03-routing-resilience.md).

## How to read the matrix

**Rows are capabilities, not pages.** The reference is the LiteLLM
documentation sitemap. Every page that describes a released gateway capability
maps to a row, and several pages may share one. These page classes have no row
of their own:

- tutorials, client setup guides and troubleshooting, sizing and policy pages;
- pages that only describe the Python SDK, which the
  [excluded SDK row](#client-protocols-and-endpoints) covers;
- per-vendor pages, which fold into the breadth rows below;
- features their own page marks unreleased. On the review date these were the
  Fusion model, LiteLLM Lens and the spend capture-rate check.

**A few rows have no LiteLLM equivalent.** Certification, accounting
completeness evidence, throughput routing and decision privacy are recorded
because they change how neighboring rows compare. They are `Ahead` and count
toward coverage. Capabilities the milestones add beyond LiteLLM have no row.

**Breadth rows close by contract, not by count.** Some rows count vendors:
provider breadth, cloud platforms, self-hosted runtimes, media providers,
vendor guardrails, logging destinations, alert channels, search vendors,
secret managers and billing integrations. Vendor-for-vendor parity is not the
goal. A breadth row reaches `Parity` when both conditions hold:

1. Every vendor the owning workstream names ships with that workstream's
   evidence, or is recorded in the milestone file with the reason it was
   declined.
2. Every other vendor LiteLLM lists is reachable through a documented
   extension contract that needs no change to OLP: a preset or plugin for
   providers, the webhook or WebAssembly contract for guardrails, OTLP or HTTPS
   sinks for logging and billing destinations, and signed webhooks for alert
   channels.

## Client protocols and endpoints

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| OpenAI Chat Completions | [/chat/completions](https://docs.litellm.ai/docs/completion) | Native and translated for every connector kind | `Parity` | |
| OpenAI Responses | [/responses](https://docs.litellm.ai/docs/response_api), bridged to chat providers | Native, translated, and stored responses for OpenAI and Azure | `Parity` | |
| OpenAI input token counting | [Token counting](https://docs.litellm.ai/docs/count_tokens) | `/v1/responses/input_tokens`, native and translated | `Parity` | |
| Responses compaction and Conversations | [/responses/compact](https://docs.litellm.ai/docs/response_api_compact) | Neither | `Gap` | M9.5 |
| Anthropic Messages and token counting | [/v1/messages](https://docs.litellm.ai/docs/anthropic_unified/), [count_tokens](https://docs.litellm.ai/docs/anthropic_count_tokens) | Native and translated | `Parity` | |
| Gemini generation, counting and embeddings | [/generateContent](https://docs.litellm.ai/docs/generateContent) | Native and translated generation and counting; `embedContent` and `batchEmbedContents` on strict routes to Gemini targets | `Parity` | |
| Gemini Interactions | [/interactions](https://docs.litellm.ai/docs/interactions) | Native `gemini-interactions` profile | `Parity` | |
| Bedrock Converse and InvokeModel | [/converse](https://docs.litellm.ai/docs/bedrock_converse), [/invoke](https://docs.litellm.ai/docs/bedrock_invoke) | Native Bedrock surface | `Parity` | |
| Certified native fidelity | Translation with optional [drop_params](https://docs.litellm.ai/docs/completion/drop_params) | Strict routes compile an interaction contract per target and refuse translation; transformed routes refuse what they cannot represent | `Ahead` | |
| Legacy text completions | [/completions](https://docs.litellm.ai/docs/text_completion) | Not served | `Gap` | M9.1 |
| Embeddings across providers | [Embeddings](https://docs.litellm.ai/docs/embedding/supported_embedding) | OpenAI, Azure, Gemini, Vertex, Bedrock Titan, compatible endpoints, Cohere v2, Voyage, TEI | `Partial` | M2.3 |
| Rerank across providers | [/rerank](https://docs.litellm.ai/docs/rerank) | Cohere, Voyage, TEI | `Partial` | M2.3 |
| Moderation | [/moderations](https://docs.litellm.ai/docs/moderation) | OpenAI, Azure, compatible endpoints | `Parity` | |
| Image generation, edits and variations | [Image generation](https://docs.litellm.ai/docs/image_generation), [edits](https://docs.litellm.ai/docs/image_edits) | OpenAI; Vertex Imagen and Bedrock Titan generation | `Partial` | M2.3 |
| Speech, transcription and translation | [Speech](https://docs.litellm.ai/docs/text_to_speech), [transcription](https://docs.litellm.ai/docs/audio_transcription) | OpenAI only | `Partial` | M2.3 |
| Video generation | [/videos](https://docs.litellm.ai/docs/videos) | OpenAI, as durable jobs | `Partial` | M2.3 |
| Files and batches | [/files](https://docs.litellm.ai/docs/files_endpoints), [/batches](https://docs.litellm.ai/docs/batches) across providers | OpenAI and Azure, with gateway-owned identifiers | `Partial` | M9.2 |
| Batch result cost tracking | [Enterprise](https://docs.litellm.ai/docs/batches) | Batch calls are accounted; per-line output usage is not settled | `Gap` | M9.2 |
| Cross-provider managed batches | [Managed batches](https://docs.litellm.ai/docs/proxy/managed_batches) (beta) | Not available | `Gap` | M9.2 |
| Fine-tuning | [/fine_tuning](https://docs.litellm.ai/docs/fine_tuning) | Not available | `Gap` | M9.3 |
| Vector stores | [/vector_stores](https://docs.litellm.ai/docs/vector_stores/) | Not available | `Gap` | M9.4 |
| RAG pipelines | [/rag/ingest](https://docs.litellm.ai/docs/rag_ingest), [/rag/query](https://docs.litellm.ai/docs/rag_query) | Not served | `Excluded` | |
| Realtime over WebSocket | [/realtime](https://docs.litellm.ai/docs/realtime) | OpenAI and Azure, plus Gemini Live | `Partial` | M9.6 |
| Realtime over WebRTC | [WebRTC](https://docs.litellm.ai/docs/proxy/realtime_webrtc) | Not available | `Gap` | M9.6 |
| OCR | [/ocr](https://docs.litellm.ai/docs/ocr), Mistral request shape | Not available | `Gap` | M9.7 |
| Web search | [/search](https://docs.litellm.ai/docs/search/), Perplexity request shape | Not available | `Gap` | M9.7 |
| Containers, skills, evals and managed agents | [/containers](https://docs.litellm.ai/docs/containers), [/skills](https://docs.litellm.ai/docs/skills), [/evals](https://docs.litellm.ai/docs/evals_api), [managed agents](https://docs.litellm.ai/docs/managed_agents) | Not available | `Gap` | M9.8 |
| Vendor pass-through endpoints | [Pass-through](https://docs.litellm.ai/docs/pass_through/intro) | Only registered surfaces | `Gap` | M9.9 |
| Model listing | `/v1/models`, model info and [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) | Key-visible routes on the OpenAI, Anthropic and Gemini surfaces | `Partial` | M11.4 |
| Prompt compression | [Headroom](https://docs.litellm.ai/docs/proxy/headroom) sidecar | Not served | `Excluded` | |
| Embeddable Python SDK and agent harness | [completion()](https://docs.litellm.ai/docs/completion/input), [litellm.agent](https://docs.litellm.ai/docs/harness) | Official vendor SDKs are the clients | `Excluded` | |
| Assistants API | [/assistants](https://docs.litellm.ai/docs/assistants) (retired by OpenAI) | Not served | `Excluded` | |
| Key-value memory | [/memory](https://docs.litellm.ai/docs/memory_management) | Not served | `Excluded` | |

## Providers and models

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Provider breadth | [About 170 provider pages](https://docs.litellm.ai/docs/providers) | Seven native connector kinds, provider plugins, and 13 OpenAI-compatible presets | `Gap` | M2.1, M2.2 |
| Self-hosted runtimes | Ollama, vLLM, LM Studio, Triton, llamafile and more | vLLM preset, custom endpoints, TEI native operations | `Partial` | M2.1 |
| Cloud AI platforms | Azure AI Foundry, Vertex partner models, SageMaker, watsonx, Databricks, Snowflake, OCI | Azure OpenAI, Vertex Gemini and Anthropic publishers, Bedrock | `Partial` | M2.2 |
| Media providers | ElevenLabs, Deepgram, Stability, Black Forest Labs, fal, Runway and more | OpenAI, Vertex Imagen, Bedrock Titan | `Gap` | M2.3 |
| Model facts and price map | [Model cost map](https://docs.litellm.ai/docs/proxy/sync_models_github), synced from GitHub | Operator model facts, context and modality facts imported by discovery, and operator-registered pricing sources | `Gap` | M2.4 |
| Custom pricing | [Custom pricing](https://docs.litellm.ai/docs/proxy/custom_pricing) | Immutable decimal pricing revisions scoped by connection, vendor or kind | `Parity` | |
| Model discovery | [Model discovery](https://docs.litellm.ai/docs/proxy/model_discovery) for wildcard models | Discovery from upstream lists or declared identifiers | `Parity` | |
| Capability certification | Not a serving gate | Exact-tuple certification before activation, by live probe or authenticated discovery | `Ahead` | |
| Custom providers | [Custom LLM server](https://docs.litellm.ai/docs/providers/custom_llm_server) (in-process Python) | Confined WebAssembly provider plugins with approved origins and grants | `Ahead` | |
| Keyless provider authentication | [OIDC to providers](https://docs.litellm.ai/docs/oidc) (beta) | Google application default credentials, the AWS default chain and Azure managed identity | `Parity` | |

## Routing and reliability

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Weighted load balancing | [Simple shuffle](https://docs.litellm.ai/docs/routing) | Priority tiers with weighted rendezvous | `Parity` | |
| Latency- and cost-based routing | [Routing strategies](https://docs.litellm.ai/docs/routing) | `latency` and `price` strategies with exact decimal price ceilings | `Parity` | |
| Throughput-based routing | Not among the documented strategies | `throughput` strategy | `Ahead` | |
| Hard policy constraints | [Tag routing](https://docs.litellm.ai/docs/proxy/tag_routing), [sensitive data routing](https://docs.litellm.ai/docs/proxy/guardrails/sensitive_data_routing) | Region, quantization, data collection, zero data retention, required parameters and price, intersected across scopes | `Ahead` | |
| Per-request routing controls | Request metadata and tags | `X-OLP-Routing`, which picks among the strategies a policy allows and otherwise only narrows it | `Parity` | |
| Routing explanation | `/utils/transform_request` shows the provider request | Deterministic draft and published route simulation of attempt order | `Ahead` | |
| Rate-limit-aware and least-busy routing | [Routing strategies](https://docs.litellm.ai/docs/routing) | Not available | `Gap` | M3.2 |
| Separate input and output token limits | [ITPM and OTPM](https://docs.litellm.ai/docs/proxy/io_token_rate_limits) (beta) | One combined token quota per connection and slot | `Gap` | M3.2 |
| Retries with per-error policy | [Reliability](https://docs.litellm.ai/docs/proxy/reliability) | Failover across targets and slots on transformed routes; strict routes stay on their first serving identity; no same-target backoff policy | `Partial` | M3.9 |
| Cross-model fallbacks | [Fallbacks](https://docs.litellm.ai/docs/proxy/reliability), including context-window and content-policy fallbacks | Failover inside a transformed route, including context-window rejections | `Partial` | M3.1 |
| Budget fallbacks | [Budget fallbacks](https://docs.litellm.ai/docs/proxy/budget_fallbacks) | An exhausted budget answers 429 | `Gap` | M3.1 |
| Context-window pre-checks | Pre-call checks | Model facts exclude targets whose context cannot fit the estimate | `Parity` | |
| Cooldowns shared across replicas | Redis-backed cooldowns | Shared credential and slot cooldowns in Valkey; per-gateway circuits | `Partial` | M3.5 |
| Timeouts | [Timeouts](https://docs.litellm.ai/docs/proxy/timeout) | Route deadline, target timeouts, first-byte and idle bounds | `Parity` | |
| Priority request queue | [Request prioritization](https://docs.litellm.ai/docs/scheduler) (beta) | A full admission pool answers 503 | `Gap` | M3.3 |
| Dynamic capacity allocation | [Dynamic TPM/RPM allocation](https://docs.litellm.ai/docs/proxy/dynamic_rate_limit) | Not available | `Gap` | M3.3 |
| Provider and deployment budgets | [Budget routing](https://docs.litellm.ai/docs/proxy/provider_budget_routing) | Not available | `Gap` | M3.4 |
| Health-check-driven routing | [Health check routing](https://docs.litellm.ai/docs/proxy/health_check_routing) | Passive circuit breakers | `Gap` | M3.5 |
| Traffic mirroring | [Traffic mirroring](https://docs.litellm.ai/docs/traffic_mirroring) | Not available | `Gap` | M3.6 |
| Automatic request routing | [Auto routing](https://docs.litellm.ai/docs/auto_router/), [adaptive router](https://docs.litellm.ai/docs/adaptive_router) (beta) | Not available | `Gap` | M3.7 |
| Custom routing logic | [Routing plugins](https://docs.litellm.ai/docs/routing_plugins) | Not available | `Gap` | M3.7 |
| Wildcard routing | [Wildcard routing](https://docs.litellm.ai/docs/wildcard_routing) | Explicit routes and bulk route creation in the console | `Partial` | M3.8 |
| Per-team credential routing | [Credential routing](https://docs.litellm.ai/docs/proxy/credential_routing) | Project boundaries and slot route and key restrictions | `Parity` | |

## Caching

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Exact response cache | [Caching](https://docs.litellm.ai/docs/proxy/caching): Redis, Valkey, S3, GCS, memory, disk | Not available | `Gap` | M8.1 |
| Semantic cache | [Semantic caching](https://docs.litellm.ai/docs/proxy/caching_semantic) | Not available | `Gap` | M8.2 |
| Per-request cache controls | [Cache controls](https://docs.litellm.ai/docs/proxy/caching_controls) | Not available | `Gap` | M8.1 |
| Provider prompt caching | Pass-through and `cache_control` injection | Native pass-through; cache read and write tokens priced, including 5-minute and 1-hour writes | `Partial` | M8.3 |

## Identity, tenancy and access

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Virtual keys | [Virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys) | Digest-only keys with scopes, expiry, route allowlists and rotation | `Parity` | |
| Scheduled key rotation | [Enterprise](https://docs.litellm.ai/docs/proxy/virtual_keys), with a grace period and delivery to a secret manager | On-demand rotation with no overlap | `Partial` | M4.3, M11.3 |
| Teams | [Teams](https://docs.litellm.ai/docs/proxy/multi_tenant_architecture) | Projects with manager and viewer membership | `Parity` | |
| Organizations and delegated admins | [Enterprise](https://docs.litellm.ai/docs/proxy/access_control) | Installation roles and project managers | `Gap` | M4.5 |
| Hierarchy depth | [Projects](https://docs.litellm.ai/docs/proxy/project_management) between teams and keys (Enterprise, beta) | Projects and keys; budget groups are cost pools, not a tenancy level | `Partial` | M4.5; remains partial under its recommended single-organization-level model |
| Roles | [RBAC](https://docs.litellm.ai/docs/proxy/access_control) | Owner, operator, developer and viewer, contract-declared per operation | `Parity` | |
| End users with limits | [Customers](https://docs.litellm.ai/docs/proxy/customers) | Attribution labels only | `Gap` | M4.1 |
| Service accounts | [Service accounts](https://docs.litellm.ai/docs/proxy/service_accounts) | Keys outlive their issuer and keep their project | `Parity` | |
| Access groups | [Model access groups](https://docs.litellm.ai/docs/proxy/model_access_groups), and [access groups](https://docs.litellm.ai/docs/proxy/access_groups) across models, MCP servers and agents | Per-key route allowlists | `Partial` | M4.3 |
| OIDC single sign-on | [Enterprise beyond five users](https://docs.litellm.ai/docs/proxy/admin_ui_sso) | Core OIDC with role mapping and push provisioning | `Ahead` | |
| SAML single sign-on | [Enterprise beyond five users](https://docs.litellm.ai/docs/proxy/saml_sso) | Not available | `Gap` | M4.5 |
| SCIM provisioning | [Enterprise](https://docs.litellm.ai/docs/proxy/identity_provisioning) | Push provisioning API, not SCIM | `Partial` | M4.5 |
| JWT authentication for requests | [Enterprise](https://docs.litellm.ai/docs/proxy/token_auth) | Not available | `Gap` | M4.4 |
| OAuth 2.0 token introspection | [Enterprise](https://docs.litellm.ai/docs/proxy/oauth2) | Not available | `Gap` | M4.4 |
| IP allowlists | [Enterprise](https://docs.litellm.ai/docs/proxy/ip_address) | Trusted-proxy client address resolution only | `Gap` | M4.3 |
| Route-level access control | [Enterprise public routes](https://docs.litellm.ai/docs/proxy/public_routes) | Every management route admitted from its contract requirement, held by a golden sweep | `Ahead` | |
| Caller-supplied provider credentials | [Client-side credentials](https://docs.litellm.ai/docs/proxy/clientside_auth) | Not available | `Gap` | M4.6 |
| Caller-chosen upstream base URLs | [Client-side `api_base`](https://docs.litellm.ai/docs/proxy/clientside_auth) | Destinations are operator-declared | `Excluded` | |
| Audit logs | [Enterprise](https://docs.litellm.ai/docs/enterprise) | Metadata-only audit for every mutation | `Ahead` | |
| Invitations and onboarding | [Self-serve](https://docs.litellm.ai/docs/proxy/self_serve) | Invitations and mapped OIDC provisioning | `Parity` | |
| Email delivery | [Enterprise](https://docs.litellm.ai/docs/proxy/email) | Not available | `Gap` | M5.4 |

## Budgets and rate limits

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Key request, token and concurrency limits | [Rate limits](https://docs.litellm.ai/docs/proxy/users) | Requests and tokens per minute, concurrency | `Parity` | |
| Key budgets with reset periods | [Budgets](https://docs.litellm.ai/docs/proxy/users) with arbitrary durations | Daily and monthly UTC windows | `Partial` | M4.2 |
| Budget reset time zone | [Reset and time zone](https://docs.litellm.ai/docs/proxy/budget_reset_and_tz) | UTC only | `Gap` | M4.2 |
| User and team budgets | [Team budgets](https://docs.litellm.ai/docs/proxy/team_budgets) | Shared budget groups; no project budget | `Partial` | M4.2 |
| Per-model limits on a key | [Model-specific budgets](https://docs.litellm.ai/docs/proxy/users) | Not available | `Gap` | M4.2 |
| Model access group budgets | [Group budgets](https://docs.litellm.ai/docs/proxy/model_access_group_budgets) | Not available | `Gap` | M4.2 |
| Tag budgets | [Enterprise](https://docs.litellm.ai/docs/proxy/tag_budgets) | Not available | `Gap` | M4.2 |
| Budget and limit tiers | [Enterprise](https://docs.litellm.ai/docs/proxy/rate_limit_tiers) | Not available | `Gap` | M4.2 |
| Temporary budget increases | [Enterprise](https://docs.litellm.ai/docs/proxy/temporary_budget_increase) | Not available | `Gap` | M4.2 |
| Soft budget alerts | [Alerting](https://docs.litellm.ai/docs/proxy/alerting) | `budget.threshold` notification rules | `Parity` | |
| Distributed enforcement | Redis counters | Valkey scripts on the server clock, PostgreSQL spend authority, reconciliation, fail-closed cost budgets | `Ahead` | |
| Deployment rate limits | Deployment `rpm` and `tpm` | Connection and slot request, token and concurrency quotas | `Parity` | |

## Cost and pricing

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Per-request cost | [Cost tracking](https://docs.litellm.ai/docs/proxy/cost_tracking) | Attempt-level cost from pinned pricing revisions; unpriced work stays visible | `Parity` | |
| Accounting completeness evidence | No equivalent documented | Completeness reports, gateway epochs and explicit gaps | `Ahead` | |
| Spend by key, team, tag and model | [Cost tracking](https://docs.litellm.ai/docs/proxy/cost_tracking) | Usage summary, breakdown and time series with attribution filters | `Parity` | |
| Spend reports | [Spend reports](https://docs.litellm.ai/docs/proxy/cost_tracking) and scheduled reports | Usage API and console | `Partial` | M5.5 |
| Discounts and margins | [Discounts](https://docs.litellm.ai/docs/proxy/provider_discounts), [margins](https://docs.litellm.ai/docs/proxy/provider_margins) | Publish-time price overrides | `Partial` | M6.2 |
| Off-peak pricing | [Off-peak pricing](https://docs.litellm.ai/docs/proxy/off_peak_pricing) | Not available | `Gap` | M6.1 |
| Provisioned throughput costs | [PTU flat cost](https://docs.litellm.ai/docs/proxy/ptu_flat_cost) | Not available | `Gap` | M6.1 |
| Pricing calculator | [Pricing calculator](https://docs.litellm.ai/docs/proxy/pricing_calculator) | Not available | `Gap` | M6.3 |
| Billing integrations | [Lago](https://docs.litellm.ai/docs/observability/lago), [OpenMeter](https://docs.litellm.ai/docs/observability/openmeter), [billing](https://docs.litellm.ai/docs/proxy/billing) | Not available | `Gap` | M6.4 |
| FOCUS cost export | [FOCUS](https://docs.litellm.ai/docs/observability/focus) | Not available | `Gap` | M6.4 |

## Guardrails

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Guardrail framework | [Guardrails](https://docs.litellm.ai/docs/proxy/guardrails/quick_start): pre-call, during-call, post-call, logging-only | RE2 block and redact rules on input and unary output | `Partial` | M7.1 |
| Decision privacy | Guardrail traces in logs | Only `{rule_id, phase, action, outcome}` persists | `Ahead` | |
| PII masking | [PII masking](https://docs.litellm.ai/docs/proxy/guardrails/pii_masking_v2) | Pattern redaction only | `Partial` | M7.2 |
| Secret detection | [Secret detection](https://docs.litellm.ai/docs/proxy/guardrails/secret_detection) | Not available | `Gap` | M7.2 |
| Prompt-injection detection | [Prompt injection](https://docs.litellm.ai/docs/proxy/guardrails/prompt_injection) | Not available | `Gap` | M7.4 |
| Vendor guardrails | [About 50 providers](https://docs.litellm.ai/docs/guardrail_providers) | Not available | `Gap` | M7.4 |
| Custom guardrails | [Custom guardrail](https://docs.litellm.ai/docs/proxy/guardrails/custom_guardrail) (in-process Python) | Not available | `Gap` | M7.3, M7.5 |
| Model-based guardrails | [LLM as a judge](https://docs.litellm.ai/docs/proxy/guardrails/llm_as_a_judge) | Not available | `Gap` | M7.4 |
| Key- and team-scoped guardrails | [Enterprise](https://docs.litellm.ai/docs/proxy/guardrails/quick_start), [policies](https://docs.litellm.ai/docs/proxy/guardrails/guardrail_policies) | Route-level content policy | `Partial` | M7.1 |
| Streaming output guardrails | Supported by some guardrails | Output rules require unary responses | `Gap` | M7.1 |
| Guardrails on batch files and realtime sessions | [Batch](https://docs.litellm.ai/docs/proxy/guardrails/batch_guardrails), [realtime](https://docs.litellm.ai/docs/proxy/guardrails/realtime_guardrails) | Content policy refuses surfaces it cannot inspect | `Gap` | M7.1 |
| Tool permissions and policies | [Tool permission](https://docs.litellm.ai/docs/proxy/guardrails/tool_permission), [tool policies](https://docs.litellm.ai/docs/proxy/tool_policies) | Not available | `Gap` | M7.6 |
| Apply-guardrail endpoint | [/guardrails/apply_guardrail](https://docs.litellm.ai/docs/apply_guardrail) | Not available | `Gap` | M7.7 |

## Observability and alerting

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Operational metrics | [Prometheus](https://docs.litellm.ai/docs/proxy/prometheus) | Admission, request, provider, limiter, accounting and worker metrics | `Parity` | |
| Usage and spend metrics | [Prometheus](https://docs.litellm.ai/docs/proxy/prometheus) token, spend and budget series | No token or spend series | `Gap` | M5.3 |
| Distributed tracing | [OpenTelemetry](https://docs.litellm.ai/docs/observability/opentelemetry_integration) | OTLP traces with a content-free attribute allowlist | `Parity` | |
| GenAI semantic conventions | [OpenTelemetry v2](https://docs.litellm.ai/docs/observability/opentelemetry_v2) | Not followed | `Gap` | M5.3 |
| Logging integrations | [About 50 destinations](https://docs.litellm.ai/docs/observability/callbacks) | Not available | `Gap` | M5.1 |
| Per-team logging destinations | [Enterprise](https://docs.litellm.ai/docs/proxy/team_logging) | Not available | `Gap` | M5.1 |
| Opt-in prompt and response capture | [store_prompts_in_spend_logs](https://docs.litellm.ai/docs/proxy/ui_logs) | Never captured | `Gap` | M5.2 |
| Per-request logging controls | [Enterprise](https://docs.litellm.ai/docs/proxy/dynamic_logging) | Not applicable while nothing is captured | `Gap` | M5.2 |
| Request log viewer | [UI logs](https://docs.litellm.ai/docs/proxy/ui_logs) | Request history with attempts, usage and policy decisions | `Parity` | |
| Session grouping | [Sessions](https://docs.litellm.ai/docs/proxy/ui_logs_sessions) | Attribution labels | `Partial` | M5.5 |
| Alert channels | [Slack, Discord, Teams](https://docs.litellm.ai/docs/proxy/alerting), [PagerDuty](https://docs.litellm.ai/docs/proxy/pagerduty) | Webhooks with optional HMAC signatures | `Partial` | M5.4 |
| Alert events | Hanging, slow and failed calls, outages, budgets, reports | `budget.threshold`, `provider.grant.lapsed` | `Partial` | M5.4 |
| Health endpoints | [Health checks](https://docs.litellm.ai/docs/proxy/health) | Private liveness, readiness and metrics; provider health API | `Parity` | |
| Log retention | [Retention](https://docs.litellm.ai/docs/proxy/spend_logs_deletion) | Retention settings enforced by the maintenance worker | `Parity` | |
| Audit and security event export | [Azure Sentinel](https://docs.litellm.ai/docs/observability/azure_sentinel), [Splunk](https://docs.litellm.ai/docs/observability/splunk_observability_cloud) | Audit API only | `Partial` | M5.1 |

## Agents and tools

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| MCP gateway | [MCP](https://docs.litellm.ai/docs/mcp) | Not available | `Gap` | M10.1 |
| MCP OAuth and per-user credentials | [MCP OAuth](https://docs.litellm.ai/docs/mcp_oauth), [per-user auth](https://docs.litellm.ai/docs/mcp_per_user_auth) | Plugin-mediated grant enrollment and refresh for provider credentials only | `Gap` | M10.1 |
| MCP token exchange, SigV4 and signed assertions | [On-behalf-of](https://docs.litellm.ai/docs/mcp_obo_auth), [ID-JAG](https://docs.litellm.ai/docs/mcp_id_jag), [SigV4](https://docs.litellm.ai/docs/mcp_aws_sigv4), [JWT signer](https://docs.litellm.ai/docs/mcp_zero_trust) | Not available | `Gap` | M10.1 |
| MCP servers from OpenAPI specifications | [MCP from OpenAPI](https://docs.litellm.ai/docs/mcp_openapi) | Not available | `Gap` | M10.1 |
| MCP cost tracking | [MCP cost](https://docs.litellm.ai/docs/mcp_cost) | Not available | `Gap` | M10.1 |
| MCP tools for every model | [MCP](https://docs.litellm.ai/docs/mcp) | Not available | `Gap` | M10.2 |
| MCP tool search and filtering | [Tool search](https://docs.litellm.ai/docs/mcp_tool_search), [semantic filter](https://docs.litellm.ai/docs/mcp_semantic_filter) | Not available | `Gap` | M10.2 |
| Server-side web search for every model | [Web search interception](https://docs.litellm.ai/docs/integrations/websearch_interception) | Not available | `Gap` | M10.2 |
| A2A agent gateway | [A2A](https://docs.litellm.ai/docs/a2a), with [iteration budgets](https://docs.litellm.ai/docs/a2a_iteration_budgets) and a [kill switch](https://docs.litellm.ai/docs/a2a_kill_switch) | Not available | `Gap` | M10.3 |
| Prompt management | [Prompt management](https://docs.litellm.ai/docs/proxy/prompt_management) | Not available | `Gap` | M10.4 |
| Skills registry | [Skills gateway](https://docs.litellm.ai/docs/skills_gateway) | Not available | `Gap` | M10.4 |
| Code-execution sandbox | [Sandbox](https://docs.litellm.ai/docs/sandbox) | Not available | `Excluded` | |

## Administration and deployment

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Admin console | [Admin UI](https://docs.litellm.ai/docs/proxy/ui) | Console for the management operations members perform; push provisioning is API-only | `Parity` | |
| Playground | Test key | Playground with streaming, native operations and realtime traces | `Parity` | |
| Model comparison playground | [Model compare](https://docs.litellm.ai/docs/proxy/model_compare_ui) | One route at a time | `Gap` | M11.7 |
| Configuration as code | [config.yaml](https://docs.litellm.ai/docs/proxy/configs) and database models | Digest-addressed export, plan and apply for projects, providers, routes and pricing | `Ahead` | |
| Live configuration changes | [Database models](https://docs.litellm.ai/docs/proxy/model_management) | Atomic, digest-verified runtime generations pinned per request | `Ahead` | |
| Management CLI | [lite CLI](https://docs.litellm.ai/docs/proxy/management_cli) | Operator commands only | `Gap` | M11.1 |
| CLI sign-in | [CLI authentication](https://docs.litellm.ai/docs/proxy/cli_sso) (beta) | Not available | `Gap` | M11.1 |
| Secret managers | [Enterprise](https://docs.litellm.ai/docs/secret_managers/overview) | Mounted key files; secrets sealed in PostgreSQL | `Gap` | M11.3 |
| Master key rotation | [Master key rotation](https://docs.litellm.ai/docs/proxy/master_key_rotations) | A versioned key ring with re-encryption and retirement checks | `Parity` | |
| Kubernetes packaging | [Helm](https://docs.litellm.ai/docs/proxy/deploy) | Helm chart and Compose | `Parity` | |
| Separate admin and worker roles | [Enterprise](https://docs.litellm.ai/docs/enterprise) | `gateway`, `control` and `worker` process modes, or `all` in one process | `Ahead` | |
| Database read replicas | [Read replica](https://docs.litellm.ai/docs/proxy/db_read_replica) | Not available | `Gap` | M11.5 |
| Multi-region deployment | [Enterprise](https://docs.litellm.ai/docs/proxy/multi_region) | Single region | `Gap` | M11.5 |
| Central control of several installations | [Enterprise](https://docs.litellm.ai/docs/proxy/global_control_plane) | Not available | `Gap` | M11.5 |
| Model hub | [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) | Not available | `Gap` | M11.4 |
| Administration through agents | [LiteAdmin MCP](https://docs.litellm.ai/docs/proxy/liteadmin_mcp) | Not available | `Gap` | M11.6 |
| Custom branding | [Logo](https://docs.litellm.ai/docs/proxy/ui/ui_edit_logo) | Not available | `Gap` | M11.7 |
| In-process hooks | [Call hooks](https://docs.litellm.ai/docs/proxy/call_hooks), [custom auth](https://docs.litellm.ai/docs/proxy/custom_auth), [post-call rules](https://docs.litellm.ai/docs/proxy/rules) | Not available | `Excluded` | |
| Console extension plugins | [UI plugins](https://docs.litellm.ai/docs/proxy/plugins) | Not available | `Excluded` | |

## Performance

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Published overhead | [Benchmarks](https://docs.litellm.ai/docs/benchmarks): 8 ms P95 at 1k RPS; 3,000 RPS large-prompt profile | Unmeasured | `Gap` | M1.1 |
| Compiled data plane | [Rust gateway](https://docs.litellm.ai/docs/proxy/rust_gateway) (beta, opt-in per model) | Go data plane on every path | `Ahead` | |
| Accurate token counting | Rust token counting in the [high-throughput profile](https://docs.litellm.ai/docs/proxy/high_throughput) | Four-characters-per-token estimate | `Gap` | M1.2 |
