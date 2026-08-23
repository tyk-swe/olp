# Performance TODO

Audit baseline: release builds on an 8-core/22 GB host, PostgreSQL 18.6 with
migrations through `0033`, and Valkey on loopback. Treat extrapolations as
hypotheses.

LLM calls take 500 ms–60 s. Optimize serial network/database waits, retained-row
scans, and route fan-out—not isolated microseconds. Protocol translation measured
~2.7 µs per output token and authenticated inference performs no PostgreSQL queries,
so neither is currently capacity-limiting under the default 256-request admission
cap.

## 1. Index `attempt_usage_facts.event_id` — critical

`admit_request_metadata_receipt` in
`crates/olp-db/src/request_metadata/ingestion.rs` checks `event_id OR request_id`.
Only `request_id` is indexed, forcing a table scan. At 2M rows this took 146.3 ms;
adding the missing index reduced it to 0.133 ms. Growing consumer lag can fill the
8,192-event buffer and drop billing events.

- Change: add a forward migration for `attempt_usage_facts(event_id)`. Use a
  non-transactional concurrent index build for a live populated table.
- Verify: run `EXPLAIN (ANALYZE, BUFFERS)` at production scale, then compare stream
  lag and `olp_request_metadata_events_dropped_total` before and after deployment.

## 2. Batch retention work — critical

`Store::run_maintenance` in `crates/olp-db/src/maintenance.rs` performs the large
`requests`, `attempt_usage_facts`, `usage_facts`, and `audit_events` retention work
inside one transaction. Measured deletes took 8.5 s for 500k `usage_facts` rows and
4.0 s for 2M `attempt_usage_facts` rows, including disk spills. Catch-up after a
paused worker, restore, or retention reduction grows without a bound.

- Change: select bounded rows with `FOR UPDATE SKIP LOCKED`, delete and roll up that
  batch, commit, and repeat. Reuse the receipt-retention pattern already in the
  function. Preserve the additive `ON CONFLICT` rollups.
- Verify: seed 5M expired rows and record wall time, WAL, temp bytes, and transaction
  age. Keep each transaction below roughly one second and confirm readers tolerate
  within-pass eventual consistency.

## 3. Remove per-target request clones — high

`select_representable_attempts_filtered` calls `operation_for_provider` once per
route target, and `failover.rs` clones the selected operation again. A 2 MB request
with 32 targets measured 4.2 ms of CPU and about 64 MB of transient allocation
before the upstream call.

- Change: validate by borrowing the canonical operation while ignoring the
  delivery-only `/__olp/openai_endpoint` hint for non-OpenAI providers. Materialize
  a provider operation only for a selected attempt. Preserve the current
  invalid-request versus unavailable classification.
- Verify: benchmark 2/8/32 targets with a 2 MB body; selection cost should no longer
  scale with clone size. Keep tests proving the endpoint hint never reaches another
  protocol.

## 4. Detach unary limit reconciliation — high

Unary chat waits for `reservation.reconcile(actual)` before returning the response;
streaming performs the same cleanup after the response is already handed off.
Valkey measured 12.3 ms at the worst observed 256-concurrency call. The configured
retry envelope can add about 825 ms when Valkey is unhealthy.

- Change: run unary reconciliation as detached cleanup, matching streaming and the
  existing cancellation cleanup. Preserve error logging and TTL recovery semantics.
- Verify: load-test unary chat at 256 concurrency against delayed Valkey and compare
  p99 latency and inference-slot occupancy. Confirm the hard-limit crash posture is
  acceptable.

## Conditional work

- Parallelize request-metadata writes and consumption only if lag remains after the
  `event_id` index. The writer and consumer are serial today, but their throughput
  ceiling has not been measured.
- Decide whether `requests` should use scheduled range partitions before its default
  partition becomes large. Otherwise replace the unused partitioning scheme with a
  plain indexed table.
- Give video creation a separate admission cap only if load tests reproduce database
  pool acquisition failures around 20 concurrent creates.
- Revisit Lua `Script` statics, native-passthrough event duplication, repeated JSON
  parsing, runtime-publication N+1 queries, and narrower redundant indexes only when
  profiling shows they matter. Their measured costs are below the four items above.
