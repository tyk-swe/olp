# Roadmap: LiteLLM parity and beyond

This roadmap takes OpenLLMProxy (OLP) from its 0.1 baseline to full feature
parity with the [LiteLLM AI Gateway](https://docs.litellm.ai/docs/simple_proxy)
and then past it. It is milestone-driven: each milestone has a fixed scope,
design constraints, decisions to settle and exit criteria, but no date. A
milestone closes when its exit criteria pass.

| Document | Purpose |
| --- | --- |
| [Parity matrix](parity.md) | Every LiteLLM gateway capability, OLP's state today, and the milestone that closes each gap |
| [M1](m01-measured-advantage.md) through [M11](m11-operator-ecosystem.md) | One specification per milestone, listed [below](#milestones) |

## Reference baseline

Reference: released [LiteLLM gateway documentation](https://docs.litellm.ai/docs/)
reviewed 2026-10-01, including beta/Enterprise features, not unreleased work.
OLP's 0.1.0 [compatibility](../compatibility.md), [routing](../provider-routing.md)
and [execution](../gateway.md) guides describe implemented behavior.

### Where OLP leads today

Native OpenAI/Anthropic/Gemini/Bedrock surfaces, certified operation tuples,
strict fidelity contracts, immutable publication, explainable routing,
exact attempt accounting, confined plugins and content-free diagnostics.

### Where it falls short

Remaining work includes chargeback/pricing breadth, streaming/extensible
guardrails, response caching, API-surface completion and agent/tool execution.
M1's harness/gate exists, but full-rate reference-hardware results remain uncollected.
The [parity matrix](parity.md) owns capability-by-capability status.

## How OLP wins

Milestones retain native fidelity, explicit refusals, measured performance,
content-free accounting and operation-level authorization. New state must have
feature ownership, reviewed egress/cryptographic purposes and portable nonsecret
intent. Everything ships in the single AGPL product, without a feature paywall.

## Milestones

M1–M5 and M11 are implemented; M6–M10 remain planned. The
[implementation review](m01-m04-implementation-review.md) records historical
focused evidence and the [architecture review](../architecture-review.md) records
later integrated qualification. Neither establishes uncollected full-rate,
live-provider or external-directory results. Implemented is not a release claim.
Dependencies in the table constrain order; there are no milestone dates.

| ID | Milestone | Outcome | Depends on | Status |
| --- | --- | --- | --- | --- |
| M1 | [Measured advantage](m01-measured-advantage.md) | Published, regression-gated overhead and client compatibility; accurate admission token estimates; standard response metadata. | None | Implemented |
| M2 | [Provider and catalog breadth](m02-provider-catalog.md) | LiteLLM's production provider families reachable through certified tiers; media providers; a signed reference catalog of model facts and prices. | None | Implemented |
| M3 | [Adaptive routing and resilience](m03-routing-resilience.md) | Cross-route fallbacks, capacity-aware selection, priority admission, supply-side budgets, active and fleet-shared health, shadow traffic, explainable request selectors. | M1 | Implemented |
| M4 | [Tenancy, identity and budgets](m04-tenancy-identity.md) | End users, a budget hierarchy with flexible windows, limit templates, route groups, workload JWTs, SAML, SCIM, MFA, organizations and caller-supplied credentials. | None | Implemented |
| M5 | [Observability, export and alerting](m05-observability.md) | Durable export sinks, opt-in payload capture, OpenTelemetry GenAI conventions, business metrics, alert channels and events. | M4 | Implemented |
| M6 | [Cost management and chargeback](m06-cost-management.md) | Complete pricing dimensions, rate cards, cost estimation, FOCUS and billing exports, invoice reconciliation. | M2, M4, M5 | Planned |
| M7 | [Guardrails platform](m07-guardrails.md) | One guardrail engine with built-in detectors, vendor adapters, webhook and WebAssembly guardrails, streaming inspection and tool governance. | M1 | Planned |
| M8 | [Response caching](m08-caching.md) | Sealed exact and semantic response caches, cache controls and provider prompt-cache automation. | M7 | Planned |
| M9 | [API surface completion](m09-api-surface.md) | Legacy completions, cross-provider files and batches, fine-tuning, vector stores, Responses completion, realtime expansion, OCR, search, provider-retained resources and governed pass-through. | M2 | Planned |
| M10 | [Agent gateway](m10-agent-gateway.md) | An MCP gateway with pinned tools, gateway-executed tools, an A2A agent gateway and a prompt registry. | M4, M7 | Planned |
| M11 | [Operator ecosystem](m11-operator-ecosystem.md) | A management CLI, a Terraform provider, KMS and external secret stores, a developer catalog, multi-region gateways and a management MCP server. | M4 | Implemented |


## Parity gate and scorecard

Parity is reached when every row of the [parity matrix](parity.md) is
`Parity`, `Ahead` or `Excluded`. Progress is reported with these measures,
recomputed whenever a milestone closes:

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
The current 150 capability rows reflect M1–M5 and M11: 23 `Ahead`, 81 `Parity`,
11 `Partial`, 33 `Gap`, 2 `Excluded`; coverage is 104/148 (70%). Status rows in the
legend are not capabilities. Definitions/registrations alone do not close broader
guardrail-execution or agent-gateway workstreams.

## Definition of done

Every milestone, and every workstream inside it, ships with:

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
9. **Documentation.** User-facing guides updated in the same change, and the
   [parity matrix](parity.md) rows the work closes moved to `Parity` or `Ahead`.

## Out of scope by design

These LiteLLM capabilities conflict with OLP's guarantees or have been retired
upstream. The parity matrix marks them `Excluded`.

| Capability | Reason |
| --- | --- |
| An embeddable Python SDK | OLP is a gateway. Official vendor SDKs are its clients, which keeps client compatibility the product rather than a translation layer. |
| OpenAI Assistants API | OpenAI scheduled its shutdown for 2026-08-26, as the [LiteLLM page](https://docs.litellm.ai/docs/assistants) notes. Responses and Conversations replace it ([M9](m09-api-surface.md)). |
| Caller-chosen upstream base URLs | Destinations stay operator-declared and egress-validated. Caller-supplied credentials for an operator-declared connection are in scope ([M4](m04-tenancy-identity.md#m46-caller-supplied-provider-credentials)). |
| Uncertified wildcard model passthrough | Routes publish only certified capabilities. Route templates create ordinary routes for newly certified models ([M3](m03-routing-resilience.md#m38-route-templates)). |
| In-process custom code hooks | Custom authentication, callbacks and guardrails run as confined WebAssembly plugins or behind signed webhook contracts, never inside the gateway process. |
| `/memory` key-value storage | Application state that no provider API defines. Applications should keep it in their own stores. |
| Prompt and response storage in request history | Durable request records stay content-free. Opt-in payload capture streams to an operator-owned sink instead ([M5](m05-observability.md#m52-payload-capture)). |

## Maintaining this roadmap

- **Status.** Each milestone file carries `Planned`, `In progress`,
  `Implemented` or `Shipped`. `Implemented` records completed code and focused
  verification with any explicit qualification handoff; `Shipped` additionally
  records completed release qualification. A status change also updates the
  table above and affected parity rows.
- **Decisions.** Each milestone lists the decisions to settle before
  implementation. Record the outcome in the milestone file, with its rationale,
  before the first implementation change lands.
- **Re-baselining.** When a milestone closes, review LiteLLM's documentation and
  [release notes](https://docs.litellm.ai/release_notes). New capabilities
  become `Gap` rows assigned to a milestone, or `Excluded` rows with a reason.
- **Scope changes.** Edit the milestone specification in the change that
  proposes the new scope, so the roadmap never trails the code.
