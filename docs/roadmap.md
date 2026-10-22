# Roadmap

Use this roadmap to choose the next implementation or qualification task. Work
is ordered by dependency, with no promised dates. Current behavior belongs in
the [Use](README.md#use-the-gateway) and [Operate](README.md#operate-an-installation)
guides. The three development plans retain binding designs and detailed
acceptance criteria. An implementation or registration is not a release
qualification result.

The capability comparison is frozen to LiteLLM v1.105.0 documentation reviewed
2026-10-13, including beta/Enterprise scope. The previous repository status review
was 2026-10-18. This milestone breakdown was checked against the local plans,
endpoint ledger and relevant implementation on 2026-10-22; it adds no test or
release qualification evidence. The
[benchmark comparator](develop/performance.md#what-is-pinned) separately remains
v1.103.2 at its recorded digest. A newer documentation baseline never changes a
historical benchmark pin. Rebaseline deliberately, documenting scope, impact and
evidence; missing credentials, time or hardware do not justify an exclusion.

## Start here

Choose a task whose entry conditions are available; milestone numbers are stable
identifiers, not a requirement to finish every lower number first.

| Lane | Next task | Entry conditions |
| --- | --- | --- |
| Shared implementation | RM4.1 API contracts, RM4.2 durable debits, and RM4.4 agent contracts | Existing resource, accounting and access owners; these designs can progress in parallel |
| Local acceptance | RM2.1 capture readback and RM3.1–RM3.4 current API checks | Disposable PostgreSQL/Valkey, appropriate SDK/browser setup and a recorded candidate |
| External qualification | RM1.1 candidate/environment manifest, then applicable RM1 tasks | Exact supported tuples, authorized accounts and reference hardware for the selected gate |
| Independent extensions | RM9.1 cloud import contracts | Authoritative export scope and representative fixtures; no dependency on agent work |

The [architecture map](develop/architecture.md#contracts-and-feature-ownership)
and [testing guide](../tests/README.md) identify implementation boundaries and
suite setup. Work-item checklists live once, either below or in the linked plan.
Use their IDs in changes and evidence records.

## Remaining outcomes

| Milestone | Current state | Completion gate |
| --- | --- | --- |
| [RM1: Existing gateway qualification](#rm1-existing-gateway-qualification) | In progress: qualification and scope decisions open | Candidate-specific performance, integration, billing and code-mode evidence |
| [RM2: Guardrails, capture and caching](#rm2-guardrails-capture-and-caching) | In progress: local code exists; live acceptance and vendor backlog open | Capture/policy acceptance, six live adapters, reference workloads and inventory review |
| [RM3: Implemented API qualification](#rm3-implemented-api-qualification) | Implemented: qualification open | SDK, database, browser, live-provider and performance gates for current tuples |
| [RM4: Shared execution foundations](#rm4-shared-execution-foundations) | Planned: new contracts and durable debits | Restart-safe reservation/settlement and independently verified agent contracts |
| [RM5: Retained APIs and retrieval](#rm5-retained-apis-and-retrieval) | Planned | Priced, owner-isolated lifecycle/query operations and per-tuple qualification |
| [RM6: Protocol and provider expansion](#rm6-protocol-and-provider-expansion) | Planned: remaining breadth only | OCR/search fidelity, live realtime control/usage and governed pass-through |
| [RM7: Consumer MCP](#rm7-consumer-mcp) | Planned | Direct execution and all declared transports/auth modes qualified |
| [RM8: Agent workflows](#rm8-agent-workflows) | Planned | Bounded loops, prompts, delegation/A2A, traces and operator journeys |
| [RM9: Cloud invoice imports](#rm9-cloud-invoice-imports) | Planned | Each cloud adapter has scoped fixtures and live closed-day reconciliation |

### RM1: Existing gateway qualification

**Accountable:** release maintainers; specialist owners are named per task.
**Entry:** existing implementations and an exact candidate. Start with RM1.1;
RM1.2–RM1.9 can then run independently when their environments are available.

- [ ] **RM1.1 — Freeze the evidence manifest** (release): record commit/image
  digest, supported tuples, tool/provider versions, commands, accounts/topology,
  artifact locations and missing prerequisites. Use the [qualification record](develop/qualification.md).
- [ ] **RM1.2 — Publish reference performance** (benchmark/release): run full-rate
  S1–S6, S1-shadow and pinned LiteLLM S1–S5; repeat from a clean checkout and retain
  raw artifacts meeting every [performance target](develop/performance.md).
- [ ] **RM1.3 — Measure token estimates** (catalog/provider): measure
  Anthropic/Gemini factors using vendor counts, publish signed provenance and
  pass live OpenAI exact-prefix checks under [provider qualification](develop/qualification.md#providers-and-token-estimates).
- [ ] **RM1.4 — Qualify providers and signed artifacts** (connector/release):
  execute live supported tuples and release-trust startup/doctor, tamper and
  rollback checks; retain exact versions and every skip.
- [ ] **RM1.5 — Qualify identity** (access): execute production IdP SAML/SCIM
  and issuer/JWKS rollover, expiry, disablement and stale-authority checks, plus
  the candidate's [access/isolation and recovery gates](develop/qualification.md#identity-and-access).
- [ ] **RM1.6 — Qualify operations** (operations/automation/security): verify
  target-cloud secret rotation and recovery, both Terraform/OpenTofu engines,
  regional failures and backup restore at the [required scope](develop/qualification.md#operator-ecosystem).
- [ ] **RM1.7 — Reconcile existing billing** (billing): run scoped
  OpenAI/Anthropic closed-day imports, pagination/restatements and commitment
  accounting/restore checks in [billing qualification](develop/qualification.md#observability-and-billing).
- [ ] **RM1.8 — Verify delivery and readback** (export/observability): qualify
  each claimed destination, CloudEvents and Stripe test metering; retain durable
  delivery, retry/duplicate, privacy and scoped-readback evidence.
- [ ] **RM1.9 — Qualify native coding subscriptions** (code-mode/qualification):
  prove enrollment, allowance, inference and provider-enforced hard bounds for
  each exact account/client/model/transport. Resolve the fresh HTTP-only
  root-registration decision in [code qualification](develop/qualification.md#code-mode-qualification).

**Exit:** all applicable candidate gates pass; missing accounts/hardware and
unresolved scope decisions remain explicit blockers, not successful results.

### RM2: Guardrails, capture and caching

**Accountable:** guardrail maintainers, with export, connector and performance
owners in the [RM2 checklist](develop/plans/guardrails.md#remaining-work-and-acceptance).
**Entry:** implemented immutable revision, scope and inspection contracts.
Capture/policy checks, inventory review and cache checks can start independently;
live adapters require target accounts and S3/cache measurements require reference hardware.
**Exit:** RM2.1–RM2.6 and the plan's acceptance gates pass. Vendor coverage remains
**Partial** while gaps remain; prompt-cache savings require separate live evidence.

### RM3: Implemented API qualification

**Accountable:** protocol/resource/SDK maintainers.
**Entry:** the existing endpoint ledger, certified tuples, inspection and prices;
current qualification does not wait for new RM4 implementations.
Follow the [RM3 checklist](develop/plans/api-surface.md#rm3-implemented-api-qualification).
**Exit:** current completions, text Decisions, OCR/search and file/batch tuples
pass database-backed SDK, console, live-provider and release-performance gates.
Current managed-batch and text-only limits remain explicit.

### RM4: Shared execution foundations

**Accountable:** gateway/SDK maintainers for RM4.1, resource/accounting maintainers
for RM4.2–RM4.3 and access/agent maintainers for RM4.4.
**Entry:** existing ownership, pricing and runtime contracts.
The [API foundation checklist](develop/plans/api-surface.md#rm4-shared-execution-foundations)
owns operation contracts and durable accounting; the [agent foundation checklist](develop/plans/agent-gateway.md#rm44-agent-contracts)
owns scopes, publication, runs and approvals. RM4.4 is not an API-family prerequisite.
**Exit:** bounded debits survive uncertainty/restart and settle once, and agent
authority/publication contracts pass their own isolation and privacy checks.

### RM5: Retained APIs and retrieval

**Accountable:** resource/connector maintainers.
**Entry:** RM4.1 for each operation; RM4.2–RM4.3 before enabling bounded paid
asynchronous work. Each family also needs historical ownership/credential
affinity and finite priced units. Query-only retrieval can proceed independently
of asynchronous job implementation.
Follow the [RM5 checklist](develop/plans/api-surface.md#rm5-retained-apis-and-retrieval).
**Exit:** the declared families pass lifecycle, nested-ID isolation, uncertain
create/restart, pricing and privacy gates, with exact provider tuples qualified.

### RM6: Protocol and provider expansion

**Accountable:** connector/realtime/routing maintainers, with security review.
**Entry:** RM4.1 for each new operation; transport-specific control, metering,
revocation and egress contracts. Realtime unresolved exposure uses RM4.2–RM4.3;
stateless codecs and pass-through do not wait for all retained families.
Follow the [RM6 checklist](develop/plans/api-surface.md#rm6-protocol-and-provider-expansion).
**Exit:** field fidelity, live close/usage and adversarial path/policy gates pass
for the declared scope; unsupported transports and fields stay explicit.

### RM7: Consumer MCP

**Accountable:** MCP/access maintainers.
**Entry:** RM4.3, RM4.4 and existing tool-governance execution contracts.
Follow the [RM7 checklist](develop/plans/agent-gateway.md#rm7-consumer-mcp): direct
Streamable HTTP first, then per-user grants and remaining declared transports.
**Exit:** pinned conformance/drift, grant isolation, bounded effects and one
financial attempt per dispatch pass. Existing registration and management MCP
do not establish consumer execution. RM2 live-vendor closure is not an entry gate.

### RM8: Agent workflows

**Accountable:** agent/access maintainers, with protocol and observability owners.
**Entry:** direct MCP (RM7.1) for loops/prompts; MCP interoperability for
delegation/A2A; those run contracts and observability exports for traces.
Follow the [RM8 checklist](develop/plans/agent-gateway.md#rm8-agent-workflows).
Retrieval-backed tools additionally require the certified `retrieval` operation
for their selected backend in RM5.2 or RM5.3.
**Exit:** immutable rendering, bounded approvals/effects, consent/revocation,
pair budgets, origin-labelled traces and accessible operator journeys pass
the agent plan's candidate gates.

### RM9: Cloud invoice imports

**Accountable:** billing maintainers, with export/connector owners.
**Entry:** existing importer and reconciliation contracts; authoritative source
scope and representative export fixtures. This milestone does not block the
limited OpenAI/Anthropic tranche in RM1.7.

- [ ] **RM9.1 — Define each import contract:** record authoritative account
  scope, source revision, currency, UTC-day mapping and idempotent late-correction
  behavior for AWS CUR, Google billing and Azure Cost Management independently.
- [ ] **RM9.2 — Implement AWS CUR import:** after its RM9.1 contract, add scoped
  fixtures for pagination, empty days, duplicates and restatements; verify
  reconciliation without duplicate charges.
- [ ] **RM9.3 — Implement Google billing import:** after its RM9.1 contract,
  pass the same scoped fixture and correction checks for the Google source.
- [ ] **RM9.4 — Implement Azure Cost Management import:** after its RM9.1
  contract, pass the same scoped fixture and correction checks for Azure.
- [ ] **RM9.5 — Qualify each adapter live:** after that adapter's fixtures pass,
  reconcile at least one closed UTC day in an authorized account; retain source
  scope/revision, currency, coverage and correction evidence, and update [FinOps](operate/finops.md).

**Exit:** all three adapters have individual fixture and live evidence. One
adapter may advance without the others; missing source access leaves its gate open.

## Deferred scope

Deferred provider breadth remains conditional: Vertex Veo needs an agreed result
and deletion contract; fal needs a certifiable authentication path; NVIDIA rerank
needs representable raw scores. Vendor speech translation, media streaming and
non-OpenAI image variations remain follow-up work. Reopening a decline requires
a reviewed contract, pricing/usage and certification evidence; see
[Compatibility](use/compatibility.md#provider-qualification-and-support-boundaries).

## Updating progress and evidence

Every slice follows [change acceptance](develop/qualification.md#change-acceptance).
**Planned** means the contract exists but serving code does not; **In progress**
means implementation or scope closure remains; **Implemented** has focused code
checks and an owned qualification gap; **Shipped** requires candidate-specific
release evidence. These states describe delivery maturity, not whether someone
is actively running a task. A milestone with mixed work records that distinction
in its state; never promote the whole milestone from one successful subtask.

Check a work item only when its stated deliverable and verification are complete;
an implementation checkbox does not imply **Shipped**. Record its ID, owner role,
commit/image digest, exact scope, commands, artifact links and remaining gates.
Track unavailable accounts, hardware, prerequisite IDs and undecided contracts
separately as blockers, with the action needed to unblock them. Do not create
dates or individual assignments without agreement.

Before a release claim, run the applicable check/integration/race/build/Helm and
performance gates on the same candidate. New surfaces must enforce compatible
guardrail/cache policies or reject them before dispatch. Update this roadmap and
the relevant guide/plan together, preserving the historical `M9.*` IDs in the
[endpoint ledger](../tests/clients/m9-endpoints.json). Scope changes require the
review and evidence described above; unavailable credentials, time or hardware
do not close a gate.
