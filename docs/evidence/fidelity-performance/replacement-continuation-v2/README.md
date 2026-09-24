# Frozen continuation barrier comparisons after `517455c3`

These three byte-identical, write-once captures use the unchanged
[barrier-v1 native reference and frozen budgets](../barrier-v1/README.md).
Each is a clean source revision of the production negotiated OpenAI Chat to
Anthropic two-turn continuation workflow. The source and runner hashes, exact
raw output, hardware, PostgreSQL conditions and load are inside each JSON.
The original `517455c3` capture is also retained in its
[first evidence directory](../replacement-517-v1/README.md).

| Capture (SHA-256) | Clean source | Load before → after | Frozen comparator misses |
| --- | --- | --- | --- |
| [517](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-517-failed.json) (`c717939b224dc8deecd299e41731edb774f9c4348e954a2f32ad476836320368`) | `517455c30a894f217faab4d4423fcd99bd0742c4` | 1.07 → 2.48 | small/c1 wall time, CPU, workflow p50/p95/p99 |
| [fa6](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-fa6-failed.json) (`e55f6357ac77c6a491c8d28eb8e796659e97492e7e563ac180302b0b38650b9d`) | `fa6e42f88d929dd34bd795e951298a66956fcd88` | 0.97 → 2.00 | small/c1 wall time, workflow p50/p95 |
| [66fb](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-66fb-rejected.json) (`6b6412d3f94e4c6762fdcaac3eb2d0236a7b2d3718a97f274539e5ed61d0d703`) | `66fb41f9cd95aeee51f42809eb5514bf4fc9b28d` | 1.56 → 1.90 | small/c1 wall time, workflow p95/p99; large/c8 sampled heap |

Every capture completed all 12 repetitions: **288** two-turn workflows,
**576** exact provider dispatches, **5,472** native events, **576** fixture tool
actions, **288** authenticated ready reads, and **zero** rejected workflows.
Each workload retained three repetitions of 24 successes, the native oracle,
ordered projected observations and encrypted complete dependency checks.
Those counts establish coverage, not a timed pass.

| Unchanged limit | `517455c3` | `fa6e42f8` | `66fb41f9` | Maximum |
| --- | ---: | ---: | ---: | ---: |
| small/c1 wall time per workflow | 24.775 ms | 22.016 ms | 21.907 ms | 21.186 ms |
| small/c1 process CPU per workflow | 19.822 ms | 17.478 ms | 18.127 ms | 19.380 ms |
| small/c1 workflow p50 | 23.305 ms | 21.875 ms | 21.200 ms | 21.360 ms |
| small/c1 workflow p95 | 29.803 ms | 24.707 ms | 26.704 ms | 24.199 ms |
| small/c1 workflow p99 | 31.814 ms | 25.541 ms | 27.170 ms | 26.521 ms |
| large/c8 sampled heap growth | within limit | within limit | 110,514,280 B | 103,420,412 B |

At `fa6e42f8`, measured small/c1 CPU and allocations were lower than in the
prior capture (19.822 → 17.478 ms CPU,
11,393 → 10,842 allocs per workflow), but it **did not pass** the frozen wall
and p50/p95 criteria. The isolated `66fb41f9` combination added the authorized
one-statement submission lookup to `fa6e42f8`. It passed selected
resource, process-loss, revocation, rotation, fault, and official SDK race
checks, but its **frozen comparison failed four limits**. That combination is
rejected as a G6 solution and remains unmerged. Differences between these
separate host captures do not establish a source-causal timing gain.

Recheck each static result from a source retaining the unchanged runner,
harness, reference and budgets:

```sh
node scripts/continuation-candidate-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-517-failed.json
node scripts/continuation-candidate-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-fa6-failed.json
node scripts/continuation-candidate-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-continuation-v2/barrier-66fb-rejected.json
```

Each command is expected to exit nonzero with the misses in the table. No
frozen criterion or measured workload was changed or excluded. The original
[v1 slow-relay control](../oif-source-v1/README.md) still fails independently;
[registered-profile media/duplex stress-v2](../replacement-383-v2/README.md)
passed only its scoped scripted criteria. Empirical model quality remains
unknown without authorized live trials. These captures do not qualify G6.
