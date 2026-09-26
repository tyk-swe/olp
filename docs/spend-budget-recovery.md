# Spend-budget initialization and recovery

A request must satisfy every configured key and budget-group cost window, each
backed by a valid, current Valkey snapshot. Missing state is unknown spend, not
evidence of zero spend. A new key, an evicted hash, a restarted Valkey, and an
expired UTC window therefore fail closed with
`503 distributed_limits_unavailable` until an authoritative snapshot arrives. A
known exhausted window continues to return HTTP 429 `budget_exhausted`.

The terminal accounting consumer and the reconciliation worker publish
cumulative PostgreSQL totals. Only these snapshot paths initialize a cost hash;
admission never creates a zero counter. With a healthy worker, a newly created
key or newly started UTC window normally waits for the next minute's
reconciliation pass. Keys without individual or group budgets keep their
rate/concurrency outage behavior. Do not remove a budget or write a synthetic
zero to work around initialization.

Malformed hashes, including a non-integer `unpriced` field and a key holding a
non-hash Redis value, are replaced from a matching authoritative snapshot.
Repair of one window must not lower the valid counter of the other window. Stale
and future snapshots cannot initialize a different current UTC window. Daily
aggregation uses the half-open interval `[daily_start, daily_end)`; accepted
clock-skewed events belonging to tomorrow must not exhaust today.

## Worker ownership

One dedicated PostgreSQL session holds the cost-reconciliation advisory lock
between minute ticks. The monthly reconstruction runs on that same session.
Other worker replicas report a skipped pass rather than repeating the scan. An
error, shutdown, cancellation, or the 120-second pass deadline drops the leader
session and makes leadership available to a later attempt. A dropped connection
is never returned to the pool holding a session-level lock.

Watch `olp_worker_task_healthy{task="cost_reconciliation"}` and its checkpoint
outcomes. Persistent initialization 503s require investigation of the worker,
PostgreSQL, Valkey, and their clocks. A skipped follower checkpoint is not proof
that the leader successfully reconciled the current window. If a normal
reconstruction exceeds the pass deadline, address the scan/application load
before enabling budgeted traffic; repeated timeouts are not successful repair.

## Recovery limits

The reconciler does not lower inflated but otherwise valid counters. Correcting
those requires a reviewed data repair; do not edit migration history, suppress
checksum validation, or reset counters to zero.
[Migrations](../internal/database/migrations/) are forward-only.

Resume budgeted traffic after authoritative initialization and reconciliation
are healthy. Concurrent admitted work may exceed a budget, and unpriced attempts
accrue no cost. See [production contracts](production-guarantees.md) for these
limits and [operations](operations.md#spend-budget-reconciliation) for incident
steps.
