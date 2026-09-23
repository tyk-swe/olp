# Source-v1 replacement capture at `bc1e28ff`

The write-once [services-off candidate](source-bc1-services-off-failed.json) is
the complete original source-v1 benchmark at clean product source
`bc1e28fffbe71a79b47c92cad9e9750ae47284a1`. Its SHA-256 is
`f702e8559eecd43d9c67a7920a7e405f9a387c61d7a3c97934dc7a5902992165`.
The artifact records the unchanged harness, runner, command, legacy route
contract, hardware and workload inventory used by the frozen
[v1 reference and budgets](../v1/). It ran from 06:25:28 to 06:29:17 UTC on
2026-09-23. The shared PostgreSQL and Valkey services had been stopped before
this attempt; the benchmark uses in-memory authority and does not depend on
them. The artifact itself records one-minute host load of 0.63 before and 3.29
after, not a per-event scheduler trace or independent proof of service state.

All **22 workloads ×3 repetitions = 66 measured runs** are present. Their
recorded iteration counts total 397,562: 288,194 successful provider dispatches
and 109,368 intentional zero-dispatch extension rejections. The frozen
comparator accepted every exact request/event/outcome check and sample floor.
It failed exactly one unchanged numeric limit:

| Workload and metric | Candidate median | Frozen maximum |
| --- | ---: | ---: |
| `native_slow_stream_64/c1/relay` max inter-event-gap p99 | **3,567 µs** | 3,544 µs |

The difference is 23 µs. All gateway metrics and every other relay metric
met their original bounds. This is still a **failed** frozen comparison. The
relay control is part of the predeclared inventory, so a passing gateway path
does not erase it. The rising aggregate load cannot prove that scheduler
noise caused the miss; stopping the owned services alone did not establish a
quiet host. Earlier [source-layer failures](../oif-source-v1/README.md) and
the clean [`517455c3` encrypted barrier failure](../replacement-517-v1/README.md)
remain separate evidence.

Reproduce the static comparison from the repository root:

```sh
node scripts/fidelity-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-source-bc1-v1/source-bc1-services-off-failed.json docs/evidence/fidelity-performance/v1/replacement-budgets.json
```

It exits unsuccessfully with the single 3,567 µs >3,544 µs failure. No
workload, repetition, rejection, sample floor or frozen limit was removed or
rebased. A later genuinely quiet, same-method capture must meet the unchanged
criteria before this source-v1 control can pass. This artifact establishes no
empirical model-quality parity or full G6 qualification.
