# M7: Guardrails platform

| Status | Depends on | Unlocks |
| --- | --- | --- |
| In progress | [M1](m01-measured-advantage.md) | [M8](m08-caching.md), [M10](m10-agent-gateway.md), redaction for [M5.2](m05-observability.md#m52-payload-capture) |

[Content policy](../gateway.md#content-policy) and [named regex definitions](../guardrails.md)
already provide bounded input/unary block/redact with metadata-only evidence.
This milestone adds broader attachments, streaming detectors, PII/secrets/tool
policies, vendor integrations, signed webhooks and confined WASM guardrails.
The execution platform is now present in code; remaining work is release
qualification and explicitly bounded vendor coverage. Registrations alone do
not establish execution support. [Guardrails](../guardrails.md) is the operator
contract; the decisions below are binding on remaining work.

## Outcome

- One engine composes guardrails at every attachment scope and admits only
  phases and surfaces on which each configured action can be enforced.
- Built-in detectors cover PII and secrets with no external service.
- Vendor guardrails, model-based guardrails, webhook guardrails and plugin
  guardrails share one contract and one evidence format.
- Tool calls are governed by allowlists and argument schemas.
- Applications can call guardrails directly.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| | Planning baseline | LiteLLM reference |
| --- | --- | --- |
| Rules | Up to 64 RE2 `block` or `redact` rules per route revision ([content policy](../gateway.md#content-policy)) | [Guardrails](https://docs.litellm.ai/docs/proxy/guardrails/quick_start) with pre-call, during-call, post-call and logging-only modes |
| Phases | Input; output on unary responses only | Including streaming for some guardrails |
| Scope | Route revision | Global, key, team and [policies](https://docs.litellm.ai/docs/proxy/guardrails/guardrail_policies) |
| Detectors | Operator patterns | [PII masking](https://docs.litellm.ai/docs/proxy/guardrails/pii_masking_v2), [secret detection](https://docs.litellm.ai/docs/proxy/guardrails/secret_detection), [vendors](https://docs.litellm.ai/docs/guardrail_providers) |
| Evidence | `{rule_id, phase, action, outcome}` only | Guardrail traces in logs |

## Scope

### M7.1 Guardrail engine

- **Resources.** A guardrail is a named, project-scoped definition with a type,
  configuration and failure policy. A guardrail policy is an ordered list of
  guardrails with phases and actions. Both are versioned with drafts and
  immutable revisions, and publish into the runtime snapshot. Each request pins
  the complete release and its ordered effective set of policy and definition
  revisions, including during retries and retained-resource follow-ups.
- **Attachment.** Policies attach to the installation, an organization, a
  project, a route and a key. All attached policies apply, broadest first, and a
  guardrail marked `mandatory` cannot be detached at a narrower scope.
- **Content policy.** Today's route `content_policy` becomes a guardrail of type
  `builtin.regex` with the same bounds and semantics. Configuration export and
  apply use configuration v2. Version 1 imports are explicitly normalized; mixed
  old and new shapes are rejected. Historical rows remain immutable.

| Phase | Behavior |
| --- | --- |
| `input` | Inspect effective provider input after defaults and rewriting, before token estimation and dispatch; may block or transform |
| `input_parallel` | Concurrent with upstream work; may block or annotate, never change dispatched input. A violation cancels the attempt before commitment or ends a committed stream in-band; observed usage remains billable |
| `output` | After a unary response is decoded and accounted, before it is written |
| `output_stream` | Over a streamed response through a holdback window of configured size; text is released only after inspection, and a violation ends the stream with an in-band error |

`monitor: true` is an entry flag on any phase, not a fifth phase. It records
results without acting. Ordered transformations feed the next entry; the first
enforced refusal stops evaluation.

| Action | Contract |
| --- | --- |
| `block` | Refuse with `400 guardrail_blocked`, or end a stream in-band |
| `redact` | Replace matched spans; transformed routes only |
| `mask` | Synchronous `input` only on transformed routes; replace entities with randomized placeholders, reuse one request-local map across retries, restore before output inspection |
| `annotate` | Record the decision and continue |

- **Fidelity.** Strict routes permit enforcing `block` and `annotate`, and
  evaluation-only entries, only on inspectable surfaces. Enforcing `redact` and
  `mask` require transformed routes; successful strict response frames retain
  their bytes. Detectors must supply valid spans to transform content.
- **Failure policy.** Each guardrail declares a timeout, which cannot exceed the
  route deadline, and `on_error: fail_closed | fail_open`. External guardrails
  have per-revision circuit breakers: five failures open the circuit, then one
  probe is allowed after 30 seconds. Missing capability is a pre-dispatch 422,
  not a service failure that `fail_open` can silently excuse. Timeouts, invalid
  verdicts and service failures record `failed_open` or `failed_closed`; an
  enforcing fail-closed result uses `400 guardrail_blocked`. Monitoring never
  blocks.
- **Surfaces.** Guardrails inspect the same canonical text as content policy,
  plus tool-call arguments and tool results. Surfaces that cannot be inspected
  return `422 guardrail_surface_unavailable` for enforcing actions; evaluation-
  only entries record unavailable coverage. Historical content-policy codes
  remain decodable. Stored/background Responses refuse output enforcement or
  input masking; opaque media and realtime require their own inspection
  contracts. Input-only policies continue to apply to retained continuations.
- **Evidence.** Each decision records `{guardrail_id, revision, phase, action,
  outcome, categories, latency_ms}` on the request and attempt records and in
  the `guardrail_decisions` export stream. Matched text, offsets, patterns and
  replacements are never recorded. `X-OLP-Guardrails: true` requests bounded
  response metadata listing acting guardrails; late decisions use trailers.
  Header truncation never truncates enforcement.

### M7.2 Built-in detectors

Bounded deterministic detectors run in process; pattern scanning uses RE2 and
a compiled keyword automaton:

- **PII:** email addresses, phone numbers in E.164 and common national formats,
  payment card numbers with Luhn validation, IBANs with checksum validation,
  US Social Security numbers, IP addresses, and operator-supplied entity
  patterns.
- **Secrets:** private-key PEM blocks, JSON Web Tokens, and the documented
  formats of AWS, Google Cloud, Azure, GitHub, GitLab, Slack, Stripe, OpenAI
  and Anthropic credentials, with checksum validation where the format defines
  one.
- **Keywords:** case-folded word lists compiled into a single automaton.
- **Schema:** JSON Schema validation of structured outputs and tool arguments.

Detectors report categories (`pii.email`, `secret.aws_access_key`), never
values. Checksums validate a matched format; they do not establish that a
credential is live. JWT detection recognizes structure, not signature validity.
The labelled corpus is the coverage contract, not a guarantee of detecting all
PII or unprefixed secrets. JSON Schema cannot load remote references.

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
in capabilities. Redaction offsets are UTF-8 byte offsets into the exact
submitted segment. Unknown segments, overlapping/out-of-bounds spans, invalid
UTF-8 boundaries and malformed verdicts follow `on_error`. The signature is
`t=<unix-seconds>,v1=<HMAC-SHA256>` over timestamp, a dot, and exact body bytes;
receiver replay checks are required. Credentials and signing keys are never
exported with portable configuration.

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
  with usage in child requests attributed to the parent and guardrail. Evaluator
  calls use the caller's route authorization, end-user identity, deadline and
  budgets. They resolve from the pinned release and skip model guardrails to
  prevent recursion. Keyless playground evaluation reserves installation,
  organization and project budgets and refuses unaccountable evaluator work.

### M7.5 Plugin guardrails

The [plugin ABI](../plugin-authoring.md#abi-reference) gains a `guardrail`
capability: a function that receives segments and returns a verdict in the
webhook contract's shape. It runs on wazero within the existing time and memory
limits, with no HTTP capability in the guardrail call. Modules are installed,
reviewed, approved and preloaded with the release. They run as confined WASM
inside the host, never as arbitrary native extension code; diagnostics from
content-bearing evaluations are suppressed.

### M7.6 Tool governance

A `builtin.tools` guardrail governs tools in requests and responses:

- allow and deny lists of tool names, with glob patterns, per scope; deny wins;
- JSON Schema constraints on tool-call arguments returned by the model;
- a trust rule that refuses to send output from tools marked `untrusted` into
  tools marked `privileged` within one request. Unattributed tool results are
  handled conservatively. This is request-local provenance enforcement, not
  cross-request taint tracking or permission to execute tools.

[M10](m10-agent-gateway.md) applies the same guardrail to MCP tool calls.

### M7.7 Apply endpoint

`POST /v1/guardrails/apply` evaluates a named guardrail policy against supplied
text, for applications that guard content outside a model call. It requires a
new API key scope, `guardrails`, records a request with operation `guardrail`,
and is subject to the key's limits and project visibility. The input names an
immutable `policy_revision_id`, phase and bounded segments; a completed policy
refusal returns a successful evaluation with `blocked: true`, while malformed
input, authentication and infrastructure errors retain their normal error
status. The console playground uses the same engine and stores no entered
content; external policies still send content to their declared services.

### M7.8 Capture redaction integration

[M5.2](m05-observability.md#m52-payload-capture) still rejects nonempty `redact`
configuration in `internal/export/capture_policy.go`. Serving-time guardrails do
not establish that captured input, output or tool arguments were redacted. This
adapter is an explicit remaining deliverable, owned jointly by export and
guardrail maintainers.

Resolve each capture policy's named redactors to immutable local definition
revisions at publication. The first adapter accepts span-producing built-in
regex, PII, secret and keyword detectors only; external/model/plugin evaluation
and masking/restoration are excluded. Inspect each complete selected capture
field in bounded request memory before serialization and enqueue. Captured
original input requires its own pass even when serving input was transformed;
output and tool arguments use the corresponding capture surfaces.

Capture redaction always fails closed for the **capture**: malformed spans,
timeout, unavailable surface, incomplete stream or exhausted memory drops the
whole capture, increments a content-free failure counter and records no payload.
It does not alter an already accepted inference result or fail open by sending
raw content. Never enqueue original bytes alongside a redacted copy, and never
use PostgreSQL/Valkey for intermediate or retry storage. Preserve M5's explicit
owner opt-in, sink restrictions and queue/drop semantics. Nonempty `redact`
remains rejected until publication validation, unary/stream/tool fixtures and
sink readback prove this contract.

## Change map

| Change | Start here |
| --- | --- |
| Engine, detectors, policies and retained legacy adapter | `internal/guardrails/`, `internal/contentpolicy/`, `internal/routing/guardrails.go` |
| Phases in the request path | `internal/gateway/policy.go`, `internal/gateway/executor.go` |
| Webhook and vendor adapters | `internal/guardrails/`, `internal/egress/` |
| Plugin capability | `sdk/plugin/abi/`, `internal/plugins/` |
| Console | `console/src/lib/features/guardrails/` |
| Capture-specific redaction adapter (remaining) | `internal/export/capture_policy.go`, `internal/gateway/events.go` |

## Binding bounds and qualification order

The engine allows at most 128 effective entries, 2,048 segments, 8 MiB of text,
4,096 findings and 256 KiB of evidence. Definition timeouts are 1–300,000 ms
and cannot exceed the route deadline. Stream holdback defaults to 65,536 bytes
and is capped at 1,048,576 bytes; UTF-8 and split-chunk matches are preserved.
Whole-result schema checks, unbounded regex requirements and enforcing external
evaluators require unary delivery when they cannot fit that contract. Tool
arguments are withheld until complete and validated within the same byte bound.

| Order | Owner and remaining deliverable | Acceptance artifact |
| --- | --- | --- |
| 1 | Guardrail/runtime maintainers: preserve the engine, migration, scope ordering and immutable-release contracts | Existing unit, integration and configuration fixtures; no regression in historical revisions |
| 2 | Gateway maintainers: qualify the surface/action matrix, cancellation, accounting, logprobs and retained-response exclusions | Unary/stream protocol corpus, failure injection and authorization/isolation sweeps |
| 3 | Connector/plugin maintainers: qualify each of six named adapters and WASM invocation | Local-fake contracts plus opt-in live-service smoke evidence by adapter and version |
| 4 | Export/guardrail maintainers: add M7.8 capture redaction without durable payload staging | Publication refusals and redacted sink readback across unary/stream/tool fixtures |
| 5 | Performance maintainers: run the built-in and streaming workload on M1 reference hardware | S3 latency, throughput, allocations and memory artifacts with no configured guardrails as control |
| 6 | Guardrail/connector maintainers: freeze the pinned reference vendor inventory and reconcile each entry | Name/version, adapter status, supported actions, local/live evidence and any specifically approved exclusion; aggregate row stays `Partial` while gaps remain |

The six built-in external adapters are Bedrock, Azure Content Safety, Azure
Prompt Shields, Model Armor, Lakera and Presidio. Webhooks and WASM permit custom
integrations but do not count as qualification of unnamed vendors. Broader
vendor parity remains `Partial` unless a separately reviewed catalog expansion
closes it; this milestone does not manufacture parity by renaming the subset.
The guardrail maintainer owns that inventory and a follow-on adapter backlog.
Every reference vendor starts as unsupported until its named adapter and live
contract evidence exist. New adapters reuse M7.3's existing credential, egress,
verdict and failure contracts; no new in-process extension model is permitted.
An explicit exclusion must identify the vendor and rationale in the parity
record; a generic webhook option is not an exclusion or proof of support.
M8 depends on the implemented pinning, input enforcement and response contracts,
not completion of M7's capture, live-service or performance qualification. M10 depends
on tool governance and must add enforcement at its own execution boundary.

## Exit criteria

- [x] Existing content-policy fixtures pass unchanged as `builtin.regex`
      guardrails.
- [x] Supported phase/action combinations have unary and streaming fixtures;
      unsupported combinations refuse explicitly, including parallel
      transformation and whole-result streaming enforcement.
- [x] Strict routes refuse `redact` and `mask` at validation, activation and
      configuration plan.
- [x] Built-in detectors pass the labelled positive/negative corpus, including
      checksum-validated formats; this is bounded corpus coverage.
- [x] Each vendor adapter and the webhook contract pass integration tests
      against local fakes, including timeouts under both failure policies.
- [x] A WebAssembly guardrail built with the SDK runs in the conformance suite.
- [x] Evidence contains no matched text in any record, export or log.
- [ ] Capture redaction resolves immutable local detectors, redacts before
      enqueue and drops incomplete/failed captures without raw-content
      fallback; unary, split-stream, tool and sink-readback fixtures prove it.
- [ ] Built-in detectors on 100K-token inputs stay within the
      [performance budget](m01-measured-advantage.md#performance-budget) for
      scenario S3.
- [ ] Each named external adapter has recorded live-service qualification,
      including authentication, supported actions and deadline behavior; local
      fakes alone do not check this box.
- [ ] Release review records a pinned, enumerated reference vendor inventory,
      the remaining vendor `Partial` row, named support/exclusion decisions and
      the owned adapter backlog, with all guardrail claims matched to evidence.
      Overall LiteLLM parity remains open while that row is `Partial`.

## Implementation and qualification notes

Checked items above record existing focused implementation evidence, not a new
qualification run during roadmap review. The code anchors include
`internal/guardrails/engine_test.go`, `registry_test.go`, `external_test.go`,
`internal/gateway/guardrails*_test.go` and configuration v2 fixtures. Unchecked
items above remain release work; the status stays **In progress**.

The platform implementation now includes immutable definitions and policies,
additive attachments, snapshot compilation, built-in detectors, signed webhooks,
vendor adapters, model evaluator calls, confined WASM support, standalone
evaluation, console editing and content-free evidence. Legacy route policies
are retained and converted without mutating their historical rows.

The holdback unit is bytes; masking maps are request-local. Enforcement that
requires a complete unary response is refused with 422 when streaming is requested.
External adapters have local-fake contract coverage. Reference-hardware performance
and live-vendor qualification remain separate release gates; this milestone is
not marked implemented until its exit checklist has been reviewed.

The local PII microbenchmark over 540 KB of ordinary prompt text measured
0.36–0.39 ms/op and 480 B/6 allocations. The unconfigured engine path measured
16–17 ns/op with zero allocations. These are development-machine measurements,
not an S3 load qualification. The broader vendor parity row remains `Partial`
because the implemented set is deliberately narrower than the reference's set.

Local verification covers the full Go suite, the management authorization and
isolation sweeps, legacy policy fixtures, configuration v2 migration/promotion,
the generated apply client, WASM conformance and focused race tests. Console
checks cover 1,316 unit/component assertions plus a Chromium keyboard and Axe
accessibility journey across all four guardrail tabs. Contract regeneration is
deterministic. These checks use local fakes, not live provider accounts.

After replacing per-character Unicode allocations with a bounded source-offset
buffer, the local streaming keyword benchmark processed 486 KB across 100
frames in about 24 ms (20 MB/s), with 1.52 MB allocated across the entire run.
Successful strict frames remain byte-identical. S3 throughput and latency still
need reference-hardware qualification.
