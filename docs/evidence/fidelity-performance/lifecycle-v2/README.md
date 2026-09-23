# Strict-aware native lifecycle qualification, v2

This is a separate prospective local scripted-provider measurement for the four
native durable/duplex workload classes in the frozen [lifecycle-v1](../lifecycle-v1/README.md)
qualification. It does not edit that harness, its baseline or its budget. The
strict v1 attempt stopped at the old `response_` ID oracle; that failed attempt
remains visible in the [replacement record](../replacement-38e-v1/README.md).
Strict Responses instead publish an owner-scoped `strict_response_` ID. A local
ID is never described here as the provider's native ID.

The reference B is the historical pre-#216 product at `033c611f`, which
already contains the frozen v1 native baseline and budget. Only this v2
measurement file, its runner/test and this README may be added to B; the
runner records and enforces that product diff. The original v1 baseline was
captured from `373c5846`, before any lifecycle replacement. A B-only v2
capture must be committed and the v2 budget frozen **before** any strict C
timed candidate. No candidate observation may set or alter a threshold.

The fixture retains the original ordered Responses input, `store:true`, exact
unary result, 66 ordered streaming events with 64 text deltas, 64 exact duplex
input/reply exchanges with audio bytes, 500 µs slow-reader pacing, and
provider-observed cancellation. It uses c1/c4, relay/gateway, three repetitions
and 24 complete successes per repetition: 48 repetitions and 1,152 exact
provider dispatches. Each gateway durable result must have the expected
qualified ID shape, independent committed PostgreSQL kind/owner/route/native
upstream mapping, strict encrypted contract where applicable, exact public
retrieval, and an authenticated other-key 404 without provider work. The 12
gateway durable repetitions require 288 mapping checks, 288 retrievals and 12
zero-dispatch negatives, with zero ambiguous outcomes. Native upstream IDs
must match the fixture's own emission records; mere local-ID syntax is never
enough. The independently retained provider document must also have the exact
upstream ID, native provider model, text, usage and terminal fields. Retrieval
and wrong-owner checks occur after each repetition's timed and resource-sampled
interval. Historical B's GET response exposes the native provider model;
strict C's GET response projects the declared client route model. Each full
document is checked against its own explicit contract, and that projection is
reported as a client identity mapping rather than native byte equality.
Publication still measures native emission to client-visible ID *before* the
SQL check; streaming request latency and run wall time include that check.

The numeric rule uses **every unchanged limit** in frozen
[`lifecycle-v1/replacement-budgets.json`](../lifecycle-v1/replacement-budgets.json),
which was calculated from the maximum of three original native B repetitions
with predeclared margin. V2 additionally freezes 24 gateway-minus-relay
latency limits for the four workloads at c1/c4 and p50/p95/p99. For each one,
the limit is `ceil(max(0, maximum of the three original same-repetition
gateway-minus-relay differences) × 1.5 + 1000 µs)`. This uses the original
v1 latency tolerance and only pre-#216 source observations; the v1 budget did
not contain an added-latency field. The B-only v2 medians for every original
metric and their gateway-minus-relay differences must first meet those
limits, otherwise host/fixture comparability fails and the study is
inconclusive. Strict C must then meet the same 16 path inventories and eight
added-latency inventories, with complete exact-count/byte/identity controls.
This is an absolute regression gate over descriptive 24-sample repetition
quantiles, not a production p99 SLO or a live-model quality claim. CPU and
allocations include client, provider, relay/gateway and checking in one Go
process; PostgreSQL is separate. Heap growth is sampled at 1 ms and is not
isolated gateway RSS. Loopback HTTP/WebSocket has no inference TLS or WAN.

Run the semantic-only public suite with required integration service settings:

```sh
OLP_LIFECYCLE_ROUTE_FIDELITY='{"mode":"strict"}' go test -mod=readonly -tags=integration -run '^Test(FidelityLifecycleV2Semantic|LifecycleV2.*OracleDetectsCorruption)$' -count=1 ./tests/integration
node --test scripts/fidelity-lifecycle-v2-benchmark.test.mjs
```

For a coordinated quiet timed capture, use a clean historical B checkout
containing only the committed measurement files. Keep PostgreSQL running and
other workloads idle. Write-once commands are:

```sh
node scripts/fidelity-lifecycle-v2-benchmark.mjs record-baseline docs/evidence/fidelity-performance/lifecycle-v2/baseline.json
node scripts/fidelity-lifecycle-v2-benchmark.mjs freeze docs/evidence/fidelity-performance/lifecycle-v2/baseline.json docs/evidence/fidelity-performance/lifecycle-v2/replacement-budgets.json

# Only after both files are committed and the final C source is locked:
OLP_LIFECYCLE_ROUTE_FIDELITY='{"mode":"strict"}' node scripts/fidelity-lifecycle-v2-benchmark.mjs record-strict /tmp/lifecycle-v2-strict-candidate.json docs/evidence/fidelity-performance/lifecycle-v2/replacement-budgets.json
node scripts/fidelity-lifecycle-v2-benchmark.mjs compare /tmp/lifecycle-v2-strict-candidate.json docs/evidence/fidelity-performance/lifecycle-v2/replacement-budgets.json
```

The runner rejects missing/dirty B budgets, changed v2/v1 harnesses,
hardware/runtime/storage/fixture changes, incomplete workload/dispatch/event
inventory, missing mappings/retrievals/negative controls, ambiguous outcomes,
and changed or invalid numeric limits. B and C use different committed
`access_test.go` setup helpers: C extracts the old constructor and installs
the encrypted resource store required for strict Responses. The B helper hash
is measured and verified at freeze; C must match the predeclared reviewed
candidate helper hash. Any further helper edit invalidates comparison. The
runner records raw Go output, source and fixture hashes, conditions, load,
full per-repetition metrics and counts.
No paid model invocation is part of this qualification. Until a complete
B-only baseline and final strict C comparison pass, G6 remains open.
