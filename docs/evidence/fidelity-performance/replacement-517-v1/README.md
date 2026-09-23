# Encrypted continuation barrier candidate at `517455c3`

The write-once [candidate](barrier-517-failed.json) is the full negotiated
OpenAI Chat-to-Anthropic two-turn continuation workload at clean product source
`517455c30a894f217faab4d4423fcd99bd0742c4`. It compares to the unchanged
[native encrypted barrier reference](../barrier-v1/README.md) and its frozen
[budgets](../barrier-v1/budgets.json), rather than resetting limits after an
earlier failure. The candidate JSON retains the exact source, runner, harness,
reference and budget hashes, raw test output, hardware, PostgreSQL 18.6/no-TLS
conditions, timestamps and one-minute host load (1.07 before, 2.48 after).
Its SHA-256 is
`c717939b224dc8deecd299e41731edb774f9c4348e954a2f32ad476836320368`.

All 12 repetitions completed: 288 two-turn workflows, 576 exact provider
dispatches, 5,472 native events, 576 fixture tool actions and 288 authenticated
ready reads. Every repetition recorded 24 workflows, 48 dispatches, 456 events,
48 actions, 24 ready reads and zero rejected workflows. The complete native
dependency and projected usage oracles passed. Completion is separate from
meeting the frozen timing limits.

The static candidate comparator failed **five** small-history, concurrency-1
limits:

| Frozen metric | Candidate median | Unchanged limit |
| --- | ---: | ---: |
| Wall time per workflow | 24.775 ms | 21.186 ms |
| Process CPU per workflow | 19.822 ms | 19.380 ms |
| Workflow p50 | 23.305 ms | 21.360 ms |
| Workflow p95 | 29.803 ms | 24.199 ms |
| Workflow p99 | 31.814 ms | 26.521 ms |

The other compared metrics, including action-ready observations, concurrency
8 and large-history workloads, passed their frozen bounds. The host-load rise
is recorded; it does not turn this comparison into a pass or prove that source
changes caused the misses. Earlier failed captures are retained under
[`barrier-v1`](../barrier-v1/README.md) and
[`replacement-38e-v1`](../replacement-38e-v1/README.md). The separately
qualified registered-profile strict and legacy media/duplex stress comparison
at `38358293` is [recorded independently](../replacement-383-v2/README.md).

Reproduce the **static** comparison from the repository root with:

```sh
node scripts/continuation-candidate-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-517-v1/barrier-517-failed.json
```

The expected result is `passed: false` with the five misses above. This
artifact is not a paid/live-model trial, a production quality result, an
isolated gateway RSS measurement, or a passing G6 claim. The original v1
slow-relay control and final-source frozen performance qualification remain
open.
