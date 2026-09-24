# Strict lifecycle final-source repeat, v3

This is a new **C-only, write-once** verification after the P10 connector
product change. The successful bc325 [v2 C artifact](../lifecycle-v2/strict-candidate.jsonl)
and original [v1 budget](../lifecycle-v1/replacement-budgets.json) remain
unchanged. The method reuses the committed [v2 B artifact](../lifecycle-v2/baseline.jsonl)
and [v2 budget](../lifecycle-v2/replacement-budgets.json), including every old
path limit and all 24 original-source gateway-minus-relay latency limits.
No limit is recalculated from the new product.

The unchanged v2 integration harness supplies the same 48 public-service
repetitions, 1,152 successes/dispatches, 288 independent resource mappings and
retrievals, and 12 cross-owner zero-dispatch negatives. The v3 runner records
its own source and method hashes and applies the unchanged v2 inventory and
numeric comparator. It requires the previous passing C bytes, pre-candidate B
bytes and the committed [P10 product lock](../final-source-repeat-v1/README.md).
The C source must follow the method and lock commits. No caller-selected output
path or second capture is accepted.

After the P10 lock is committed and a clean descendant C source exists, keep
the same isolated PostgreSQL 18.6/no-TLS service and idle Valkey, Go 1.27.1,
same host/runtime/fixture settings, and pause unrelated compute. No paid
model call is used. The semantic-only integration suite can run before the
timed reservation. The sole timed command is:

```sh
OLP_LIFECYCLE_ROUTE_FIDELITY='{"mode":"strict"}' node scripts/fidelity-lifecycle-v3-benchmark.mjs record-strict
node scripts/fidelity-lifecycle-v3-benchmark.mjs compare
```

The fixed `strict-candidate.jsonl` path is reserved exclusively before Go.
An interruption or failure leaves its record and cannot be retried under v3.
The resulting scope is scripted local lifecycle cost, not production latency,
isolated gateway RSS or live-model quality.
