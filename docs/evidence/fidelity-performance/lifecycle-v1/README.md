# Native lifecycle performance baseline

This supplements the unchanged [initial baseline](../README.md) with public native
Responses resource publication and duplex traffic. It measures the current Azure
v1 Responses profile, a scripted local provider and a minimal authorized native
relay. Provider/profile/route/key configuration goes through management APIs;
the gateway commits real resource mappings to isolated PostgreSQL before the
client accepts a visible resource ID. No external model or paid inference runs.

The four workloads are durable unary, durable streaming (created event, 64 exact
text deltas, complete terminal), 64 duplex event exchanges, and the same duplex
exchange with a 500 µs requested delay before each client read. Concurrency is 1
and 4, on both relay and gateway paths: 16 combinations, three repetitions each,
24 successful requests/sessions per repetition. Warmups are checked and excluded.
The provider checks every original request and duplex input. The client checks
whole result documents, exact event fields/order/framing and audio bytes.
Cancellation is abrupt client closure while the provider is still reading;
every session waits for provider-side closure. Dispatches must equal successes.

The native request deliberately uses explicit ordered Responses input messages.
The legacy implementation normalizes scalar input into that representation; it
cannot serve as a byte-preserving positive control for scalar input. Strict scalar
preservation is a separate conservation regression, not an altered benchmark.

## Measurement and limits

`publication` is provider emission to client observation of the durable ID, before
the client's separate SQL validation. It includes transport and mapping overhead;
it is not isolated PostgreSQL commit latency. Streaming request duration also
includes the client's durability check after its first event. The native relay
retains provider IDs and has no storage check. This difference is intentional and
must remain visible. This workload does not qualify encrypted translated-tool
state, readiness, recovery, or exactly-once execution.

`event-roundtrip` pools all 64 exchanges per session. The slow variant includes
client pacing and OS timer granularity. It measures RTT variation, not one-way
media timing or real-provider VAD quality. `cancellation` measures client closure
to the provider reader observing closure, excluding session duration.

CPU and allocations include client, fixture, gateway/relay and checking in the
same Go process; the PostgreSQL process is separate. Heap growth is sampled every
1 ms plus a final sample, relative to a post-GC initial sample; it is not isolated
RSS or exact peak memory. Fixed request samples are descriptive order statistics,
not high-confidence request-tail estimates. Event RTT has 1,536 observations per
repetition. There is no WAN or TLS on the two HTTP/WebSocket hops. The artifact
records actual PostgreSQL version/TLS, hardware, toolchain, runtime settings,
source/harness/runner identities, system load, raw output, and all measured values.

## Commands

Supply the standard integration-test service environment. Missing PostgreSQL must
fail an explicitly selected run. Stop other builds/tests during measurement;
PostgreSQL is deliberately running and recorded in this scope. Other unrelated
service workloads must be idle. Do not print service URLs or credentials.

```sh
# Short correctness run (four requests per combination), never timing evidence.
go test -mod=readonly -tags=integration -run 'Test(FidelityLifecyclePerformance|Lifecycle.*OracleDetectsCorruption)$' -count=1 ./tests/integration
node --test scripts/fidelity-lifecycle-benchmark.test.mjs

# Baseline capture, once, before lifecycle replacement; use a clean source commit.
node scripts/fidelity-lifecycle-benchmark.mjs record docs/evidence/fidelity-performance/lifecycle-v1/baseline.json
node scripts/fidelity-lifecycle-benchmark.mjs freeze docs/evidence/fidelity-performance/lifecycle-v1/baseline.json docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json

# Later candidates must retain every workload, sample, event and outcome.
node scripts/fidelity-lifecycle-benchmark.mjs record /tmp/lifecycle-candidate.json
node scripts/fidelity-lifecycle-benchmark.mjs compare /tmp/lifecycle-candidate.json docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json

OLP_LIFECYCLE_ROUTE_FIDELITY='{"mode":"strict"}' node scripts/fidelity-lifecycle-benchmark.mjs record /tmp/lifecycle-strict.json
node scripts/fidelity-lifecycle-benchmark.mjs compare-strict /tmp/lifecycle-strict.json docs/evidence/fidelity-performance/lifecycle-v1/replacement-budgets.json
```

All artifacts are write-once. Numeric thresholds are declared from the maximum of
three baseline repetitions ×1.5, plus 1 ms timing/CPU, 16 KiB allocated bytes or
8 MiB sampled heap. Allocation counts use ×1.25 +64. The candidate median must
meet every threshold. These are regression tolerances, not service latency claims.
The comparator requires unchanged harness/runner, hardware/runtime/service
conditions and full successful inventory. Setup helper hashes are recorded for
review because wiring the new production authority can change those helpers;
changes to setup require review and may not change the measured workload/limits.

The only permitted candidate selection is the actual persisted route fidelity
mode. Strict activation must succeed; a legacy run cannot qualify as strict.
Do not reset limits to fit a candidate. The original baseline and its unresolved
slow-relay limit remain separately visible. Full G6 remains pending until both
legacy and strict replacement captures and the additional encrypted-tool/resource
stress qualification are complete.

## Execution status

The short public-service suite and both result/event corruption oracles passed
with race detection. The five runner tests passed, including deliberate omission,
changed hardware, state publication/cancellation regression and invalid budget
mutations. The clean harness at `373c5846` captured all 48 repetitions (1,152 measured
successes and matching provider dispatches) in 23.73 seconds on 2026-09-22.
The separate `replacement-budgets.json` froze the declared formula before any
lifecycle replacement. Its baseline self-comparison passed and establishes
artifact integrity only. All candidate comparisons remain pending. The team
paused local builds/tests for capture; required PostgreSQL and the existing
idle shared test-service containers remained running.
