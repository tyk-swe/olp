# Architecture and change map

The Go module owns production code under `internal/`; `cmd/olp` starts the CLI.
The console mirrors feature ownership under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/database/migrations/`.

| Change | Start here |
| --- | --- |
| Provider configuration, models, credentials, certification, revisions | `internal/providers/` and `console/src/lib/features/providers/` |
| Route drafts, target selection, publication, history | `internal/routes/` and `console/src/lib/features/routes/` |
| Users, sessions, OIDC, projects, API keys, budgets, tokens, audit | `internal/access/` and `console/src/lib/features/access/` |
| Installation settings and notification destinations/rules | `internal/access/` and console access/settings features |
| Configuration export, plan, and apply | `internal/configuration/` |
| Content-policy validation and matching | `internal/contentpolicy/` |
| Provider-resource mappings and stored-response accounting | `internal/resources/` and `internal/gateway/` |
| Admission, request execution, retries, cancellation | `internal/gateway/` |
| Immutable operation sources, envelopes, provenance and codec linking | `internal/oif/` |
| Ordered generation views and independent operation contracts | `internal/operations/` |
| Strict generation admission, continuation and semantic obligations | `internal/interaction/` |
| Registered non-generation strict operation contracts | `internal/operationplan/`, `internal/operationregistry/` |
| Strict media, batch, realtime and Gemini lifecycle contracts | `internal/mediacontract/`, `internal/durablecontract/`, `internal/realtimecontract/`, `internal/geminilifecycle/` |
| Legacy provider preparation and wire defaults | `internal/providerinvoke/` |
| OpenAI, Anthropic, Gemini, Bedrock codecs and legacy adapters | `internal/protocols/` |
| Immutable runtime publication, activation, authority refresh, strict contract compilation | `internal/runtime/` |
| Distributed reservations, rates, concurrency, cost budgets | `internal/limits/` |
| Accounting, pricing, request history, ingestion, retention, notification delivery | `internal/usage/` and `console/src/lib/features/usage/` |
| Uploads, durable media jobs, reconciliation | `internal/media/` and `console/src/lib/features/media/` |
| Configuration, connections, startup, shutdown | `internal/config/`, `internal/process/`, `internal/database/` |
| HTTP middleware, development origin, body limits | `internal/process/` |
| Outbound networking and secrets | `internal/egress/`, `internal/secrets/` |
| Metrics, traces, readiness, worker health | `internal/observability/` |

Feature packages own their SQL, transactions, and workflows; mutations receive
explicit audit provenance. `openapi/management.json` defines the management
contract. `make api` generates Go transport types and ignored TypeScript
declarations; `/api/v1/openapi.json` serves the embedded contract. Integration
tests check handler/contract parity.

Inference pins an immutable runtime snapshot. `internal/gateway/attempts.go`
owns shared attempt progression, reservations, settlement, health and failover
for canonical inference and ordinary media. `executor.go` and `media.go` retain
their transport deadlines, streaming commitment and delivery; adjacent resource,
video, Bedrock, and realtime paths handle their specific lifecycles. Protocol
codecs live in `internal/protocols/`. Independent key-authority refresh prevents
a failed activation from retaining revoked access.

OIF is an in-process contract, not a public API or another request authority.
`internal/oif` owns immutable JSON source spans, exact presence and numeric
representations, request/result/event envelopes, bounded blob references, and
explicit provenance. It imports no provider, transport, storage, or generation
implementation. `internal/operations/generation` owns ordered messages, nodes,
tool dependencies, candidate branches, and native control scopes. Dialect
adapters in `internal/protocols` derive those views from source and link them
through a trusted build-time registry; a non-generation operation does not
extend generation types.

Ingress rejects duplicate decoded keys and malformed UTF-8 or surrogate escapes
before dispatch. Compatibility request accessors return copies; resource and
content-policy changes create overlays without replacing the caller source.
Unary results retain their native source before model rewriting. Stream codecs
lift bounded native events before projection, including framing metadata, and
keep no unbounded event history. Existing protocol validators continue to own
terminal grammar, while the gateway retains cancellation and response commitment.

`protocols.PrepareTarget` records the existing legacy mapper's destination and
the defaults it actually applied. Explicit destination dialects cannot fall
back to another API. `protocols.PrepareIdentity` preserves native subtrees and
allows only registered model/resource/transport changes; it does not normalize
Responses input strings, rename token controls, or qualify an interaction by
itself. Strict contracts compile per published route target: `internal/interaction`
owns generation admission, `internal/operationplan` and the `internal/operationregistry`
composition root own the other unary operations, and `internal/mediacontract`,
`internal/durablecontract`, `internal/realtimecontract` and `internal/geminilifecycle`
own their surfaces. The planner still owns policy coverage, profile compatibility,
source requirements and client continuation; `internal/providerinvoke` retains the
legacy admission path. See [the source decision](adr/0001-immutable-operation-sources.md).

PostgreSQL owns durable state. Valkey coordinates limits, hints, and accounting
events; retries and deduplication support recovery. Durable media jobs and
provider resources retain their admitted credential references through rotation
and restart. Workers remain with the feature they maintain.

Process startup composes gateway, control, worker, and all-in-one modes, owning
resource lifetimes and listeners. Draining, metadata delivery, worker shutdown,
and trace flushing share one deadline. Production serves static console assets;
development uses Vite with configured API proxies.

See [gateway execution](gateway.md), [access control](access.md), and the
[worker runbook](operations.md#replicated-worker-health) for their operational
contracts. See [CONTRIBUTING.md](../CONTRIBUTING.md) for setup, checks,
integration, and the fresh-installation contract.
