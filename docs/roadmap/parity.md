# Parity matrix

This matrix compares every released capability of the LiteLLM AI Gateway with
OLP and is the authoritative parity scope of the [roadmap](README.md). The
milestones also add OLP-original capabilities, which have no row here. The
LiteLLM column links its documentation as reviewed on 2026-10-01; the OLP
column describes this repository at 0.1.0, updated for what a milestone in
progress has delivered so far ([M1](m01-measured-advantage.md#delivered),
[M2](m02-provider-catalog.md#delivered)).

| Status | Meaning |
| --- | --- |
| `Ahead` | OLP offers the capability with a stronger guarantee, or in its core product where LiteLLM requires an Enterprise license. |
| `Parity` | OLP offers an equivalent capability. |
| `Partial` | OLP covers part of the capability; the milestone completes it. |
| `Gap` | OLP lacks the capability; the milestone delivers it. |
| `Excluded` | Out of scope by design; see [out of scope](README.md#out-of-scope-by-design). |

Milestone references name a workstream, such as `M3.1` for the first workstream
of [M3](m03-routing-resilience.md).

## Client protocols and endpoints

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| OpenAI Chat Completions | [/chat/completions](https://docs.litellm.ai/docs/completion) | Native and translated for every connector kind | `Parity` | |
| OpenAI Responses | [/responses](https://docs.litellm.ai/docs/response_api), bridged to chat providers | Native, translated, and stored responses for OpenAI and Azure; Codex CLI and the OpenAI Agents SDK are [qualified](../clients.md) | `Parity` | |
| Responses compaction and Conversations | [/responses/compact](https://docs.litellm.ai/docs/response_api_compact) | Neither | `Gap` | M9.5 |
| Anthropic Messages and token counting | [/v1/messages](https://docs.litellm.ai/docs/anthropic_unified/), [count_tokens](https://docs.litellm.ai/docs/anthropic_count_tokens) | Native and translated; Claude Code is [qualified](../clients.md) | `Parity` | |
| Gemini generation, counting and embeddings | [/generateContent](https://docs.litellm.ai/docs/generateContent) | Native and translated generation and counting; `embedContent` and `batchEmbedContents` on strict routes to Gemini targets; Gemini CLI is [qualified](../clients.md) | `Parity` | |
| Gemini Interactions | [/interactions](https://docs.litellm.ai/docs/interactions) | Native `gemini-interactions` profile | `Parity` | |
| Bedrock Converse and InvokeModel | [/converse](https://docs.litellm.ai/docs/bedrock_converse), [/invoke](https://docs.litellm.ai/docs/bedrock_invoke) | Native Bedrock surface | `Parity` | |
| Certified native fidelity | Translation with optional [drop_params](https://docs.litellm.ai/docs/completion/drop_params) | Strict routes compile an interaction contract per target and refuse lossy translation | `Ahead` | |
| Legacy text completions | [/completions](https://docs.litellm.ai/docs/text_completion) | Not served | `Gap` | M9.1 |
| Embeddings across providers | [Embeddings](https://docs.litellm.ai/docs/embedding/supported_embedding) | OpenAI, Azure, Gemini, Vertex, Bedrock Titan, compatible endpoints, Cohere v2, Voyage, Jina, Mistral, NVIDIA NIM, Together, Infinity, TEI and the embedding presets | `Parity` | |
| Rerank across providers | [/rerank](https://docs.litellm.ai/docs/rerank) | Cohere, Voyage, Jina, Together, Infinity, TEI and Bedrock rerank models | `Parity` | |
| Moderation | [/moderations](https://docs.litellm.ai/docs/moderation) | OpenAI, Azure, compatible endpoints | `Parity` | |
| Image generation, edits and variations | [Image generation](https://docs.litellm.ai/docs/image_generation), [edits](https://docs.litellm.ai/docs/image_edits) | OpenAI and Azure OpenAI; Gemini image models on the Gemini API and Vertex, and Vertex Imagen until its retirement; Bedrock Titan and Stability; Stability AI, Recraft, Black Forest Labs and xAI. Variations are OpenAI's alone | `Parity` | |
| Speech, transcription and translation | [Speech](https://docs.litellm.ai/docs/text_to_speech), [transcription](https://docs.litellm.ai/docs/audio_transcription) | Speech from OpenAI, Azure OpenAI, Gemini, ElevenLabs, Deepgram and Amazon Polly; transcription from OpenAI, Azure OpenAI, Gemini, Groq, ElevenLabs, Deepgram and AssemblyAI; translation from OpenAI, Azure OpenAI and Groq | `Parity` | |
| Video generation | [/videos](https://docs.litellm.ai/docs/videos) | OpenAI and Runway, including Runway's Veo models, as durable jobs. Vendors whose video models are ending are declined ([M2.3](m02-provider-catalog.md#settled-during-implementation-1)) | `Parity` | |
| Files and batches | [/files](https://docs.litellm.ai/docs/files_endpoints), [/batches](https://docs.litellm.ai/docs/batches) across providers | OpenAI and Azure, with gateway-owned identifiers | `Partial` | M9.2 |
| Batch result cost tracking | [Enterprise](https://docs.litellm.ai/docs/batches) | Batch calls are accounted; per-line output usage is not settled | `Gap` | M9.2 |
| Cross-provider managed batches | [Managed batches](https://docs.litellm.ai/docs/proxy/managed_batches) (beta) | Not available | `Gap` | M9.2 |
| Fine-tuning | [/fine_tuning](https://docs.litellm.ai/docs/fine_tuning) | Not available | `Gap` | M9.3 |
| Vector stores and retrieval | [/vector_stores](https://docs.litellm.ai/docs/vector_stores/), [RAG query](https://docs.litellm.ai/docs/rag_query) | Not available | `Gap` | M9.4 |
| Realtime over WebSocket | [/realtime](https://docs.litellm.ai/docs/realtime) | OpenAI and Azure, plus Gemini Live | `Partial` | M9.6 |
| Realtime over WebRTC | [WebRTC](https://docs.litellm.ai/docs/proxy/realtime_webrtc) | Not available | `Gap` | M9.6 |
| OCR | [/ocr](https://docs.litellm.ai/docs/ocr), Mistral request shape | Not available | `Gap` | M9.7 |
| Web search | [/search](https://docs.litellm.ai/docs/search/), Perplexity request shape | Not available | `Gap` | M9.7 |
| Containers, skills and evals | [/containers](https://docs.litellm.ai/docs/containers), [/skills](https://docs.litellm.ai/docs/skills), [/evals](https://docs.litellm.ai/docs/evals_api) | Not available | `Gap` | M9.8 |
| Vendor pass-through endpoints | [Pass-through](https://docs.litellm.ai/docs/pass_through/intro) | Only registered surfaces | `Gap` | M9.9 |
| Model listing | `/v1/models`, model info and [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) | Key-visible routes on each surface | `Partial` | M11.4 |
| Assistants API | [/assistants](https://docs.litellm.ai/docs/assistants) (retired by OpenAI) | Not served | `Excluded` | |
| Key-value memory | [/memory](https://docs.litellm.ai/docs/memory_management) | Not served | `Excluded` | |

## Providers and models

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Provider breadth | [About 170 provider pages](https://docs.litellm.ai/docs/providers) | Nine native connector kinds, first-party profiles and native dialects, provider plugins with authoring templates, and 50 reviewed OpenAI-compatible presets | `Parity` | |
| Self-hosted runtimes | Ollama, vLLM, LM Studio, Triton, llamafile and more | vLLM, Ollama, LM Studio, llama.cpp, Docker Model Runner and Infinity presets, custom endpoints, TEI native operations | `Parity` | |
| Cloud AI platforms | Azure AI Foundry, Vertex partner models, SageMaker, watsonx, Databricks, Snowflake, OCI | Azure OpenAI and AI Foundry, Vertex Gemini, Anthropic and OpenAI-compatible partner models, Bedrock, SageMaker AI, watsonx.ai, Databricks, Snowflake Cortex; OCI through a plugin built from the signed-request template | `Parity` | |
| Media providers | ElevenLabs, Deepgram, Stability, Black Forest Labs, fal, Runway and more | ElevenLabs, Deepgram, AssemblyAI, Stability AI, Black Forest Labs, Recraft, Runway, Amazon Polly and the media of Azure OpenAI, Gemini, Groq and xAI. fal is declined ([M2.3](m02-provider-catalog.md#settled-during-implementation-1)) | `Parity` | |
| Model facts and price map | [Model cost map](https://docs.litellm.ai/docs/proxy/sync_models_github), synced from GitHub | A [signed reference catalog](../catalog.md) of model facts, list prices and lifecycle dates with provenance, refreshed as a reviewed pricing source and offered to discovery | `Parity` | |
| Custom pricing | [Custom pricing](https://docs.litellm.ai/docs/proxy/custom_pricing) | Immutable decimal pricing revisions scoped by connection, vendor or kind | `Parity` | |
| Model discovery | [Model discovery](https://docs.litellm.ai/docs/proxy/model_discovery) for wildcard models | Discovery from upstream lists or declared identifiers | `Parity` | |
| Capability certification | Not a serving gate | Exact-tuple live certification before activation | `Ahead` | |
| Custom providers | [Custom LLM server](https://docs.litellm.ai/docs/providers/custom_llm_server) (in-process Python) | Confined WebAssembly provider plugins with approved origins and grants | `Ahead` | |

## Routing and reliability

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Weighted load balancing | [Simple shuffle](https://docs.litellm.ai/docs/routing) | Priority tiers with weighted rendezvous | `Parity` | |
| Latency- and cost-based routing | [Routing strategies](https://docs.litellm.ai/docs/routing) | `latency` and `price` strategies with exact decimal price ceilings | `Parity` | |
| Throughput-based routing | Not among the documented strategies | `throughput` strategy | `Ahead` | |
| Hard policy constraints | [Tag routing](https://docs.litellm.ai/docs/proxy/tag_routing), [sensitive data routing](https://docs.litellm.ai/docs/proxy/guardrails/sensitive_data_routing) | Region, quantization, data collection, zero data retention, required parameters and price, intersected across scopes | `Ahead` | |
| Per-request routing controls | Request metadata and tags | `X-OLP-Routing`, which can only narrow published policy | `Parity` | |
| Routing explanation | `/utils/transform_request` shows the provider request | Deterministic draft and published route simulation of attempt order | `Ahead` | |
| Rate-limit-aware and least-busy routing | [Routing strategies](https://docs.litellm.ai/docs/routing) | `capacity` strategy ordering slots by remaining request, token and concurrency headroom from the admission windows, in one pipelined read, explained in simulation | `Parity` | |
| Retries with per-error policy | [Reliability](https://docs.litellm.ai/docs/proxy/reliability) | Per-class same-slot retries with full-jitter backoff and `Retry-After`, inside the attempt budget and never after commitment | `Parity` | |
| Cross-model fallbacks | [Fallbacks](https://docs.litellm.ai/docs/proxy/reliability), including context-window and content-policy fallbacks | Fallback routes on exhaustion, context window, content filter, rate limit and spend caps, acyclic and bounded by the named route's deadline and budget | `Ahead` | |
| Context-window pre-checks | Pre-call checks | Model facts exclude targets whose context cannot fit the estimate, counted for each target's model family | `Parity` | |
| Cooldowns shared across replicas | Redis-backed cooldowns | Shared credential and slot cooldowns and circuits in Valkey, honored fleet-wide within five seconds | `Parity` | |
| Timeouts | [Timeouts](https://docs.litellm.ai/docs/proxy/timeout) | Route deadline, target timeouts, first-byte and idle bounds | `Parity` | |
| Priority request queue | [Request prioritization](https://docs.litellm.ai/docs/scheduler) (beta) | Bounded weighted-fair admission queue in four classes, with key-capped priority | `Parity` | |
| Dynamic capacity allocation | [Dynamic TPM/RPM allocation](https://docs.litellm.ai/docs/proxy/dynamic_rate_limit) | Per-priority shares of connection and slot quotas above a saturation threshold, enforced atomically across gateways | `Parity` | |
| Provider and deployment budgets | [Budget routing](https://docs.litellm.ai/docs/proxy/provider_budget_routing) | Exact-decimal daily and monthly caps on connections, slots and routes that remove them from selection and can start a fallback | `Ahead` | |
| Health-check-driven routing | [Health check routing](https://docs.litellm.ai/docs/proxy/health_check_routing) | Opt-in accounted active probes and fleet-shared circuits that order unhealthy targets last | `Parity` | |
| Traffic mirroring | [Traffic mirroring](https://docs.litellm.ai/docs/traffic_mirroring) | Sampled shadow targets under the request's hard constraints, in their own pool, accounted to the route, with an experiment report | `Ahead` | |
| Automatic request routing | [Auto routing](https://docs.litellm.ai/docs/auto_router/), [adaptive router](https://docs.litellm.ai/docs/adaptive_router) (beta) | Ordered route selectors over request features and classifier routes, simulated before publication, with a savings report | `Parity` | |
| Custom routing logic | [Routing plugins](https://docs.litellm.ai/docs/routing_plugins) | Confined WebAssembly route predicates that can only narrow the candidate set | `Ahead` | |
| Wildcard routing | [Wildcard routing](https://docs.litellm.ai/docs/wildcard_routing) | Route templates that publish newly certified models as ordinary routes, without uncertified passthrough | `Ahead` | |
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
| Scheduled key rotation | [Enterprise](https://docs.litellm.ai/docs/proxy/virtual_keys) | Declared intervals, expiry/due reminders, explicit idempotent rotation and bounded overlap | `Parity` | M4.3 |
| Teams | [Teams](https://docs.litellm.ai/docs/proxy/multi_tenant_architecture) | Projects with manager and viewer membership | `Parity` | |
| Organizations and delegated admins | [Enterprise](https://docs.litellm.ai/docs/proxy/access_control) | One-level organizations, inherited project scope, delegated management, immutable project ownership and aggregate caps | `Parity` | M4.5 |
| Roles | [RBAC](https://docs.litellm.ai/docs/proxy/access_control) | Owner, operator, developer and viewer, contract-declared per operation | `Parity` | |
| End users with limits | [Customers](https://docs.litellm.ai/docs/proxy/customers) | Project-scoped digests, reporting, key/project limits and blocking; concurrent and protocol-boundary evidence | `Parity` | M4.1 |
| Service accounts | [Service accounts](https://docs.litellm.ai/docs/proxy/service_accounts) | Keys outlive their issuer and keep their project | `Parity` | |
| Model access groups | [Access groups](https://docs.litellm.ai/docs/proxy/model_access_groups) | Project route groups, live authority refresh, union allowlists | `Parity` | M4.3 |
| OIDC single sign-on | [Enterprise beyond five users](https://docs.litellm.ai/docs/proxy/admin_ui_sso) | Core OIDC with role mapping and push provisioning | `Ahead` | |
| SAML single sign-on | [SAML](https://docs.litellm.ai/docs/proxy/saml_sso) | Signed SP-initiated login, metadata import, browser binding, role/link/owner protection, local signing keys and promotion | `Parity` | M4.5 |
| SCIM provisioning | [Enterprise](https://docs.litellm.ai/docs/proxy/identity_provisioning) | SCIM Users/Groups, atomic PATCH, discovery/filtering, inherited role/project grants, console and mapping promotion | `Parity` | M4.5 |
| JWT authentication for requests | [Enterprise](https://docs.litellm.ai/docs/proxy/token_auth) | Declared issuers, bounded JWT verification, digest principals, live revocation and promotion | `Parity` | M4.4 |
| IP allowlists | [Enterprise](https://docs.litellm.ai/docs/proxy/ip_address) | Key and management CIDRs with trusted-proxy resolution, live-session checks and deployment recovery | `Parity` | M4.3 |
| Route-level access control | [Enterprise public routes](https://docs.litellm.ai/docs/proxy/public_routes) | Every management route admitted from its contract requirement, held by a golden sweep | `Ahead` | |
| Caller-supplied provider credentials | [Client-side credentials](https://docs.litellm.ai/docs/proxy/clientside_auth) | Request-only credentials on operator-declared connections; isolated transports, redacted errors, source accounting and caller-paid route budgets | `Parity` | M4.6 |
| Audit logs | [Enterprise](https://docs.litellm.ai/docs/enterprise) | Metadata-only audit for every mutation | `Ahead` | |
| Invitations and onboarding | [Self-serve](https://docs.litellm.ai/docs/proxy/self_serve) | Invitations and mapped OIDC provisioning | `Parity` | |
| Email delivery | [Enterprise](https://docs.litellm.ai/docs/proxy/email) | Not available | `Gap` | M5.4 |

## Budgets and rate limits

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Key request, token and concurrency limits | [Rate limits](https://docs.litellm.ai/docs/proxy/users) | Requests and tokens per minute, concurrency; the OpenAI and Anthropic surfaces return the remaining allowance in [response headers](../gateway.md#rate-limit-headers) | `Parity` | |
| Key budgets with reset periods | [Budgets](https://docs.litellm.ai/docs/proxy/users) | Calendar day, ISO-Monday week and month windows; admission also reserves the request's estimated cost ([cost reservation](../gateway.md#cost-reservation)) | `Parity` | M4.2 |
| Budget reset time zone | [Reset and time zone](https://docs.litellm.ai/docs/proxy/budget_reset_and_tz) | IANA installation calendar, next-boundary changes, DST and subhour retention | `Parity` | M4.2 |
| User and team budgets | [Team budgets](https://docs.litellm.ai/docs/proxy/team_budgets) | Shared groups, organization/project/installation caps and project templates, with independent reservations, calendar windows, system accounting and durable refusal levels | `Parity` | M4.2 |
| Per-model limits on a key | [Model-specific budgets](https://docs.litellm.ai/docs/proxy/users) | Per-key route RPM, TPM, concurrency and day/week/month caps with shared fleet counters and historical accounting | `Parity` | M4.2 |
| Tag budgets | [Enterprise](https://docs.litellm.ai/docs/proxy/tag_budgets) | Project label/value day/week/month caps, shared accounting, required/pinned labels and promotion | `Parity` | M4.2 |
| Budget and limit tiers | [Enterprise](https://docs.litellm.ai/docs/proxy/rate_limit_tiers) | Not available | `Gap` | M4.2 |
| Temporary budget increases | [Enterprise](https://docs.litellm.ai/docs/proxy/temporary_budget_increase) | Audited additive increases with window-bounded expiry, replay, revocation, fleet enforcement and console controls | `Parity` | M4.2 |
| Soft budget alerts | [Alerting](https://docs.litellm.ai/docs/proxy/alerting) | `budget.threshold` notification rules | `Parity` | |
| Distributed enforcement | Redis counters | Valkey scripts on the server clock, PostgreSQL spend authority, reconciliation, fail-closed cost budgets, estimate-based cost reservation | `Ahead` | |
| Deployment rate limits | Deployment `rpm` and `tpm` | Connection and slot request, token and concurrency quotas | `Parity` | |

## Cost and pricing

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Per-request cost | [Cost tracking](https://docs.litellm.ai/docs/proxy/cost_tracking) | Attempt-level cost from pinned pricing revisions; unpriced work stays visible; a unary response states its cost in `X-OLP-Cost` for a key that [opts in](../access.md#key-response-metadata) | `Parity` | |
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
| Vendor guardrails | [About 50 providers](https://docs.litellm.ai/docs/guardrail_providers) | Not available | `Gap` | M7.4 |
| Custom guardrails | [Custom guardrail](https://docs.litellm.ai/docs/proxy/guardrails/custom_guardrail) (in-process Python) | Not available | `Gap` | M7.3, M7.5 |
| Model-based guardrails | [LLM as a judge](https://docs.litellm.ai/docs/proxy/guardrails/llm_as_a_judge) | Not available | `Gap` | M7.4 |
| Key- and team-scoped guardrails | [Enterprise](https://docs.litellm.ai/docs/proxy/guardrails/quick_start), [policies](https://docs.litellm.ai/docs/proxy/guardrails/guardrail_policies) | Route-level content policy | `Partial` | M7.1 |
| Streaming output guardrails | Supported by some guardrails | Output rules require unary responses | `Gap` | M7.1 |
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
| Request log viewer | [UI logs](https://docs.litellm.ai/docs/proxy/ui_logs) | Request history with attempts, usage and policy decisions | `Parity` | |
| Session grouping | [Sessions](https://docs.litellm.ai/docs/proxy/ui_logs_sessions) | Attribution labels | `Partial` | M5.5 |
| Alert channels | [Slack, Discord, Teams](https://docs.litellm.ai/docs/proxy/alerting), [PagerDuty](https://docs.litellm.ai/docs/proxy/pagerduty) | Signed webhooks | `Partial` | M5.4 |
| Alert events | Hanging, slow and failed calls, outages, budgets, reports | `budget.threshold`, `provider.grant.lapsed`, `key.expiring` | `Partial` | M5.4 |
| Health endpoints | [Health checks](https://docs.litellm.ai/docs/proxy/health) | Private liveness, readiness and metrics; provider health API | `Parity` | |
| Log retention | [Retention](https://docs.litellm.ai/docs/proxy/spend_logs_deletion) | Retention settings enforced by the maintenance worker | `Parity` | |
| Audit and security event export | [Azure Sentinel](https://docs.litellm.ai/docs/observability/azure_sentinel), [Splunk](https://docs.litellm.ai/docs/observability/splunk_observability_cloud) | Audit API only | `Partial` | M5.1 |

## Agents and tools

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| MCP gateway | [MCP](https://docs.litellm.ai/docs/mcp) | Not available | `Gap` | M10.1 |
| MCP OAuth and per-user credentials | [MCP OAuth](https://docs.litellm.ai/docs/mcp_oauth), [per-user auth](https://docs.litellm.ai/docs/mcp_per_user_auth) | Grant enrollment and refresh exist for provider plugins | `Gap` | M10.1 |
| MCP tools for every model | [MCP](https://docs.litellm.ai/docs/mcp) | Not available | `Gap` | M10.2 |
| MCP cost tracking | [MCP cost](https://docs.litellm.ai/docs/mcp_cost) | Not available | `Gap` | M10.1 |
| A2A agent gateway | [A2A](https://docs.litellm.ai/docs/a2a), with [iteration budgets](https://docs.litellm.ai/docs/a2a_iteration_budgets) and a [kill switch](https://docs.litellm.ai/docs/a2a_kill_switch) | Not available | `Gap` | M10.3 |
| Prompt management | [Prompt management](https://docs.litellm.ai/docs/proxy/prompt_management) | Not available | `Gap` | M10.4 |

## Administration and deployment

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Admin console | [Admin UI](https://docs.litellm.ai/docs/proxy/ui) | Console for every management operation a member may perform | `Parity` | |
| Playground | Test key | Playground with streaming, native operations and realtime traces | `Parity` | |
| Configuration as code | [config.yaml](https://docs.litellm.ai/docs/proxy/configs) and database models | Digest-addressed export, plan and apply | `Ahead` | |
| Live configuration changes | [Database models](https://docs.litellm.ai/docs/proxy/model_management) | Atomic, digest-verified runtime generations pinned per request | `Ahead` | |
| Management CLI | [lite CLI](https://docs.litellm.ai/docs/proxy/management_cli) | Operator commands only | `Gap` | M11.1 |
| Secret managers | [Enterprise](https://docs.litellm.ai/docs/secret_managers/overview) | Mounted key files; secrets sealed in PostgreSQL | `Gap` | M11.3 |
| Kubernetes packaging | [Helm](https://docs.litellm.ai/docs/proxy/deploy) | Helm chart and Compose | `Parity` | |
| Separate admin and worker roles | [Enterprise](https://docs.litellm.ai/docs/enterprise) | `gateway`, `control` and `worker` process modes | `Ahead` | |
| Database read replicas | [Read replica](https://docs.litellm.ai/docs/proxy/db_read_replica) | Not available | `Gap` | M11.5 |
| Multi-region deployment | [Enterprise](https://docs.litellm.ai/docs/proxy/multi_region) | Single region | `Gap` | M11.5 |
| Central control of several installations | [Enterprise](https://docs.litellm.ai/docs/proxy/global_control_plane) | Not available | `Gap` | M11.5 |
| Model hub | [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) | Not available | `Gap` | M11.4 |
| Administration through agents | [LiteAdmin MCP](https://docs.litellm.ai/docs/proxy/liteadmin_mcp) | Not available | `Gap` | M11.6 |
| Custom branding | [Logo](https://docs.litellm.ai/docs/proxy/ui/ui_edit_logo) | Not available | `Gap` | M11.7 |

## Performance

| Capability | LiteLLM | OLP today | Status | Milestone |
| --- | --- | --- | --- | --- |
| Published overhead | [Benchmarks](https://docs.litellm.ai/docs/benchmarks): 8 ms P95 at 1k RPS; 3,000 RPS large-prompt profile | The benchmark harness, LiteLLM comparison and CI regression gate are built; the gate passes on hosted runners and the comparison has run S1 to S5 at reduced rates; no full-rate results are published ([performance](../performance.md#status-of-the-numbers)) | `Partial` | M1.1 |
| Compiled data plane | [Rust gateway](https://docs.litellm.ai/docs/proxy/rust_gateway) (beta, per model) | Go data plane on every path, with hot-path benchmarks gated in CI; overhead measured at reduced rates only | `Partial` | M1.1 |
| Accurate token counting | Rust token counting in the [high-throughput profile](https://docs.litellm.ai/docs/proxy/high_throughput) | Exact in-repository counts for the OpenAI `o200k_base` and `cl100k_base` encodings, bounded for long prompts; Anthropic, Gemini and other families use the four-characters-per-token heuristic, scaled by a per-family factor from the signed catalog once one is measured; none is yet ([estimates](../gateway.md#how-the-prompt-is-estimated)) | `Partial` | M2.4 |
