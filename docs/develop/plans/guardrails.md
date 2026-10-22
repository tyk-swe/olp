# Guardrail completion plan

Status: **In progress**. The engine and capture-redaction implementation exist;
release acceptance, live adapters and broader vendor coverage remain open.
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
- [ ] **RM2.3 — Qualify all six live adapters** (connector/plugin): record each
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
- [ ] Each of the six named adapters has live-service evidence, distinct from fakes.
- [ ] Release review records the pinned 60-entry inventory, each support or
  exclusion decision and an owned backlog. Coverage remains **Partial** while
  unsupported entries remain; no overall vendor parity claim is made.
- [ ] The related RM2.6 cache gate has paired reference measurements and
  isolation/replay/failure evidence; any live prompt-cache claim has its own proof.

## Vendor inventory

Aggregate vendor coverage: **Partial**. This inventory freezes the existing
LiteLLM v1.105.0 comparison baseline at commit
`80b9556dd312d18c535ae06884faa97d1fdb8f46`, enumerating all 60 values in
[`SupportedGuardrailIntegrations`](https://github.com/BerriAI/litellm/blob/80b9556dd312d18c535ae06884faa97d1fdb8f46/litellm/types/guardrails.py) in source order.
The registry includes services, local filters and execution hooks; 60 registry
entries does not mean 60 distinct vendors. It is not a moving latest-release
claim. Source file SHA-256: `6614ced8eb1db5a8845c862c4ad0b992139c3a14f5e1e955c4beacf445dfbb15`.

The OLP mapping and action columns describe inspected local code, not a claim of
LiteLLM configuration compatibility. No live-service qualification was performed
in the source inventory review. Unsupported entries are an explicit backlog, with no approved
exclusions. A generic webhook or confined WASM extension does not qualify a named
vendor.

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
- **None**: no named adapter or qualification evidence.

Actions below are engine capabilities on admitted surfaces. `mask` is synchronous
input only; enforcing transformations require transformed serving routes. Monitor
can observe supported actions. Capture permits only the four local types in the
[capture contract](../../operate/observability.md#payload-capture), regardless of serving capabilities.
Bedrock transformations require a returned replacement; Presidio uses analyzed
spans with OLP's local replacement. Neither claims every upstream anonymizer option.

| Registry integration | OLP mapping / status | Supported OLP actions | Local evidence | Live qualification | Follow-up owner |
| --- | --- | --- | --- | --- | --- |
| `aporia` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `bedrock` | `bedrock` (ApplyGuardrail) | block, redact, mask, annotate | V | Not run | Connector maintainers: Bedrock |
| `dynamoai` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `guardrails_ai` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `lakera` | Unsupported legacy contract; OLP implements v2 | None | None | Unqualified; no named adapter | Connector maintainers: Lakera |
| `lakera_v2` | `lakera` (`/v2/guard`) | block, annotate | V | Not run | Connector maintainers: Lakera |
| `presidio` | `presidio` (analysis spans, local replacement) | block, redact, mask, annotate | V | Not run | Connector maintainers: Presidio |
| `hide-secrets` | Local analogue: `builtin.secrets` | block, redact, mask, annotate | B | Not applicable; local comparison pending | Guardrail maintainers: corpus comparison |
| `hiddenlayer` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `aim` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `cato_networks` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `pangea` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `crowdstrike_aidr` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `lasso` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `pillar` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `grayswan` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `panw_prisma_airs` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `cisco_ai_defense` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `azure/prompt_shield` | `azure.prompt_shields` | block, annotate | V | Not run | Connector maintainers: Azure |
| `azure/text_moderations` | `azure.content_safety` | block, annotate | V | Not run | Connector maintainers: Azure |
| `model_armor` | `model_armor` (verdict only) | block, annotate | V | Not run | Connector maintainers: Google |
| `openai_moderation` | `model` via operator-configured moderation route | block, annotate | M | Not run | Guardrail / connector maintainers: OpenAI |
| `noma` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `noma_v2` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `tool_permission` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `zscaler_ai_guard` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `javelin` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `enkryptai` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `ibm_guardrails` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `litellm_content_filter` | Local analogue: `builtin.keywords` / `builtin.regex` | block, redact, mask, annotate | B | Not applicable; local comparison pending | Guardrail maintainers: rule comparison |
| `mcp_security` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `onyx` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `promptguard` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `xecguard` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `prompt_security` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `generic_guardrail_api` | `webhook` uses OLP contract; reference API compatibility unsupported | block, redact, mask, annotate (OLP contract only) | W | Not run | Guardrail maintainers: contract comparison |
| `qualifire` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `custom_code` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `ovalix` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `microsoft_purview` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `semantic_guard` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `mcp_end_user_permission` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `block_code_execution` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `akto` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `mcp_jwt_signer` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `llm_as_a_judge` | `model` via operator-configured evaluator route | block, annotate | M | Not run | Guardrail maintainers: judge mapping |
| `deepkeep` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `qostodian_nexus` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `rubrik` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `vigil_guard` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `repelloai` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `singulr` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `headroom` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `compresr` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `typesafe` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `straiker` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `alice` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `agent_365` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `conduct` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |
| `decision_model` | Unsupported | None | None | Unqualified; no named adapter | Guardrail / connector maintainers |

### Owned follow-up

Connector maintainers own live authentication, API-version, action, timeout and
failure-policy evidence for each of the six existing adapters. Guardrail
maintainers own prioritization of every unsupported registry entry and explicit
support/exclusion decisions with rationale. A local analogue does not close an
integration gap; tool governance in OLP is request-local and does not implement
MCP authorization/signing hooks from this inventory. Native Python custom code
is unsupported; OLP's plugin contract remains confined WASM.

Release reviewers must record the inventory review and attach artifacts before
closing the inventory acceptance gate. Aggregate coverage stays `Partial` while gaps remain.
[Qualification](../qualification.md#guardrail-and-capture-qualification) tracks
commands and outstanding acceptance.
