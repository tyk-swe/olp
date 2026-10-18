# Payload capture and redaction

Capture requires installation-owner opt-in and an enabled, installation-owned
JSON, JSONL or OTLP sink. Project managers may manage their own project policies;
route-only policies require installation settings authority. Delivery is
best-effort from bounded memory. `payload_captured` means queued, not delivered.
Payloads never enter PostgreSQL, Valkey, durable export staging, logs or spans.

## Publish and refresh redactors

`redact` is an ordered array of up to 128 distinct guardrail definition names.
Creating a policy, or explicitly including `redact` in a PATCH, resolves each
name to its active immutable revision in the policy's project. A route-only
policy uses the route's project; an unassigned route cannot publish redactors.
Names are case-insensitive within that project. Missing, ambiguous, inactive,
retired and unsupported definitions are refused. Only `builtin.regex`,
`builtin.pii`, `builtin.secrets` and `builtin.keywords` are eligible. External,
model, webhook, plugin, schema, tool-governance and masking evaluation are excluded.

```json
{
  "project_id": "01900000-0000-7000-8000-000000000001",
  "sink": "01900000-0000-7000-8000-000000000002",
  "sample_ratio": "0.05",
  "include": ["input", "output", "tool_calls"],
  "redact": ["customer-pii", "credentials"],
  "max_bytes": 65536
}
```

The read-only `redactor_revisions` response lists each name, guardrail ID,
revision ID, revision number and detector type in execution order. It contains
no configuration or captured text. Subsequent detector activation, rename or
retirement does not change an existing pin. Unrelated PATCH requests must omit
`redact`; including the same names explicitly refreshes every pin. Sending `[]`
removes redaction. `null` is invalid. The console supports ordered selection,
pin visibility and **Refresh pinned revisions on save**.

Migration `0074_capture_redactors.sql` replaces the empty-list constraint and
adds ordered bindings with foreign keys to immutable revisions. Existing empty
policies retain their settings. Detector compilation happens when the policy
cache reloads, outside inference processing. Invalid or unavailable bindings
suppress capture; they never fall through to an unredacted policy.

## Capture behavior and bounds

Selected input is the prepared input available to capture, including serving
transformations. Capture applies its own independent inspection; serving
redaction is not proof of capture redaction. Input JSON values and tool argument
JSON strings are decoded before inspection and serialized after replacements.
The caller's original request, provider invocation and response frames are not
changed. Empty `redact` means **unredacted capture**, even when serving guardrails
are configured. Configured detectors provide their documented bounded coverage;
they cannot promise to identify every secret or personal detail.

Streaming capture assembles text and tool arguments by native choice, content
block, item or tool index. Supported families are Chat Completions, legacy text
completions, Responses, Anthropic, Gemini GenerateContent, Bedrock
ConverseStream, Mistral FIM and Cohere Chat. Responses terminal
item/text/argument snapshots are deduplicated; conflicting snapshots drop
capture. Completion signals are required, and truncated, malformed,
unsupported or incomplete streams drop the whole capture. Gemini's partial-argument
extensions, opaque media and unrecognized output types are not capture surfaces.

Limits remain 64 KiB per input field, output and combined tool calls, 1 MiB per
record (or the policy's smaller `max_bytes`), 256 active collectors, 64 queued
records, 4 MiB per project per minute and 4,096 tracked projects. Inspection also
limits JSON nesting to 64 and decoded segments to 2,048. Redaction has a total
100 ms deadline, further bounded by each detector's timeout and caller
cancellation. The local engine checks elapsed deadlines after detector evaluation;
this is cooperative cancellation, not preemption of an individual RE2 call.

Capture always forces `fail_closed`, even if a detector serves with `fail_open`.
Invalid spans, unavailable fields, malformed arguments, deadline expiry and
exhausted inspection bounds drop the entire record and increment content-free
drop/redaction-failure counters. Final transformed fields must still fit the
original bounds. Only the final redacted serialized record can enter the queue.
Inference status, response bytes and usage accounting remain independent.

A policy/sink disable, delete, revision refresh or other ETag change makes queued
records stale. The sender rechecks the current policy, ETag and owner switch
before delivery. There is no durable retry store. Cache refresh is every five
seconds, with a fifteen-second freshness ceiling. Queue and project-budget drops
retain the existing best-effort semantics. Monitor `olp_capture_dropped_total`
and `olp_capture_redaction_failed_total` alongside delivery counters.

## Qualification

Focused unit and component checks cover the implementation. The new service
fixtures are compiled but require human execution: publication isolation,
forward migration, redacted sink readback, unchanged inference and absence of
payloads from durable metadata. See [M7 handoff](roadmap/m07-qualification.md)
for commands and acceptance conditions. Release qualification remains open.
