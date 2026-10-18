# Roadmap: LiteLLM parity and beyond

This roadmap takes OpenLLMProxy (OLP) from its 0.1 baseline toward the capability
scope of the [LiteLLM AI Gateway](https://docs.litellm.ai/docs/simple_proxy)
and then beyond it. Each milestone defines the outcome, scope, binding design
decisions, dependencies and acceptance evidence. Work is ordered by dependency,
not by a promised date. Implementation completion and release qualification are
reported separately.

Repository status reviewed 2026-10-18. This review reconciles specifications with
code and recorded evidence; it is not a new release-test report.

| Document | Purpose |
| --- | --- |
| [Parity matrix](parity.md) | The tracked LiteLLM capability scope, OLP's implemented coverage, and the workstream that closes each gap |
| [M1](m01-measured-advantage.md) through [M11](m11-operator-ecosystem.md) | One specification per milestone, listed [below](#milestones) |
| [Delivery order](#delivery-order-and-entry-gates) | The next work, prerequisite gates and accountable implementation owners |

## Reference baseline

Reference: released [LiteLLM gateway documentation](https://docs.litellm.ai/docs/)
reviewed 2026-10-13 ([v1.105.0](https://docs.litellm.ai/release_notes/v1.105.0/v1-105-0)),
including beta/Enterprise features, not unreleased work. Keep this comparison
frozen until an explicit re-baseline; live documentation can change independently
of that release. Provider names and page counts are discovery inputs, not proof
that every operation or model is supported.
OLP's 0.1.0 [compatibility](../compatibility.md), [routing](../provider-routing.md)
and [execution](../gateway.md) guides describe implemented behavior.

### Where OLP leads today

Native OpenAI/Anthropic/Gemini/Bedrock surfaces, certified operation tuples,
strict fidelity contracts, immutable publication, explainable routing,
exact attempt accounting, confined plugins and content-free diagnostics.

### Where it falls short

Remaining work includes measured non-OpenAI token calibration, invoice
reconciliation beyond OpenAI and Anthropic, commitment release qualification,
guardrail vendor breadth and
capture-redaction integration, API-surface completion, and agent/tool execution.
M1's harness/gate exists, but full-rate reference-hardware results remain
uncollected. Live-provider and external identity qualification are separate from
fixture coverage.
The [parity matrix](parity.md) owns capability-by-capability status.

## How OLP wins

Milestones retain native fidelity, explicit refusals, measured performance,
content-free accounting and operation-level authorization. New state must have
feature ownership, reviewed egress/cryptographic purposes and portable nonsecret
intent. Everything ships in the single AGPL product, without a feature paywall.

## Milestones

M1–M6, M8 and M11 are implemented; M7 is in progress, and M9–M10 remain planned.
M6 has local verification; its full integration and benchmark qualification remain
open as recorded in [the commitment evidence](m06-commitment-evidence.md). The
[implementation review](m01-m04-implementation-review.md) records historical
focused evidence and the [architecture review](../architecture-review.md) records
later integrated qualification. Neither establishes uncollected full-rate,
live-provider or external-directory results. Implemented is not a release claim.
Dependencies in the table name required capabilities, not a requirement to wait
for every unrelated release check in that milestone. The entry gates below
identify the evidence needed before dependent behavior can ship.

| ID | Milestone | Outcome | Depends on | Status |
| --- | --- | --- | --- | --- |
| M1 | [Measured advantage](m01-measured-advantage.md) | Published, regression-gated overhead and client compatibility; accurate admission token estimates; standard response metadata. | None | Implemented |
| M2 | [Provider and catalog breadth](m02-provider-catalog.md) | Reviewed provider/media inventory with certified tiers and explicit declines; a signed reference catalog of model facts and prices. | None | Implemented |
| M3 | [Adaptive routing and resilience](m03-routing-resilience.md) | Cross-route fallbacks, capacity-aware selection, priority admission, supply-side budgets, active and fleet-shared health, shadow traffic, explainable request selectors. | M1 | Implemented |
| M4 | [Tenancy, identity and budgets](m04-tenancy-identity.md) | End users, a budget hierarchy with flexible windows, limit templates, route groups, workload JWTs, SAML, SCIM, MFA, organizations and caller-supplied credentials. | None | Implemented |
| M5 | [Observability, export and alerting](m05-observability.md) | Durable export sinks, opt-in payload capture, OpenTelemetry GenAI conventions, business metrics, alert channels and events. | M4 | Implemented |
| M6 | [Cost management and chargeback](m06-cost-management.md) | Dimension-aware pricing, rate cards, cost estimation, complete commitment exports, and OpenAI/Anthropic invoice reconciliation. | M2, M4, M5 | Implemented |
| M7 | [Guardrails platform](m07-guardrails.md) | One guardrail engine with built-in detectors, vendor adapters, webhook and WebAssembly guardrails, streaming inspection and tool governance. | M1 | In progress |
| M8 | [Response caching](m08-caching.md) | Sealed exact and semantic response caches, cache controls and provider prompt-cache automation. | M7 | Implemented |
| M9 | [API surface completion](m09-api-surface.md) | Legacy completions, cross-provider files and batches, fine-tuning, vector stores, Responses completion, realtime expansion, OCR, search, Decisions, provider-retained resources and governed pass-through. | M2; M4, M6, M7 per slice | Planned |
| M10 | [Agent gateway](m10-agent-gateway.md) | An MCP gateway with pinned tools, gateway-executed tools, an A2A agent gateway, prompt registry, content-free agent traces and delegated identities. | M4, M5, M6, M7, M11.2 | Planned |
| M11 | [Operator ecosystem](m11-operator-ecosystem.md) | A management CLI, a Terraform provider, KMS and external secret stores, a developer catalog, multi-region gateways and a management MCP server. | M4 | Implemented |

## Delivery order and entry gates

Work can proceed in parallel when the entry gate is met. Within each lane, the
milestone's delivery sequence is authoritative. The owner below is the code-area
role accountable for the contract, implementation, tests and runbook; the person
taking a slice owns it through qualification.

| Order / lane | Next deliverable | Entry gate | Accountable owner and completion evidence |
| --- | --- | --- | --- |
| 1 · qualification | M1 full-rate results and remaining M2–M6/M11 external checks | Existing implementation and reproducible pinned fixtures | Runtime/qualification owner; benchmark artifacts, provider/identity results and explicit unsupported tuples, tied to the tested revision |
| 1 · guardrails | Close M7 vendor coverage decisions and capture-redaction integration | Existing immutable engine, streaming holdback and M5 capture boundary | Guardrails/export owners; named adapter contracts, capture leakage/failure tests and M7 exit evidence |
| 1 · accounting | M2.4 calibration and M6 live billing/sink qualification; additional invoice adapters follow | Signed calibration provenance and the existing commitment/invoice ledgers | Catalog/billing owners; measured family corpus, total commitment-cost reconciliation and provider-specific import fixtures |
| 2 · stateless APIs | M9 completions and Decisions first; OCR/search after the shared contracts | M2 certification; M7 inspection contract and M6 units for each supported surface | Protocol/connector owners; explicit operation/provider ledger, native fixtures and SDK qualification |
| 2 · retained APIs | M9 resource lifecycle and files/batches, then fine-tuning, vector stores, Responses and other retained families | Historical target/credential authority, cancellation and usage-settlement contract | Resource/worker owners; cross-project denial, replay, ambiguous-create, cleanup and late-usage tests |
| 2 · realtime | M9 realtime expansion | Transport-specific inspection, accounting and revocation controls are demonstrable | Realtime owner; connection teardown, reconnect and usage-completeness qualification before enabling each transport |
| 2 · tools | M10 direct MCP, then gateway tool loops and A2A | Existing MCP registration plus M4 authority, M7 tool governance, M5 facts and M6 prices | MCP/agent owners; pinned-tool drift, grant isolation, cancellation and one-accounting-event-per-attempt evidence |
| 2 · agent support | M10 prompt registry, traces and delegated identities | The corresponding M10 resource/identity contracts; traces use M5 content-free facts | Agent/access owners; immutable rendering, scoped metadata queries and delegation/revocation tests |
| 3 · release | Qualify the integrated release candidate | Implementation gates for every included feature pass | Release owner; same-revision suite results, operational recovery and all applicable external/performance evidence |

M8 can use the implemented M7 engine now. Its release claim still requires the
cache/guardrail interaction and M1 performance checks; M7's unqualified external
adapters do not qualify merely because caching works. Existing M11 management
MCP and upstream registration are prerequisites for M10, not proof of a serving
MCP gateway. Newly enabled M9 surfaces must either satisfy the applicable M7/M8
contracts or reject incompatible policies before dispatch.

## Binding cross-cutting decisions

These decisions apply to every remaining slice. A milestone may narrow them;
changing them requires a documented replacement decision and impact review in
the same change.

| Concern | Decision |
| --- | --- |
| Authority and publication | Management contract metadata defines authorization. New serving policy is compiled into the immutable runtime release; requests pin one generation. Recheck live revocation at existing authority boundaries. Retained resources keep their original project, target and credential-version identity. |
| Defaults and bounds | New content export, external evaluation, caching, tool execution and delegated access require explicit configuration. Specify validated finite request, concurrency, byte, time and retention limits before enabling a new path. No unbounded background queue or silent feature fallback. |
| Retries and side effects | Retry only operations whose contract proves replay safe, within the parent attempt/deadline budget. An ambiguous remote create or tool side effect is recorded as uncertain and reconciled; never redispatch it automatically. Local mutation idempotency is not a promise of upstream exactly-once execution. |
| Money and failure | Unknown usage/prices remain unknown, never zero. Enforced cost budgets fail closed when a safe reservation cannot be computed. Cancellation, retries, evaluators, cache embeddings, shadow calls and tools retain distinct attempt facts under their parent operation. |
| Data and egress | Durable request, audit and trace facts stay content-free. Sealed response caches, acknowledged semantic vectors, operator-authored prompt templates and opt-in external capture have separate declared storage contracts. Every new outbound path uses reviewed egress, explicit credential purposes and redacted errors. |
| Compatibility | Certification is per provider/model/operation/transport tuple. Unsupported parameters, fidelity changes and unenforceable mandatory policies refuse explicitly. A provider-family name or registered endpoint does not certify its operations. |
| Recovery and promotion | Forward-only migrations and portable nonsecret configuration are mandatory. Secret bindings and enabled external dependencies are checked before activation; failed publication retains the last valid generation. Recovery tests cover historical resource ownership and current revocation. |

## Parity gate and scorecard

Implemented parity is reached when every capability row of the
[parity matrix](parity.md) is `Parity`, `Ahead` or `Excluded`, with explicit
scope and evidence. A release parity claim also requires the applicable release
qualification gates. An exclusion needs a product/design reason; missing time,
credentials or test hardware is not an exclusion. Recompute these measures
whenever a capability status changes:

| Measure | Definition |
| --- | --- |
| Parity coverage | Rows marked `Parity` or `Ahead`, divided by all rows not marked `Excluded`. |
| Added latency | Gateway minus direct-to-mock latency at p50, p95 and p99 for each [M1 scenario](m01-measured-advantage.md#scenarios), for OLP and the pinned LiteLLM release. |
| Efficiency | Sustained requests per second per vCPU, and resident memory per 1,000 open streams. |
| Compatibility | Pass rate of the SDK and client qualification suites across their pinned versions. |
| Accounting completeness | Share of admitted requests with complete usage, and share of attempts that are unpriced. |
| Authorization coverage | Management operations exercised by the authorization and isolation sweeps (must stay 100%). |

At the 0.1.0 baseline the matrix has 150 rows: 15 `Ahead`, 33 `Parity`, 30
`Partial`, 70 `Gap` and 2 `Excluded`, a parity coverage of 48 of 148 (32%).
This historical scorecard is preserved for comparison.
The current 153 capability rows reflect implemented capabilities, including M8,
project limit templates and the scoped model catalog, with incomplete FOCUS
commitment export corrected to `Partial`: 23 `Ahead`, 101 `Parity`, 7 `Partial`,
20 `Gap`, 2 `Excluded`; coverage is 124/151 (82.1%).
Status rows in the legend are not capabilities. Definitions/registrations alone
do not close broader guardrail-execution or agent-gateway workstreams.

## Definition of done

Every implementation slice supplies the applicable items below. A documentation
review does not rerun or replace implementation qualification.

1. **Contract.** Management operations in
   [`openapi/management.json`](../../openapi/management.json) with their
   security requirements, regenerated types (`make api`), and updated
   `internal/access/testdata/authorization.golden.json`. The authorization and
   isolation sweeps pass.
2. **Desired state.** New configuration participates in
   [configuration export, plan and apply](../configuration.md#configuration-promotion-artifacts)
   without secrets, and in runtime publication when the data plane reads it.
3. **Data plane discipline.** Inference reads configuration from the pinned
   runtime snapshot, key authority and Valkey; only retained-resource
   operations look up their PostgreSQL mappings, as they do today. Benchmarks
   show no regression beyond the
   [performance budget](m01-measured-advantage.md#performance-budget).
4. **Privacy and security review.** A persisted-data inventory, egress policy
   coverage for every outbound call, declared seal and digest purposes, and
   credential redaction for every upstream error path.
5. **Accounting.** Attempt-level facts for every billable or quota-relevant
   event, metadata-only, with completeness evidence.
6. **Tests at the right boundary.** Unit and fixture tests beside the feature,
   `integration`-tagged service tests for PostgreSQL and Valkey behavior, SDK
   suites for client-visible protocol, and Chromium journeys for console
   workflows; see [behavioral validation](../../tests/README.md).
7. **Console.** A feature folder under `console/src/lib/features/` that follows
   the [console design system](../../console/README.md#design-system)
   and passes the axe checks.
8. **Operations.** Forward-only migrations; Helm values, schema and templates
   updated together; metrics, readiness and runbook entries in
   [operations](../operations.md).
9. **Documentation and evidence.** Update user-facing guides and affected
   [parity matrix](parity.md) rows in the same change. Record tested revision,
   environment, commands/artifact locations and untested combinations. Move a
   row only when its complete stated scope is implemented and evidenced.

For `Implemented`, all implementation checks must pass and remaining external
or release-only checks must be identified with an owner and acceptance condition.
For `Shipped`, the release candidate must pass those applicable checks, including
full-rate claims, supported live tuples and recovery. A historical green suite,
reduced-rate benchmark or mock provider cannot satisfy a different revision's
release gate. A failure is fixed or the supported scope is explicitly revised
before release; it is not converted to a checked box.

## Out of scope by design

The matrix's two `Excluded` rows are Assistants and key-value memory. An
embeddable Python SDK is outside this gateway comparison. The other entries
below constrain how OLP provides an in-scope capability; they are not additional
`Excluded` rows.

| Capability | Reason |
| --- | --- |
| An embeddable Python SDK | OLP is a gateway. Official vendor SDKs are its clients, which keeps client compatibility the product rather than a translation layer. |
| OpenAI Assistants API | OpenAI's [deprecation schedule](https://developers.openai.com/api/docs/deprecations#2025-08-20-assistants-api) gives 2026-08-26 as the shutdown date. Responses and Conversations replace it ([M9](m09-api-surface.md)). |
| Caller-chosen upstream base URLs | Destinations stay operator-declared and egress-validated. Caller-supplied credentials for an operator-declared connection are in scope ([M4](m04-tenancy-identity.md#m46-caller-supplied-provider-credentials)). |
| Uncertified wildcard model passthrough | Routes publish only certified capabilities. Route templates create ordinary routes for newly certified models ([M3](m03-routing-resilience.md#m38-route-templates)). |
| In-process custom code hooks | Custom authentication, callbacks and guardrails run as confined WebAssembly plugins or behind signed webhook contracts, never inside the gateway process. |
| `/memory` key-value storage | Application state that no provider API defines. Applications should keep it in their own stores. |
| Prompt and response storage in request history | Durable request records stay content-free. Opt-in payload capture streams to an operator-owned sink instead ([M5](m05-observability.md#m52-payload-capture)). |

## Maintaining this roadmap

- **Status.** `Planned` means the contract is decided but serving implementation
  is outstanding; `In progress` means implementation or scope closure remains;
  `Implemented` means code and focused checks are complete with an explicit
  qualification handoff; `Shipped` additionally means release qualification is
  recorded. Completed prerequisites do not promote their parent milestone.
  Update this table and affected parity rows together.
- **Decisions.** The chosen contracts/defaults in each milestone are binding.
  There are no deferred architectural alternatives for an implementation owner
  to choose silently. New evidence may reopen a decision: record the replacement,
  rationale, compatibility/data impact and revised acceptance evidence before
  implementing it. External qualification remains work, not an open design choice.
- **Re-baselining.** When a milestone closes, review LiteLLM's documentation and
  [release notes](https://docs.litellm.ai/release_notes). New capabilities
  become `Gap` rows assigned to a milestone, or `Excluded` rows with a reason.
- **Scope changes.** Edit the milestone specification in the change that
  proposes the new scope, so the roadmap never trails the code.
