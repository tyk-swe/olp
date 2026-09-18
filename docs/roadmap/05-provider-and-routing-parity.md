# M5: Remaining protocols, providers, and routing

[Roadmap](README.md) | [Previous: limits and accounting](04-limits-and-accounting.md) |
[Next: media and console parity](06-media-and-console-parity.md)

**Status:** Implemented and qualified on Linux amd64 (2026-09-16).
**Prerequisites:** M4 complete. Inherits the open M1 native arm64 qualification gate.

[Qualification evidence](evidence/provider-and-routing-parity.md) covers all ten
tickets, service and SDK contracts, browser journeys, dependency review, and
five-run build measurements.

Complete the existing non-media compatibility matrix and advanced routing
workflows. Reuse M3 execution and M4 accounting for each connector so retries,
limits, privacy, and pricing retain one behavioral implementation.

## Backlog

### M5-01

- [x] **Complete shared operations and cross-protocol codecs.**

**Depends on:** Milestone prerequisites.

**Deliver:** Extend the operation model and codecs for Anthropic Messages and
Gemini generation, response streams, tools, token counting, and model reads.
Implement the existing native/translated support and refusal rules across
OpenAI Chat, Responses, Anthropic, and Gemini.

**Accept:** Native source extensions survive supported round trips. Unsupported
request semantics fail explicitly; documented response-extension drops remain
bounded and tested. Preserve tool-call completion behavior, usage completeness,
stream sequencing, and existing provider-owned resource restrictions.

**References:** [Canonical operations](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/protocols/canonical),
[Anthropic codecs](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/protocols/anthropic),
[Gemini codecs](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/protocols/gemini),
[translation limits](../compatibility.md).

### M5-02

- [x] **Implement native Anthropic and Gemini connectors and surfaces.**

**Depends on:** [M5-01](#m5-01).

**Deliver:** Restore native SDK authentication, unary/streaming dispatch,
discovery/certification, token counting, errors, and key-filtered model
listing/retrieval. Serve Anthropic under `/anthropic/v1` and Gemini under both
`/gemini/v1` and `/gemini/v1beta`.

**Accept:** Official SDK success and typed-error scenarios pass through the Go
gateway. Connectors use the existing attempt lifecycle, bounds, and accounting.
Cross-protocol calls preserve the support matrix; adding a native surface does
not make previously unsupported combinations eligible.

**References:** [Anthropic connector](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/anthropic),
[Gemini connector](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/gemini),
[native surface tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/system/anthropic_gemini_inference/native_surfaces.rs),
[SDK suite](../../tests/sdk-smoke/smoke.mjs).

### M5-03

- [x] **Restore Azure OpenAI and Vertex AI.**

**Depends on:** [M5-01](#m5-01).

**Deliver:** Implement Azure endpoint/deployment/API-version handling and the
existing Vertex project, location, endpoint, and credential modes. Reuse native
codecs with the necessary cloud authentication and request construction.
Document each required cloud-auth module in the dependency inventory.

**Accept:** Existing cloud identity fixtures, refresh behavior, model rewriting,
generation/streaming, token counting, and typed errors pass. Authentication
requests are bounded and secrets are redacted. Cloud client retries cannot
multiply the executor's attempt budget or bypass its deadline.

**References:** [Azure connector](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/azure_openai.rs),
[Vertex connector](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/vertex.rs),
[Vertex auth](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/vertex/oauth.rs),
[connector conformance](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/conformance/provider_connectors).

### M5-04

- [x] **Restore Bedrock with an explicit dependency budget.**

**Depends on:** [M5-01](#m5-01).

**Deliver:** Implement Bedrock discovery, Converse generation/streaming, and
supported token counting. Preserve configured and ambient credential behavior,
including the existing process/SSO paths, signing, regions, and model IDs/ARNs.
Use maintained cloud authentication, signing, and event-stream components;
record the footprint of every imported SDK module.

**Accept:** Existing Bedrock fixtures pass across exposed client surfaces.
Retries/deadlines remain owned by the executor. Record each documented
translation/usage/request-ID/media/response-bound exception and its Go evidence;
any stronger new bound must be tested, and no exception silently disappears.
Re-measure backend builds after adding the cloud dependency graph.

**References:** [Bedrock guide](../providers/bedrock.md),
[Bedrock implementation](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/bedrock),
[connector exceptions](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/conformance/provider_connectors/matrix.rs),
[current dependencies](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/Cargo.toml).

### M5-05

- [x] **Restore every existing compatible-vendor profile.**

**Depends on:** [M5-01](#m5-01).

**Deliver:** Implement DeepSeek, Fireworks, DeepInfra, Hugging Face, Perplexity,
Cohere, and Voyage profiles over the shared HTTP connectors. Preserve vendor
identity, discovery/probe differences, declared versus certified capabilities,
supported Responses-to-Chat translation, and embedding parameter normalization.

**Accept:** Each profile passes its current contract, including unsupported
parameters, Cohere embedding restrictions, and Voyage dimensions/truncation/
encoding behavior. A generic compatible endpoint does not inherit official
OpenAI media support or cross-protocol certification.

**References:** [Profiles](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/profiles.rs),
[profile tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/profiles_tests.rs),
[catalog](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/catalog.rs),
[provider routing](../provider-routing.md).

### M5-06

- [x] **Complete token counting, embeddings, and moderation.**

**Depends on:** [M5-02](#m5-02), [M5-03](#m5-03), [M5-04](#m5-04), [M5-05](#m5-05).

**Deliver:** Wire every supported non-generation, non-media operation into
endpoint authorization, certification, provider selection, bounds, native
errors, pricing, and usage completeness. Include OpenAI Responses input-token
counting and native Anthropic/Gemini counting.

**Accept:** Every supported tuple in the maintained compatibility matrix has
deterministic evidence. Unsupported modes/providers are refused before dispatch.
Model reads remain gateway-owned and key-filtered. New operation registration
cannot accidentally grant inference or model-read permissions.

**References:** [Endpoint registry](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/inference/http/endpoint_policy/registry.rs),
[selected operations](../../tests/fixtures/protocols/selected-operation-families.json),
[operation conformance](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/conformance/selected_operations.rs),
[compatibility](../compatibility.md).

### M5-07

- [x] **Restore custom connections and mounted configuration.**

**Depends on:** [M5-02](#m5-02), [M5-03](#m5-03), [M5-05](#m5-05).

**Deliver:** Restore custom native endpoints, explicit credentialless and
encrypted-header authentication, provider parameter defaults, deployment
overrides, and file-backed connector configuration. Preserve native/cloud
configuration boundaries and operator egress controls.

**Accept:** Reserved transport/routing headers and envelope fields cannot be
overridden. Caller parameters retain their documented precedence. Credentials
and custom headers are write-only; private/HTTP endpoints require explicit
exceptions, with DNS pinning and redirect refusal retained. Mounted and
database-managed configuration obey their existing ownership rules.

**References:** [HTTP options](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/http_options.rs),
[configuration overrides](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/connectors/overrides.rs),
[mounted connectors](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/mounted.rs),
[flexibility tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/provider_flexibility_postgres.rs).

### M5-08

- [x] **Restore model facts, bulk workflows, and routing policies.**

**Depends on:** [M5-06](#m5-06), [M5-07](#m5-07).

**Deliver:** Restore operator model facts/canonical identity, privacy evidence,
capability metadata, bulk model validation, and grouped route drafts. Implement
installation, route-draft/revision, key, and request policy scopes, including
the optional `X-OLP-Routing` header on every native inference surface.

**Accept:** Discovery refresh preserves operator facts. Bulk operations report
per-item outcomes, bounded cancellation, and retryable failures. Hard constraints
intersect; unknown facts cannot satisfy a requirement. Request preferences
cannot expand access, targets, deadlines, attempts, or allowed strategies, and
the raw routing header is never forwarded upstream or persisted.

**References:** [Routing policy](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/routes/policy.rs),
[key policy](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/api_keys/http/policy.rs),
[provider options](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/options.rs),
[routing contract](../provider-routing.md).

### M5-09

- [x] **Restore advanced selection, cooldowns, and simulation.**

**Depends on:** [M5-08](#m5-08).

**Deliver:** Implement weighted, price, latency, and throughput strategies within
operator priority/order tiers; freshness/sample requirements; fallback limits;
and shared selection for route dry runs and playground requests. Finish slot
quota/cooldown and endpoint circuit behavior across all connector failure classes.

**Accept:** M4 price revisions and successful-attempt measurements drive both
preview and execution. Unknown/stale facts follow the documented ordering.
401 affects the secret version, 429 cools down the logical slot across rotation,
and credential-only outcomes do not penalize sibling endpoint probes.
Each real credential attempt consumes the attempt budget.

**References:** [Provider selection](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/inference/provider_selection.rs),
[performance](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/inference/performance.rs),
[pool transport](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers/pool_transport.rs),
[simulation tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/route_draft_simulation_postgres.rs).

### M5-10

- [x] **Complete provider/routing console workflows and conformance.**

**Depends on:** [M5-06](#m5-06), [M5-07](#m5-07), [M5-08](#m5-08), [M5-09](#m5-09).

**Deliver:** Adapt vendor selection, cloud/custom connection editors, model
comparison, bulk validation/route creation, policy editors, routing preferences,
and explanation views. Port the remaining non-media connector/protocol corpus
and run all three official JavaScript SDK clients against Go.

**Accept:** Every current non-media support/refusal cell has evidence, including
native extensions, translation loss, streaming, body/time limits, usage, and
failure/cancellation. Console decisions agree with the shared selection engine.
The retired OpenAI prefix and LiteLLM header remain unavailable. Record SDK
versions and distinguish fixture qualification from paid live-provider results.

**References:** [Provider console](../../console/src/lib/features/providers/),
[route console](../../console/src/lib/features/routes/),
[routing journeys](../../console/tests/journeys/provider-routing.ts),
[conformance suites](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/conformance).

## Exit scenarios

- Exercise every retained non-media surface/provider/operation/mode combination and refusal.
- Run official OpenAI, Anthropic, and Gemini SDK success, streaming, and typed-error checks.
- Verify custom/cloud authentication, refresh, credential rotation, cooldowns, and egress.
- Compare simulated and actual routing under policy intersections and missing/stale facts.
- Run bulk validation, model comparison, route creation, and policy browser journeys.
- Re-run build measurements and review cloud/native dependencies before starting M6.
