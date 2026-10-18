# Pinned guardrail integration coverage

Aggregate vendor coverage: **Partial**. This inventory freezes the existing
LiteLLM v1.105.0 comparison baseline at commit
`80b9556dd312d18c535ae06884faa97d1fdb8f46`, enumerating all 60 values in
[`SupportedGuardrailIntegrations`](https://github.com/BerriAI/litellm/blob/80b9556dd312d18c535ae06884faa97d1fdb8f46/litellm/types/guardrails.py) in source order.
The registry includes services, local filters and execution hooks; 60 registry
entries does not mean 60 distinct vendors. It is not a moving latest-release
claim. Source file SHA-256: `6614ced8eb1db5a8845c862c4ad0b992139c3a14f5e1e955c4beacf445dfbb15`.

The OLP mapping and action columns describe inspected local code, not a claim of
LiteLLM configuration compatibility. No live-service qualification was performed
for this change. Unsupported entries are an explicit backlog, with no approved
exclusions. A generic webhook or confined WASM extension does not qualify a named
vendor. No additional adapters are introduced here.

Evidence keys:

- **V**: [`TestVendorContracts`](../internal/guardrails/external_test.go), six local
  fake request/auth/verdict contracts; transformations still require valid spans.
- **B**: [`engine_test.go`](../internal/guardrails/engine_test.go) and
  [`registry_test.go`](../internal/guardrails/registry_test.go), bounded built-in
  detector and engine fixtures. These are semantic analogues, not vendor adapters.
- **M**: [`guardrails_evaluator_regression_test.go`](../internal/gateway/guardrails_evaluator_regression_test.go)
  and [`guardrail_platform_test.go`](../tests/integration/guardrail_platform_test.go),
  operator-configured model evaluator routing/accounting; no dedicated vendor proof.
- **W**: [`external.go`](../internal/guardrails/external.go), inspected shared
  webhook implementation; [`guardrails_test.go`](../internal/gateway/guardrails_test.go)
  has a parallel evaluation fixture. No named third-party API compatibility proof.
- **None**: no named adapter or qualification evidence.

Actions below are engine capabilities on admitted surfaces. `mask` is synchronous
input only; enforcing transformations require transformed serving routes. Monitor
can observe supported actions. Capture permits only the four local types in the
[capture contract](payload-capture.md), regardless of serving capabilities.
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

## Owned follow-up

Connector maintainers own live authentication, API-version, action, timeout and
failure-policy evidence for each of the six existing adapters. Guardrail
maintainers own prioritization of every unsupported registry entry and explicit
support/exclusion decisions with rationale. A local analogue does not close an
integration gap; tool governance in OLP is request-local and does not implement
MCP authorization/signing hooks from this inventory. Native Python custom code
is unsupported; OLP's plugin contract remains confined WASM.

Release reviewers must record the inventory review and attach artifacts before
checking M7's inventory exit item. The [parity row](roadmap/parity.md) stays
`Partial` while gaps remain. [M7 qualification](roadmap/m07-qualification.md)
tracks commands and outstanding human acceptance.
