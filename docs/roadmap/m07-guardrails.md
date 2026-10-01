# M7: Guardrails platform

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M1](m01-measured-advantage.md) | [M3](m03-routing-resilience.md) (shadow traffic), [M4](m04-tenancy-identity.md) (organization scope), [M5](m05-observability.md) (capture redaction, decision export), [M9](m09-api-surface.md) (batch files) | [M8](m08-caching.md), [M10](m10-agent-gateway.md) |

OLP's [content policy](../gateway.md#content-policy) is a deterministic RE2
filter: block or redact rules on input and unary output, with metadata-only
evidence. LiteLLM offers a guardrail framework with about 50 vendor
integrations, PII masking, secret detection, tool policies and per-key
attachment. This milestone generalizes content policy into one guardrail engine
that keeps OLP's properties (in-process by default, bounded, fidelity-aware and
content-free in its evidence) and reaches the long tail through a signed
webhook contract and confined WebAssembly guardrails rather than in-process
code.

## Outcome

- One engine evaluates every guardrail, at every attachment scope, in every
  phase including streaming output.
- Built-in detectors cover PII and secrets with no external service.
- Vendor guardrails, model-based guardrails, webhook guardrails and plugin
  guardrails share one contract and one evidence format.
- Tool calls are governed by allowlists and argument schemas.
- Applications can call guardrails directly.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Rules | Up to 64 RE2 `block` or `redact` rules per route revision ([content policy](../gateway.md#content-policy)) | [Guardrails](https://docs.litellm.ai/docs/proxy/guardrails/quick_start) with pre-call, during-call, post-call and logging-only modes |
| Phases | Input; output on unary responses only | Including streaming for some guardrails |
| Surfaces | Canonical request and response text; surfaces that cannot be inspected answer `422 content_policy_surface_unavailable` | Also [batch files](https://docs.litellm.ai/docs/proxy/guardrails/batch_guardrails), [realtime turns](https://docs.litellm.ai/docs/proxy/guardrails/realtime_guardrails) and pass-through endpoints |
| Scope | Route revision | Global, key, team and [policies](https://docs.litellm.ai/docs/proxy/guardrails/guardrail_policies) |
| Detectors | Operator patterns | [PII masking](https://docs.litellm.ai/docs/proxy/guardrails/pii_masking_v2), [secret detection](https://docs.litellm.ai/docs/proxy/guardrails/secret_detection), [prompt injection](https://docs.litellm.ai/docs/proxy/guardrails/prompt_injection), [vendors](https://docs.litellm.ai/docs/guardrail_providers) |
| Evidence | `{rule_id, phase, action, outcome}` only | Guardrail traces in logs |

## Scope

### M7.1 Guardrail engine

- **Resources.** A guardrail is a named, project-scoped definition with a type,
  configuration and failure policy. A guardrail policy is an ordered list of
  guardrails, each with its phases, action and mode. Both are versioned with
  drafts and immutable revisions, and publish into the runtime snapshot so that
  every request pins the policy revisions in effect.
- **Attachment.** Policies attach to the installation, a project, a route and a
  key, and to an organization once
  [M4.5](m04-tenancy-identity.md#m45-enterprise-identity) ships. All attached
  policies apply, broadest first, and a guardrail marked `mandatory` cannot be
  detached at a narrower scope.
- **Content policy.** Today's route `content_policy` becomes a guardrail of type
  `builtin.regex` with the same bounds and semantics. Configuration export and
  apply move to the new shape in one release, as 0.x permits.
- **Starter policies.** The console offers reviewed starting points, such as
  PII protection and secret blocking, that create ordinary policy drafts.

| Phase | Behavior |
| --- | --- |
| `input` | Before token estimation and dispatch; may block or transform |
| `input_parallel` | Concurrently with the upstream call; a violation cancels the attempt before commitment, or ends a committed stream in-band |
| `output` | After a unary response is decoded and accounted, before it is written |
| `output_stream` | Over a streamed response through a holdback window of configured size; text is released only after inspection, and a violation ends the stream with an in-band error |

| Action | Contract |
| --- | --- |
| `block` | Refuse with `400 guardrail_blocked`, or end a stream in-band |
| `redact` | Replace matched spans; transformed routes only |
| `mask` | Replace entities with placeholders before dispatch and restore them in the response; transformed routes only, with the mapping held in request memory |
| `annotate` | Record the decision and continue |

- **Mode.** Each guardrail in a policy runs in `enforce` or `monitor` mode. In
  `monitor` mode it evaluates in its phase and records the decision it would
  have made, and its action is never applied.
- **Fidelity.** Strict routes accept `block` and `annotate` in every phase, and
  any guardrail in `monitor` mode, because none of them change a successful
  native invocation. `redact` and `mask` require transformed routes, as
  `redact` does today.
- **Masking.** Placeholders are derived deterministically from the request, so
  identical requests mask identically. On a streamed response the
  `output_stream` holdback window restores placeholders before text is
  released, so a placeholder split across chunks never reaches the caller.
- **Failure policy.** Each guardrail declares a timeout, which cannot exceed the
  route deadline, and `on_error: fail_closed | fail_open`. External guardrails
  have their own circuit breakers.
- **Surfaces.** Guardrails inspect the same canonical text as content policy,
  plus tool-call arguments and tool results. Batch input files are inspected
  line by line at upload, the one moment the gateway holds their content, and
  realtime sessions are inspected per turn at their transcript events.
  A provider-generated audio transcript may arrive after generation starts,
  so it cannot prove pre-response input enforcement. Audio with an enforcing
  input guardrail is refused with `422 guardrail_surface_unavailable` unless
  a certified, bounded pre-dispatch transcription path inspects the complete
  turn before any audio is sent upstream. A post-dispatch transcript alone
  never qualifies that path; no new transcription service is included here.
  Cloud-bucket batches in [M9.2](m09-api-surface.md#m92-files-and-batches-across-providers)
  bypass upload inspection, so they are rejected before submission whenever
  an input guardrail applies.
  Surfaces that cannot be inspected keep answering
  `422 content_policy_surface_unavailable`, renamed
  `guardrail_surface_unavailable`.
- **Evidence.** Each decision records `{guardrail_id, revision, phase, mode,
  action, outcome, categories, latency_ms}` on the request and attempt records
  and in the `guardrail_decisions` export stream. Matched text, offsets,
  patterns and replacements are never recorded. Keys may opt into an
  `X-OLP-Guardrails` response header listing the guardrails that acted.

### M7.2 Built-in detectors

Deterministic, linear-time detectors run in process:

- **PII:** email addresses, phone numbers in E.164 and common national formats,
  payment card numbers with Luhn validation, IBANs with checksum validation,
  US Social Security numbers, IP addresses, and operator-supplied entity
  patterns.
- **Secrets:** private-key PEM blocks, JSON Web Tokens, and the documented
  formats of AWS, Google Cloud, Azure, GitHub, GitLab, Slack, Stripe, OpenAI
  and Anthropic credentials, with checksum validation where the format defines
  one.
- **Keywords:** case-folded word lists compiled into a single automaton.
- **Schema:** JSON Schema validation of complete structured outputs and tool
  arguments, within the configured response or argument size cap. Full-document
  schema checks use the unary `output` phase or complete tool arguments;
  `output_stream` is unsupported because bounded holdback cannot prove required
  properties or cross-field constraints. A streaming request with a required
  output schema policy is refused before dispatch, never partly released.

Detectors report categories (`pii.email`, `secret.aws_access_key`), never
values.

### M7.3 Webhook guardrails

A `webhook` guardrail sends the inspected segments to an operator endpoint and
applies the verdict:

```json
{
  "request": {
    "guardrail": "acme-dlp",
    "phase": "input",
    "request_id": "0199…",
    "route": "assistant",
    "segments": [{ "id": "m0", "role": "user", "text": "…" }]
  },
  "response": {
    "action": "redact",
    "categories": ["pii.account_number"],
    "redactions": [{ "segment": "m0", "start": 12, "end": 28, "replacement": "[ACCOUNT]" }]
  }
}
```

Requests are signed with `X-OLP-Signature`, pass the egress policy, carry a
bounded body and timeout, and may use mutual TLS through the provider network
credential mechanism. Because content leaves OLP, a webhook guardrail requires
an explicit `content_egress: true` acknowledgement, recorded in audit and shown
in capabilities.

### M7.4 Vendor and model guardrails

- **Vendor adapters:** Amazon Bedrock Guardrails (`ApplyGuardrail`), Azure AI
  Content Safety and Prompt Shields, Google Cloud Model Armor, Lakera Guard,
  and Microsoft Presidio for self-hosted analysis and anonymization. Each
  adapter uses the webhook guardrail's egress, credential and failure rules,
  with credentials sealed under a new `guardrail_credential` purpose.
- **Model guardrails:** a `model` guardrail sends segments to an OLP route
  (moderation, classification or generation) and maps its result to a verdict
  through a declared rule. This serves OpenAI moderation, Llama Guard,
  ShieldGemma and judge prompts through providers already certified in OLP,
  with their usage accounted as attempts attributed to the guardrail.
- **Prompt injection.** Detection is delivered by these adapters and model
  guardrails (Prompt Shields, Lakera Guard, a classifier or judge route), over
  request text and tool results. No bundled heuristic claims to detect it.

### M7.5 Plugin guardrails

The [plugin ABI](../plugin-authoring.md#abi-reference) gains a `guardrail`
method: an entry point that receives segments and returns a verdict in the
webhook contract's shape. It runs on wazero within the existing time and memory
limits, reaches only its approved origins, and is installed, reviewed and
approved like any plugin. Custom guardrails therefore never run inside the
gateway process.

### M7.6 Tool governance

A `builtin.tools` guardrail governs tools in requests and responses:

- allow and deny lists of tool names, with glob patterns, per scope;
- JSON Schema constraints on tool-call arguments returned by the model;
- a trust rule that refuses to send output from tools marked `untrusted` into
  tools marked `privileged` within one request.

[M10](m10-agent-gateway.md) applies the same guardrail to MCP tool calls.

### M7.7 Apply endpoint

`POST /v1/guardrails/apply` evaluates a named guardrail policy against supplied
text, for applications that guard content outside a model call. It requires a
new API key scope, `guardrails`, records a request with operation `guardrail`,
and is subject to the key's limits. Its path follows the roadmap's
[endpoint decision](README.md#cross-milestone-decisions). The console offers
the same evaluation as a guardrail playground that stores nothing.

## Non-goals

- In-process custom guardrail code. Custom logic runs behind the webhook
  contract or as a confined plugin.
- Machine-learning classifiers bundled in the gateway binary. Model-based
  detection goes through certified routes or vendor adapters.
- Recording matched text, offsets or replacements anywhere.
- Guardrails on pass-through routes, which carry no interaction contract
  ([M9.9](m09-api-surface.md#m99-governed-pass-through)).
- One adapter per guardrail vendor. The vendor row closes under the
  [breadth rule](parity.md#how-to-read-the-matrix).

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Guardrails and policies, including patterns and word lists | Drafts and immutable revisions in PostgreSQL; the runtime snapshot | As route revisions today | None |
| Decisions (`guardrail_id`, phase, mode, action, outcome, categories, latency) | Request and attempt records in PostgreSQL | Request retention | None; metadata only |
| Vendor and webhook guardrail credentials | PostgreSQL, sealed | Until rotated | New seal purpose `guardrail_credential` |
| `mask` mappings and inspected segments | Request memory | Never stored | None |
| Content sent to webhook, vendor and model guardrails | The operator-declared destination | Outside OLP | Requires the `content_egress` acknowledgement |

## Change map

| Change | Start here |
| --- | --- |
| Engine, detectors, policies | `internal/contentpolicy/` becomes `internal/guardrails/` |
| Phases in the request path | `internal/gateway/server.go`, `internal/gateway/policy.go`, `internal/gateway/executor.go` |
| Fidelity rules | `internal/runtime/fidelity.go` |
| Webhook and vendor adapters | `internal/guardrails/`, `internal/egress/` |
| Plugin method | `sdk/plugin/abi/`, `internal/plugins/` |
| Console | new `console/src/lib/features/guardrails/` |

## Decisions to settle

1. Holdback window units for streaming output: bytes or estimated tokens
   (recommended: bytes, which bound memory exactly).
2. Whether `mask` placeholders are stable across a conversation (recommended:
   no; placeholders are deterministic per request, which keeps the mapping in
   request memory and still lets identical requests share a cache entry in
   [M8](m08-caching.md)).
3. The vendor adapter set beyond those above, which the webhook contract and
   plugins otherwise cover.

## Exit criteria

- [ ] **M7.1** Existing content-policy fixtures pass unchanged as
      `builtin.regex` guardrails.
- [ ] **M7.1** Every supported phase/action combination has unary or streaming
      tests as applicable, including
      `input_parallel` cancellation before and after commitment, and `monitor`
      mode records a decision without changing any response.
- [ ] **M7.1** Policies attached at every available scope apply broadest first,
      and a `mandatory` guardrail cannot be detached at a narrower scope.
      Organization scope joins this test when M4.5 ships; before then the
      other four scopes are tested.
- [ ] **M7.1** Strict routes refuse `redact` and `mask` at validation,
      activation and configuration plan.
- [ ] **M7.1** A violating line in a batch input file is refused at upload, and
      a violating inspectable realtime text turn ends before the model
      responds. Enforcing input policies refuse audio sessions without the
      certified pre-dispatch inspection path, including a fake provider that
      emits audio before its transcript. Once M9.2
      ships, cloud-bucket batches with an input guardrail are refused before
      provider submission with `422 guardrail_surface_unavailable`.
- [ ] **M7.2** Built-in detectors pass a labeled corpus with no false negatives
      on checksum-validated formats.
- [ ] **M7.2** Full-document schema checks reject invalid unary outputs and
      complete tool arguments; unsupported streaming schema configurations and
      requests are refused before any response bytes are released.
- [ ] **M7.3, M7.4** Each vendor adapter and the webhook contract pass
      integration tests against local fakes, including timeouts under both
      failure policies.
- [ ] **M7.5** A WebAssembly guardrail built with the SDK runs in the
      conformance suite.
- [ ] **M7.6** A denied tool, an argument that violates its schema, and
      untrusted output flowing into a privileged tool are each refused.
- [ ] **M7.7** The apply endpoint returns the same verdict as the same policy
      on a route, requires the `guardrails` scope and counts against the key's
      limits.
- [ ] **M7.1** Evidence contains no matched text in any record, export or log.
- [ ] **M7.2** Built-in detectors on 100K-token inputs stay within the
      [performance budget](m01-measured-advantage.md#performance-budget) for
      scenario S3.
- [ ] The [parity matrix](parity.md) guardrail rows are `Parity` or better,
      with the vendor row closed under the
      [breadth rule](parity.md#how-to-read-the-matrix).
