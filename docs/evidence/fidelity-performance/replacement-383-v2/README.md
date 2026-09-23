# Registered-profile stress replacement at `38358293`

The [lifecycle-stress-v2 native reference and budgets](../lifecycle-stress-v2/README.md)
were frozen from clean pre-#216 source `d78be007` before either replacement
candidate. The reference's 36 repetitions completed all original 4 MiB video,
slow 1 MiB content and 64-event duplex workflows at one-minute host load
0.69→0.96. These later clean candidates used the **same** runner, harness,
profiles, scripted provider, original bytes, observation counts, local
PostgreSQL 18.6/no TLS, hardware and predeclared numeric limits. Each JSON
artifact pins its own source, full raw output, runner/harness/reference/budget
hashes and exact host conditions. No live model or paid inference was used.

| Product source revision and contract | Write-once candidate | Frozen comparison |
| --- | --- | --- |
| `4aa83d7b`, strict | [retained failure](stress-v2-strict-4aa-failed.json) | Failed only duplex c1 added-latency p95/p99: 4,265 µs > 3,294 µs. |
| `4aa83d7b`, legacy | [retained failure](stress-v2-legacy-4aa-failed.json) | Failed only duplex c1 added-latency p95/p99: 3,636 µs > 3,294 µs. |
| `38358293`, strict | [passing candidate](stress-v2-strict-383-passed.json) | **Passed every frozen workload/path, numeric and added-latency limit.** |
| `38358293`, legacy | [passing candidate](stress-v2-legacy-383-passed.json) | **Passed every frozen workload/path, numeric and added-latency limit.** |

The successful strict and legacy captures each contain 36/36 repetitions,
288 completed workflows, 576 exact provider dispatches, all accepted/50%-partial/
completed status, byte-for-byte asset/content and 6,144 RTT/6,048 jitter
observations, plus all three required gateway rejections with **zero** provider
work and spool residue. There were no missing, incomplete or ambiguous timed
workflows. Strict host load was 0.40→0.72; legacy was 0.66→0.73. The same
controls failed before the reviewed product change: a bounded, per-direction
write watchdog replaced per-frame timer construction, and the legacy usage
observer skipped native events that cannot contain `response.done`. Native
frames, limits, policy, ownership, cancellation, revocation and terminal
semantics remained subject to the public race suites; no workload or budget was
changed to obtain a pass.

Reproduce the comparisons from this source or an unchanged later runner:

```sh
node scripts/lifecycle-stress-v2-benchmark.mjs compare-strict docs/evidence/fidelity-performance/replacement-383-v2/stress-v2-strict-383-passed.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
node scripts/lifecycle-stress-v2-benchmark.mjs compare docs/evidence/fidelity-performance/replacement-383-v2/stress-v2-legacy-383-passed.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
```

The earlier [38e failures](../replacement-38e-v1/README.md) remain part of the
history. This supplement qualifies the stated local scripted media/duplex
workloads only. The frozen encrypted continuation barrier and original
22-workload relay control remain open, as do empirical intelligence parity,
WAN/TLS inference and isolated gateway RSS. It does not by itself close G6 or
turn an unavailable protocol/model combination into a native claim.
