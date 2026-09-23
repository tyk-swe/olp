# Prospective source-cost method r7

R7 is a new, one-shot local scripted source-cost study, frozen before any B7 or
C7 timing. It does not edit or re-score the frozen v1 source baseline, budgets,
runner, fixture/oracle, failed replacement captures, r5 invalid B-only capture,
or r6 invalid C capture. The original r6 method and artifacts remain available
under their original paths. The r6 C attempt reached its first scheduled
`native_unary/c1/relay` arm and stopped with a collapsed oracle/effect/sample
error before any complete block. Its underlying cause is unconfirmed. The
byte-identical r6 invalid JSON and journal are pinned by SHA-256
`33f5efb6dc681b2be570e5e2352e53ea35f01d8adfbaff836a05083a8bf1f023`
and `25f76f751721fa4252d4969e5c0583ea354a1deecd34eeb13e0953552c41bdde`.
R6 remains an adverse/incomplete semantic attempt; it is never retried or
reported as passing.

The candidate product is locked to commit
`bc325da50c575803c771531dc4e3aab1ac203a53`. C7 must descend from that
commit and have the same tracked production Go blob inventory across the
repository. No artificial hot-path change is permitted to establish chronology.
The r7 measurement adapter is test-only. A newly measured historical B7
checkout stays at its exact method commit M7 with untracked, write-once
`baseline-r7.json` and journal reservations. A separate evidence branch makes a
normal commit E7 that creates those two files together directly from M7.
After E7 is merged unchanged into the C7 branch, E7 must be a strict ancestor
of the locked C7 measurement checkout. R6 B-only evidence cannot substitute
for B7: the seed, schedule, adapter hash, paths, and method commit differ.

The original 22 workload/path/concurrency names, 224 path metrics, 30
added-latency metrics, provider-bound native request oracle, complete SSE
oracle, exact PNG/authentication, and zero-dispatch rejection remain fixed.
Numeric headroom for each metric stays `M = old v1 absolute limit L − median of
three historical B repetitions`. A new sealed seed uses the same SHA-256 rank
algorithm to balance 32 blocks per workload/concurrency group. The primary
r7 test is strict `d_(23) < M` on sorted paired C-minus-B block differences;
relay also requires `d_(10) > −M`. Contemporary B medians in both B7 and the
paired capture must stay within their original absolute L limits. Every exact
semantic result is mandatory. The r7 one-sided per-metric sign bound is
`P[Bin(32, 0.5) <= 22] = 0.9899691965`, so standalone alpha is
`0.0100308035`. Conservatively summing r6's original d_(22) alpha
`0.0250512299` and r7 alpha gives `0.0350820334`, below the newly declared
5% two-opportunity target *for one metric*. R6 had zero complete numeric
blocks; the sum is a conservative disclosure, not a claim that r6 used r7's
stricter bound. No across-metric familywise claim is made. There is no r7
retry if its C attempt is failed, invalid, interrupted, or inconclusive.

Before any timed block, the runner reserves its fixed append-only journal and
executes a hashed, fixed semantic gate covering all 22 names. In a paired run,
C then B use the same name, starting with the exact adverse r6 C relay name.
Each gate entry uses the original independent oracle, a checked warmup and
calibration, exactly 64 fresh requests, and exact provider effects. B-only
runs its 22 B entries. Complete gate replies are retained in JSON/journal but
their durations are excluded from timed vectors and numeric comparison. A C
oracle, effect, or sample-count failure is an adverse candidate result and
stops the one-shot attempt; it is not relabeled as host instability. The r7
adapter records the first observed failure stage, bounded/redacted error text,
HTTP status when available, dispatched/completed/expected effects,
benchmark iterations, reply iterations, and sample count. The runner also
retains bounded/redacted process diagnostics. `testing.Benchmark` discards its
internal error output, so uninstrumented benchmark setup errors may still have
only a fallback stage; they fail closed.

The frozen v1 provider fixture increments `completed` in a handler defer.
Because the client may finish reading just before that defer executes, r7
waits at most 250 ms for the exact completion count *after* CPU capture and
`b.StopTimer`. It records `provider_effect_wait_ns` outside timed metrics.
Extra dispatches or permanently missing completions still fail. This removes
a plausible harness race; it is **not** a retrospective diagnosis of r6.
Selected forced-failure and delayed/stuck-count tests verify both paths.

The host preflight and block-level external-CPU rules from r6 are unchanged:
60 seconds, 13 five-second samples, every load below 2, CPU some pressure
below 5%, and external busy cores below 1; during capture, a single block at
2 external cores or two successive blocks at 0.75 invalidates the entire
attempt. No noisy block is dropped or replaced. Both B7 and C7 are prebuilt
long-lived Go test processes; the host, Go version and runtime environment
must match the frozen v1 source conditions. Owned PostgreSQL/Valkey and
unrelated builds/tests must be stopped for the capture. No paid inference is
used. A complete r7 comparison also reports all 224 contemporary C medians
against old absolute v1 L limits, including failures, **descriptively**. Those
comparisons do not change paired acceptance or erase old v1 failures.

Freeze the method and manifest in M7, then use the fixed paths:

```sh
node scripts/fidelity-paired-r7.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json
node --test scripts/fidelity-paired-r7.test.mjs
OLP_SOURCE_PAIRED_R7_DIAGNOSTIC_TEST=1 go test -mod=readonly ./internal/gateway -run '^TestFidelityPairedFailureIsStructured$' -benchtime=64x -count=1
node scripts/fidelity-paired-r7.mjs record-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json /home/ubuntu/.cache/olp-source-r7-B.test "$PWD"
node scripts/fidelity-paired-r7.mjs compare-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json

# Copy B7's exact JSON and journal to a separate evidence worktree at M7.
# Commit both together as E7; merge E7 unchanged into C7 based on bc325.
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
node scripts/fidelity-paired-r7.mjs record-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r7.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json" /home/ubuntu/.cache/olp-source-r7-B-confirm.test /path/to/measured/B-at-M7 /home/ubuntu/.cache/olp-source-r7-C.test "$PWD"
node scripts/fidelity-paired-r7.mjs compare-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r7.json" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r7.json "$PWD" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r7.json"
```

The B7 evidence must be committed and verified before invoking the paired
command. The runner builds fresh binaries into unused paths, checks exact
source and chronology, performs the full host preflight before reservation,
and refuses any existing JSON/journal path. The comparison command rechecks
its journal, original B7 bytes, product lock, schedule, gate, source identity,
semantic counts, and all formal margins. A partial journal or failed capture
cannot qualify.
