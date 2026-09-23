# Source performance, paired v2

This is an additive, prospective *local scripted source-cost* study. It does
not modify or re-score the frozen v1 baseline, budgets, runner, oracle or any
failed replacement capture. The study design was informed by seeing those
failures. A passing v2 result would support its own paired added-cost claim,
not a v1 pass, production latency SLO, isolated gateway RSS or live-model
quality claim.

Historical product B is `8e52f775df815c3a1a25d75064c3ffd540fb07f5`.
The branch containing this file adds only measurement code and byte-identical
copies of the v1 baseline and budgets to that checkout. The first
`manifest.json` is retained as a superseded, pre-data design revision: review
found that it accepted any nonempty C route contract, including legacy.
**`manifest-r2.json` is the active manifest** and requires the exact explicit
`{"fidelity":{"mode":"strict"}}` overlay for every native, translated and
rejected route category. It must be committed with this runner before any B-only
baseline or C observation. The Go v2
adapter calls the **unchanged** v1 fixture/request/stream oracle, including the
provider-bound native document, exact PNG, authentication, complete SSE
content/identity/usage/terminal grammar, and zero-dispatch unmapped-extension
outcome. The original 22 workload/path/concurrency combinations remain fixed.

The manifest pins a SHA-256 schedule with 32 blocks per workload/concurrency
group. Sixteen blocks use B-C-C-B on the relay and C-B-B-C on the gateway;
sixteen reverse those orders. Within each treatment order, eight put relay
outermost and eight put gateway outermost. The eight subruns in a successful
block form a four-arm palindrome. Rejection has only B/C gateway arms. B-only measurement
uses the identical schedule with C slots omitted. Every subrun checks one warm
request and a one-request Go benchmark calibration, then records **exactly 64**
fresh requests; neither warmup contributes to its metrics. B and C are
separate long-lived Go processes freshly built by the runner from exact clean
checkouts before timed blocks. It refuses existing binary paths. The runner records the raw 64
per-request timings, checked effects and content counts, runtime/GC data and
host pressure, GC and scheduler observations for every subrun and block. It writes an append-only journal;
invalid or interrupted output stays visible. Do not remove/retry a block based
on its load or timing.

For each of 224 path metrics, the frozen margin is `M = old v1 budget limit −
median of the three historical B repetitions`. Thirty gateway-minus-relay
latency comparisons use the same old gateway margins and report B and C
vectors separately. For example, frozen `native_unary/c1/relay` `ns/op` has
limit 1,621,702 and historical B median 407,272, giving margin 1,214,430;
its gateway `latency-p99-us` has limit 3,007 and B median 1,282, giving
margin 1,725 µs. These are computed from old artifacts without C data. The paired cost for a
block is the mean of its two C subrun statistics minus the mean of its two B
subrun statistics. A primary metric passes only when sorted `d_(22) < M` across
32 complete blocks. Relay controls additionally require `d_(11) > −M`. The
median of all 64 contemporary B subruns must stay within the old B limit for
every metric in both the B-only and paired captures. A failed control makes the
study inconclusive. Every exact request/event/effect count is mandatory.

Before running, stop owned PostgreSQL/Valkey and unrelated builds/tests;
coordinate one quiet host window. Use the same eight-logical-CPU Haswell host,
kernel, Go 1.27.1, GOMAXPROCS=4 and GOGC=100 as v1. No external inference or
paid provider is used. The old 100 µs *requested* slow-reader delay remains;
actual OS timer granularity may be coarser. The independent B-only capture and
its pass/fail result must be committed before building or observing a locked C
confirmatory capture. Historical C observations from v1 are disclosed but may
not change this rule.

```sh
# In the clean B measurement checkout, after pre-baseline review and before C measurement:
node scripts/fidelity-paired-v2.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r2.json
node --test scripts/fidelity-paired-v2.test.mjs
go test -mod=readonly ./internal/gateway -run 'TestFidelityBenchmarkOracleDetectsCorruption|TestFidelityPairedLookupPreservesFrozenInventory' -count=1
node scripts/fidelity-paired-v2.mjs record-B /tmp/oif-source-B-only.json docs/evidence/fidelity-performance/source-paired-v2/manifest-r2.json /tmp/oif-source-B.test "$PWD"

# Commit the complete, passing B-only artifact and its SHA-256 before C.
# The runner builds fresh B/C binaries into unused paths before timed blocks.
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
node scripts/fidelity-paired-v2.mjs record-paired /tmp/oif-source-paired.json docs/evidence/fidelity-performance/source-paired-v2/manifest-r2.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline.json" /tmp/oif-source-B-confirm.test /path/to/clean/B /tmp/oif-source-C.test "$PWD"
```

The route-contract JSON shown is the exact required C shape from the current
published route model. The runner rejects legacy, transformed, omitted, or
ambiguous overlays before building C and validates the recorded artifact again
on comparison. Both
processes are built before timed blocks; B always has legacy environment and C
receives only the recorded explicit contract. The runner never selects a later
block, threshold or retry after seeing a C result. A failed study requires a
new named attempt and prospective sequential-testing rule.
