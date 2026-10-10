# Changelog

All notable changes to OpenLLMProxy are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning and take their source from root `package.json`;
`console/package.json`, `deploy/helm/Chart.yaml` and the release image agree.

## [Unreleased]

### Security

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

### Added

- Project-scoped code-mode accounts, explicit pools, published native-model
  routes, conversation-tree pins, token budgets and metadata-only diagnostics,
  with management APIs and console controls.
- Raw HTTP/SSE and WebSocket Codex forwarding, upstream-first handshakes,
  durable response references, live continuation authority checks and retained
  grant refresh across provider slot changes.
- Codex device enrollment and OLP-key-only client configuration. Official CLI
  0.160.0 is qualified against controlled peers; live subscription compatibility,
  positive hard-token bounds and the remaining client scenarios are tracked in
  [the qualification matrix](docs/qualification/code-mode.md).
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
  models. See [the reference catalog](docs/catalog.md).
- Plugin authoring templates for OAuth 2.0 client credentials, signed requests
  and token exchange, and a signed index of reviewed plugins the console
  browses.
