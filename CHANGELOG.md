# Changelog

All notable changes to OpenLLMProxy are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning and take their source from root `package.json`;
`console/package.json`, `deploy/helm/Chart.yaml` and the release image agree.

## [Unreleased]

### Added

- MiniMax M Plan, Kimi Code and Qwen Token Plan code-mode adapters. The
  `minimax-plan`, `kimi-code` and `qwen-token-plan` plugins enroll a pasted
  plan key, fingerprinted as the principal and fenced from ordinary routes.
  MiniMax and Alibaba enrollment refuses keys without the plan prefixes their
  vendors document, because pay-as-you-go keys share the host. Routes serve
  Anthropic Messages and Chat Completions on each vendor's global and mainland
  China upstreams, and client configuration generates Claude Code and OpenCode
  setup, qualified against controlled peers.

- Add opt-in code-mode research tools through an enrolled Z.ai global account,
  independently of inference models. Freeze the account and services at route
  publication, proxy bounded MCP traffic with live authorization and isolated
  sessions, and generate Codex, Claude Code and OpenCode setup using an OLP key.
  Claude's companion files appear in optional `additional_files`. Migration
  0080 stores only transport and authorization metadata.

- Expose metadata-only batch settlement progress and in-flight submission
  intents in provider-resource listings; document M9.2 provider scope,
  configuration and operator recovery.

### Security

- Capture policies pin ordered local detector revisions and independently redact
  complete input, output and tool arguments before enqueue. JSON escapes and
  native stream identities are respected; failed, incomplete or out-of-bounds
  inspection drops capture under a 100 ms total redaction budget without changing
  inference. Add forward-only migration 0074, console ordering/pin refresh,
  focused fixtures and an enumerated LiteLLM v1.105.0 vendor inventory. Service,
  live-vendor and performance qualification remain open; vendor parity is Partial.

- OCR and search result inspection fails closed on unrecognized fields: only
  nulls, numbers, booleans and empty containers pass uninspected, and upstream
  search ids never reach the caller — transformed results mint `search_` ids.
- Decisions refuse token caps, and USD caps outside caller-paid exempt routes,
  with `unbounded_work_budget` before dispatch. Route simulation and key
  inspection report the same refusal, including hierarchy and named-route budgets.
  Legacy completions refuse token-ID prompts and `logprobs` under enforcing
  guardrails, keep stream segments open until a nonempty finish reason, and
  reject inconsistent `total_tokens` or cache-write counts instead of accounting
  them.

- Reject uninspectable streamed log-probability output and isolate tool-call
  completion by candidate. Enforce system budgets for playground model evaluators,
  preserve their pinned routes and end-user attribution, and persist their parent
  request links through a forward-only database migration.
- Validate guardrail credential ownership before publication, reject opaque
  transformed input, and enforce admission for standalone evaluations. Preserve
  streaming redaction across Unicode and word boundaries, independent candidates,
  completion snapshots and exact tool-argument numbers.
- Billing-import admin keys are sealed under the dedicated `billing_credential`
  purpose, readable only by the import worker and never used for inference.
  Stripe meter keys are sealed as `sink_credential` and validated to `sk_`/`rk_`.
- Preserve native credential destinations and trust settings across provider
  edits, imports and restores; validate Vertex hosting before acquiring or
  sending Google credentials. Bind enrolled grants to their plugin digest and
  profile while keeping grants used by live retained resources refreshable.
- Reject unenforceable consumption budgets for asynchronous batches and Gemini
  Live sessions, and keep consumption admission closed when accounting is
  incomplete. Preserve local token reservations during settlement and retain
  unknown usage-event versions for compatible consumers.
- Apply current slot restrictions and published route fidelity to retained and
  strict operations. Enforce Responses storage opt-in after request preparation,
  project owned file identifiers in historical batches and return content-free
  cancellation receipts when a retained response can no longer be returned.
- Persist only video-job lifecycle metadata, bound video-list provider polling,
  and reject ambiguous realtime accounting aliases. Bound source-index nodes,
  forced-stream output-item retention and actual cumulative simulation policy
  work, including nested tool arguments and empty strings.
- Bind OIDC discovery destinations and client authentication to reviewed saved
  configuration, require fresh pricing edits after ETag conflicts, redact plugin
  directory errors, scope restricted DNS egress to configured resolvers and
  remove mutable caches from release candidate builds.
- Migrations 0045 and 0046 remove saved native video documents and add immutable
  grant profile bindings. Existing grants without a provable profile require
  re-enrollment; existing OIDC configurations require an owner to review and save
  discovery again. Restricted Helm deployments must configure DNS resolver CIDRs.

### Changed

- A2A 1.0 requests require `A2A-Version: 1.0` or the equivalent query
  parameter. Requests without a version use the A2A 0.3 adapter.
- Notification webhooks refuse redirects instead of following up to five,
  because 301/302/303 turned signed POSTs into bodyless GETs that were recorded
  as delivered; refused redirects record `redirect`. Email notifications send
  indented evidence as quoted-printable with RFC 2047-encoded addresses.
- Email notifications record SMTP replies as `smtp_4xx`/`smtp_5xx` instead of
  `network`, and a failed QUIT after message acceptance no longer resends it.
- Active health probes send a text completion to connectors whose generation
  target is a native completion profile. The `price` routing strategy uses input
  and output token prices for Decisions. Chat usage records
  `prompt_tokens_details.cache_write_tokens` as cache-write input.
- Configuration promotion activates matching guardrail drafts, advances policy
  revisions past existing drafts and supports installation definition updates.
- Configuration exports use `openllmproxy.dev/config/v2` for guardrail
  definitions, policies and attachments; version 1 imports are normalized into
  the new format. Migration 0068 adds the guardrail platform and converts
  historical route policies without changing their source revisions.
- Condense and refresh operational/developer documentation; move repository
  guidance into contributor/console guides and remove `AGENTS.md`.
- The reference catalog is `openllmproxy.dev/catalog/v2`, carrying the new
  price components; older releases refuse it as an unsupported version.
  Gemini image models price text and thinking output at the text rate and
  image output at the image output rate.
- An attempt that used a metered server-side tool its price has no fee for is
  recorded unpriced; publish the catalog snapshot or add tool fees (a fee may be
  `0`) to price such attempts.
- Observability sink credentials are validated against the sink's type at
  create and update; capture sinks accept only the `json`, `jsonl` and `otlp`
  formats.
- The `focus_export` and `billing_import` worker tasks join the health,
  staleness and notification registries.
- Migrations 0063 to 0067 add the price components, commitments, discounts,
  rate cards, charges, budget cost bases, per-attempt price lines, the
  caller-credential marker, metering/FOCUS sink types and settings, the FOCUS
  delivery ledger and the billing-import tables.

### Added

- Legacy text completions (`POST /v1/completions`, M9.1): native unary and SSE
  on the `openai-completions`, `vllm-completions`, `tgi-completions`,
  `fireworks-completions` and `together-completions` profiles, plus a
  single-prompt mapping to Chat Completions on transformed routes. Includes
  guardrail inspection, payload capture, exact usage and console playground
  templates. See [compatibility](docs/use/compatibility.md#legacy-text-completions-and-decisions).
- The text-only Decisions API (`POST /v1/decisions` and `POST /v1/systemone`,
  M9.10) as operation `decision`, with native OpenAI Decisions and TypeSafe
  System One profiles, a restricted one-way OpenAI-to-System-One mapping and
  playground routing. Images, box questions, streaming and System-One-to-OpenAI
  translation are refused.
- The M9 endpoint ledger (`tests/clients/m9-endpoints.json`) and the pinned
  OpenAI Go SDK suite (`CLIENTS=m9-go tests/clients/run.sh`). Console/browser
  and live-provider qualification of both slices remain open.
- OCR (`POST /v1/ocr`, M9.7) on the Mistral `mistral-ocr` profile for
  `https:`/`data:` `document_url`/`image_url` documents, metering
  `usage_info.pages_processed` as media units and enforcing the
  unbounded-work-budget admission rules. File documents, annotations,
  confidence/block controls and other fields are refused before dispatch.
- Web search (`POST /v1/search` with `X-OLP-Route`, M9.7) on the Perplexity
  `perplexity-search` profile plus transformed mappings to Tavily, Exa and
  Parallel (`X-Api-Key` credential). Transformed results project the Perplexity
  shape with gateway-minted `search_` ids; token, locale and freshness controls
  are refused rather than approximated. Search attempts charge `per_request`.
  Console playground templates and a Chromium gateway journey cover both
  operations; per-provider live qualification remains open.
- Opt-in encrypted response caching in a dedicated Valkey, with key isolation,
  native stream replay, request controls, scoped purge, zero-provider-cost
  accounting, metrics and Route Studio controls. Semantic caching uses Valkey
  Search and separately metered embedding routes; transformed routes can also
  insert Anthropic prompt-cache breakpoints and OpenAI cache keys.

- A guardrail platform with immutable definitions and policies, scoped
  attachments, built-in detectors, external and model evaluators, confined WASM
  plugins, streaming enforcement and content-free decision evidence. The
  console includes editors and a playground; `/v1/guardrails/apply` has generated
  Go and TypeScript clients. See [guardrails](docs/use/guardrails.md).
- Price components for every structure the major vendors publish: audio and
  image rates with cached shares, input-token tiers, time-of-day windows,
  per-request and per-tool-call fees and a batch multiplier, priced exactly in
  accounting and admission. See [pricing](docs/operate/pricing.md).
- Audio, image and server-side tool-call metering from OpenAI, Anthropic and
  Gemini responses.
- Connection commitments for provisioned capacity, allocated across consumers
  in usage reports.
- Pricing discounts by connection, vendor or kind, and rate cards for the
  installation, organizations and projects. Usage records, reports, exports
  and request history carry charge beside cost, and each budget enforces cost
  or charge (`cost_basis`).
- Cost estimates in route simulation, the `max_cost` routing control, a
  pricing quote endpoint and a console cost calculator.
- The `metering` export stream: one record per priced attempt, to CloudEvents
  consumers on HTTPS sinks and Stripe Billing meter events on `stripe_meter`
  sinks with idempotent identifiers and per-sink project-to-customer maps.
- Daily FOCUS 1.4 gzip-CSV datasets on object sinks in provider-cost or charge
  basis, with revisioned restatement and in-repo conformance checking; see
  [finops](docs/operate/finops.md).
- Per-attempt price lines (`cost_lines`, `charge_lines`) recording each
  billed component's quantity, unit and unit prices.
- OpenAI and Anthropic billing imports and a usage reconciliation report
  (`/api/v1/usage/reconciliation`) comparing attributed and invoiced spend by
  UTC day with capture rate, unpriced share and provisional marking.
- Console surfaces: the observability sink forms and FOCUS datasets view, the
  provider billing-import section and the Usage reconciliation panel.
- Process metrics `olp_focus_dataset_deliveries_total`,
  `olp_focus_pending_datasets`, `olp_billing_imports_total` and
  `olp_billing_capture_rate`.
- Project-scoped code-mode accounts, explicit pools, published native-model
  routes, conversation-tree pins, token budgets and metadata-only diagnostics,
  with management APIs and console controls.
- Raw HTTP/SSE and WebSocket Codex forwarding, upstream-first handshakes,
  durable response references, live continuation authority checks and retained
  grant refresh across provider slot changes.
- Codex device enrollment and OLP-key-only client configuration. Official CLI
  0.160.0 is qualified against controlled peers; live subscription compatibility,
  positive hard-token bounds and the remaining client scenarios are tracked in
  [the qualification matrix](docs/develop/qualification.md).
- OpenCode Go and Z.ai GLM Coding Plan code-mode adapters. The `opencode-go`
  and `zai-coding` plugins enroll a pasted API key, fingerprinted as the
  principal and fenced from ordinary routes. Routes serve Anthropic Messages,
  Chat Completions and, for OpenCode Go, the Responses API, and derive their
  adapters from their accounts. Client configuration generates Claude Code 2.1.286
  and OpenCode 1.18.34 setup, which is qualified against controlled peers.
- Code-mode routes that mix subscriptions. One pool can hold Codex, OpenCode Go
  and GLM Coding Plan accounts; each request reaches an account that lists its
  model and whose subscription serves the client's path, and a conversation tree
  pins one account per model and holds at most one account of each
  subscription. Client configuration offers each client the models
  it can reach and a planning model, which Claude Code uses through `opusplan`
  and OpenCode through its plan agent, so one session can plan on one
  subscription's model and implement on another's.
- Plugin grant profiles can declare a `secret` input, which the console masks.
- Reviewed vendor contracts: one declared table of every vendor's operations,
  dialects, refused parameters, credential placement and error classes, each
  backed by fixtures transcribed from the vendor's documentation. Fifty
  OpenAI-compatible presets, up from 13, including xAI, Cerebras, Moonshot, DashScope,
  Z.ai, MiniMax, Databricks, Cloudflare Workers AI, Ollama, LM Studio and
  llama.cpp; a preset whose vendor speaks a dialect exactly carries its profile
  and discovery setting.
- Azure AI Foundry, Vertex OpenAI-compatible and Cohere v2 profiles; the
  `sagemaker` and `watsonx` connector kinds, the latter with IBM Cloud IAM
  authentication; Bedrock rerank and Stability images; and native
  `mistral-fim` and `cohere-chat-v2` generation at
  `/native/{dialect}/models/{route}` on strict routes.
- Vendor media codecs on transformed routes: Azure OpenAI, Gemini, Stability
  AI, Recraft, Black Forest Labs and xAI images; Azure OpenAI, Gemini,
  ElevenLabs, Deepgram and Amazon Polly speech; Azure OpenAI, Gemini, Groq,
  ElevenLabs, Deepgram and AssemblyAI transcription; Runway video as durable
  jobs; and Jina, Together and Infinity rerank. Each is certified by a
  costless authenticated request or, where a vendor offers none, its smallest
  real call.
- A signed reference catalog of model facts, list prices, lifecycle dates and
  token-estimation factors, verified at startup and published with each
  release. Catalog pricing sources refresh from it with anti-rollback;
  discovery suggests its facts, and routes warn about deprecated and retiring
  models. See [the reference catalog](docs/develop/catalog.md).
- Plugin authoring templates for OAuth 2.0 client credentials, signed requests
  and token exchange, and a signed index of reviewed plugins the console
  browses.
