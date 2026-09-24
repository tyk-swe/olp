# Registered-profile lifecycle stress reference — version 2

This is a **new, separately frozen** native reference for the same 4 MiB video asset, 1 MiB slow-reader content, and 64-event duplex workloads described in [lifecycle-stress-v1](../lifecycle-stress-v1/README.md). Version 1 remains immutable, including its baseline and budgets. Its attempted true strict candidate failed at route activation because the video fixture used an unversioned provider configuration; the failed run is retained in `/tmp/olp-spec-context/merge-lifecycle-true-strict-semantic.log`. Changing that fixture would invalidate the v1 harness hash, so v2 adds a registered serving profile and must establish new numeric criteria before any replacement candidate.

The video provider is publicly configured as direct OpenAI `openai-responses` profile revision `1`; realtime remains the registered Azure v1 Responses profile revision `1`. The v2 test reads both persisted provider configurations and fails unless those exact profile identities are present. Its local seeded video capability certificate is bound to the registered transport/credential fingerprint; no paid certification call runs. The public route selects explicit legacy fidelity for the reference or explicit strict fidelity for a later candidate. The local scripted provider, authenticated native relay, asset bytes and SHA-256 check, original controls, response/status oracle, client byte check, realtime event grammar, 4 MiB/1 MiB/64 workload sizes, 1/2 media and 1/4 duplex concurrency, eight samples × three repetitions, and three zero-dispatch media negative controls are unchanged from v1.

The v2 runner pins the v2 test hash, the unchanged v1 shared helper hash, the independent native duplex fixture hash, its own runner hash, hardware/runtime/storage settings, and both registered profile identities. It refuses dirty sources, incomplete or dirty candidate captures, missing accepted/partial/retrieval/media/event observations, missing or dispatched negative controls, altered fixture hashes, and changed measurement conditions. Failed process output is retained separately as a write-once `.failed-*.json`, never as passing evidence. The reused v1 negative-control helper keeps its original log marker; the v2 runner deliberately parses that marker and pins the helper hash. No v1 code or threshold is rewritten.

## Commands

Use the standard local integration-service environment. An explicitly selected test must fail if PostgreSQL is absent. Keep required PostgreSQL/Valkey containers running but idle for timed captures. Pause unrelated local builds/tests/service traffic and coordinate a comparable host-load window.

```sh
# Correctness only; smoke timings are not performance evidence.
go test -mod=readonly -tags=integration -run '^TestLifecycleStressV2Performance$' -count=1 ./tests/integration
node --test scripts/lifecycle-stress-v2-benchmark.test.mjs

# Write-once clean pre-#216 native reference, before any candidate.
node scripts/lifecycle-stress-v2-benchmark.mjs record docs/evidence/fidelity-performance/lifecycle-stress-v2/baseline.json
node scripts/lifecycle-stress-v2-benchmark.mjs freeze docs/evidence/fidelity-performance/lifecycle-stress-v2/baseline.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
node scripts/lifecycle-stress-v2-benchmark.mjs compare docs/evidence/fidelity-performance/lifecycle-stress-v2/baseline.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json

# On a clean final integrated PR revision under comparable conditions.
node scripts/lifecycle-stress-v2-benchmark.mjs record /tmp/lifecycle-stress-v2-legacy-candidate.json
node scripts/lifecycle-stress-v2-benchmark.mjs compare /tmp/lifecycle-stress-v2-legacy-candidate.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
OLP_LIFECYCLE_STRESS_V2_ROUTE_FIDELITY='{"mode":"strict"}' node scripts/lifecycle-stress-v2-benchmark.mjs record /tmp/lifecycle-stress-v2-strict-candidate.json
node scripts/lifecycle-stress-v2-benchmark.mjs compare-strict /tmp/lifecycle-stress-v2-strict-candidate.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
```

The frozen limit rule is declared before candidate capture: the maximum of three legacy native-reference repetitions ×1.5, plus 1 ms for timing/CPU, 16 KiB allocated bytes, 8 MiB sampled whole-process heap or 1 MiB sampled spool; allocation counts use ×1.25 +64. Added p50/p95/p99 latency bounds use the maximum positive gateway-minus-relay reference difference ×1.5 +1 ms. The candidate median must meet every workload/path bound, while complete semantics, three local rejections and zero final spool reservation have no tolerance. These are descriptive local regression limits, not production tail SLOs. See v1 for measurement attribution and unmeasured quality scope; partial batch item errors, live intelligence parity, WAN/TLS inference hops and encrypted tool barriers remain outside this supplement.

## Execution status

The registered-profile public semantic smoke and six runner mutation tests passed before timed capture. The v1 source and runner hashes exactly match their frozen artifact. A first quiet-window `record` attempt was rejected by the runner: it exported the v1 measurement variable, so the Go test produced only one repetition of two samples instead of the required three repetitions of eight. The write-once [failed capture](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/lifecycle-stress-v2/baseline.json.failed-20260923T025739193Z.json) retains that incomplete output. The runner was corrected before any baseline or budget was frozen.

Clean pre-#216 source `d78be007` then recorded the [native baseline](baseline.json) on 2026-09-23 with 36/36 repetitions, 288 completed workflows, 576 exact provider dispatches, all accepted/partial/retrieval/media-byte/duplex-event observations and three local rejections with zero provider work or spool residue. The Go test took 22.33 seconds; one-minute host load was 0.69 before and 0.96 after, with PostgreSQL 18.6 local without TLS. The separate [replacement budgets](replacement-budgets.json) were frozen from this reference before any v2 replacement candidate. The baseline self-comparison passes, establishing artifact integrity only.

The clean final source `bc325da5` separately captured
[strict](../final-bc325-v2/stress-strict.json) and
[legacy](../final-bc325-v2/stress-legacy.json) replacements. Each offline
comparison passed the unchanged v2 limits with 36 repetitions, 288 completed
workflows, 576 exact dispatches and three zero-dispatch negatives. The strict
and legacy SHA-256 values are respectively
`e172116d960f7a7e3c17f36fd3c1839235a2b35441173022dfbc9b14d54ec0df`
and `2975425365c11e8104cc63cedca95a2e220a1746f7e4323b65f61155882997b1`.
The frozen v1 failure is unchanged; source-cost and empirical-quality gaps
still leave full G6 open.
