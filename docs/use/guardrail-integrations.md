# Guardrail integrations

A named guardrail integration sends inspected text to an external or local
evaluation service in that service's native format and converts its response into
an OLP guardrail finding. Configure integrations through immutable
[guardrail definitions and policy entries](guardrails.md).

The OLP policy entry decides whether a finding blocks, annotates, redacts or
masks the inspected content. A vendor's advisory result does not override that
local policy choice. Setting `monitor: true` records findings without enforcing
them.

## Connection and credentials

External definitions require an explicit operation URL, `credential_ref`, and
`content_egress: true`. For the named adapters below, `endpoint` is the complete
operation URL, including its path. The adapter does not discover a service or
append a path. Onyx is the exception: give its base URL; the adapter appends the
credential-bearing native operation path. The established Bedrock, Azure, Model
Armor, Lakera v2, and Presidio adapters retain their existing endpoint rules in
[the guardrail guide](guardrails.md#external-and-model-evaluation).

For example, after creating a project-owned credential, create this definition:

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

Activate the definition revision, then reference it from an activated policy.
Keep credentials out of `parameters`. Create them through
`POST /api/v1/guardrail-credentials`; an API-key or bearer-token credential
document is `{"api_key":"SECRET"}`. Header names in the tables describe how that
sealed value is sent. `Bearer` means `Authorization: Bearer SECRET`.

HiddenLayer can instead use a sealed
`{"client_id":"CLIENT_ID","client_secret":"CLIENT_SECRET"}` document and a
separate `auth_endpoint`. It exchanges those credentials with HTTP Basic auth
and `grant_type=client_credentials`, then uses the returned bearer token. Both
the authentication endpoint and inspection endpoint must satisfy the same
egress policy. Without `auth_endpoint`, supply a preissued bearer token.
Microsoft Purview takes a preissued Microsoft Graph bearer token in `api_key`;
acquiring and refreshing that token is the credential operator's responsibility.

`network` accepts the existing provider connection options, including approved
proxies and mutual TLS. Endpoints cannot contain embedded credentials, query
strings, or fragments. Akto adds its fixed inspection query parameters itself.
Onyx puts its sealed credential in the vendor-required URL path; configure
upstream access logging to avoid recording that path.

## Native HTTP adapters

Paths below are native operation paths relative to the vendor's configured host,
unless shown as a complete URL. Optional parameters may be omitted; **required**
parameters must be supplied inside `configuration.parameters`. “Replacement”
means the adapter can return validated text for an entry configured to redact
or mask. It does not enable rewriting for a blocking or annotation entry.

Unless a row restricts phases, an adapter accepts `input`, `input_parallel`,
`output`, and `output_stream`. Runtime streaming restrictions still apply:
complete-message inspection may require unary delivery, and parallel input
cannot rewrite content that has already been dispatched.

| Type | Native operation path | Authentication | Parameters | Native finding or replacement |
| --- | --- | --- | --- | --- |
| `aporia` | `/validate` | `X-APORIA-API-KEY` | None | `block` finds a violation; `passthrough` allows. Unsupported `modify`/`rephrase` responses fail evaluation. |
| `dynamoai` | `/v1/moderation/analyze/` | Bearer | `model_id`, `policy_ids` | `BLOCK` and `WARN` find violations; `NONE` allows. Other actions fail evaluation. |
| `guardrails_ai` | `/guards/{name}/validate` | Bearer | None | `validationPassed`; changed `validatedOutput` supplies a replacement. Configure the validation deployment to accept the bearer credential. |
| `lakera_v1` | `/v1/prompt_injection` | Bearer | `category_thresholds`, mapping `jailbreak` or `prompt_injection` to values in `[0,1]` | Flag or configured score threshold. Input and parallel input only. |
| `hiddenlayer` | `/detection/v1/interactions` | Bearer, or client credentials through `auth_endpoint` `/oauth2/token` | `project_id`, `model` | `Allow`, `Block`, or `Redact`; redaction reads modified messages. |
| `aim` | `/fw/v1/analyze` | Bearer | None | Monitor/block findings or anonymized-message replacement. Sends `x-aim-litellm-hook`. |
| `cato_networks` | `/fw/v1/analyze` | Bearer | None | Monitor/block findings or anonymized-message replacement. Sends `x-cato-litellm-hook`. |
| `pangea` | `/v1beta/guard` | Bearer | `input_recipe`, `output_recipe` | Policy verdict and transformed messages. |
| `crowdstrike_aidr` | `/v1/guard_chat_completions` | Bearer | `model` | Guard result with optional transformed messages. |
| `lasso` | `/gateway/v3/classify`; use `/gateway/v3/classifix` for masking | `lasso-api-key` | `user_id`, `mask` | Classification finding; `mask: true` enables native masked-text replacement and requires the classifix endpoint. |
| `pillar` | `/api/v1/protect` | Bearer | `model`, `provider` | Policy violation or anonymized-message replacement. |
| `grayswan` | `/cygnal/monitor` | Bearer and `grayswan-api-key` | `violation_threshold` (default `0.5`), `policy_id`, `reasoning_mode` (`off`, `hybrid`, `thinking`), `categories` string map | Violation score meets threshold. |
| `panw_prisma_airs` | `/v1/scan/sync/request` | `x-pan-token` | `profile_id`, `profile_name` | `allow`/`block`, with prompt or response masked-data replacement. |
| `cisco_ai_defense` | `/api/v1/inspect/chat` | `X-Cisco-AI-Defense-API-Key` | `integration_profile_id`, `integration_profile_version`, `integration_tenant_id`, `integration_type`, `enabled_rules` | Safety/action verdict and sanitized-text replacement. Each enabled rule has `rule_name` or `rule_id`, and optional `entity_types`/`classification`. |
| `noma` | `/ai-dr/v2/prompt/scan` | Bearer | `application_id` | Security finding; sensitive-data-only findings may supply anonymized content. |
| `noma_v2` | `/litellm/guardrail` | Bearer | `application_id`, `gateway_name` | `NONE`, `BLOCKED`, or `GUARDRAIL_INTERVENED` with replacement. |
| `zscaler_ai_guard` | `/v1/detection/execute-policy` | Bearer | `policy_id` (nonnegative integer) | `ALLOW`, `DETECT`, or `BLOCK`; both detection actions produce policy findings. |
| `javelin` | `/v1/guardrail/{name}/apply` | `x-javelin-apikey` | `application`, `config` object | Any `request_reject` assessment produces a finding. Input and parallel input only. |
| `enkryptai` | `/guardrails/policy/detect` | `apikey` | `policy_name` | Summary flags or toxicity detections produce a finding. Policy name is sent as `x-enkrypt-policy`. |
| `ibm_guardrails` | Direct: `/api/v1/text/contents`; orchestrator: `/api/v2/text/detection/content` | Bearer; direct mode also sends `detector-id` | **`detector_id`**; `orchestrator`, `detector_params` object, `score_threshold` in `[0,1]` | Typed detections, optionally filtered by score. Set `orchestrator: true` for the orchestrator path. |
| `onyx` | Base URL such as `https://ai-guard.onyx.security`; appends `/guard/evaluate/v1/{credential}/litellm` | Sealed credential in path | None | `allowed: false` produces a finding. |
| `promptguard` | `/api/v1/guard` | `X-API-Key` | None | `allow`, `block`, or `redact` with replacement messages. |
| `xecguard` | `/xecguard/v1/scan` | Bearer | `model` (default `xecguard_v2`), `policy_names` | `SAFE` or `UNSAFE`. This adapter uses the scan API. |
| `prompt_security` | `/api/protect` | `APP-ID` | `user`, `system_prompt` | Prompt/response action, with replacement on `modify`. |
| `qualifire` | `/api/evaluation/evaluate`; `/api/evaluation/invoke` for a saved evaluation | `X-Qualifire-API-Key` | **`evaluation_id`, or at least one check/assertion**; `prompt_injections`, `hallucinations_check`, `grounding_check`, `pii_check`, `content_moderation_check`, `assertions` | Any flagged evaluation result produces a finding. Match the endpoint to saved-evaluation or check-based configuration. |
| `ovalix` | `/tracking/custom_application/checkpoint` | Bearer | **`application_id`, `checkpoint_id`, `actor`** | `allow`, `block`, or `anonymize`; modified content supplies replacement. Use separate definitions for distinct input/output checkpoints. |
| `microsoft_purview` | `/users/{user-id}/dataSecurityAndGovernance/processContent` | Preissued Graph Bearer | **`application_id`, `application_name`** | A restrict-access policy action with `restrictionAction: block` produces a finding. |
| `akto` | `/api/http-proxy` | Raw `Authorization` value | **`account_id`, `vxlan_id`** | `data.guardrailsResult.Allowed`; fixed `akto_connector=litellm&guardrails=true` query, without asynchronous ingestion. |
| `deepkeep` | `/v3/openai/beta/litellm_basic_guardrail_api` | `X-API-Key` | **`firewall_id`** | `NONE`, `BLOCKED`, or `GUARDRAIL_INTERVENED`; optional returned texts supply replacement. |
| `qostodian_nexus` | `/beta/litellm_basic_guardrail_api` | `X-API-Key` | `source`, `container`, `identity` | Basic Guardrail API action and replacement. Adds native identifier headers and a generated trace identifier. |
| `rubrik` | Input: `/v1/before_prompt/openai/v1`; output: `/v1/after_completion/openai/v1` | Bearer | None | Input refusal completion produces a finding; changed output text produces a finding. Use separate definition revisions for the two endpoints. |
| `vigil_guard` | `/v1/guard/analyze` | Bearer | None | `ALLOWED`, `BLOCKED`, or `SANITIZED`; sanitization requires returned text. |
| `repelloai` | `/sdk/v1/analyze/prompt` or `/sdk/v1/analyze/response` | `X-API-Key` | **`asset_id`** | `passed`, `flagged`, or `blocked`; flagged/blocked produce findings. Select the endpoint for the phase. |
| `singulr` | `/api/v1/ai-gateway/litellm-v2` | `X-Singulr-Gateway-Token` | `application_id`, `guardrail_id` | `should_block`; optional parameters become native policy headers. |
| `headroom` | `/v1/compress` | Bearer | `model` | Returned compressed message supplies replacement. Input only. |
| `compresr` | `/api/compress/question-specific/` | `X-API-Key` | **`query`**; `compression_model_name` (default `latte_v2`), `target_compression_ratio` (default `0.5`), `coarse`, `dynamic` | `data.compressed_context` supplies replacement. Input only. |
| `straiker` | `/api/v1/detect/webhook` | Bearer and `X-Straiker-Webhook-Format: litellm` | `source` (default `olp`) | Schema v1 webhook: `NONE`, `BLOCKED`, or `GUARDRAIL_INTERVENED` with replacement. |
| `alice` | `/v2/evaluate/litellm` | `af-api-key` | None | `ALLOW`, `DETECT`, `BLOCK`, or `MASK`; mask requires indexed replacement text. |
| `conduct` | `/mcp` | Bearer; optional `X-Workspace-Id` | `workspace_id`, `model`, `provider` | JSON-RPC `tools/call` of `guard_check_prompt`; advisory/warning/block/pending-approval findings. Input and parallel input only. |
| `generic_guardrail_api` | `/beta/litellm_basic_guardrail_api` | `x-api-key` | None | Native Basic Guardrail API: `NONE`, `BLOCKED`, or `GUARDRAIL_INTERVENED`, with validated replacement text. |
| `openai_moderation` | `/v1/moderations` | Bearer | `model` | Exactly one moderation result with boolean `flagged`. |

The established adapters use top-level `configuration` fields rather than
`configuration.parameters`, and append their operation path to the configured
base URL:

| Type | Appended operation | Authentication | Configuration and behavior |
| --- | --- | --- | --- |
| `bedrock` | `/guardrail/{guardrail}/version/{version}/apply` | AWS Signature V4 using sealed `access_key_id`, `secret_access_key`, optional `session_token` | Required `region`, `guardrail`, `version`. `GUARDRAIL_INTERVENED` detects a violation and may supply replacement output. |
| `azure.prompt_shields` | `/contentsafety/text:shieldPrompt?api-version=2024-09-01` | `Ocp-Apim-Subscription-Key` | Detects attacks in user text or captured tool-result documents. |
| `azure.content_safety` | `/contentsafety/text:analyze?api-version=2024-09-01` | `Ocp-Apim-Subscription-Key` | `threshold` in `[0,7]`; any native category severity greater than the threshold produces a finding. |
| `model_armor` | `/v1/{template}:sanitizeUserPrompt` or `:sanitizeModelResponse` | Bearer | Required `template` resource name. Native filter match produces a finding. |
| `lakera` | `/v2/guard` | Bearer | A boolean `flagged` result. This type uses Lakera v2; `lakera_v1` selects the earlier API. |
| `presidio` | `/analyze`; optional separate `anonymizer_endpoint` base receives `/anonymize` | Bearer | Optional `language`, default `en`; native entity offsets become UTF-8 byte spans, with optional native anonymizer replacement. |

`webhook` uses the exact endpoint and the OLP request/verdict schema. Its sealed
credential contains `signing_key`; requests carry an `X-OLP-Signature` HMAC over
the timestamp and exact body bytes. The webhook protocol is distinct from the
native `generic_guardrail_api` protocol.

Rubrik's input endpoint intentionally returns no completion choices for an
allowed prompt; an empty JSON object is a valid native success response. Other
adapters require their documented verdict fields. Unknown actions, missing
required fields, invalid replacement types/counts, and malformed responses are
evaluation failures rather than implicit allows.

Headroom and Compresr perform stateless text compression. They do not persist
original tool results, inject retrieval tools, or run recovery loops. Their
replacement must fit the shared text bound. Agent-history pruning and retrieval
belong to the agent runtime rather than this per-request inspection API.

## Model and local integrations

Model-backed definitions use an authorized OLP route rather than a vendor URL
or guardrail credential. Requests use the caller's pinned release, identity,
deadlines, budgets, and accounting; evaluator usage is attributed as a child
request. Internal evaluator requests skip model guardrails to prevent recursion.

| Type | Configuration and behavior |
| --- | --- |
| `model` | `route` and `rule: {pointer, equals, category, prompt?}`. Evaluate the configured JSON Pointer equality against a generation, classification, or moderation route's result. Use this for an LLM-as-a-judge policy. |
| `decision_model` | The same explicit, accounted model-result rule. Configure the chosen route and response rule; it does not execute a hidden vendor model call. |
| `semantic_guard` | `route` naming an embeddings route and `semantic: {category, similarity_threshold, utterances}`. A cosine similarity at or above the threshold produces a finding. |
| `builtin.secrets` | Local equivalent of the `hide-secrets` inventory entry; use documented secret categories and optional patterns/replacement. |
| `builtin.keywords` and `builtin.regex` | Local content-filter policies, including the `litellm_content_filter` inventory use case. |
| `builtin.tool_permission` | Ordered `rules` with `id`, `tool_name?`, `tool_type?`, `decision: allow\|deny`, and optional `allowed_param_patterns`; `default_action` defaults to `deny`. Checks captured function-tool definitions and calls without executing them. Patterns are full-match RE2; parameter paths use dotted names and `[]` arrays. |
| `builtin.code_execution` | `blocked_languages`, `confidence_threshold`, `detect_execution_intent`. Detects fenced code and execution requests without running code. Supports local blocking, annotation, or replacement. |
| `plugin` | `plugin_id` and immutable SHA-256 `plugin_version`. Runs an approved, confined WASM guardrail. This is the supported custom-code boundary; arbitrary Python is not loaded in the gateway. |

A semantic guard embeds all inspected segments and reference utterances in one
accounted request, so a retry or failover cannot mix vector spaces. It accepts
1–64 reference utterances, each at most 8,192 bytes and together at most 64 KiB;
the threshold must be greater than zero and at most one. Each evaluation permits
at most 128 segments and 4,096 vector dimensions. Results must include every
unique index, consistent dimensions, finite values, and nonzero vector norms.

## Failure handling and limits

External requests share the provider egress rules, pooled connections, pinned
credential material, and definition deadline. The definition timeout is
1–300,000 milliseconds, capped by the remaining request deadline. Service errors,
timeouts, invalid JSON, and invalid native verdicts follow the configured
`fail_open` or `fail_closed` policy. Five consecutive failures open a
per-definition-revision circuit for 30 seconds, followed by one probe.

Conduct identity denials are policy findings: HTTP 401/403 and native
`identity_required` replies are not converted to availability failures by a
`fail_open` setting. The policy entry still controls whether findings block or
are monitored. Missing inspection capability is refused independently of
availability policy.

An evaluation accepts at most 128 effective entries, 2,048 text segments, and
8 MiB of text. HTTP responses are bounded to 1 MiB. Replacement text must be
valid UTF-8 and at most 4,096 bytes; the adapter supplies a validated full-segment
span. Redaction/masking requires a transformed route. Masking is synchronous
input only; parallel input cannot redact. Complete-message external checks may
require unary delivery instead of streaming.

OLP does not persist submitted text, provider response bodies, or replacement
text as guardrail evidence. Evidence contains bounded identifiers, categories,
actions, outcomes, and latency. External services receive the inspected content
and apply their own retention policies. These external adapters are not eligible
as local capture-redaction detectors.

The [pinned integration inventory](../develop/plans/guardrails.md#vendor-inventory)
records implementation scope and separate live-vendor qualification. MCP
identity/authorization hooks and complete tool-history pruning require their
own MCP or agent-runtime context; they are not exposed as pretend text scanners.
