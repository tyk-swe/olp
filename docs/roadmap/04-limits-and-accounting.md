# M4: Distributed limits, pricing, and recovery

[Roadmap](README.md) | [Previous: core gateway](03-core-gateway.md) |
[Next: providers and routing](05-provider-and-routing-parity.md)

**Status:** Complete (2026-09-18). **Prerequisites:** M3 complete.

[Final qualification and native evidence](evidence/release-qualification.md).

[M4 evidence](evidence/limits-and-accounting.md) |
[Operations guide](../go-gateway.md)

Complete distributed enforcement and durable attempt accounting before adding
routing strategies that depend on price and performance history. PostgreSQL
owns durable facts and spend; GLIDE connects to Valkey for coordination.

Every ticket below is implemented under `internal/limits/`, `internal/usage/`,
and the gateway, access, and process packages, with unit, integration,
multi-process, and browser qualification recorded in the evidence document.
M4 added no third-party dependency. M7 closes the native arm64 gate and
qualifies the complete dependency graph. The formerly deferred
`olp_limits_fail_open_total` counter is now exported by M6 observability, and
both PostgreSQL and Valkey TLS tests pass in the final service run.

## Backlog

### M4-01

- [x] **Implement distributed rate and concurrency enforcement.**

**Depends on:** Milestone prerequisites.

**Deliver:** Port the key, connection, and credential-slot admission scripts
through GLIDE, including request/token windows, server-time decisions,
concurrency leases, refunds, renewal/reconciliation, and Retry-After.
Connect reservations and cleanup to the M3 lifecycle.

**Accept:** Competing gateway replicas enforce shared limits. A rejected
reservation does not leave unrelated dimensions charged. Duplicate cleanup is
harmless, cancellation releases held concurrency, and abandoned leases expire.
Configured provider quotas fail closed; preserve the documented installation
override for eligible key rate/concurrency limits only.

**References:** [Distributed limits](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits/distributed.rs),
[Lua scripts](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits),
[Valkey limit tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/distributed_limits_valkey.rs),
[configuration](../configuration.md).

### M4-02

- [x] **Implement exact pricing and revision provenance.**

**Depends on:** Milestone prerequisites.

**Deliver:** Restore pricing currency, effective revisions, connection/vendor/
connector scope resolution, token and media unit rates, and explicit unpriced
decisions. Carry the selected price revision through each admitted attempt.
Use exact decimal arithmetic across database, Go, wire values, and Valkey scripts.

**Accept:** Scope precedence and effective dates select the expected rate.
Fractional prices, cached-input usage, missing components, and all retained
unit types have fixtures. Binary floating-point rounding cannot alter budget
or price-ceiling decisions. Missing usage/pricing stays visible as unpriced.

**References:** [Pricing](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/pricing.rs),
[pricing API](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/pricing_http.rs),
[concepts](../concepts.md),
[attempt accounting tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/operations_postgres/attempt_accounting.rs).

### M4-03

- [x] **Persist terminal accounting without duplicate attribution.**

**Depends on:** [M4-02](#m4-02).

**Deliver:** Connect M3 terminal events to durable requests, ordered attempts,
usage facts, provenance, and aggregate updates. Enforce request/attempt/event
uniqueness and use transactional deduplication when retrying persistence.

**Accept:** Duplicate delivery or a crash after commit cannot double-count
usage or cost. Failed/retried attempts and partial stream usage remain attached
to their own identities. Missing usage remains incomplete; no usage or price
is fabricated to close an event. Durable facts and aggregate changes agree.

**References:** [Ingestion persistence](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/ingestion/persistence.rs),
[attempt facts](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/ingestion/persistence/attempt_facts.rs),
[batched writes](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/batched_writes_postgres.rs),
[attempt HTTP tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/system/operations_http_postgres/attempts.rs).

### M4-04

- [x] **Restore recoverable metadata delivery and completeness.**

**Depends on:** [M4-03](#m4-03).

**Deliver:** Implement bounded GLIDE Streams producers/consumers, pending-entry
recovery, acknowledgement after persistence, consumer health, gateway epochs,
loss/gap accounting, and wire-version handling. Namespace every queue and
coordination key by the Go installation.

**Accept:** Restarting workers recovers pending work without duplicate
attribution. Unsupported event versions remain pending; malformed/lost records
follow an explicit recorded-loss policy. Queue acknowledgements are never
described as an fsync guarantee. Separate installations sharing Valkey cannot
consume or acknowledge one another's events.

**References:** [Queue](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/queue),
[ingestion](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/ingestion),
[consumer tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/request_metadata_consumer_valkey),
[shared Valkey](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/ha/shared_valkey.rs).

### M4-05

- [x] **Restore UTC accrued-spend admission.**

**Depends on:** [M4-01](#m4-01), [M4-02](#m4-02), [M4-03](#m4-03).

**Deliver:** Implement daily/monthly key budget checks against authoritative
cumulative spend snapshots. Preserve exact thresholds, window ends,
unpriced-attempt counts, exhausted-budget responses, and unknown-state errors.

**Accept:** Missing, malformed, or wrong-window spend fails closed, including
new keys and new windows pending initialization. Admission never initializes
unknown spend to zero. Exhaustion returns 429; unavailable valid state returns
503. Concurrent admitted work may exceed accrued thresholds, and unpriced work
accrues no money; the API and UI preserve these documented limitations.

**References:** [Budget admission](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits/budgets.rs),
[spend controls](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/spend_controls_postgres.rs),
[budget recovery](../spend-budget-recovery.md),
[production contracts](../production-guarantees.md).

### M4-06

- [x] **Reconcile spend and recover worker leadership.**

**Depends on:** [M4-04](#m4-04), [M4-05](#m4-05).

**Deliver:** Rebuild and monotonically apply current-window PostgreSQL totals
to Valkey. Restore dedicated-session leadership, bounded reconciliation passes,
worker health/checkpoints, and safe release of session-level advisory locks on
errors, cancellation, and shutdown.

**Accept:** Valkey loss or malformed values recover from matching durable
snapshots. A stale/future snapshot cannot initialize another window or lower
valid spend. An errored leader relinquishes ownership without returning a
locked connection to the pool. Follower skips do not claim successful repair,
and inflated valid counters remain a reported repair condition.

**References:** [Cost reconciliation](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits/distributed/cost_reconciliation.rs),
[reconciliation worker](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/ingestion/reconciliation.rs),
[spend recovery tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/spend_recovery_postgres.rs),
[worker HA](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/worker_ha_postgres.rs).

### M4-07

- [x] **Restore request history, analytics, and retention.**

**Depends on:** [M4-03](#m4-03), [M4-04](#m4-04).

**Deliver:** Implement request/attempt detail and filtering, usage summaries,
breakdowns and series, pricing provenance, completeness queries, and bounded
retention/aggregation workers. Keep expensive analytics in control processes.
Expose successful-attempt timing and token measurements needed by M5 routing.

**Accept:** Pagination and aggregates agree with retained facts; retries remain
visible as distinct attempts. Gaps, unpriced work, and incomplete usage stay
visible after aggregation. Retention respects durable media/reference needs
and does not invalidate current spend reconstruction or worker recovery.

**References:** [History](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/history.rs),
[reports](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/reports), [retention](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage/retention.rs),
[query tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/operations_postgres/query_contracts.rs).

### M4-08

- [x] **Connect usage, history, pricing, and quota views.**

**Depends on:** [M4-01](#m4-01), [M4-05](#m4-05), [M4-06](#m4-06), [M4-07](#m4-07).

**Deliver:** Adapt usage charts, pricing controls, request timelines, key
budget/limit views, and connection/slot quota displays to Go. Present current
window ends, accrued values, unknown initialization state, unpriced attempts,
and delivery completeness.

**Accept:** Values and filtering agree with persisted fixtures, including
fractional prices and multiple attempts. Loading, unavailable, exhausted, and
incomplete states are distinct. Console claims match accrued-cost enforcement
and recovery limits. Regenerated contracts and UI checks pass.

**References:** [Usage console](../../console/src/lib/features/usage/),
[key console](../../console/src/lib/features/access/api-keys/),
[credential pool](../../console/src/lib/features/providers/ProviderCredentialPool.svelte),
[history journeys](../../console/tests/journeys/request-history.ts).

### M4-09

- [x] **Qualify multi-process accounting and outage recovery.**

**Depends on:** [M4-06](#m4-06), [M4-08](#m4-08).

**Deliver:** Port the shared-service, distributed-limit, durable accounting,
consumer-gap, and worker-recovery scenarios to the Go process harness.
Exercise two gateway/worker replicas and isolated installations sharing Valkey.

**Accept:** Cover failures before/after database commit and queue acknowledgement,
duplicate delivery, lost connections, cancellation, expired leases, UTC
boundaries, and leader death. Prove no duplicate attribution or cross-installation
consumption, and report irrecoverable gaps honestly. All M4 UI journeys pass.

**References:** [Distributed process tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract/distributed_limits.rs),
[durable contracts](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract/durable.rs),
[HA recovery](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/ha/worker_recovery.rs),
[fault injection](../../deploy/toxiproxy.integration.json).

## Exit scenarios

- Enforce shared key/connection/slot limits across gateway replicas.
- Reconcile duplicate, cancelled, retried, partial, and unpriced attempts.
- Exercise new/expired UTC spend windows and missing/corrupt Valkey state.
- Restart or isolate workers/PostgreSQL/Valkey around persistence and acknowledgement.
- Verify visible delivery gaps, retained history, exact pricing, and budget UI states.
- Update the dependency inventory and capability map with the completed evidence.
