# Guardrail completion plan

Status: **Implementation complete; release qualification pending**. Named text
adapters, local filters and accounted model evaluation are implemented. Live
service, deployment and hardware acceptance remain open below.
[Guardrails](../../use/guardrails.md) owns the current engine contract;
[Observability](../../operate/observability.md#payload-capture) owns capture.
This plan owns [roadmap RM2](../../roadmap.md#rm2-guardrails-capture-and-caching),
including the related cache qualification gate. It covers remaining outcomes
and support decisions; unchecked items below are not new evidence.

## Dependencies and boundaries

Preserve immutable revision pins, additive attachment order, strict-route
fidelity, byte-bounded streaming holdback and content-free evidence. Cache use
depends on those implemented contracts, not completion of external qualification.
The planned agent gateway must enforce tool governance at its own call boundary.
Changes use the existing credential, egress, verdict and failure contracts;
custom guardrails run as confined WASM or signed webhooks.

## Remaining work and acceptance

**Entry:** implemented engine/capture contracts and a recorded candidate.
RM2.1–RM2.2 need disposable services; RM2.3 needs authorized vendor accounts.
RM2.4 follows the local correctness checks and needs frozen reference hardware.
RM2.5 and RM2.6 can proceed independently; cache qualification depends on the
implemented revision/execution contracts, not completion of RM2.3 or RM2.5.

- [ ] **RM2.1 — Execute capture acceptance** (export/gateway): run the actual
  `TestCapture` service fixtures, the prescribed browser checks and manual
  Settings capture workflow. Verify
  publication isolation, migration, immutable pins, unary/split-stream/tool
  sink readback, unchanged caller output, stale-ETag suppression and no durable
  payload staging. Retain results from the [qualification commands](../qualification.md#guardrail-and-capture-qualification);
  compile-only runs and missing-service skips cannot close this item.
- [ ] **RM2.2 — Close engine regression gates** (gateway/guardrail): run the
  surface/action matrix, cancellation, evaluator budgets/accounting, logprobs
  and retained-response exclusions through protocol, authorization/isolation,
  integration and race checks. Retain accessible console evidence and resolve
  failures before advancing the candidate.
- [ ] **RM2.3 — Qualify every supported live adapter** (connector/plugin): record each
  adapter's authentication, API version, phases/actions, valid replacement
  spans, both failure policies and actual deadline/circuit behavior. Execute
  positive, negative and malformed/timeout cases using the qualification guide;
  retain separate WASM conformance evidence. Local fakes cannot close live gates.
- [ ] **RM2.4 — Measure configured workloads** (performance): after RM2.1–RM2.2,
  qualify 100K-token built-ins, streaming and capture at full S3 load. Configure
  these workloads explicitly; stock S3 alone does not exercise them. Record
  latency, throughput, allocations and memory against unconfigured controls
  on frozen reference hardware.
- [ ] **RM2.5 — Review the 60-entry inventory** (guardrail/connector): review
  every row below, prioritize named adapter work and record each support or
  exclusion decision with rationale, owner and evidence. Give newly accepted
  adapter work its own contract/fixture/live tasks; reviewing a row does not
  implement the adapter or remove the coverage gap.
- [ ] **RM2.6 — Qualify response and prompt caching** (cache/performance): use
  a dedicated cache instance and paired hit/bypass S1 runs to establish
  incremental p95 ≤1 ms. Verify guarded replay, isolation, purge, eviction,
  outage and rotation under [cache qualification](../qualification.md#performance-and-routing).
  Record provider prompt-cache control and savings evidence separately;
  fixture token savings are not live savings.

**Exit:** all items and the criteria below have candidate-specific evidence.
Use the [roadmap evidence rules](../../roadmap.md#updating-progress-and-evidence)
to record blockers and update status without conflating local and live checks.

The six existing external adapters are Bedrock, Azure Content Safety, Azure
Prompt Shields, Model Armor, Lakera v2 and Presidio. A generic webhook or WASM
option does not qualify any unnamed vendor. An exclusion must name the
integration and rationale; absence of credentials or hardware is not an exclusion.

## Exit criteria

- [ ] Capture service and sink-readback evidence meets the complete
  [qualification conditions](../qualification.md#guardrail-and-capture-qualification).
- [ ] Surface/action, privacy, authorization, cancellation and accounting gates
  pass on the candidate, including independent capture inspection and no raw fallback.
- [ ] Built-in/capture/streaming workloads meet the
  [performance budget](../performance.md#performance-budget) on reference hardware.
- [ ] Every supported native adapter has live-service evidence, distinct from fakes.
- [ ] Release review records the pinned 60-entry inventory, each support or
  exclusion decision and an owned backlog. Implementation mappings and scope decisions do not imply live qualification
  or overall upstream configuration parity.
- [ ] The related RM2.6 cache gate has paired reference measurements and
  isolation/replay/failure evidence; any live prompt-cache claim has its own proof.

## Vendor inventory

Aggregate vendor coverage: **Implemented within the recorded scope; live
qualification pending**. This inventory freezes the existing LiteLLM v1.105.0 comparison baseline at commit
`80b9556dd312d18c535ae06884faa97d1fdb8f46`, enumerating all 60 values in
[`SupportedGuardrailIntegrations`](https://github.com/BerriAI/litellm/blob/80b9556dd312d18c535ae06884faa97d1fdb8f46/litellm/types/guardrails.py) in source order.
The registry includes services, local filters and execution hooks; 60 registry
entries does not mean 60 distinct vendors. It is not a moving latest-release
claim. Source file SHA-256: `6614ced8eb1db5a8845c862c4ad0b992139c3a14f5e1e955c4beacf445dfbb15`.

The OLP mapping and action columns describe inspected local code, not a claim of
LiteLLM configuration compatibility. No live-service qualification was performed
in the source inventory review. Every registry entry has an implementation mapping or an explicit scope decision
below. Five execution hooks depend on RM7/RM8 rather than the RM2 text evaluation
contract. A generic webhook or confined WASM extension does not qualify a named
HTTP vendor; native Python execution remains outside the confined plugin contract.

Evidence keys:

- **V**: [`TestVendorContracts`](../../../internal/guardrails/external_test.go), six local
  fake request/auth/verdict contracts; transformations still require valid spans.
- **B**: [`engine_test.go`](../../../internal/guardrails/engine_test.go) and
  [`registry_test.go`](../../../internal/guardrails/registry_test.go), bounded built-in
  detector and engine fixtures. These are semantic analogues, not vendor adapters.
- **M**: [`guardrails_evaluator_regression_test.go`](../../../internal/gateway/guardrails_evaluator_regression_test.go)
  and [`guardrail_platform_test.go`](../../../tests/integration/guardrail_platform_test.go),
  operator-configured model evaluator routing/accounting; no dedicated vendor proof.
- **W**: [`external.go`](../../../internal/guardrails/external.go), inspected shared
  webhook implementation; [`guardrails_test.go`](../../../internal/gateway/guardrails_test.go)
  has a parallel evaluation fixture. No named third-party API compatibility proof.
- **N**: [`vendors_a_test.go`](../../../internal/guardrails/vendors_a_test.go),
  [`vendors_b_test.go`](../../../internal/guardrails/vendors_b_test.go),
  [`vendors_c_test.go`](../../../internal/guardrails/vendors_c_test.go), and
  [`vendors_test.go`](../../../internal/guardrails/vendors_test.go): native HTTP
  authentication, phase, request, verdict, replacement and malformed-response
  fixtures. These are local contracts, not live qualification.
- **L**: [`tool_permission_test.go`](../../../internal/guardrails/tool_permission_test.go)
  and [`code_execution_test.go`](../../../internal/guardrails/code_execution_test.go):
  local ordered permission rules and executable-code detection.
- **S**: [`semantic_test.go`](../../../internal/guardrails/semantic_test.go):
  bounded embedding batches and validated cosine comparison; gateway regression
  checks cover caller authorization, accounting and recursion.
- **Scope**: explicit architectural disposition below, not vendor qualification.

Actions below are engine capabilities on admitted surfaces. `mask` is synchronous
input only; enforcing transformations require transformed serving routes. Monitor
can observe supported actions. Capture permits only the four local types in the
[capture contract](../../operate/observability.md#payload-capture), regardless of serving capabilities.
Bedrock transformations require a returned replacement; Presidio uses analyzed
spans with OLP's local replacement. Neither claims every upstream anonymizer option.

| Registry integration | OLP mapping / status | Supported OLP actions | Local evidence | Live qualification | Follow-up owner |
| --- | --- | --- | --- | --- | --- |
| `aporia` | `aporia` native text contract | block, annotate | N | Not run | Connector maintainers: aporia |
| `bedrock` | `bedrock` (ApplyGuardrail) | block, redact, mask, annotate | V | Not run | Connector maintainers: Bedrock |
| `dynamoai` | `dynamoai` native text contract | block, annotate | N | Not run | Connector maintainers: dynamoai |
| `guardrails_ai` | `guardrails_ai` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: guardrails_ai |
| `lakera` | `lakera_v1` native text contract; input phases only | block, annotate | N | Not run | Connector maintainers: lakera |
| `lakera_v2` | `lakera` (`/v2/guard`) | block, annotate | V | Not run | Connector maintainers: Lakera |
| `presidio` | `presidio` (analysis spans, local replacement) | block, redact, mask, annotate | V | Not run | Connector maintainers: Presidio |
| `hide-secrets` | Local analogue: `builtin.secrets` | block, redact, mask, annotate | B | Not applicable; local comparison pending | Guardrail maintainers: corpus comparison |
| `hiddenlayer` | `hiddenlayer` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: hiddenlayer |
| `aim` | `aim` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: aim |
| `cato_networks` | `cato_networks` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: cato_networks |
| `pangea` | `pangea` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: pangea |
| `crowdstrike_aidr` | `crowdstrike_aidr` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: crowdstrike_aidr |
| `lasso` | `lasso` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: lasso |
| `pillar` | `pillar` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: pillar |
| `grayswan` | `grayswan` native text contract | block, annotate | N | Not run | Connector maintainers: grayswan |
| `panw_prisma_airs` | `panw_prisma_airs` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: panw_prisma_airs |
| `cisco_ai_defense` | `cisco_ai_defense` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: cisco_ai_defense |
| `azure/prompt_shield` | `azure.prompt_shields` | block, annotate | V | Not run | Connector maintainers: Azure |
| `azure/text_moderations` | `azure.content_safety` | block, annotate | V | Not run | Connector maintainers: Azure |
| `model_armor` | `model_armor` (verdict only) | block, annotate | V | Not run | Connector maintainers: Google |
| `openai_moderation` | `openai_moderation` native text contract | block, annotate | N | Not run | Connector maintainers: openai_moderation |
| `noma` | `noma` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: noma |
| `noma_v2` | `noma_v2` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: noma_v2 |
| `tool_permission` | `builtin.tool_permission`: ordered RE2 rules on captured function tools | block, annotate | L | Not applicable | Guardrail maintainers |
| `zscaler_ai_guard` | `zscaler_ai_guard` native text contract | block, annotate | N | Not run | Connector maintainers: zscaler_ai_guard |
| `javelin` | `javelin` native text contract; input phases only | block, annotate | N | Not run | Connector maintainers: javelin |
| `enkryptai` | `enkryptai` native text contract | block, annotate | N | Not run | Connector maintainers: enkryptai |
| `ibm_guardrails` | `ibm_guardrails` native text contract | block, annotate | N | Not run | Connector maintainers: ibm_guardrails |
| `litellm_content_filter` | Local analogue: `builtin.keywords` / `builtin.regex` | block, redact, mask, annotate | B | Not applicable; local comparison pending | Guardrail maintainers: rule comparison |
| `mcp_security` | Shared MCP execution checks the live registered server, pinned publication and caller authority | Gateway fixture, JSON and MCP | Identity | Bounded attempt decision | MCP / access maintainers |
| `onyx` | `onyx` native text contract | block, annotate | N | Not run | Connector maintainers: onyx |
| `promptguard` | `promptguard` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: promptguard |
| `xecguard` | `xecguard` native text contract | block, annotate | N | Not run | Connector maintainers: xecguard |
| `prompt_security` | `prompt_security` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: prompt_security |
| `generic_guardrail_api` | `generic_guardrail_api` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: generic_guardrail_api |
| `qualifire` | `qualifire` native text contract | block, annotate | N | Not run | Connector maintainers: qualifire |
| `custom_code` | `plugin`: approved confined WASM; Python source requires a port | block, redact, mask, annotate | Existing plugin implementation | WASM conformance remains separate | Plugin / guardrail maintainers |
| `ovalix` | `ovalix` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: ovalix |
| `microsoft_purview` | `microsoft_purview` native text contract | block, annotate | N | Not run | Connector maintainers: microsoft_purview |
| `semantic_guard` | `semantic_guard`: accounted embedding similarity against configured utterances | block, annotate | S | Not run | Guardrail / accounting maintainers |
| `mcp_end_user_permission` | Shared personal-grant resolver checks the exact live caller and server binding | Gateway fixture, missing and revoked grants | Identity | Bounded attempt decision | MCP / access maintainers |
| `block_code_execution` | `builtin.code_execution`: fenced languages and execution intent | block, redact, mask, annotate | L | Not applicable | Guardrail maintainers |
| `akto` | `akto` native text contract | block, annotate | N | Not run | Connector maintainers: akto |
| `mcp_jwt_signer` | Dedicated sealed Ed25519 key signs short-lived server, project and individual claims for the configured audience | Gateway fixture, signature and cross-audience rejection | Identity | Bounded attempt decision | MCP / access maintainers |
| `llm_as_a_judge` | `model` via operator-configured evaluator route | block, annotate | M | Not run | Guardrail maintainers: judge mapping |
| `deepkeep` | `deepkeep` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: deepkeep |
| `qostodian_nexus` | `qostodian_nexus` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: qostodian_nexus |
| `rubrik` | `rubrik` native text contract; separate input/output endpoints | block, annotate | N | Not run | Connector maintainers: rubrik |
| `vigil_guard` | `vigil_guard` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: vigil_guard |
| `repelloai` | `repelloai` native text contract | block, annotate | N | Not run | Connector maintainers: repelloai |
| `singulr` | `singulr` native text contract | block, annotate | N | Not run | Connector maintainers: singulr |
| `headroom` | `headroom` native text contract; synchronous input compression and request-local workflow recovery | block, redact, mask, annotate | N | Not run | Connector maintainers: headroom |
| `compresr` | `compresr` native text contract; synchronous input compression and request-local workflow recovery | block, redact, mask, annotate | N | Not run | Connector maintainers: compresr |
| `typesafe` | Gateway workflow pruning of complete prior tool exchanges through an accounted System One decision route | input pruning | N | Not run | Agent / accounting maintainers |
| `straiker` | `straiker` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: straiker |
| `alice` | `alice` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers: alice |
| `agent_365` | Shared consumer Entra OBO user, issuer and audience checks; fixture-qualified in RM7.2 | Refuse before exchange or dispatch | MCP grant owner | Content-free identity decision | MCP / access maintainers |
| `conduct` | `conduct` native text contract; input phases only | block, annotate | N | Not run | Connector maintainers: conduct |
| `decision_model` | `decision_model`: accounted model route with explicit JSON verdict rule | block, annotate | M | Not run | Guardrail / accounting maintainers |

### Scope decisions and owned follow-up

The five hooks outside text inspection require authority or execution state
absent from a text segment. MCP identity hooks now run in the shared consumer
execution owner; the remaining hooks still have the dependencies described below:

- `mcp_security` compares tool servers with an authorized MCP registry;
  `mcp_end_user_permission` requires the caller's server-specific grant;
  `mcp_jwt_signer` signs downstream calls with delegated identity. MCP/access
  maintainers own these in RM7.1/RM7.2. `internal/mcp/identity.go` and the
  personal-grant owner enforce them; the delegated-identity gateway regression
  verifies JSON and MCP dispatch, revocation and audience isolation.
- `agent_365` runs in the shared consumer identity and grant owners. It requires
  a verified delegated user assertion, explicit issuer and audience binding,
  and a sealed middle-tier credential. Local fixtures qualify the exchange;
  live tenant qualification remains separate.
- `typesafe` groups complete tool calls and results into ordered exchanges and
  sends prior exchanges to the configured decision route for relevance scoring.
  It retains the latest complete exchange and removes calls and results together.
  The relevance attempt has a separate debit under the workflow run. System One
  has no enforceable output bound, so a finite run cost limit prevents dispatch.

The local code/tool filters expose explicit OLP configuration, not Python
configuration compatibility. Semantic guardrails batch content and configured
reference utterances through one authorized embedding route; decision and judge
guardrails use explicit model verdict rules. Confined WASM remains the only
native custom-code execution contract. Compression adapters provide text
replacement and gateway workflows can recover compressed originals from
request-local memory. Recovery calls are accounted without persisting the
original content. Xecguard provides its scan endpoint; paired grounding
is outside the segment contract. Purview uses an operator-rotated Graph access
token; it does not impersonate the requesting user.

Connector maintainers own live authentication, API-version, action, timeout and
failure-policy qualification for every HTTP adapter. Release reviewers must
record candidate-specific inventory review and attach artifacts before closing
RM2.5. Implementation coverage is complete within these explicit boundaries;
live qualification remains pending. The
[operator adapter reference](../../use/guardrail-integrations.md) lists endpoints,
credentials, parameters and phase/action restrictions.
[Qualification](../qualification.md#guardrail-and-capture-qualification) tracks
commands and outstanding acceptance.
