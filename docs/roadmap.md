# Roadmap

Work is ordered by dependency, with no promised dates. Current behavior belongs
in the [Use](README.md#use-the-gateway) and [Operate](README.md#operate-an-installation)
guides. The table below tracks unfinished outcomes; the three development plans
retain binding designs and acceptance criteria. An implementation or registration
is not a release qualification result.

The capability comparison is frozen to LiteLLM v1.105.0 documentation reviewed
2026-10-13, including beta/Enterprise scope. The previous repository status review
was 2026-10-18; this consolidation adds no implementation evidence. The
[benchmark comparator](develop/performance.md#what-is-pinned) separately remains
v1.103.2 at its recorded digest. A newer documentation baseline never changes a
historical benchmark pin. Rebaseline deliberately, documenting scope, impact and
evidence; missing credentials, time or hardware do not justify an exclusion.

## Remaining outcomes

| Outcome | Dependencies | Accountable owner | Completion gate |
| --- | --- | --- | --- |
| Publish full-rate overhead, efficiency and capacity evidence | Frozen topology, pinned peers and candidate image | Benchmark / release maintainers | Clean-checkout S1–S6, S1-shadow and LiteLLM S1–S5, raw artifacts and all [Performance](develop/performance.md) targets |
| Measure non-OpenAI token calibration and live estimate accuracy | Vendor-count corpus, signed provenance | Catalog / provider maintainers | Anthropic/Gemini measured factors; live OpenAI exact-prefix counts; [qualification](develop/qualification.md#providers-and-token-estimates) |
| Qualify deployed provider, identity and operator integrations | Exact supported tuples and target accounts/topology | Connector / access / operations maintainers | Live provider/IdP/JWKS, signed artifacts, secret rotation, two-engine automation, regional failure and recovery [gates](develop/qualification.md) |
| Complete billing and sink qualification; extend cloud invoice imports | Published prices, commitment ledgers, scoped export delivery | Billing / export maintainers | Live OpenAI/Anthropic closed-day reconciliation and sink readback; AWS CUR, Google billing and Azure Cost Management adapters each need scoped fixtures/live import |
| Close guardrail and capture acceptance; expand named integration coverage | Immutable engine, scope authority, capture boundary | Guardrail / export / connector maintainers | [Guardrail plan](develop/plans/guardrails.md): service readback, six live adapters, S3 workload and reviewed 60-entry backlog; vendor coverage stays Partial |
| Qualify response caching on reference hardware | Guardrail revision/execution contracts and dedicated cache instance | Cache / performance maintainers | Hit/bypass S1 incremental p95 ≤1 ms, isolation, purge/outage/rotation; live prompt-cache claims separately evidenced |
| Qualify implemented completions, Decisions, OCR/search and files/batches | Endpoint ledger, certified tuples, inspection and prices | Protocol / resource / SDK maintainers | [API qualification](develop/qualification.md#api-surfaces), including DB-backed SDKs, console, live providers and release performance; current managed-batch and text-only limits stay explicit |
| Add fine-tuning, retrieval, Conversations/compaction and retained families | Historical ownership/credential affinity; durable provisional debits and settlement | Resource / accounting maintainers | [API plan](develop/plans/api-surface.md), nested-ID isolation, unknown-create/restart, finite priced units and privacy |
| Expand realtime and pending OCR/search; add governed pass-through | Transport-specific control, metering, revocation and egress | Realtime / connector / security maintainers | [API plan](develop/plans/api-surface.md), live close/usage, field fidelity and adversarial path/policy tests |
| Serve consumer MCP, gateway tool loops and A2A | Existing MCP registry, tool governance, authority, prices and provisional ledger | MCP / agent maintainers | [Agent plan](develop/plans/agent-gateway.md), pinned drift checks, grant isolation, bounded approvals/effects, one financial attempt per step |
| Add prompts, agent traces and delegated identities | Agent publication/run/identity contracts; content-free exports | Agent / access maintainers | Immutable rendering, origin-labelled scoped traces, pair budgets, consent and revocation in the [agent plan](develop/plans/agent-gateway.md) |
| Qualify native coding subscriptions and unresolved bounds | Authorized live accounts and exact client/model/transport | Code-mode / qualification maintainers | [Code qualification](develop/qualification.md#code-mode-qualification), enrollment/allowance/inference and provider-enforced hard bounds; fresh HTTP-only review needs an explicit root-registration decision |

Independent lanes may proceed once their entry contract holds. Retained paid work
cannot ship before safe settlement/exposure accounting. Agent retrieval uses the
certified query operation. Existing management MCP and upstream registration do
not establish consumer tool execution. New surfaces must enforce compatible
guardrail/cache policies or reject them before dispatch.

Deferred provider breadth remains conditional: Vertex Veo needs an agreed result
and deletion contract; fal needs a certifiable authentication path; NVIDIA rerank
needs representable raw scores. Vendor speech translation, media streaming and
non-OpenAI image variations remain follow-up work. Reopening a decline requires
a reviewed contract, pricing/usage and certification evidence; see
[Compatibility](use/compatibility.md#provider-qualification-and-support-boundaries).
Cloud invoice adapters must define authoritative scope, revision, currency,
UTC-day mapping and idempotent late corrections with pagination/empty-day/duplicate
fixtures. They do not block the explicitly limited OpenAI/Anthropic tranche.

Every slice follows [change acceptance](develop/qualification.md#change-acceptance).
**Planned** means the contract exists but serving code does not; **In progress**
means implementation or scope closure remains; **Implemented** has focused code
checks and an owned qualification gap; **Shipped** requires candidate-specific
release evidence. Update this roadmap and the relevant guide/plan together.
