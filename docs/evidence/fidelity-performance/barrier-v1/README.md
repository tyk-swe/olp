# Encrypted continuation barrier reference

This supplementary pre-replacement reference covers the cost of retaining a
complete native reasoning/tool dependency history before enabling tool actions.
It leaves the original `v1` and native `lifecycle-v1` evidence unchanged. It does
not qualify the production translated continuation implementation or full G6.

The test uses the frozen Anthropic two-turn corpus: 19 events, native thinking
and signature, text before and after two tool calls, and both correlated tool
results in the second request. Every request, event, final result and dispatch
count is independently checked. The client is a Go native-wire client using a
separately SDK-qualified corpus, not an actual JavaScript/Python SDK process.

Twelve workloads combine small/256 KiB additional user history, concurrency 1/8,
and these three paths:

- An authorized fixed-destination minimal native relay.
- The publicly configured current strict native gateway.
- The same strict gateway plus reference-side encrypted submission claim,
  pre-dispatch journal and atomic ready-payload/state commit using the existing
  KeyRing and PostgreSQL resource authorities and forward schema 0025.

On the reference path, a separate transaction verifies committed ready state
and decrypts the complete expected dependency document before two fixture tool
actions are enabled and the second request starts. No real tools run. Native
wire tool visibility is measured separately; the native gateway does not claim
to withhold those bytes. A translated durable tool barrier must be compared to
`action-ready`, not to the earlier `wire-tool` observation.

Each workload has one warmup and three repetitions of 24 successful two-turn
workflows. Measurements include whole workflow, first event, native tool wire
visibility, recoverable action availability, individual persistence phases,
process CPU, allocations and 1 ms sampled heap growth. Resource measurements
include the client, provider, gateway/relay, oracle and instrumentation; they
exclude the separate PostgreSQL process and do not establish isolated gateway
RSS. Hardware, runtime, source, helper/oracle hashes and storage conditions are
recorded. The shared disposable PostgreSQL and idle Valkey services remain
running; compilation and tests in other worktrees are paused during capture.

## Reproduction and immutable criteria

With the repository's disposable integration service configuration exported,
run from a clean commit:

```sh
node scripts/continuation-barrier-benchmark.mjs record /tmp/barrier-reference.json
node scripts/continuation-barrier-benchmark.mjs freeze /tmp/barrier-reference.json /tmp/barrier-budgets.json
node scripts/continuation-barrier-benchmark.mjs compare /tmp/barrier-reference.json /tmp/barrier-budgets.json
```

The predeclared rule uses each metric's largest of three reference observations:
1.5 times that value, plus 1 ms for latency/CPU, 16 KiB for allocated bytes and
8 MiB for sampled heap growth. Allocation counts use 1.25 times plus 64. Candidate
medians must satisfy every workload/metric limit. This is the same tolerance
method as the supplemental native lifecycle baseline; no candidate timing
informed it. All 24 workflows, 48 provider dispatches, 456 events and 48 enabled
actions per repetition are required. The runner refuses changed conditions,
missing work, incorrect observation order and changed harness/oracle identity.

Production translated projection has a different client contract. Its candidate
harness and correspondence to this reference must be separately versioned and
reviewed before measurement. That review must retain these frozen budgets,
input/history sizes, concurrency, provider request/event/next-turn oracle and
complete outcomes; it must not relabel a changed projection as this native
reference or reset limits to accommodate the candidate.

Race smoke, database corruption and runner mutation checks establish correctness
of this reference only. Ciphertext corruption, non-ready state, lost assistant
history, signature loss, tool correlation changes and missing terminal events
must fail. Production restart, replay, partial delivery and revocation checks
belong to the continuation implementation's independent qualification.

## Captured result

The clean reference source `29e18268ac23f7290d1881cbda3af2cc2ccab018`
produced [baseline.json](baseline.json); [budgets.json](budgets.json) freezes the
predeclared criteria. All 36 repetitions completed: 864 workflows, 1,728 provider
dispatches, 16,416 native events and 1,728 enabled fixture actions. The Go test
completed in 26.136 s. The baseline passes its frozen self-comparison.

Correctness checks passed separately: the full race smoke in 42.865 s, storage
corruption checks with race detection in 3.178 s, Go vet, and all five runner
mutation tests. Captured timings are from the ordinary non-race binary. These
results establish reference integrity; no translated candidate is measured or
qualified by this artifact.
