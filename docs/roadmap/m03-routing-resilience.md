# M3: Adaptive routing and resilience

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M1](m01-measured-advantage.md) | [M4](m04-tenancy-identity.md) (per-route key limits), [M5](m05-observability.md) (circuit events, sessions), [M7](m07-guardrails.md) (guardrails on shadow traffic), [M8](m08-caching.md) (prompt-cache keys) | None |

OLP's planner already orders attempts by priority, preferred order and strategy
under hard policy constraints, and explains the result through simulation. It
lacks the adaptive behaviors LiteLLM offers: cross-model fallbacks,
capacity-aware selection, queueing, supply-side budgets, proactive health,
mirroring and automatic request routing. This milestone adds them inside the
existing planner, so every new behavior is deterministic, bounded and visible in
route simulation and plan decisions.

## Outcome

- Routes declare fallbacks to other routes for named failure classes.
- Selection can prefer the credential slots with the most remaining capacity,
  and keep a session on the target that holds its prompt cache.
- Saturated gateways queue work by priority instead of refusing it, and
  provider capacity can be reserved per priority.
- Connections and slots carry spend caps that remove them from selection.
- Health is proactive and shared across the gateway fleet.
- Operators can mirror traffic to candidate targets and route requests by
  declared rules, including classifier results.
- Route templates publish newly certified models without uncertified wildcard
  passthrough.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Strategies | `weighted`, `price`, `latency`, `throughput` within priority and preferred-order tiers ([policies](../provider-routing.md#policies-and-caller-preferences)) | Weighted pick, rate-limit aware, latency, least busy, cost, custom ([routing](https://docs.litellm.ai/docs/routing)) |
| Failover | Within a route, before commitment, for connect, timeout, rate-limit, credential, server and context-window failures ([request path](../gateway.md#request-path), [`attempts.go`](../../internal/gateway/attempts.go)). Strict routes, the default, restrict later attempts to the first serving identity and model, and pin the slot when the principal is unknown. They can still dispatch again to an eligible candidate with that identity after an uncommitted retryable failure; at-most-once dispatch is not guaranteed ([`interaction.go`](../../internal/gateway/interaction.go), [same-principal slot test](../../internal/gateway/interaction_test.go)) | General, context-window and content-policy fallbacks across model groups ([reliability](https://docs.litellm.ai/docs/proxy/reliability)) |
| Health | Per-gateway circuits (five counted failures in 30 seconds open for 30 seconds); credential and slot cooldowns shared in Valkey | Background health checks that remove deployments ([health check routing](https://docs.litellm.ai/docs/proxy/health_check_routing)) |
| Overload | A full admission pool answers 503 with `Retry-After: 1`. The pool admits by path, before authentication ([`public.go`](../../internal/observability/public.go)) | A [priority queue](https://docs.litellm.ai/docs/scheduler) and [priority capacity shares](https://docs.litellm.ai/docs/proxy/dynamic_rate_limit) |
| Caller controls | `X-OLP-Routing` picks a strategy the policy allows, orders targets, narrows constraints and lowers `max_attempts` | Request metadata, tags and [budget fallbacks](https://docs.litellm.ai/docs/proxy/budget_fallbacks) |
| Automation | Bulk route creation in the console | [Auto router](https://docs.litellm.ai/docs/auto_router/), [adaptive router](https://docs.litellm.ai/docs/adaptive_router), [routing plugins](https://docs.litellm.ai/docs/routing_plugins), [wildcards](https://docs.litellm.ai/docs/wildcard_routing), [mirroring](https://docs.litellm.ai/docs/traffic_mirroring) |

## Invariants

Every workstream preserves these rules from [concepts](../concepts.md) and
[gateway execution](../gateway.md):

- A request can narrow published policy, never widen it.
- A committed stream never restarts on another target, and an ambiguous
  resource creation never repeats.
- A strict route keeps one serving identity and model binding per request;
  an unknown principal also pins its credential slot.
- Every attempt, including fallback and shadow attempts, pins its revisions and
  produces its own record.
- Simulation explains the same decision the gateway makes, without contacting a
  provider.

M3.9 adds a stricter rule, also applied by M3.1: once a strict request has been
sent, it is never retried on the same target or another. This is a planned
tightening of the current attempt loop, not an existing at-most-once guarantee.

## Scope

### M3.1 Fallback routes

A route revision gains an ordered `fallbacks` list:

```json
{
  "fallbacks": [
    { "route": "assistant-long-context", "on": ["context_window"] },
    { "route": "assistant-backup", "on": ["exhausted", "content_filter"] }
  ]
}
```

- `on` names the conditions that start a fallback: `exhausted` (every
  attempt failed with a retryable class), `context_window`, `content_filter`,
  `rate_limit` and `budget`.
- `budget` fires on a supply-side cap from M3.4, and on the caller key's own
  limit for this route once [M4.2](m04-tenancy-identity.md#m42-budget-hierarchy-windows-and-templates)
  adds per-route limits. The key's overall budget still answers
  `429 budget_exhausted`: a fallback moves spend to another route, never past
  the key's ceiling.
- `content_filter` is a new class in [`internal/upstream`](../../internal/upstream/upstream.go)
  for typed upstream refusals such as content-filter errors. It does not count
  against provider health.
- Fallback routes run with their own targets, policy and fidelity. A strict
  route may fall back only to strict routes. The key must be permitted to use
  the fallback route; otherwise the plan records `fallback_route_forbidden` and
  skips it.
- On a strict route a fallback starts only while no dispatch has been admitted:
  from planning-time conditions such as `context_window` by model facts,
  `budget`, or no eligible target. It follows the planned post-send prohibition
  in M3.9 rather than today's more permissive same-identity attempt loop.
- The primary route's overall deadline and attempt budget bound the whole
  request, fallbacks included.
- Fallback graphs must be acyclic, stay within one project boundary and be at
  most three routes deep. Publication rejects anything else.

### M3.2 Capacity-aware selection and session affinity

- A `capacity` strategy orders credential slots by remaining headroom, the
  smallest of their remaining request, token and concurrency fractions read
  from the same Valkey windows that admission uses, in one pipelined read per
  request. Slots without quotas rank after slots with known headroom, as
  unknown prices rank after known ones.
- **Input and output token quotas.** Connection and slot quotas may declare
  separate input (ITPM) and output (OTPM) token windows beside the combined one,
  for providers that publish separate limits. Before every provider attempt,
  distributed admission reserves the effective request's conservative input
  and output estimates independently in shared Valkey windows; a configured
  combined quota reserves their sum. Each scope atomically checks all its
  configured dimensions, and both connection and slot reservations must succeed
  before dispatch. A refusal refunds any partial reservation. Settlement
  reconciles each dimension against its own complete usage, retaining the
  conservative reservation and recording a gap for any unknown dimension;
  combined usage requires both counts. Undispatched attempts refund all token
  reservations, and retries reserve anew. Unreadable quota state fails closed.
  Headroom uses whichever remaining fraction is tightest; capacity ranking
  never substitutes for these admission checks.
- **Sessions.** A session is a caller-chosen key that groups related requests:
  the value of the attribution label a route names as its session label, or
  the dialect's own cache key (OpenAI `prompt_cache_key`). This is the one
  definition of a session in the roadmap;
  [M5.5](m05-observability.md#m55-reports-and-sessions) and
  [M8.3](m08-caching.md#m83-provider-prompt-cache-automation) reuse it.
- **Session affinity.** A route may set `affinity` to keep requests with the
  same session on the same target and slot while it stays eligible. The
  session is hashed into the rendezvous seed, which raises provider
  prompt-cache hit rates without any gateway state. Affinity stores nothing of
  its own: a session label is recorded as attribution, like any label, and a
  dialect cache key is not recorded at all.

### M3.3 Priority admission and capacity reservation

- **Admission queue.** A bounded queue holds inference requests when the
  gateway is saturated, with four classes: `critical`, `high`, `normal` and
  `low`. Dequeueing is weighted fair (8:4:2:1), so lower classes are never
  starved. A queued request waits at most
  `OLP_HTTP_ADMISSION_QUEUE_TIMEOUT` and never beyond its route deadline, then
  receives today's 503. Queue depth is bounded by
  `OLP_HTTP_ADMISSION_QUEUE_DEPTH`.
- **Placement.** Today's pool admits by path before authentication, where
  neither the key's priority nor the route deadline is known. The queue
  therefore sits after authentication and guards dispatch. The outer pool
  keeps bounding connections and unauthenticated work, sized to hold the queue
  depth.
- **Priority source.** A key policy sets a default priority and a
  `max_priority`. Requests may choose a class up to that ceiling through
  `X-OLP-Routing`, for example `{"priority":"high"}`; a higher value is refused
  as an attempt to widen policy, like a `max_attempts` above the policy's.
- **Capacity shares.** A connection or slot quota may declare per-priority
  shares and a saturation threshold. Below the threshold any class may use idle
  capacity; above it, each class is held to its share. The Valkey quota scripts
  enforce shares atomically with the existing windows.
- Metrics: `olp_admission_queue_depth{class}`,
  `olp_admission_queue_wait_seconds{class}` and rejections by class.

### M3.4 Supply-side budgets

Connections, credential slots and routes gain optional daily and monthly cost
caps. They use the same exact-decimal accounting, PostgreSQL authority and
Valkey snapshots as key budgets ([spend reconciliation](../operations.md#spend-budget-reconciliation)).
An exhausted cap removes the connection, slot or route from selection with the
plan reason `connection_budget_exhausted`, `slot_budget_exhausted` or
`route_budget_exhausted`, which can start a `budget` fallback. A missing or
malformed snapshot skips the target, matching how unreadable provider quotas
behave today. Like key budgets, a cap refuses work once accrued spend reaches
it, so in-flight work can overshoot by the cost admitted between
reconciliations.

### M3.5 Proactive and fleet-shared health

- **Shared circuits.** Gateways publish circuit transitions to Valkey with
  bounded staleness, so a provider that fails on one replica is avoided by all
  of them. Local circuits remain the fallback when Valkey is unavailable.
- **Active probes.** A worker task, `health_probes`, sends bounded synthetic
  requests to targets whose connection enables probing, using the certification
  probe codecs with minimal output. Results join the shared health state.
  Probes are opt-in because they cost money; their usage is accounted to the
  installation under a `system` actor and appears in usage reports.
- Targets marked unhealthy move to the end of the attempt order rather than
  disappearing, so a fleet-wide false positive degrades latency instead of
  availability.
- `GET /api/v1/provider-health` reports probe results and shared circuit state.
- The `provider.circuit.open` and `provider.circuit.closed` events ship with
  whichever of this workstream and
  [M5.4](m05-observability.md#m54-alert-channels-and-events) lands second.

### M3.6 Shadow traffic

A route target may be declared `shadow` with a sample ratio. Sampled requests
are mirrored to it after admission:

- The caller's response comes only from the primary path. Shadow attempts run
  in their own low-priority admission pool, with their own deadline, and are
  dropped rather than queued when capacity is short.
- Shadow targets must satisfy every hard constraint the request is subject to,
  including region, data collection and zero data retention, and the request's
  content policy. Once [M7](m07-guardrails.md) ships, the same holds for its
  guardrails.
- Only stateless generation, embeddings and rerank are mirrored. Stateful
  Responses fields, files, batches, realtime and media uploads never are.
- Shadow attempts are recorded with `shadow: true` and accounted to the route,
  not to the caller's key or budgets.
- An experiment report compares the primary and shadow paths by status, latency,
  time to first token, token usage and cost, from metadata only.

### M3.7 Request selectors

A route revision may carry ordered `selectors`. Each selector has a predicate
and an action that chooses a subset of the route's targets or delegates to
another route:

- **Predicates** use features admission derives from the decoded request:
  operation, estimated input tokens, requested output tokens, streaming, tool
  presence, input modalities, structured-output requests and reasoning effort.
  Admission already has the operation, the token estimate and the supplied
  parameter names; this workstream adds modalities and reasoning effort to
  that view.
- **Labels.** Attribution labels never take part in predicates. A session label
  may seed affinity (M3.2) within the eligible set, but no label changes which
  targets are eligible.
- **Classifier predicates** send the request text to an OLP classification or
  generation route, for example a TEI classifier, and test the returned label.
  The classifier call is an accounted request with its own deadline; a
  classifier failure falls through to the next selector.
- **Plugin predicates** run a confined WebAssembly function from an approved
  [plugin](../plugins.md) over the same features. A plugin predicate can only
  narrow the candidate set.
- The first matching selector wins, and plan decisions record its identifier.
  Route simulation evaluates selectors against a sample request, so tiered
  routing (for example simple, complex and reasoning tiers) is testable before
  publication.
- Usage reports compare each selector's cost with the route's most expensive
  eligible target, reporting savings without inventing usage.

### M3.8 Route templates

A route template turns certified models into ordinary routes. It names a
provider selector (`vendor:<id>` or `provider:<uuid>`), a model filter, a slug
pattern built from the canonical model identity, a fidelity, a policy and
whether to publish automatically. When a provider activation certifies a model
that matches, the template creates a route draft, or a published route when
`auto_publish` is set. Every generated route is a normal route, with its own
revision history, simulation and access control, so nothing reaches a caller
without certification.

### M3.9 Retry policy

A route revision may declare `retry` per retryable failure class: a maximum
number of same-target retries, a base and maximum backoff with full jitter, and
whether to honor `Retry-After`. Retries consume the attempt budget and the
overall deadline, never apply after commitment or to ambiguous creations, and
are recorded as attempts.

This workstream tightens strict execution beyond today's serving-identity
restriction: a same-target retry is allowed only when transport evidence proves
the request was never sent. A connect error alone is not sufficient evidence.
After a send, or when sending is uncertain, a strict route returns the failure
without another dispatch, including for 429, 5xx and timeout failures and even
when another slot has the same known principal. Post-send retries remain
available only on transformed routes, subject to the commitment and ambiguous
creation rules above. Tests that currently permit same-principal retry after
429 change with this workstream.

## Non-goals

- Selection that simulation cannot reproduce, such as routing weights learned
  online. Classifier predicates are declared rules with recorded outcomes.
- Uncertified wildcard passthrough. Route templates create certified routes
  instead.
- In-process routing code. Custom predicates are confined WebAssembly
  functions.
- Mirroring stateful or content-retaining operations.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Fallbacks, selectors, retry policy, affinity and shadow settings | Route drafts and immutable revisions in PostgreSQL; the runtime snapshot | As route revisions today | None |
| Route templates | PostgreSQL | Until deleted | None |
| Supply-side spend counters | PostgreSQL authority, Valkey snapshots | As key budget windows today | None |
| Input, output and combined quota reservations and counters | Shared limits Valkey | The quota window | None; metadata only |
| Shared circuit and probe state | Valkey | The staleness bound | None |
| Shadow, probe and classifier attempts | Attempt records in PostgreSQL | Request retention | None; metadata only |
| Session labels | Request records, as attribution, as labels are today | Request retention | None |
| Dialect cache keys used as sessions | Request memory, hashed into the selection seed | Never stored | None |

## Change map

| Change | Start here |
| --- | --- |
| Fallbacks, selectors and retry policy as configuration | `internal/routes/`, `internal/runtime/plan.go`, `internal/configuration/` |
| Fallback, retry and shadow execution | `internal/gateway/attempts.go`, `internal/gateway/interaction.go` |
| Capacity reads, shares and supply-side caps | `internal/limits/` |
| Admission queue | `internal/observability/admission.go`, `internal/observability/public.go`, `internal/gateway/server.go` |
| Shared circuits and probes | `internal/gateway/health.go`, `internal/process/workers.go` |
| Route templates | `internal/routes/`, provider activation in `internal/providers/` |
| Console | `console/src/lib/features/routes/`, bulk creation in `console/src/lib/features/providers/` |

## Decisions to settle

1. Whether selectors live on ordinary routes or on a distinct router route kind
   (recommended: ordinary routes, so one route model covers every behavior).
2. The staleness bound for shared circuit state (recommended: five seconds,
   matching key-authority polling).
3. Whether shadow traffic may target another project's routes (recommended: no).
4. Confirm M3.9's proposed tightening: strict routes exclude all post-send
   retries, including explicit rate-limit rejections that today's loop can
   retry within one serving identity. Implement and test that change together;
   it must not be claimed as baseline behavior. Under the proposed contract,
   operators who want post-send retry resilience use transformed routes.
5. Where the priority queue sits relative to the pre-authentication pool
   (recommended: after authentication, with the outer pool sized to the queue
   depth).

## Exit criteria

- [ ] **M3.1, M3.7, M3.8, M3.9** Fallbacks, selectors, templates and retry
      policy round-trip through route drafts, revisions, restore and
      configuration export, plan and apply.
- [ ] **M3.1, M3.2, M3.7** Route simulation explains fallbacks, selector
      matches, capacity ordering and affinity for a given request.
- [ ] **M3.2** Reservation and settlement tests independently exercise input,
      output and combined token counters at both connection and slot scope,
      including partial-admission rollback, undispatched refunds, retries,
      missing usage, duplicate settlement and window rollover.
- [ ] **M3.2** Two gateways concurrently exhaust ITPM while OTPM and combined
      headroom remain, then OTPM while ITPM and combined headroom remain, at
      each quota scope. No over-limit attempt dispatches; settlement restores
      unused reservations using each counter's corresponding usage, and unreadable
      quota state never admits unmetered work.
- [ ] **M3.1, M3.9** Integration tests prove that fallbacks and retries never
      follow a committed stream, and that the new strict rule prevents a second
      send after 429, 5xx, post-send timeout or uncertain-send failures, even
      with another slot under the same principal. Proven pre-send failures may
      retry without leaving the serving identity, and a `budget` fallback never
      exceeds the key's overall budget.
- [ ] **M3.3** Requests cannot raise priority above the key's ceiling, the
      queue never exceeds its depth or its timeout, and capacity shares hold
      under concurrent load across two gateways.
- [ ] **M3.4** An exhausted connection, slot or route cap removes it from
      selection on every gateway, and the plan records the reason.
- [ ] **M3.5** A circuit opened on one gateway is honored by another within the
      staleness bound, and a failing probe moves its target to the end of the
      attempt order without removing it.
- [ ] **M3.6** Shadow attempts never change caller-visible latency in benchmark
      S1 and are excluded from key budgets.
- [ ] **M3.8** A newly certified model that matches a template becomes a route
      draft, or a published route only when `auto_publish` is set.
- [ ] Every workstream meets the [performance budget](m01-measured-advantage.md#performance-budget)
      when unconfigured; the `capacity` strategy adds at most one Valkey round
      trip per request.
- [ ] The [parity matrix](parity.md) routing rows are `Parity` or better.
