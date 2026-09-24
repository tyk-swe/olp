# Fidelity performance evidence

Under the September 24, 2026 [release criteria](../../qualification/fidelity/release-validation.md),
bounded representative performance smoke and resource/overflow correctness
are required. Long captures and statistical studies are optional follow-up.
All historical outcomes and frozen criteria below remain unchanged; failures
and invalid captures do not become passes under the revised release gate.

Superseded experimental runners and repeated raw captures are preserved in
the [commit-pinned archive index](archive.md), with exact paths and SHA-256
checksums. Canonical benchmark commands, original references/budgets and all
functional validation remain in the active tree.

The [exact final-source v2 record](final-bc325-v2/README.md) preserves passing
local lifecycle, stress and encrypted-barrier captures alongside the invalid
source paired attempt. The original v1 baselines, limits and failures remain
unchanged. They did not complete G6 under the original study requirements;
current candidate validation is separate.

This harness records an authenticated native relay and the current gateway before
replacement of its dispatch representation. It sends HTTP requests through the
real gateway to an independently scripted local provider and validates every final
provider request, complete unary document or exact stream content, identity, usage and terminal
grammar. A rejected translation must dispatch zero provider requests. Performance
cannot pass by dropping the successful or rejected workloads.

The baseline measures the legacy runtime at its recorded commit. It does not
establish strict-contract qualification or model-quality parity. Management setup,
PostgreSQL/Valkey accounting persistence, real provider inference, continuation
barriers, TLS/WAN effects and duplex realtime jitter are outside this baseline.
These require their own scoped measurements before making claims about them.

## Commands

Run from the repository root with the supported Go toolchain. Stop unrelated tests,
builds, containers and load generators during measurement. No external provider,
credentials or paid inference is used.

```sh
# Fast complete-workload correctness check; these timings are not evidence.
go test -mod=readonly -run '^$' -bench '^BenchmarkFidelity$' -benchmem -benchtime=1x -count=1 -cpu=4 ./internal/gateway

# About four minutes on the recorded machine; paths must not already exist.
node scripts/fidelity-benchmark.mjs record /tmp/fidelity-candidate.json
node scripts/fidelity-benchmark.mjs compare /tmp/fidelity-candidate.json docs/evidence/fidelity-performance/v1/replacement-budgets.json

# Runner behavior tests, included in make test-scripts.
node --test scripts/fidelity-benchmark.test.mjs
```

The record command invokes exactly:

```sh
GOMAXPROCS=4 GOGC=100 GOMEMLIMIT=off GODEBUG='' go test -mod=readonly -run '^$' -bench '^BenchmarkFidelity$' -benchmem -benchtime=2s -count=3 -cpu=4 -timeout=15m ./internal/gateway
```

For the initial baseline only, before replacement implementation:

```sh
node scripts/fidelity-benchmark.mjs record docs/evidence/fidelity-performance/v1/baseline.json
node scripts/fidelity-benchmark.mjs freeze docs/evidence/fidelity-performance/v1/baseline.json docs/evidence/fidelity-performance/v1/replacement-budgets.json
```

Evidence files are write-once. Do not replace baseline values or derive budgets
from candidate results. New workloads or materially different hardware require a
new evidence version and an explicit reason. Preserve this version as provenance.
The `freeze` command is for initial declaration, never a way to resolve a failing
candidate comparison.

## Workloads and observations

Each workload runs at concurrency 1 and 8 with GOMAXPROCS=4, three repetitions,
warm HTTP/1.1 keep-alive pools and two loopback HTTP hops. The native relay
independently enforces the fixture client credential, fixed authorized route,
provider credential injection, request size cap, redirect refusal and the same
validated/pinned egress policy. Its native request fixture is independent of the
gateway encoder. It does not perform gateway planning or Attempt accounting.

| Workload | Native reference and client-visible validation |
| --- | --- |
| `native_unary` | Chat request and one complete text result with stop reason. |
| `native_stream_256` | 256 content events of 128 bytes; exact content, identity and usage, ordered stop and DONE. |
| `native_slow_stream_64` | 64 content events of 16 KiB; client requests a 100 µs sleep after each event. Actual OS timer granularity can be larger. |
| `native_asset_png` | Deterministic valid 512 × 512 PNG; approximately 1 MiB base64/JSON request; exact original image content reaches provider. |
| `translated_unary` | Chat to Anthropic Messages with explicit max_tokens and stream=false; independent native Messages request and complete response. |
| `rejected_extension` | Unmapped required native extension on translated request; precise unsupported_parameter and zero dispatch. No relay reference is claimed for an unmappable invocation. |

There are 22 workload/concurrency/path combinations. Results include successful,
rejected and dispatched counts; request byte sizes; response content-event counts;
allocations/bytes per request; whole-process CPU per request; throughput; request
p50/p95/p99 latency; first content-event percentiles; and percentiles of each
request's largest observed inter-event gap. The latter is **not** a pooled
inter-event distribution. The artifact records exact iteration counts, duration,
source/harness hashes, toolchain, CPU/quota/memory and before/after system load.

Go's benchmark duration is a target; expensive workloads can have few observations.
Percentiles from small samples are descriptive order statistics, not stable tail
estimates or confidence bounds. Repetitions run serially in fixed workload order.
Differences of independently measured gateway and relay percentiles are reported
as descriptive differences, not paired per-request added latency.

CPU, allocations and sampled heap include the load client, provider validation,
relay/gateway and instrumentation in the same process. Heap growth is sampled every
5 ms relative to the post-GC initial heap and includes a final observation; it is
neither isolated gateway memory, exact peak memory nor RSS. Native versus gateway
comparisons use the same provider fixture and client validation. The bounded
in-memory sink discards completed envelopes instead of accumulating request data.
The current runs configure no distributed quotas or durable accounting writer.

## Frozen regression budgets

Numeric limits are recorded per workload in `v1/replacement-budgets.json` before
replacement. The declaration rule is the maximum of three baseline runs multiplied
by 1.5, plus 1 ms for timing/CPU, 16 KiB for per-request allocated bytes, or 8 MiB for
sampled heap growth. Allocation counts use 1.25 times the maximum plus 64.
The candidate's median across three repetitions must meet each limit. This headroom
allows local scheduling and GC variation; it is a regression threshold, not an
advertised service latency target. Both relay and gateway must fit to detect a
materially changed measurement environment.

Comparison fails if any workload/repetition is absent, response-event/request size
changes, successes become rejections, rejected requests dispatch, hardware/CPU
quota differs, metrics are invalid, or a measured metric exceeds its frozen limit.
It also requires at least the smaller of 20 observations and the minimum baseline
count in each repetition. Counts and the independent fixture checks remain required
regardless of timing. Workloads with unknown quality stay unknown; a fast response
cannot override a semantic-conservation failure.

## Recorded execution

See `v1/baseline.json` for raw Go output and all measured values, and
`v1/replacement-budgets.json` for the separate numeric declaration. The baseline's
comparison against its own frozen budgets verifies artifact integrity only. Actual
replacement qualification requires recording and comparing a later candidate;
that result must be reported separately.

## Later strict-contract qualification with an unchanged harness

Normal `compare` requires a legacy candidate and the exact baseline harness and
runner hashes, command, toolchain, runtime settings and measurement conditions.
An edited payload, oracle, measurement loop or fixture cannot be accepted merely
because its byte/event counts match. Reviewed harness changes need a new evidence
version; keep the old baseline and numeric budgets as historical evidence.

The harness already supports an explicit test-only contract overlay so strict
routing/profile implementations can be measured later without changing its bytes:

```sh
OLP_FIDELITY_BENCH_ROUTE_CONTRACT="$(cat /tmp/strict-route-contracts.json)" \
OLP_FIDELITY_BENCH_PROVIDER_CONTRACT="$(cat /tmp/strict-provider-contracts.json)" \
node scripts/fidelity-benchmark.mjs record /tmp/fidelity-explicit.json
node scripts/fidelity-benchmark.mjs compare-explicit /tmp/fidelity-explicit.json docs/evidence/fidelity-performance/v1/replacement-budgets.json
```

Each overlay file is an object with exactly three nonempty objects named `native`,
`translated` and `rejected`. Their members use the published runtime's actual
contract/profile fields introduced by the corresponding implementation. Provider
selection is optional if the strict implementation has an already appropriate
profile; explicit route selection is required. Only contract/profile metadata
names are allowed. Endpoint, auth, model, capability inventory, routing targets,
timeouts, defaults, source payloads and response oracles cannot be overridden.
Unknown or altered fields fail a serialize/decode/serialize check before warmup;
the current pre-OIF implementation therefore refuses future strict settings.

The runner records both overlay objects and separates legacy from explicit results.
An explicit result is strict evidence only after review confirms those recorded
fields select the intended native-identity/qualified-interaction implementation;
explicitly selecting a legacy contract does not qualify it as strict. Retain a
normal legacy candidate too. Both comparisons use the original frozen payloads,
relay, complete-response checks and numeric budgets. The strict implementation
must preserve successful translated controls and precise pre-dispatch rejection.
