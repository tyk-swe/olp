# Implementation roadmap

This roadmap tracks implementation work and milestones in dependency order,
with no promised dates. **A milestone is Done when its implementation is
complete.** Each checkbox represents an implementation deliverable.

Feature scope uses LiteLLM v1.105.0 as the comparison baseline. Detailed designs
live in the [guardrail](develop/plans/guardrails.md),
[API surface](develop/plans/api-surface.md) and
[agent gateway](develop/plans/agent-gateway.md) plans. Current behavior is
documented in the [Use](README.md#use-the-gateway) and
[Operate](README.md#operate-an-installation) guides.

## Start here

Choose work whose implementation dependencies are available. Milestone numbers
are stable identifiers; independent work can proceed in parallel.

| Workstream | Next implementation | Dependencies |
| --- | --- | --- |
| Guardrail integrations | Implemented RM2; live qualification tracked separately | Existing engine, credential and egress contracts |
| Retained APIs and retrieval | Implemented RM5; service and live qualification tracked separately | Implemented RM4 foundations and each family's ownership and pricing contracts |
| Protocol and provider expansion | RM6.1–RM6.6, independently by transport | Implemented RM4 foundations and each transport's control and accounting contracts |
| Consumer MCP | RM7.1 direct Streamable HTTP execution | Implemented RM4 accounting and agent contracts, plus existing tool governance |
| Agent workflows | RM8.1 tool loops and RM8.2 prompt rendering | RM7.1 direct execution |
| Cloud invoice imports | RM9.1 import contracts, then each cloud adapter | Existing importer and reconciliation machinery |

The [architecture map](develop/architecture.md#contracts-and-feature-ownership)
identifies implementation boundaries and owners.

<a id="remaining-outcomes"></a>

## Implementation milestones

| Milestone | Status | Implementation scope |
| --- | --- | --- |
| [RM1: Existing gateway](#rm1-existing-gateway) | Done | Provider routing, access, operations, billing, exports and code mode |
| [RM2: Guardrails, capture and caching](#rm2-guardrails-capture-and-caching) | Done | Named text integrations, local/model filters, capture and caching; MCP/agent hooks assigned to RM7/RM8 |
| [RM3: Current APIs](#rm3-current-apis) | Done | Current completions, text Decisions, OCR/search, files and batches |
| [RM4: Shared execution foundations](#rm4-shared-execution-foundations) | Done | Operation contracts, durable debits, recovery, settlement and agent contracts |
| [RM5: Retained APIs and retrieval](#rm5-retained-apis-and-retrieval) | Done | Fine-tuning, vector stores, retrieval and retained resource families |
| [RM6: Protocol and provider expansion](#rm6-protocol-and-provider-expansion) | Planned | Additional OCR/search transports, realtime profiles and pass-through |
| [RM7: Consumer MCP](#rm7-consumer-mcp) | Planned | Direct execution, per-user grants, HTTP+SSE and stdio |
| [RM8: Agent workflows](#rm8-agent-workflows) | Planned | Tool loops, prompts, delegation, A2A, traces and operator views |
| [RM9: Cloud invoice imports](#rm9-cloud-invoice-imports) | Planned | AWS CUR, Google billing and Azure Cost Management imports |

### RM1: Existing gateway

**Status: Done.** **Owners:** gateway, connector, access, operations, billing
and code-mode maintainers.

Implemented scope includes provider routing and token estimation, signed
artifacts, identity and access controls, deployment and recovery tooling,
OpenAI/Anthropic billing imports, commitment accounting, export delivery,
CloudEvents, Stripe metering and native coding subscription integration.

### RM2: Guardrails, capture and caching

**Status: Done.** **Owners:** guardrail, connector, export and cache
maintainers. **Dependencies:** existing immutable revision, scope and inspection
contracts.

- [x] Implement the guardrail engine, built-in detectors, model evaluators,
  confined WASM and signed webhook execution, including action policies,
  streaming bounds and evaluator accounting.
- [x] Implement capture redaction, immutable capture-policy pins, scoped
  publication and external sink delivery without durable payload staging.
- [x] Implement the six existing external adapters: Bedrock, Azure Content
  Safety, Azure Prompt Shields, Model Armor, Lakera v2 and Presidio.
- [x] Implement exact and semantic response caching, guarded replay, scoped
  cache identity, purge, eviction and provider prompt-cache controls.
- [x] Implement the remaining named text integrations and local/model filters in the
  [vendor inventory](develop/plans/guardrails.md#vendor-inventory), including
  each adapter's authentication, request/response mapping, supported actions,
  replacement handling, deadlines and failure policies.

The inventory records every mapping and five explicit MCP/agent execution
boundaries. Those hooks require RM7/RM8 authority or tool-history contracts and
are owned there. Native text adapters preserve shared credential, egress,
revision, deadline and failure-policy contracts. Live-service and deployment
qualification remains tracked separately in the guardrail plan.

### RM3: Current APIs

**Status: Done.** **Owners:** protocol, resource, SDK and console maintainers.

Implemented scope includes legacy completions, text Decisions, current
OCR/search operations, and current file/batch operations. The current
managed-batch and text-only boundaries remain part of that scope. Additional
resource families and provider transports are listed in RM5 and RM6.

### RM4: Shared execution foundations

**Status: Done.** **Owners:** gateway/SDK maintainers for RM4.1,
resource/accounting maintainers for RM4.2–RM4.3, and access/agent maintainers
for RM4.4.

- [x] **RM4.1 — Operation contracts:** implement operation admission
  declarations, authorization and inspection contracts in the endpoint ledger.
- [x] **RM4.2 — Durable provisional debits:** persist submission intent and
  finite exposure across budget owners before dispatch, with immutable
  target/credential and price affinity and idempotent settlement.
- [x] **RM4.3 — Recovery and settlement:** implement fenced recovery,
  persistent unresolved exposure, atomic accounting, reconciliation and
  restoration of outstanding debits. Ambiguous acceptance does not trigger
  automatic redispatch or expiry of unresolved exposure.
- [x] **RM4.4 — Agent authority and publication:** implement agent scopes,
  immutable publications, configuration promotion, run references, one-use
  approvals and operator console controls.

New RM5/RM6 operations extend these contracts within their own implementation
slice. Bounded asynchronous work and realtime exposure use the shared debit
ledger. Agent execution additionally uses RM4.4; API families can proceed
independently of agent work.

### RM5: Retained APIs and retrieval

**Status: Done.** **Owners:** resource and connector maintainers, with
accounting and SDK owners. **Dependencies:** RM4 operation contracts,
owner/credential affinity and priced units; asynchronous jobs also use the
implemented durable accounting foundation.

Retained paid mutations have durable submission fences, immutable accounting
pins, pending budget subjects and one provider/operator evidence ledger before
dispatch. Unbounded paid work requires uncapped callers; delayed training/evals
always refuse TPM caps. Immediate compaction and retrieval use request accounting,
and zero-cost housekeeping remains available to authorized owners.

- [x] **RM5.1 — Fine-tuning:** implement OpenAI jobs, followed by the declared
  Azure and Vertex methods, including file affinity, unknown-create recovery,
  training accounting and resulting-model publication.
- [x] **RM5.2 — Native vector stores:** implement OpenAI/Azure lifecycle,
  file attachments, batches and search, with nested-ID ownership, pagination
  and Responses `file_search` identifier rewriting.
- [x] **RM5.3 — Query-only retrieval:** implement project-owned Bedrock
  Knowledge Bases, Vertex AI RAG Engine and Azure AI Search bindings, including
  filters, limits, text, scores, citations, egress and pricing. Index creation
  and synchronization remain outside this scope.
- [x] **RM5.4 — Conversations and compaction:** implement native lifecycle,
  items and compact methods, owner/target affinity, inspection and separately
  priced compaction without retaining local conversation history.
- [x] **RM5.5 — Containers:** implement the declared OpenAI and supported
  Azure lifecycle/file methods with owner isolation.
- [x] **RM5.6 — Evals:** implement native OpenAI definitions/runs, data-source
  affinity, durable reservations and terminal settlement.
- [x] **RM5.7 — Skills:** implement native Anthropic skills/versions and
  retained file/version ownership.
- [x] **RM5.8 — Context caches:** implement Gemini `cachedContents`, owner-bound
  IDs, expiry and storage accounting/reconciliation.
- [x] **RM5.9 — Managed-agent resources:** Gemini v1beta create/list/get/delete,
  API-key ownership, pinned credentials, persistent creation fences, metadata
  discovery and console workflows. PATCH and native environment references are
  explicitly unsupported. Agent execution is covered by RM8. Focused local
  checks pass; service integration scenarios are compiled, not yet executed,
  and live qualification remains required.

Focused native, ownership, reference, retrieval, billing and console checks pass;
management clients are regenerated. Database/process and restoration scenarios
compile but service execution, browser, SDK-service and live qualification remain
open in the [qualification record](develop/qualification.md#rm5-retained-apis-and-retrieval).
Provider retirements and explicit unsupported methods are documented without
calendar-based gateway switches.

Families remain independent. Retrieval-backed tools need only the implemented
`retrieval` operation for their selected RM5.2 or RM5.3 backend.

### RM6: Protocol and provider expansion

**Status: Planned.** **Owners:** connector maintainers for RM6.1–RM6.3,
realtime maintainers for RM6.4–RM6.5, and routing maintainers for RM6.6.
**Dependencies:** RM4 operation contracts plus transport-specific control,
metering, revocation and egress behavior. Realtime exposure uses the shared
durable accounting foundation.

- [ ] **RM6.1 — Brave GET-query transport:** implement pagination, filters,
  citations, time ranges, limits and request pricing, with explicit refusal
  of unmappable fields.
- [ ] **RM6.2 — Azure Document Intelligence polling:** implement bounded
  202/poll transport, deadlines, cancellation, page/document pricing,
  output mapping and provider-fetched URL egress controls.
- [ ] **RM6.3 — Vertex document processing:** implement the GCP-authenticated
  profile, credential isolation, field mapping, egress and observed-unit
  accounting.
- [ ] **RM6.4 — Governed WebRTC:** implement OpenAI server-mediated session
  creation/control, five-second revocation checks, close/usage accounting,
  session expiry and lost-control exposure. Extend Azure through its separate
  transport contract.
- [x] **RM6.5 — Native duplex profiles:** implement xAI realtime, Vertex AI
  Live and Bedrock Nova Sonic independently, with owner-scoped sessions,
  declared transcription support, cancellation and terminal usage accounting.
- [ ] **RM6.6 — Governed pass-through:** implement the separate route kind
  and anchored namespace, path normalization, query/header allowlists,
  redirect refusal, credential isolation, bounds, policy compatibility and
  per-request pricing or explicit unpriced accounting.

These implementations can proceed independently of one another and of retained
resource families.

### RM7: Consumer MCP

**Status: Planned.** **Owners:** MCP and access maintainers.
**Dependencies:** RM4 durable accounting and agent contracts, plus existing
guardrail argument/result execution.

- [ ] **RM7.1 — Direct Streamable HTTP execution:** publish pinned toolsets,
  static credentials, bounded sessions and compiled schemas. Serve `/mcp` and
  JSON tool endpoints through one authorized call path with catalog-drift
  handling, argument/result policies, limits, priced attempts and durable
  dispatch state. Repeated call IDs must not repeat effects.
- [ ] **RM7.2 — OAuth and per-user grants:** implement server/client/audience/
  scope/project/user binding, single-flight refresh, expiry, revocation and
  isolated credentials. Missing grants must refuse execution. Include the
  inventory's `mcp_security`, `mcp_end_user_permission`, `mcp_jwt_signer` and
  `agent_365` hooks only after server authority and caller identity are available.
- [ ] **RM7.3 — Legacy HTTP+SSE:** implement the explicit `2024-11-05`
  transport while preserving modern sessions' selected transport.
- [ ] **RM7.4 — Experimental stdio:** implement operator-installed executable
  dispatch with the same call, isolation and accounting limits as HTTP.
  Executable configuration remains operator-controlled.

Implement RM7.1 first; RM7.2–RM7.4 can then proceed independently.

### RM8: Agent workflows

**Status: Planned.** **Owners:** agent and access maintainers, with protocol,
prompt, observability and console owners. **Dependencies:** RM7.1 for tool loops
and prompts; the relevant MCP transport/auth implementation for delegation and
A2A. Retrieval-backed tools also use their selected RM5 backend.

- [ ] **RM8.1 — Bounded tool loops:** implement OpenAI, Anthropic and Gemini
  dialects plus Responses projections, iteration/time/concurrency/cost bounds,
  per-step accounting, approvals, digest-bound continuation, revocation and
  durable handling of cancellation, restart and unknown effects. Include
  `typesafe` ordered tool-exchange pruning and compression recovery/tool
  reinjection from the guardrail inventory.
- [ ] **RM8.2 — Prompt publication and rendering:** implement immutable
  revisions, labels and bounded string substitution across API, playground
  and import, with fixed in-flight revisions, variable handling, input guards
  and content-free persistence.
- [ ] **RM8.3 — Delegated identities:** extend workload issuers and implement
  consent, scoped tokens, intersected rights, user/agent/pair budgets, expiry,
  renewal and either-principal revocation.
- [ ] **RM8.4 — A2A workflows:** implement the declared protocol adapters,
  owner-bound tasks, authenticated callbacks, send/stream/read/cancel/
  resubscribe operations, callback accounting and kill-switch behavior.
  Preserve unresolved remote effects; delegated cases use RM8.3.
- [ ] **RM8.5 — Traces and operator views:** implement bounded OTLP ingestion,
  scoped trace links, origin-labelled exports and accessible operator views,
  with content stripping, project isolation and retention controls.

RM8.5 builds on the RM8.1–RM8.4 workflow contracts and existing observability
exports.

### RM9: Cloud invoice imports

**Status: Planned.** **Owners:** billing maintainers, with export and connector
owners. **Dependencies:** existing importer and reconciliation contracts.

- [ ] **RM9.1 — Cloud import contracts:** add source account scope, source
  revision, currency, UTC-day mapping and idempotent late-correction handling
  for AWS CUR, Google billing and Azure Cost Management.
- [ ] **RM9.2 — AWS CUR import:** implement scoped ingestion, pagination,
  empty-day handling, deduplication, restatements and reconciliation without
  duplicate charges.
- [ ] **RM9.3 — Google billing import:** implement scoped ingestion,
  pagination, empty-day handling, deduplication, restatements and reconciliation
  for the Google source.
- [ ] **RM9.4 — Azure Cost Management import:** implement scoped ingestion,
  pagination, empty-day handling, deduplication, restatements and reconciliation
  for the Azure source.

Each adapter depends on its own RM9.1 contract and can be implemented
independently of the other adapters and of agent work.

## Deferred implementation

- Vertex Veo: define and implement result and deletion behavior.
- fal: add a supported authentication integration.
- NVIDIA rerank: add representable raw scores.
- Vendor speech translation, media streaming and non-OpenAI image variations:
  add the corresponding provider operations and transports.

<a id="updating-progress-and-evidence"></a>

## Updating progress

- **Planned:** implementation has not started.
- **In progress:** some implementation work remains.
- **Done:** all implementation work in the milestone's stated scope is complete.

Check each deliverable when implemented and mark its milestone **Done** when
all deliverables are implemented. Record remaining code work and implementation
dependencies alongside the affected item. Keep milestone and work-item IDs
stable, including historical `M9.*` IDs in the
[endpoint ledger](../tests/clients/m9-endpoints.json).
