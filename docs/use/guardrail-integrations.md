# Guardrail integrations

A named guardrail integration sends inspected text to an external or local evaluation service in that service's native format and converts its response into an OLP guardrail finding. Configure integrations through immutable [guardrail definitions and policy entries](guardrails.md).

The OLP policy entry decides whether a finding blocks, annotates, redacts or masks the inspected content. A vendor's advisory result does not override that local policy choice. Setting `monitor: true` records findings without enforcing them.

## Connection and credentials

External definitions require an explicit operation URL, `credential_ref` and `content_egress: true`. For the named adapters below, `endpoint` is the complete operation URL including its path because the adapter does not discover a service or append a path. Onyx is the exception where you supply the base URL and the adapter appends the credential-bearing native operation path. The established Bedrock, Azure, Model Armor, Lakera v2 and Presidio adapters retain their existing endpoint rules in [the guardrail guide](guardrails.md#external-and-model-evaluation).

For example, after creating a project-owned credential, create this definition.

```json
{
  "name": "Prompt inspection",
  "project_id": "PROJECT_UUID",
  "type": "repelloai",
  "configuration": {
    "endpoint": "https://argusapi.repello.ai/sdk/v1/analyze/prompt",
    "credential_ref": "CREDENTIAL_REFERENCE",
    "content_egress": true,
    "parameters": {"asset_id": "ASSET_ID"}
  },
  "timeout_ms": 3000,
  "on_error": "fail_closed"
}
```

Activate the definition revision, then reference it from an activated policy. Keep credentials out of `parameters`. Create credentials through `POST /api/v1/guardrail-credentials`, where an API-key or bearer-token document is `{"api_key":"SECRET"}`. Header names in the tables below describe how that sealed value is sent, with `Bearer` meaning `Authorization: Bearer SECRET`.

HiddenLayer can instead use a sealed `{"client_id":"CLIENT_ID","client_secret":"CLIENT_SECRET"}` document and a separate `auth_endpoint`. It exchanges those credentials with HTTP Basic auth and `grant_type=client_credentials`, then uses the returned bearer token. Both the authentication endpoint and inspection endpoint must satisfy the same egress policy. Without `auth_endpoint`, supply a preissued bearer token. Microsoft Purview takes a preissued Microsoft Graph bearer token in `api_key`, and acquiring and refreshing that token is the credential operator's responsibility.

`network` accepts the existing provider connection options, including approved proxies and mutual TLS. Endpoints cannot contain embedded credentials, query strings or fragments. Akto adds its fixed inspection query parameters itself. Onyx puts its sealed credential in the vendor-required URL path, so configure upstream access logging to avoid recording that path.

## Native HTTP adapters

Paths below are native operation paths relative to the vendor's configured host unless shown as a complete URL. Optional parameters may be omitted, while **required** parameters must be supplied inside `configuration.parameters`. "Replacement" means the adapter can return validated text for an entry configured to redact or mask. It does not enable rewriting for a blocking or annotation entry.

Unless a row restricts phases, an adapter accepts `input`, `input_parallel`, `output` and `output_stream`. Runtime streaming restrictions still apply because complete-message inspection may require unary delivery and parallel input cannot rewrite content that has already been dispatched.

| Type | Native operation path | Authentication | Parameters | Native finding or replacement |
| --- | --- | --- | --- | --- |
| `aporia` | `/validate` | `X-APORIA-API-KEY` | None | `block` finds a violation and `passthrough` allows. Unsupported `modify` or `rephrase` responses fail evaluation. |
| `dynamoai` | `/v1/moderation/analyze/` | Bearer | `model_id`, `policy_ids` | `BLOCK` and `WARN` find violations and `NONE` allows. Other actions fail evaluation. |
| `guardrails_ai` | `/guards/{name}/validate` | Bearer | None | `validationPassed` determines pass or finding, and changed `validatedOutput` supplies a replacement. Configure the validation deployment to accept the bearer credential. |
| `lakera_v1` | `/v1/prompt_injection` | Bearer | `category_thresholds`, mapping `jailbreak` or `prompt_injection` to values in `[0,1]` | Flag or configured score threshold. Input and parallel input only. |
| `hiddenlayer` | `/detection/v1/interactions` | Bearer, or client credentials through `auth_endpoint` `/oauth2/token` | `project_id`, `model` | `Allow`, `Block` or `Redact`, with redaction reading modified messages. |
| `aim` | `/fw/v1/analyze` | Bearer | None | Monitor or block findings, or anonymized-message replacement. Sends `x-aim-litellm-hook`. |
| `cato_networks` | `/fw/v1/analyze` | Bearer | None | Monitor or block findings, or anonymized-message replacement. Sends `x-cato-litellm-hook`. |
| `pangea` | `/v1beta/guard` | Bearer | `input_recipe`, `output_recipe` | Policy verdict and transformed messages. |
| `crowdstrike_aidr` | `/v1/guard_chat_completions` | Bearer | `model` | Guard result with optional transformed messages. |
| `lasso` | `/gateway/v3/classify`, or `/gateway/v3/classifix` for masking | `lasso-api-key` | `user_id`, `mask` | Classification finding. Setting `mask: true` enables native masked-text replacement and requires the classifix endpoint. |
| `pillar` | `/api/v1/protect` | Bearer | `model`, `provider` | Policy violation or anonymized-message replacement. |
| `grayswan` | `/cygnal/monitor` | Bearer and `grayswan-api-key` | `violation_threshold` (default `0.5`), `policy_id`, `reasoning_mode` (`off`, `hybrid`, `thinking`), `categories` string map | Violation score meets threshold. |
| `panw_prisma_airs` | `/v1/scan/sync/request` | `x-pan-token` | `profile_id`, `profile_name` | `allow` or `block`, with prompt or response masked-data replacement. |
| `cisco_ai_defense` | `/api/v1/inspect/chat` | `X-Cisco-AI-Defense-API-Key` | `integration_profile_id`, `integration_profile_version`, `integration_tenant_id`, `integration_type`, `enabled_rules` | Safety or action verdict and sanitized-text replacement. Each enabled rule has `rule_name` or `rule_id`, and optional `entity_types` or `classification`. |
| `noma` | `/ai-dr/v2/prompt/scan` | Bearer | `application_id` | Security finding. Sensitive-data-only findings may supply anonymized content. |
| `noma_v2` | `/litellm/guardrail` | Bearer | `application_id`, `gateway_name` | `NONE`, `BLOCKED` or `GUARDRAIL_INTERVENED` with replacement. |
| `zscaler_ai_guard` | `/v1/detection/execute-policy` | Bearer | `policy_id` (nonnegative integer) | `ALLOW`, `DETECT` or `BLOCK`, where both detection actions produce policy findings. |
| `javelin` | `/v1/guardrail/{name}/apply` | `x-javelin-apikey` | `application`, `config` object | Any `request_reject` assessment produces a finding. Input and parallel input only. |
| `enkryptai` | `/guardrails/policy/detect` | `apikey` | `policy_name` | Summary flags or toxicity detections produce a finding. Policy name is sent as `x-enkrypt-policy`. |
| `ibm_guardrails` | Direct `/api/v1/text/contents` or orchestrator `/api/v2/text/detection/content` | Bearer, and direct mode also sends `detector-id` | **`detector_id`**, `orchestrator`, `detector_params` object, `score_threshold` in `[0,1]` | Typed detections, optionally filtered by score. Set `orchestrator: true` for the orchestrator path. |
| `onyx` | Base URL such as `https://ai-guard.onyx.security`, appending `/guard/evaluate/v1/{credential}/litellm` | Sealed credential in path | None | `allowed: false` produces a finding. |
| `promptguard` | `/api/v1/guard` | `X-API-Key` | None | `allow`, `block` or `redact` with replacement messages. |
| `xecguard` | `/xecguard/v1/scan` | Bearer | `model` (default `xecguard_v2`), `policy_names` | `SAFE` or `UNSAFE`. Uses the scan API. |
| `prompt_security` | `/api/protect` | `APP-ID` | `user`, `system_prompt` | Prompt or response action, with replacement on `modify`. |
| `qualifire` | `/api/evaluation/evaluate`, or `/api/evaluation/invoke` for a saved evaluation | `X-Qualifire-API-Key` | **`evaluation_id`, or at least one check or assertion**, `prompt_injections`, `hallucinations_check`, `grounding_check`, `pii_check`, `content_moderation_check`, `assertions` | Any flagged evaluation result produces a finding. Match the endpoint to saved-evaluation or check-based configuration. |
| `ovalix` | `/tracking/custom_application/checkpoint` | Bearer | **`application_id`, `checkpoint_id`, `actor`** | `allow`, `block` or `anonymize`, with modified content supplying replacement. Use separate definitions for distinct input and output checkpoints. |
| `microsoft_purview` | `/users/{user-id}/dataSecurityAndGovernance/processContent` | Preissued Graph Bearer | **`application_id`, `application_name`** | A restrict-access policy action with `restrictionAction: block` produces a finding. |
| `akto` | `/api/http-proxy` | Raw `Authorization` value | **`account_id`, `vxlan_id`** | `data.guardrailsResult.Allowed`, sending fixed `akto_connector=litellm&guardrails=true` query without asynchronous ingestion. |
| `deepkeep` | `/v3/openai/beta/litellm_basic_guardrail_api` | `X-API-Key` | **`firewall_id`** | `NONE`, `BLOCKED` or `GUARDRAIL_INTERVENED`, with optional returned texts supplying replacement. |
| `qostodian_nexus` | `/beta/litellm_basic_guardrail_api` | `X-API-Key` | `source`, `container`, `identity` | Basic Guardrail API action and replacement. Adds native identifier headers and a generated trace identifier. |
| `rubrik` | Input `/v1/before_prompt/openai/v1` and output `/v1/after_completion/openai/v1` | Bearer | None | Input refusal completion or changed output text produces a finding. Use separate definition revisions for the two endpoints. |
| `vigil_guard` | `/v1/guard/analyze` | Bearer | None | `ALLOWED`, `BLOCKED` or `SANITIZED`, where sanitization requires returned text. |
| `repelloai` | `/sdk/v1/analyze/prompt` or `/sdk/v1/analyze/response` | `X-API-Key` | **`asset_id`** | `passed`, `flagged` or `blocked`, where flagged and blocked produce findings. Select the endpoint for the phase. |
| `singulr` | `/api/v1/ai-gateway/litellm-v2` | `X-Singulr-Gateway-Token` | `application_id`, `guardrail_id` | `should_block`, with optional parameters sent as native policy headers. |
| `headroom` | `/v1/compress` | Bearer | `model` | Returned compressed message supplies replacement. Input only, with request-local recovery inside gateway workflows. |
| `compresr` | `/api/compress/question-specific/` | `X-API-Key` | **`query`**, `compression_model_name` (default `latte_v2`), `target_compression_ratio` (default `0.5`), `coarse`, `dynamic` | `data.compressed_context` supplies replacement. Input only, with request-local recovery inside gateway workflows. |
| `straiker` | `/api/v1/detect/webhook` | Bearer and `X-Straiker-Webhook-Format: litellm` | `source` (default `olp`) | Schema v1 webhook returning `NONE`, `BLOCKED` or `GUARDRAIL_INTERVENED` with replacement. |
| `alice` | `/v2/evaluate/litellm` | `af-api-key` | None | `ALLOW`, `DETECT`, `BLOCK` or `MASK`, where mask requires indexed replacement text. |
| `conduct` | `/mcp` | Bearer and optional `X-Workspace-Id` | `workspace_id`, `model`, `provider` | JSON-RPC `tools/call` of `guard_check_prompt` returning advisory, warning, block or pending-approval findings. Input and parallel input only. |
| `generic_guardrail_api` | `/beta/litellm_basic_guardrail_api` | `x-api-key` | None | Native Basic Guardrail API returning `NONE`, `BLOCKED` or `GUARDRAIL_INTERVENED` with validated replacement text. |
| `openai_moderation` | `/v1/moderations` | Bearer | `model` | Exactly one moderation result with boolean `flagged`. |

The established adapters use top-level `configuration` fields rather than `configuration.parameters` and append their operation path to the configured base URL.

| Type | Appended operation | Authentication | Configuration and behavior |
| --- | --- | --- | --- |
| `bedrock` | `/guardrail/{guardrail}/version/{version}/apply` | AWS Signature V4 using sealed `access_key_id`, `secret_access_key` and optional `session_token` | Required `region`, `guardrail` and `version`. `GUARDRAIL_INTERVENED` detects a violation and may supply replacement output. |
| `azure.prompt_shields` | `/contentsafety/text:shieldPrompt?api-version=2024-09-01` | `Ocp-Apim-Subscription-Key` | Detects attacks in user text or captured tool-result documents. |
| `azure.content_safety` | `/contentsafety/text:analyze?api-version=2024-09-01` | `Ocp-Apim-Subscription-Key` | `threshold` in `[0,7]`. Any native category severity greater than the threshold produces a finding. |
| `model_armor` | `/v1/{template}:sanitizeUserPrompt` or `:sanitizeModelResponse` | Bearer | Required `template` resource name. Native filter match produces a finding. |
| `lakera` | `/v2/guard` | Bearer | Boolean `flagged` result. Uses Lakera v2 while `lakera_v1` selects the earlier API. |
| `presidio` | `/analyze`, plus `/anonymize` on optional `anonymizer_endpoint` base | Bearer | Optional `language` (default `en`). Native entity offsets become UTF-8 byte spans for local `[REDACTED]` replacement, or forward to `anonymizer_endpoint` for remote replacement. |

`webhook` uses the exact endpoint and the OLP request and verdict schema. Its sealed credential contains `signing_key`, and requests carry an `X-OLP-Signature` HMAC over the timestamp and exact body bytes. The webhook protocol is distinct from the native `generic_guardrail_api` protocol.

Rubrik's input endpoint intentionally returns no completion choices for an allowed prompt, so an empty JSON object is a valid native success response. Other adapters require their documented verdict fields. Unknown actions, missing required fields, invalid replacement types or counts, and malformed responses are evaluation failures rather than implicit allows.

Outside gateway workflows, `headroom` and `compresr` perform stateless synchronous input compression where returned replacement text must fit the shared text bound and no original segments are retained. Inside gateway agent workflows, input evaluation enables request-local workflow recovery (`WorkflowRecovery`), keeping the pre-compression segments in request-local memory (never persisted), appending `[recovery id: <n>]` to each compressed segment, and letting the workflow recover original text through an accounted recovery tool call that settles at zero cost and exempts recovered tool results from re-compression on subsequent iterations.

## Model and local integrations

Model-backed definitions use an authorized OLP route rather than a vendor URL or guardrail credential. Requests use the caller's pinned release, identity, deadlines, budgets and accounting, with evaluator usage attributed as a child request. Internal evaluator requests skip model guardrails to prevent recursion.

| Type | Configuration and behavior |
| --- | --- |
| `model` | `route` and `rule: {pointer, equals, category, prompt?}`. Evaluates the configured JSON Pointer equality against a generation, classification or moderation route's result for LLM-as-a-judge policies. |
| `decision_model` | The same explicit, accounted model-result rule. Configures the chosen route and response rule without executing a hidden vendor model call. |
| `semantic_guard` | `route` naming an embeddings route and `semantic: {category, similarity_threshold, utterances}`. A cosine similarity at or above the threshold produces a finding. |
| `typesafe` | `route` naming an authorized System One `decision` route. Active on `input` with `monitor: false` during gateway agent workflows. Groups complete prior tool exchanges, retains the latest exchange and skips recovered exchanges, queries the decision route in batches of up to 256 exchanges, and prunes exchanges scoring below 0.2 as an accounted child classifier attempt. |
| `builtin.secrets` | Local analogue of the `hide-secrets` inventory entry using documented secret categories and optional patterns or replacement. |
| `builtin.keywords` and `builtin.regex` | Local content-filter policies, including the `litellm_content_filter` inventory use case. |
| `builtin.tool_permission` | Ordered `rules` with `id`, optional `tool_name` or `tool_type`, `decision` (`allow` or `deny`), and optional `allowed_param_patterns`, with `default_action` defaulting to `deny`. Checks captured function-tool definitions and calls without executing them. Patterns are full-match RE2 and parameter paths use dotted names and `[]` arrays. |
| `builtin.code_execution` | `blocked_languages`, `confidence_threshold` and `detect_execution_intent`. Detects fenced code and execution requests without running code, supporting local blocking, annotation or replacement. |
| `plugin` | `plugin_id` and immutable SHA-256 `plugin_version`. Runs an approved, confined WASM guardrail as the supported custom-code boundary because arbitrary Python is not loaded in the gateway. |

A semantic guard embeds all inspected segments and reference utterances in one accounted request so a retry or failover cannot mix vector spaces. It accepts 1 to 64 reference utterances, each at most 8,192 bytes and together at most 64 KiB, with a threshold greater than zero and at most one. Each evaluation permits at most 128 segments and 4,096 vector dimensions. Results must include every unique index, consistent dimensions, finite values and nonzero vector norms.

## Failure handling and limits

External requests share the provider egress rules, pooled connections, pinned credential material and definition deadline. The definition timeout is 1 to 300,000 milliseconds, capped by the remaining request deadline. Service errors, timeouts, invalid JSON and invalid native verdicts follow the configured `fail_open` or `fail_closed` policy. Five consecutive failures open a per-definition-revision circuit for 30 seconds, followed by one probe.

Conduct identity denials are policy findings. HTTP 401 and 403 replies and native `identity_required` responses are not converted to availability failures by a `fail_open` setting, while the policy entry still controls whether findings block or are monitored. Missing inspection capability is refused independently of availability policy.

An evaluation accepts at most 128 effective entries, 2,048 text segments and 8 MiB of text. HTTP responses are bounded to 1 MiB. Replacement text must be valid UTF-8 and at most 4,096 bytes, with the adapter supplying a validated full-segment span. Redaction and masking require a transformed route. Masking is synchronous input only, and parallel input cannot redact. Complete-message external checks may require unary delivery instead of streaming.

OLP does not persist submitted text, provider response bodies or replacement text as guardrail evidence. Evidence contains bounded identifiers, categories, actions, outcomes and latency. External services receive the inspected content and apply their own retention policies. These external adapters are not eligible as local capture-redaction detectors.

The [pinned integration inventory](#vendor-inventory) records implementation scope and separate live-vendor qualification. MCP identity and authorization hooks and complete tool-history pruning require their own MCP or agent-runtime context and are not exposed as pretend text scanners.

## Vendor inventory

Aggregate vendor coverage is **Implemented within the recorded scope, with live qualification pending**. This inventory freezes the existing LiteLLM v1.105.0 comparison baseline at commit `80b9556dd312d18c535ae06884faa97d1fdb8f46`, enumerating all 60 values in [`SupportedGuardrailIntegrations`](https://github.com/BerriAI/litellm/blob/80b9556dd312d18c535ae06884faa97d1fdb8f46/litellm/types/guardrails.py) in source order. The registry includes services, local filters and execution hooks, so 60 registry entries does not mean 60 distinct vendors and is not a moving latest-release claim. The source file SHA-256 is `6614ced8eb1db5a8845c862c4ad0b992139c3a14f5e1e955c4beacf445dfbb15`.

The OLP mapping and action columns describe inspected local code rather than a claim of LiteLLM configuration compatibility. No live-service qualification was performed in the source inventory review. Every registry entry has an implementation mapping or an explicit scope decision below. Five execution hooks depend on MCP and agent execution rather than the text evaluation contract. A generic webhook or confined WASM extension does not qualify a named HTTP vendor, and native Python execution remains outside the confined plugin contract.

| Evidence key | Local coverage |
| --- | --- |
| **V** | [`TestCloudContracts`](../../internal/guardrails/vendor/cloud_test.go), covering six local fake request, auth and verdict contracts. Transformations still require valid spans. |
| **B** | [`engine_test.go`](../../internal/guardrails/engine_test.go) and [`registry_test.go`](../../internal/guardrails/registry_test.go), covering bounded built-in detector and engine fixtures as semantic analogues rather than vendor adapters. |
| **M** | [`TestModelGuardrailRoutingAndAccounting`](../../internal/gateway/kernel/guardrails_test.go) and [`TestModelGuardrailDeduplicationAuthorizationRecursionAndPlayground`](../../internal/gateway/kernel/guardrails_test.go) in [`guardrails_test.go`](../../internal/gateway/kernel/guardrails_test.go), testing `modelGuardrail` routing, child accounting, target deduplication, caller route authorization, recursion skipping and keyless playground evaluation across generation, moderation, classification and semantic embedding routes, alongside selection and recursion checks in [`external_test.go`](../../internal/guardrails/external_test.go). No live third-party vendor proof. |
| **W** | [`external.go`](../../internal/guardrails/external.go) shared webhook implementation and parallel evaluation fixtures in [`guardrails_test.go`](../../internal/gateway/kernel/guardrails_test.go). No named third-party API compatibility proof. |
| **N** | [`vendor_test.go`](../../internal/guardrails/vendor/vendor_test.go) and the family tests beside it in [`vendor`](../../internal/guardrails/vendor), testing native HTTP authentication, phase, request, verdict, replacement and malformed-response contracts locally rather than live qualification. |
| **L** | [`tool_permission_test.go`](../../internal/guardrails/tool_permission_test.go) and [`code_execution_test.go`](../../internal/guardrails/code_execution_test.go), testing local ordered permission rules and executable-code detection. |
| **S** | [`semantic_test.go`](../../internal/guardrails/semantic_test.go) and gateway regression checks covering bounded embedding batches, validated cosine comparison, caller authorization, accounting and recursion. |
| **Scope** | Explicit architectural disposition described below rather than vendor qualification. |

Actions below are engine capabilities on admitted surfaces. `mask` is synchronous input only, and enforcing transformations require transformed serving routes. Monitor can observe supported actions. Capture permits only the four local types in the [capture contract](../operate/observability.md#payload-capture) regardless of serving capabilities. Bedrock transformations require a returned replacement, while Presidio either applies OLP's local `[REDACTED]` replacement to analyzed spans or forwards analyzer results to an optional configured `anonymizer_endpoint` (`/anonymize`) for remote replacement. Neither claims every upstream anonymizer option.

| Registry integration | OLP mapping and status | Supported OLP actions | Local evidence | Live qualification | Follow-up owner |
| --- | --- | --- | --- | --- | --- |
| `aporia` | `aporia` native text contract | block, annotate | N | Not run | Connector maintainers (aporia) |
| `bedrock` | `bedrock` (ApplyGuardrail) | block, redact, mask, annotate | V | Not run | Connector maintainers (Bedrock) |
| `dynamoai` | `dynamoai` native text contract | block, annotate | N | Not run | Connector maintainers (dynamoai) |
| `guardrails_ai` | `guardrails_ai` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (guardrails_ai) |
| `lakera` | `lakera_v1` native text contract, input phases only | block, annotate | N | Not run | Connector maintainers (lakera) |
| `lakera_v2` | `lakera` (`/v2/guard`) | block, annotate | V | Not run | Connector maintainers (Lakera) |
| `presidio` | `presidio` (analysis spans with local replacement or optional `anonymizer_endpoint` `/anonymize`) | block, redact, mask, annotate | V | Not run | Connector maintainers (Presidio) |
| `hide-secrets` | Local analogue `builtin.secrets` | block, redact, mask, annotate | B | Not applicable, local comparison pending | Guardrail maintainers (corpus comparison) |
| `hiddenlayer` | `hiddenlayer` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (hiddenlayer) |
| `aim` | `aim` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (aim) |
| `cato_networks` | `cato_networks` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (cato_networks) |
| `pangea` | `pangea` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (pangea) |
| `crowdstrike_aidr` | `crowdstrike_aidr` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (crowdstrike_aidr) |
| `lasso` | `lasso` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (lasso) |
| `pillar` | `pillar` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (pillar) |
| `grayswan` | `grayswan` native text contract | block, annotate | N | Not run | Connector maintainers (grayswan) |
| `panw_prisma_airs` | `panw_prisma_airs` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (panw_prisma_airs) |
| `cisco_ai_defense` | `cisco_ai_defense` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (cisco_ai_defense) |
| `azure/prompt_shield` | `azure.prompt_shields` | block, annotate | V | Not run | Connector maintainers (Azure) |
| `azure/text_moderations` | `azure.content_safety` | block, annotate | V | Not run | Connector maintainers (Azure) |
| `model_armor` | `model_armor` (verdict only) | block, annotate | V | Not run | Connector maintainers (Google) |
| `openai_moderation` | `openai_moderation` native text contract | block, annotate | N | Not run | Connector maintainers (openai_moderation) |
| `noma` | `noma` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (noma) |
| `noma_v2` | `noma_v2` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (noma_v2) |
| `tool_permission` | `builtin.tool_permission` ordered RE2 rules on captured function tools | block, annotate | L | Not applicable | Guardrail maintainers |
| `zscaler_ai_guard` | `zscaler_ai_guard` native text contract | block, annotate | N | Not run | Connector maintainers (zscaler_ai_guard) |
| `javelin` | `javelin` native text contract, input phases only | block, annotate | N | Not run | Connector maintainers (javelin) |
| `enkryptai` | `enkryptai` native text contract | block, annotate | N | Not run | Connector maintainers (enkryptai) |
| `ibm_guardrails` | `ibm_guardrails` native text contract | block, annotate | N | Not run | Connector maintainers (ibm_guardrails) |
| `litellm_content_filter` | Local analogue `builtin.keywords` and `builtin.regex` | block, redact, mask, annotate | B | Not applicable, local comparison pending | Guardrail maintainers (rule comparison) |
| `mcp_security` | Shared MCP execution checks live registered server, pinned publication and caller authority | Gateway fixture, JSON and MCP | Identity | Bounded attempt decision | MCP and access maintainers |
| `onyx` | `onyx` native text contract | block, annotate | N | Not run | Connector maintainers (onyx) |
| `promptguard` | `promptguard` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (promptguard) |
| `xecguard` | `xecguard` native text contract | block, annotate | N | Not run | Connector maintainers (xecguard) |
| `prompt_security` | `prompt_security` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (prompt_security) |
| `generic_guardrail_api` | `generic_guardrail_api` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (generic_guardrail_api) |
| `qualifire` | `qualifire` native text contract | block, annotate | N | Not run | Connector maintainers (qualifire) |
| `custom_code` | `plugin` approved confined WASM (Python source requires a port) | block, redact, mask, annotate | Existing plugin implementation | WASM conformance remains separate | Plugin and guardrail maintainers |
| `ovalix` | `ovalix` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (ovalix) |
| `microsoft_purview` | `microsoft_purview` native text contract | block, annotate | N | Not run | Connector maintainers (microsoft_purview) |
| `semantic_guard` | `semantic_guard` accounted embedding similarity against configured utterances | block, annotate | S | Not run | Guardrail and accounting maintainers |
| `mcp_end_user_permission` | Shared personal-grant resolver checks exact live caller and server binding | Gateway fixture, missing and revoked grants | Identity | Bounded attempt decision | MCP and access maintainers |
| `block_code_execution` | `builtin.code_execution` fenced languages and execution intent | block, redact, mask, annotate | L | Not applicable | Guardrail maintainers |
| `akto` | `akto` native text contract | block, annotate | N | Not run | Connector maintainers (akto) |
| `mcp_jwt_signer` | Dedicated sealed Ed25519 key signs short-lived server, project and individual claims for the configured audience | Gateway fixture, signature and cross-audience rejection | Identity | Bounded attempt decision | MCP and access maintainers |
| `llm_as_a_judge` | `model` via operator-configured evaluator route | block, annotate | M | Not run | Guardrail maintainers (judge mapping) |
| `deepkeep` | `deepkeep` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (deepkeep) |
| `qostodian_nexus` | `qostodian_nexus` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (qostodian_nexus) |
| `rubrik` | `rubrik` native text contract with separate input and output endpoints | block, annotate | N | Not run | Connector maintainers (rubrik) |
| `vigil_guard` | `vigil_guard` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (vigil_guard) |
| `repelloai` | `repelloai` native text contract | block, annotate | N | Not run | Connector maintainers (repelloai) |
| `singulr` | `singulr` native text contract | block, annotate | N | Not run | Connector maintainers (singulr) |
| `headroom` | `headroom` native text contract with synchronous input compression and request-local workflow recovery | block, redact, mask, annotate | N | Not run | Connector maintainers (headroom) |
| `compresr` | `compresr` native text contract with synchronous input compression and request-local workflow recovery | block, redact, mask, annotate | N | Not run | Connector maintainers (compresr) |
| `typesafe` | Gateway workflow pruning of complete prior tool exchanges through an accounted System One decision route | input pruning | N | Not run | Agent and accounting maintainers |
| `straiker` | `straiker` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (straiker) |
| `alice` | `alice` native text contract | block, redact, mask, annotate | N | Not run | Connector maintainers (alice) |
| `agent_365` | Shared consumer Entra OBO user, issuer and audience checks (fixture-qualified) | Refuse before exchange or dispatch | MCP grant owner | Content-free identity decision | MCP and access maintainers |
| `conduct` | `conduct` native text contract, input phases only | block, annotate | N | Not run | Connector maintainers (conduct) |
| `decision_model` | `decision_model` accounted model route with explicit JSON verdict rule | block, annotate | M | Not run | Guardrail and accounting maintainers |

### Scope decisions and owned follow-up

The five hooks outside text inspection require authority or execution state absent from a text segment. MCP identity hooks run in the shared consumer execution owner, while the remaining hooks depend on their runtime contexts as follows.

`mcp_security` compares tool servers with an authorized MCP registry, `mcp_end_user_permission` requires the caller's server-specific grant, and `mcp_jwt_signer` signs downstream calls with delegated identity. MCP and access maintainers own these hooks. `internal/mcp/identity.go` and the personal-grant owner enforce them, and the delegated-identity gateway regression verifies JSON and MCP dispatch, revocation and audience isolation.

`agent_365` runs in the shared consumer identity and grant owners. It requires a verified delegated user assertion, explicit issuer and audience binding, and a sealed middle-tier credential. Local fixtures qualify the exchange, while live tenant qualification remains separate.

`typesafe` groups complete tool calls and results into ordered exchanges and sends prior exchanges to the configured System One decision route for relevance scoring. It retains the latest complete exchange, skips recovered exchanges, and removes pruned calls and results together. The relevance attempt records a separate child debit under the workflow run. With a finite run cost limit, dispatch requires pinned context and output bounds, pricing and enough remaining allowance for the conservative cost bound.

The local code and tool filters expose explicit OLP configuration rather than Python configuration compatibility. Semantic guardrails batch content and configured reference utterances through one authorized embedding route, while decision and judge guardrails use explicit model verdict rules. Confined WASM remains the only native custom-code execution contract. Compression adapters provide synchronous input text replacement, and inside gateway workflows OLP retains the pre-compression segments in request-local memory so recovery calls can retrieve them on demand with zero-cost accounting and without persisting the original content. Xecguard provides its scan endpoint, while paired grounding is outside the segment contract. Purview uses an operator-rotated Graph access token rather than impersonating the requesting user.

Connector maintainers own live authentication, API-version, action, timeout and failure-policy qualification for every HTTP adapter. Release reviewers must record candidate-specific inventory review and attach artifacts. Implementation coverage is complete within these explicit boundaries, while live qualification remains pending. The adapter tables above list endpoints, credentials, parameters and phase or action restrictions, and [Qualification](../develop/qualification.md#guardrail-and-capture-qualification) records current status.
