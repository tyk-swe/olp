# Prospective paired encrypted-continuation experiment, v2

This is an additive measurement of the local scripted cost of production
negotiated OpenAI Chat-to-Anthropic continuation against the historical native
encrypted reference. The frozen [barrier-v1 baseline](../barrier-v1/baseline.json),
[limits](../barrier-v1/budgets.json), runner, failed v1 candidates and native
request/event oracle are untouched. A paired pass would qualify only this
predeclared relative-cost experiment; it would not turn failed v1 comparisons
into passes or establish empirical model quality.

## Frozen method before candidate observation

The historical B product is `29e18268ac23f7290d1881cbda3af2cc2ccab018`.
The B checkout may add only
`tests/integration/continuation_barrier_paired_v2_test.go`, a measurement-only
command loop that calls the unchanged native reference workflow. Its independent
encrypted claim, dispatch journal, atomic ready commit and authenticated
post-commit decryption still gate the two fixture actions. The C loop calls the
unchanged production candidate workflow; the provider checks exactly the same
first/next native requests and 19 native stream events. B and C use distinct
temporary PostgreSQL databases on the same local PostgreSQL 18.6/no-TLS
server and stay running with warmed HTTP connections while timed blocks are
serialized. Compilation, setup and one checked warmup per subrun are excluded
from each 24-workflow measurement.

[criteria.json](criteria.json) is mechanically derived from the original three
B repetitions and original frozen limits, before any v2 C observation. For
each original B path/metric, `M = old limit L - median(old B repetitions)`.
The four translated strata use the 11 directly corresponding encrypted-reference
metrics: ns/op, process CPU, B/op, allocs/op, sampled heap growth, workflow
p50/p95/p99 and durable action-ready p50/p95/p99. The B relay/gateway/reference
controls retain all original path metrics, including native first-event,
wire-tool and reference persistence phases. Native wire-tool visibility is not
called durable actionability. Raw per-workflow timings are retained; p50/p95/p99
are calculated within each 24-workflow subrun with the original order-statistic
rule. The file records the fixed seed, 32 blocks per stratum, and all 16+16
balanced palindromic orders. Both B-reference/C adjacencies remain within each
block.

For every metric and block, the two C subrun statistics are averaged and the
two matching B statistics are averaged; their difference is one paired added
cost. Its sorted 32-block value `d_(22)` is the exact one-sided upper median
bound (97.494877% coverage), and **must be strictly less than M**. The 64 B
subruns per original path/metric must have median no greater than original L.
The sorted blockwise absolute difference between the two same-source B subruns
must have `a_(22) < M` for every original B path/metric. A control excursion
makes the full profile inconclusive, regardless of C's adjusted result. All
44 primary bounds, B controls, exact counts and semantic checks are conjunctive;
no failed, slow, rejected or incomplete block is trimmed or replaced. A failed
run remains visible. Further C measurement requires a new named attempt under
an explicit sequential-testing plan.

The B-only capture runs the same 32 scheduled blocks without C positions:
64 subruns for each of the 12 historical B combinations, 18,432 complete
two-turn workflows, 36,864 exact provider dispatches, 350,208 checked native
events and 36,864 enabled fixture actions. The B encrypted-reference path
has 6,144 separate ready-state reads. The subsequent paired capture has those
same B totals plus 6,144 C workflows, 12,288 C dispatches, 116,736 C native
events, 12,288 C fixture actions, 6,144 C authenticated ready-state reads,
79,872 ordered C first-turn observations and 12,288 final-turn observations.
There is zero allowance for missing, rejected, ambiguous or duplicate work.

The record commands require clean, committed method and product checkouts and
write a new artifact only once. The historical B overlay must be a clean commit
whose only difference from B is the reference measurement file. The B-only
artifact must itself be committed and pass the old B envelope and stability
controls before `record-paired` can start C. The candidate is measured once at
one locked final source revision. Tests can exercise synthetic C values but
must not observe C timings before this chronology completes.

```sh
node --test scripts/continuation-barrier-paired-v2.test.mjs
OLP_PAIRED_B_ROOT=/tmp/olp-worktrees/paired-barrier-reference-v2 \
  node scripts/continuation-barrier-paired-v2.mjs record-baseline \
  docs/evidence/fidelity-performance/paired-barrier-v2/baseline.json
OLP_PAIRED_B_ROOT=/tmp/olp-worktrees/paired-barrier-reference-v2 \
  node scripts/continuation-barrier-paired-v2.mjs record-paired \
  docs/evidence/fidelity-performance/paired-barrier-v2/candidate.json \
  docs/evidence/fidelity-performance/paired-barrier-v2/baseline.json
node scripts/continuation-barrier-paired-v2.mjs compare \
  docs/evidence/fidelity-performance/paired-barrier-v2/candidate.json
```

The owner supplies the disposable integration-service environment without
printing credentials. The runner records source/harness/fixture hashes,
hardware, Go runtime, per-block load and CPU pressure, GC pauses, scheduler
latency histogram p99 and goroutine counts. Process CPU and memory include the local client, provider,
gateway/relay, oracle and instrumentation, but exclude PostgreSQL itself.
They are not an isolated gateway RSS, WAN/TLS test, actual SDK process CPU, or
live-model quality measurement. The separate public race, recovery, replay,
corruption and SDK suites retain their own gates.
