# Source performance, paired v2

This is an additive, prospective *local scripted source-cost* study. It does
not modify or re-score the frozen v1 baseline, budgets, runner, oracle or any
failed replacement capture. The study design was informed by seeing those
failures. A passing v2 result would support its own paired added-cost claim,
not a v1 pass, production latency SLO, isolated gateway RSS or live-model
quality claim.

Historical product B is `8e52f775df815c3a1a25d75064c3ffd540fb07f5`.
The branch containing this file adds only measurement code and byte-identical
copies of the v1 baseline and budgets to that checkout. `manifest.json`,
`manifest-r2.json`, `manifest-r3.json` and `manifest-r4.json` remain visible as superseded pre-data
design revisions. The first accepted a non-strict C route; r2 omitted
post-baseline product chronology; r3 did not bind the write-once journal or
fix the output paths; r4 left B checkout reuse ambiguous after committing
evidence. **`manifest-r5.json` is active.** It requires the
exact explicit `{"fidelity":{"mode":"strict"}}` overlay for every native,
translated and rejected route category, plus Git proof that the exact B-only
artifact was committed before an affected production Go change. It must be
committed with this runner before any B-only baseline or C observation. The Go v2
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
separate long-lived Go processes freshly built by the runner from the exact
source revisions before timed blocks. The paired B checkout may contain only
its two fixed untracked evidence files. The runner refuses existing binary
paths. It records the raw 64
per-request timings, checked effects and content counts, runtime/GC data and
host pressure, GC and scheduler observations for every subrun and block. The
fixed JSON and append-only journal paths are both under this directory. The
journal is reserved before the first timed subrun; a failed run writes invalid
status to the same JSON path, and an interrupted run retains its journal. Both
paths prevent another r5 attempt. Do not remove/retry a block based on load or timing.

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
its passing JSON **and journal together in one commit** must precede a locked C
confirmatory capture. Historical C observations from v1 are disclosed but may
not change this rule.

The source fixture uses the frozen `fakeRuntime` key map, not the
`runtime.Manager` refresh clock used by service-backed benchmarks. A selected
semantic check waited beyond the Manager's 60-second stale-authority boundary
and still observed two successful authenticated provider dispatches; it emits
no performance artifact.

After the B-only artifact is committed, C must have a **later non-merge commit**
changing a production `.go` file in `internal/gateway/`, `internal/resources/`
or `internal/interaction/`; `_test.go` files do not count. The evidence commit
must be a strict ancestor of that product commit, which must be an ancestor of
the locked C revision. The runner checks the exact Git blobs and SHA-256 values
of `baseline.json` and `baseline.json.journal.jsonl` through all of C's
ancestry. Exactly one normal commit must create both; a no-ff merge may only
carry unchanged blobs. Later edits, deletions or recreations fail. It records
both evidence paths/hashes, commit IDs and changed paths, and rechecks them
after timed capture. `compare-paired` repeats this proof against the recorded C
revision even if the checkout's HEAD later advances through documentation-only
commits. A product change on a pre-evidence side branch does not become a
post-evidence change just because it was later merged.

The measured B checkout stays at the same **method commit M** for both the
B-only and paired runs, retaining its fixed JSON and journal as untracked
reservations. Create a separate evidence worktree from M, copy those exact two
files there and commit them together as E (E's sole parent must be M). Merge E
into C, then make the affected product commit. The paired runner requires B's
revision and binary hash to match the B-only capture, requires its two reserved
files to remain untracked and byte-identical to C's committed copies, and
requires E to have M as its direct parent. Committing the files on the measured
B checkout would change B's revision and fail this gate.

```sh
# In the clean B measurement checkout, after pre-baseline review and before C measurement:
node scripts/fidelity-paired-v2.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json
node --test scripts/fidelity-paired-v2.test.mjs
go test -mod=readonly ./internal/gateway -run 'TestFidelityBenchmarkOracleDetectsCorruption|TestFidelityPairedLookupPreservesFrozenInventory' -count=1
OLP_SOURCE_PAIRED_LONG_LIVED_TEST=1 go test -mod=readonly -run '^TestFidelityPairedFixturePastManagerStaleWindow$' -benchtime=1x -count=1 -timeout=3m ./internal/gateway
node scripts/fidelity-paired-v2.mjs record-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json /tmp/oif-source-B.test "$PWD"

# From a separate clean evidence worktree branched at measured B commit M,
# copy both reserved B files and commit them together as E. Merge E into C.
# Keep the measured B checkout at M with its untracked reservations.
# Make a later affected production Go commit, then lock C.
# The runner builds fresh B/C binaries into unused paths before timed blocks.
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
node scripts/fidelity-paired-v2.mjs record-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r5.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline.json" /tmp/oif-source-B-confirm.test /path/to/measured/B-at-M /tmp/oif-source-C.test "$PWD"
node scripts/fidelity-paired-v2.mjs compare-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r5.json" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r5.json "$PWD" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline.json"
```

The route-contract JSON shown is the exact required C shape from the current
published route model. The runner rejects legacy, transformed, omitted, or
ambiguous overlays before building C and validates the recorded artifact again
on comparison. Both
processes are built before timed blocks; B always has legacy environment and C
receives only the recorded explicit contract. The runner never selects a later
block, threshold or retry after seeing a C result. A failed study requires a
new named attempt and prospective sequential-testing rule.
