# M3: Complete OpenAI request path

[Roadmap](README.md) | [Previous: access and control](02-access-and-control.md) |
[Next: limits and accounting](04-limits-and-accounting.md)

**Status:** Not started. **Prerequisites:** M2 complete.

Deliver a usable Go gateway: configure an OpenAI connection, certify models,
publish a route, issue a key, and make unary and streaming SDK requests.
Distributed quotas and durable accounting complete in M4.

## Backlog

### M3-01

- [ ] **Implement bounded ingress and outbound HTTP.**

**Depends on:** Milestone prerequisites.

**Deliver:** Implement connection/work admission, JSON and decompressed-body
limits, provider response/event limits, request IDs, trusted proxy handling,
inference CORS, and bounded HTTP transports. Restore HTTPS defaults, endpoint
validation, DNS resolution/pinning, redirect refusal, and explicit provider
egress exceptions.

**Accept:** Oversized/compressed bodies, malformed headers, unsafe DNS answers,
redirects, and disallowed destinations fail before unbounded work or dispatch.
Timeout and cancellation close upstream bodies. Keep streaming deadlines and
backpressure explicit instead of applying a unary timeout to every response.

**References:** [Request admission](../../src/http/request_admission/),
[listener](../../src/http/listener.rs), [egress](../../src/net/egress/),
[egress corpus](../../tests/conformance/ssrf.rs).

### M3-02

- [ ] **Implement OpenAI generation codecs and transport.**

**Depends on:** [M3-01](#m3-01).

**Deliver:** Implement Chat Completions and Responses request/response handling,
native extensions, model rewriting, tools, structured output where supported,
usage extraction, typed errors, and fragmented SSE parsing/encoding. Keep
request/stream types sufficient for later cross-protocol adapters.

**Accept:** Native round trips preserve supported fields and unknown source
extensions while validating the gateway envelope. Unary and streaming golden
fixtures pass, including tools, malformed/truncated events, missing usage,
oversized events, and unsupported stateful resource references.

**References:** [OpenAI codecs](../../src/protocols/openai/),
[OpenAI transport](../../src/providers/openai/transport/),
[SSE](../../src/protocols/sse.rs), [protocol tests](../../tests/protocols/).

### M3-03

- [ ] **Restore provider drafts, discovery, and certification.**

**Depends on:** [M3-02](#m3-02).

**Deliver:** Implement OpenAI connection CRUD, model discovery/manual identifiers,
enabled capabilities, bounded connection/model probes, draft testing,
activation, immutable revisions, and history. Retain certification evidence
only for unchanged eligible tuples and distinguish declared from certified.

**Accept:** Draft edits cannot change serving traffic. Activation validates the
completed draft and exact model/operation support. Transport changes invalidate
affected evidence, probes respect concurrency/time/body bounds, and probe
content never enters persistent diagnostics. Unimplemented media remains unavailable.

**References:** [Provider lifecycle](../../src/providers/lifecycle.rs),
[model handlers](../../src/providers/http/models/),
[certification](../../src/providers/connectors/certification.rs),
[revision tests](../../tests/persistence/provider_revisions_postgres.rs).

### M3-04

- [ ] **Restore credential pools and exact version references.**

**Depends on:** [M3-03](#m3-03).

**Deliver:** Implement default/additional credential slots, model eligibility,
validation, rotation, enable/disable controls, and explicit secret-version
revocation. Persist connection/slot quota configuration for M4 enforcement and
retain exact credential versions referenced by published revisions.

**Accept:** Secrets remain write-only; rotation validates the new credential
and does not silently substitute it into a pinned revision. Slot failures do
not make sibling credentials unusable. Version revocation is durable authority
state independent of a new provider activation. Media-specific retention joins in M6.

**References:** [Pool model](../../src/providers/pool.rs),
[pool transport](../../src/providers/pool_transport.rs),
[credential HTTP](../../src/providers/http/credentials.rs),
[provider routing](../provider-routing.md).

### M3-05

- [ ] **Restore route drafts, publication, and history.**

**Depends on:** [M3-03](#m3-03), [M3-04](#m3-04).

**Deliver:** Implement route slugs, allowed operations, eligible targets,
priority tiers, deterministic weighted selection, overall/target deadlines,
attempt budgets, ETags, publication, revision comparison, and restoring a
historical configuration as a new draft.

**Accept:** Invalid or uncertified targets cannot publish. Model listing and
retrieval expose only routes permitted by the key's explicit scopes/allowlist.
Every actual credential attempt consumes the attempt budget; the count need
not equal the number of distinct targets. Stale edits cannot overwrite newer drafts.

**References:** [Route drafts](../../src/routes/drafts.rs),
[revisions](../../src/routes/revisions.rs),
[selection](../../src/inference/provider_selection.rs),
[routing fixtures](../../tests/fixtures/routing/).

### M3-06

- [ ] **Restore atomic runtime publication and authority refresh.**

**Depends on:** [M3-04](#m3-04), [M3-05](#m3-05).

**Deliver:** Implement durable publication/outbox handling, immutable snapshot
digests, validation, atomic replacement, startup recovery, and per-request
snapshot pinning. Keep key and credential-revocation refresh independent of
whether a gateway can install the newest provider configuration.

**Accept:** Repeated publication is harmless, and partial/invalid releases
cannot become active. Preserve the five-second authority poll and 60-second
staleness cutoff measured monotonically from the read's start. Stale authority
stops new admissions; already admitted streams retain their pinned policy.
Revoked credential versions cannot be selected from retained releases.

**References:** [Runtime](../../src/runtime/),
[authority tests](../../src/runtime/manager/authority_tests.rs),
[activation authority](../../src/runtime/activation/tests/authority_postgres.rs),
[HA authority](../../tests/ha/authority.rs).

### M3-07

- [ ] **Implement the request lifecycle, failover, and cancellation.**

**Depends on:** [M3-02](#m3-02), [M3-06](#m3-06).

**Deliver:** Compose authentication, positive endpoint authorization, route
selection, bounded attempts, timeout/error classification, circuit health,
credential cooldowns, and streaming commitment. Handle unary completion,
client cancellation, disconnects, and late callbacks through one terminal path.

**Accept:** Retryable failures before commitment can select the next eligible
attempt within the original budget/deadline. A committed stream cannot restart
on another provider. Retry-After and credential/endpoint failure classes remain
distinct. Cancellation closes upstream work and releases local resources once.

**References:** [Executor](../../src/inference/executor.rs),
[lifecycle](../../src/inference/lifecycle.rs),
[failover](../../src/inference/failover/),
[HTTP scenarios](../../src/inference/http/tests/).

### M3-08

- [ ] **Emit bounded request and attempt facts.**

**Depends on:** [M3-07](#m3-07).

**Deliver:** Define the terminal envelope and attempt identities used by M4:
ordered attempts, pinned revisions, credential slot/version, status, timing,
observed usage, and completeness. Add metadata-only structured diagnostics and
request/attempt metric hooks at the lifecycle boundary.

**Accept:** Success, failure, and cancellation each produce one terminal
envelope; late callbacks cannot produce a second. Missing usage is explicit,
and failed attempts remain observable. Prompts, outputs, tool data, uploaded
content, raw headers, and credentials never enter the envelope.
M4 owns durable delivery and pricing guarantees.

**References:** [Events](../../src/inference/events.rs),
[telemetry](../../src/inference/telemetry.rs),
[usage emitter](../../src/usage/emitter.rs),
[data safety](../../tests/contract/data_safety.rs).

### M3-09

- [ ] **Connect and qualify the complete console-to-SDK workflow.**

**Depends on:** [M3-03](#m3-03), [M3-04](#m3-04), [M3-05](#m3-05),
[M3-06](#m3-06), [M3-07](#m3-07), [M3-08](#m3-08).

**Deliver:** Adapt provider setup/details/pools/history, route editing/history,
key route selection, and playground to the Go contracts. Run the actual
OpenAI SDK through the Go fixture and installation, with native error checks.

**Accept:** A browser user reaches successful unary and streaming inference
from an empty installation. Journeys cover stale drafts, failed activation,
revocation, and in-flight publication changes. Go protocol/service tests cover
the retained generation corpus; screenshots document visible UI changes.

**References:** [Provider console](../../console/src/lib/features/providers/),
[route console](../../console/src/lib/features/routes/),
[playground](../../console/src/lib/features/inference/playground/),
[SDK checks](../../tests/sdk-smoke/smoke.mjs).

## Exit scenarios

- Configure, certify, activate, publish, issue a key, and call both OpenAI generation endpoints.
- Verify key-filtered model listing/retrieval and typed scope/route rejections.
- Change configuration during a stream; exercise failed activation and key revocation.
- Test fragmented SSE, pre-commit failover, post-commit failure, slow readers, and disconnects.
- Reject unsafe egress and oversized input/output without leaking request content.
- Run Go generation suites and provider/route/playground journeys at both origins.
