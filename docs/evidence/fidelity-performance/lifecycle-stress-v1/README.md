# Native lifecycle stress reference — version 1

This is a separately versioned supplement to the unchanged [original baseline](../README.md), [native lifecycle baseline](../lifecycle-v1/README.md), and [encrypted continuation barrier](../barrier-v1/README.md). It uses a local scripted provider, an authenticated minimal native relay, and public gateway routes backed by isolated PostgreSQL and the real bounded media spool. No external model or paid inference is used.

The reference is the **legacy native** route before the #216 lifecycle replacement. Its receipt and event oracles are authored in `tests/integration/lifecycle_stress_performance_test.go`; media originals are checked by SHA-256 at the provider, and the client checks every accepted receipt, partial/terminal status, downloaded byte, and realtime event. This measures preservation and process resource cost, not model intelligence.

## Fixed inventory

| Workload | Public contract and required counts per eight-sample repetition |
| --- | --- |
| `media_cycle_4m` | Each request uploads one exact 4 MiB image `input_reference` to create a video job, sees the accepted ID, polls one `in_progress` 50% status and one `completed` 100% status, then retrieves exact 1 MiB video content. Eight accepted jobs, eight partial statuses, sixteen status retrievals, eight content retrievals, 32 provider dispatches. |
| `media_slow_content_1m` | Content retrieval of a previously accepted/completed job. The client delays 250 µs before each 8 KiB read and verifies all bytes. Eight content retrievals and eight provider dispatches. Setup creates its jobs outside measurement. |
| `duplex_jitter_64` | Sixty-four exact bidirectional native JSON/audio exchanges per session through the existing independent native lifecycle fixture, followed by abrupt client closure and observed provider closure. Eight sessions, 512 events, eight dispatches. |

Media runs at concurrency 1 and 2; duplex runs at 1 and 4. There are twelve workload/concurrency/path combinations and three repetitions with eight successful samples each. The two concurrent media workers use different, equally authorized API keys because one key may own only one multipart parser lease. A same-key second upload is an explicit capacity refusal. The native relay receives distinct reference-key identities and injects the same provider credential. Fixed order, provider fixture, operation controls, warm connection pools, loopback placement, no TLS on measured inference hops, and Go runtime settings are recorded in each artifact.

Three gateway negative controls are mandatory outside timed samples: a 21 MiB over-limit video input, cancellation during a partially uploaded image, and a second upload while the same key has a held parser. Each must have one local rejection, zero provider dispatch, and zero final spool reservation. They stay in the denominator even though they do not count as successful quality samples. Any missing successful or rejected outcome, provider dispatch, accepted job, partial status, retrieval, byte or event makes comparison invalid.

Measured fields include request latency p50/p95/p99; first content byte and inter-read gap; duplex event RTT and absolute successive RTT-difference jitter p50/p95/p99; cancellation propagation; process CPU, allocations and bytes per request; 1 ms sampled whole-process heap growth; and sampled/final spool reservations. Gateway-minus-relay latency percentiles are descriptive differences between independent samples, not paired causal estimates. Samples of eight are descriptive order statistics, not reliable production tail SLOs. CPU, allocations and heap include client, provider, validation, relay/gateway and instrumentation; PostgreSQL is separate. Heap sampling can miss shorter peaks and is not gateway RSS. Slow-reader gaps include the requested delay and OS timer granularity. The duplex fixture is shared with `lifecycle-v1`; the new jitter calculation and media receipt/byte oracles are separate.

This version does **not** measure partial batch item errors, encrypted translated-tool ready barriers, live-provider quality, WAN/TLS inference hops, isolated gateway RSS, real-provider VAD/media quality, or exactly-once work. Those remain separate qualifications. The video `in_progress` sample is partial progress, not a partial-failure batch result. The original relay slow-stream budget failure and any continuation candidate failures remain visible in their own evidence versions.

## Commands and acceptance

Use the standard integration service variables from `tests/README.md`; an explicitly selected run fails without PostgreSQL. Keep required PostgreSQL/Valkey idle and pause unrelated builds or captures for timed runs. Do not print service URLs or credentials.

```sh
# Semantic smoke and mutation tests; smoke timings are never evidence.
go test -mod=readonly -tags=integration -run '^TestLifecycleStressPerformance$' -count=1 ./tests/integration
node --test scripts/lifecycle-stress-benchmark.test.mjs

# Write-once native reference, only from a clean pre-replacement commit.
node scripts/lifecycle-stress-benchmark.mjs record docs/evidence/fidelity-performance/lifecycle-stress-v1/baseline.json
node scripts/lifecycle-stress-benchmark.mjs freeze docs/evidence/fidelity-performance/lifecycle-stress-v1/baseline.json docs/evidence/fidelity-performance/lifecycle-stress-v1/replacement-budgets.json
node scripts/lifecycle-stress-benchmark.mjs compare docs/evidence/fidelity-performance/lifecycle-stress-v1/baseline.json docs/evidence/fidelity-performance/lifecycle-stress-v1/replacement-budgets.json

# Later, on the complete replacement PR branch, keep the same runner and controls.
node scripts/lifecycle-stress-benchmark.mjs record /tmp/lifecycle-stress-legacy-candidate.json
node scripts/lifecycle-stress-benchmark.mjs compare /tmp/lifecycle-stress-legacy-candidate.json docs/evidence/fidelity-performance/lifecycle-stress-v1/replacement-budgets.json
OLP_LIFECYCLE_STRESS_ROUTE_FIDELITY='{"mode":"strict"}' node scripts/lifecycle-stress-benchmark.mjs record /tmp/lifecycle-stress-strict-candidate.json
node scripts/lifecycle-stress-benchmark.mjs compare-strict /tmp/lifecycle-stress-strict-candidate.json docs/evidence/fidelity-performance/lifecycle-stress-v1/replacement-budgets.json
```

The runner selects an explicit legacy fidelity on both video and duplex routes for the reference. `compare-strict` requires `{ "mode": "strict" }` on both actual published routes; strict activation failure is a failure, not permission to fall back to legacy. The runner refuses to overwrite evidence, validates every run and negative control, and pins its Go harness and native duplex fixture hashes plus hardware/runtime/storage conditions. A failed process or incomplete output writes a separate, write-once `.failed-*.json` capture rather than a passing artifact. Relevant provisioning helper changes still require review before interpreting a candidate.

Before seeing a replacement, the budget is frozen from the maximum of three native-reference repetitions ×1.5, plus 1 ms for timing/CPU, 16 KiB allocated bytes, 8 MiB sampled heap or 1 MiB sampled spool. Allocations use ×1.25 +64. Added latency uses the maximum positive gateway-minus-relay reference difference ×1.5 +1 ms. Candidate medians must meet **every** fixed bound. Zero final spool reservation, complete counts and negatives have no tolerance. Neither a baseline self-comparison nor a fast scripted fixture establishes full G6 or intelligence parity; failures and unknown scopes must be retained.

## Execution status

The public semantic smoke and selected integration race passed, as did tagged Go vet, all script tests and six runner mutations (including failed-capture retention). Explicit selection without PostgreSQL failed as required. From clean source `6386a2e1` on 2026-09-23, a coordinated quiet-window reference recorded 36 repetitions in 26.2 seconds wall time: 288 completed workflows, 576 provider dispatches, 96 accepted jobs, 96 partial progress statuses, 288 retrievals, 6,144 exact duplex events/RTT observations, 6,048 jitter observations, and all three required local rejections with zero provider dispatch or spool residue. Host load was 1.07 before and 1.64 after; PostgreSQL 18.6 was running locally without TLS. `replacement-budgets.json` froze the stated rule from that reference before any #216 candidate. The baseline self-comparison passed, proving artifact integrity only. Legacy and strict replacement captures remain pending, as do the separate original-v1 slow-relay and translated-continuation failures; this is not a passing G6 report.
