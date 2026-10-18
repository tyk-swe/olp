# Guardrails

Guardrails are project-owned definitions with typed configuration, a timeout and
an error policy. An ordered policy pins immutable definition revisions. Both
resources have a draft revision and an explicitly activated revision. Editing a
draft does not change serving requests or existing attachments.

The console's **Guardrails** page manages definitions, policies, activation and
attachments. Its playground evaluates the same engine without storing entered
content. It requires a signed-in member with playground access and project
visibility. External evaluations still send content to their configured service.

## Definitions and policies

Create a definition with `POST /api/v1/guardrails`:

```json
{
  "name": "Private data",
  "project_id": "PROJECT_UUID",
  "type": "builtin.pii",
  "configuration": {"categories": ["pii.email", "pii.payment_card"]},
  "timeout_ms": 1000,
  "on_error": "fail_closed"
}
```

Activate its `revision_id` with
`POST /api/v1/guardrails/{guardrail_id}/activate`. Create a policy with
`POST /api/v1/guardrail-policies`:

```json
{
  "name": "Assistant safeguards",
  "project_id": "PROJECT_UUID",
  "entries": [{
    "guardrail_id": "GUARDRAIL_UUID",
    "revision_id": "GUARDRAIL_REVISION_UUID",
    "phase": "input",
    "action": "mask",
    "mandatory": true,
    "monitor": false
  }]
}
```

Activate the policy through
`POST /api/v1/guardrail-policies/{policy_id}/activate`. Referenced definition
revisions must have been activated. Old activated revisions remain usable;
activation never changes what an existing revision means. Writes use
`Idempotency-Key`; edits, activation and retirement require the observed
`If-Match`. Readers need project visibility, and changes require configuration
permission and project change authority. Attachment writes additionally use the scope's existing permission: `settings` for installation, `manage_organization` for an organization, `configure` for project/route, and `keys` for an API key.

`POST /api/v1/guardrail-attachments` attaches an activated policy revision:

```json
{
  "scope": "project",
  "scope_id": "PROJECT_UUID",
  "policy_revision_id": "POLICY_REVISION_UUID",
  "order": 0
}
```

Scopes run in installation, organization, project, route, then API-key order.
The installation identifier is `installation`; other identifiers are the
resource UUIDs. Attachment positions and policy entry order are preserved.
Attachments add safeguards. Removing a narrower attachment cannot remove an
inherited entry, including a mandatory entry. Project, route and key attachments
must reference policies in that project; organization policies must belong to a
project in that organization. All published definitions, policies, attachments
and plugin references live in the immutable runtime release. Requests pin that
release and perform no PostgreSQL guardrail-configuration lookup.

## Execution

| Phase | Behavior |
| --- | --- |
| `input` | Inspect the actual provider request after defaults, before estimating tokens or dispatching. May transform it. |
| `input_parallel` | Evaluate during the provider call; a refusal cancels upstream work and retains observed usage. |
| `output` | Inspect a complete unary result after observing provider usage. |
| `output_stream` | Inspect decoded native text through a bounded byte holdback, including matches across chunks. Tool arguments remain buffered until complete and governed. |

Actions are `block`, `redact`, `mask`, and `annotate`. `monitor: true` records
results without acting. Redaction and masking require transformed routes;
masking requires synchronous input. A request's placeholder map is kept only in
memory, reused across its attempts, and restored before inspecting output.
Strict routes retain successful native frames byte-for-byte.

The `guardrails.stream_window_bytes` installation setting controls retained
stream data (default 65,536 bytes, maximum 1,048,576). Expressions requiring
unbounded lookbehind or complete content, schema validation of a whole result,
and enforcing external evaluators require unary delivery when their requirements
cannot fit the window. Such requests return `422 guardrail_surface_unavailable`;
there is no silent reduction in enforcement. Opaque media and retained surfaces
without a complete inspection contract are refused in the same way.
Chat streams requesting `logprobs` are refused before dispatch when output
guardrails or input masking are active, including when provider defaults enable
them. Unexpected log-probability payloads are refused before their frame is sent.

Stored or background Responses requests are refused when an output policy or
input masking is configured, because later retrieval cannot replay that
inspection or restore the request's masking map. Input-only policies remain
active on requests that use a previous response or continuation, including
when its provider or route revision is historical. Migrated strict input rules
continue matching individual schema keys and scalar values.

Refusals use `400 guardrail_blocked`; refusals after stream commitment use the
native in-band error. The gateway joins parallel evaluators before sending a
successful terminal marker. Historical route content-policy error codes remain
decodable for compatibility.

## Built-in configuration

| Type | Configuration |
| --- | --- |
| `builtin.regex` | `rules: [{id, pattern, replacement?}]`; bounded RE2, literal replacements, at most 64 rules and 16 KiB of patterns. |
| `builtin.pii` | Optional `categories`, `patterns`, and `replacement`. Omitting categories selects all built-in PII categories. |
| `builtin.secrets` | Optional `categories`, `patterns`, and `replacement`. |
| `builtin.keywords` | `words`, optional `substrings`, and `replacement`; Unicode case folding and one compiled automaton. |
| `builtin.schema` | `schema`; JSON Schema with remote-reference loading disabled. |
| `builtin.tools` | `allow`, `deny`, `privileged`, `untrusted` glob lists, and `schemas` keyed by tool name. Deny wins over allow. |

PII categories are `pii.email`, `pii.phone`, `pii.payment_card`, `pii.iban`,
`pii.us_ssn`, `pii.ip_address`, and `pii.custom`. Payment cards use Luhn and IBANs
use their country length and mod-97 checksum. Tool-result provenance is retained
for output governance; unattributed tool results are treated conservatively when
privileged calls are requested.

Secret categories are `secret.private_key`, `secret.jwt`,
`secret.aws_access_key`, `secret.google_cloud`, `secret.azure`, `secret.github`,
`secret.gitlab`, `secret.slack`, `secret.stripe`, `secret.openai`, and
`secret.anthropic`. They identify documented token formats; random unprefixed
strings cannot be reliably classified as credentials. JWT detection recognizes
structure, and does not claim to authenticate the token's signature.

## External and model evaluation

`webhook`, `bedrock`, `azure.content_safety`, `azure.prompt_shields`,
`model_armor`, `lakera`, and `presidio` use the shared segment/verdict contract.
External configuration requires an `endpoint`, a `credential_ref`, and explicit
`content_egress: true`. The acknowledgement is audited. Requests use the provider
egress policy, pinned credential material, bounded bodies and response sizes,
per-definition deadlines, and independent circuit breakers. A circuit opens
after five failures and permits one probe after 30 seconds.

Credentials are created through `POST /api/v1/guardrail-credentials` and sealed
with the `guardrail_credential` purpose. The credential document can contain
`api_key`, `signing_key`, or AWS `access_key_id`, `secret_access_key`, and optional
`session_token`. Network credential references use the same project-owned
credential mechanism. The `network` object supports the provider network
settings, including mutual TLS and an approved proxy. Secrets never enter
configuration export.

Webhooks receive `{guardrail, phase, request_id, route, segments}` and return
`{action, categories, redactions}`. Redactions contain a segment ID, UTF-8 byte
`start` and `end`, and a literal replacement. The `X-OLP-Signature` header carries
`t=<unix-seconds>,v1=<HMAC-SHA256>` over the timestamp, a dot, and the exact request
body. Use a signing key of at least 32 bytes and reject replayed timestamps.
Overlapping spans, invalid boundaries, unknown segments and malformed verdicts
fail according to `on_error`; diagnostics do not include submitted content.

Bedrock additionally specifies `region`, `guardrail`, and `version`. Model Armor
specifies its `projects/.../locations/.../templates/...` resource in `template`.
Presidio accepts `language` (default `en`). Azure Content Safety accepts a severity
`threshold`. Contract references: [Bedrock ApplyGuardrail](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ApplyGuardrail.html),
[Azure Prompt Shields](https://learn.microsoft.com/en-us/rest/api/contentsafety/text-operations/shield-prompt?view=rest-contentsafety-2024-09-01),
[Azure Content Safety](https://learn.microsoft.com/en-us/rest/api/contentsafety/text-operations/analyze-text?view=rest-contentsafety-2024-09-01),
[Model Armor](https://docs.cloud.google.com/model-armor/sanitize-prompts-responses),
[Lakera](https://docs.lakera.ai/api-reference/lakera-api/guard/screen-content).

A `model` configuration specifies `route` and `rule: {pointer, equals, category,
prompt?}`. The evaluator uses a generation, classification or moderation route
under the caller's authorization, deadlines and budgets. Its usage is a child
request attributed to the parent and guardrail. Internal evaluator requests skip
model guardrails to prevent recursion.
Evaluator routes resolve from the request's full pinned release, including for
retained continuations. Evaluator usage preserves the caller's resolved end-user
identity. Keyless playground evaluations reserve installation, organization and
project budgets before dispatch and stop when cost accounting is unavailable.

Presidio may also specify `anonymizer_endpoint`; its `/anonymize` request uses the analyzer results and the same egress policy, credentials and deadline. Without it, analyzer offsets drive local replacement.

A `plugin` configuration pins the manifest name in `plugin_id` and the module digest in
`plugin_version`. Its manifest declares `guardrail: true`, and the optional SDK
`Guardrail` interface receives the same segments and returns the same verdict.
Only approved confined WASM modules run this method. Modules are preloaded at
release installation; the existing time and memory limits apply. Content-bearing
plugin diagnostics are suppressed.

## Apply API and evidence

`POST /v1/guardrails/apply` requires the `guardrails` API-key scope, project
visibility and the key's limits:

```json
{
  "policy_revision_id": "POLICY_REVISION_UUID",
  "phase": "input",
  "segments": [{"id": "text", "role": "user", "text": "Text to evaluate"}]
}
```

A successful evaluation returns `{blocked, segments, decisions}`, including
`blocked: true` for a policy refusal. The operation is recorded as `guardrail`.
The [OpenAPI contract](../openapi/guardrails.json) and generated Go and TypeScript
clients live under [sdk/guardrails](../sdk/guardrails/).

Evidence contains only identifiers, revisions, phase, action, outcome,
categories and latency. Request and attempt history, telemetry and the
`guardrail_decisions` export stream never contain matches, offsets, replacements
or patterns. Clients may request `X-OLP-Guardrails: true` metadata; late decisions
use the response trailer. Values are bounded and may list only the first acting
guardrails when the header limit is reached.
Each attempt records its own input and output decisions; request history keeps
the combined evidence across attempts.

## Migration and qualification

Configuration export writes `openllmproxy.dev/config/v2`. Version 1 import is
explicitly normalized into regex definitions, ordered policies and attachments.
Mixing old and new forms is rejected. Promotion resolves portable project,
policy and definition names plus credential references; local revision IDs and
secret values are not exported. Repeated unchanged promotion reuses revisions.

Migration retains historical route and definition revisions and makes one
immutable conversion for each historical route policy. Existing copied policies
keep their rule order, bounds and literal replacements. See the
[M7 qualification checklist](roadmap/m07-guardrails.md#exit-criteria) for the
remaining release gates, including reference-hardware and live-vendor checks.

## Capture and vendor qualification

[Payload capture](payload-capture.md) independently redacts complete selected
fields using pinned local detector revisions before enqueue. Serving-time
redaction does not establish that capture content has been inspected. External
adapters are not eligible for capture redaction. The [pinned integration
inventory](guardrail-vendor-coverage.md) records all 60 entries from the existing
LiteLLM v1.105.0 baseline, local evidence, unsupported entries and follow-up owners.
Aggregate vendor coverage remains `Partial`; live qualification is outstanding.
