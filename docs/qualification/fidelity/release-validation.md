# Fidelity release validation

The September 24, 2026 release-criteria amendment in
[T08, T09 and G6](../../../OLP-implementation-spec.md) makes long statistical
quality and performance studies optional follow-up work. All 66 user stories,
product decisions, operation coverage and public behavior remain in scope.
This amendment changes release requirements; it does not change a recorded
study's criteria or turn its failed, invalid or unknown result into a pass.

Release still requires semantic conservation and corruption controls, complete
supported interactions, authorization/egress and sensitive-state protection,
resource/overflow correctness, recovery and migration, official SDKs, browser
journeys, unchanged independent fixture inventories, generated contracts,
repository checks and code review. A fixture's existence or an ancestor's run
does not establish candidate execution. Report actual commands and source
revisions separately from historical evidence.

## Bounded performance smoke

Run the canonical local benchmark with a fixed iteration count and timeout:

```sh
go test -mod=readonly -run '^$' -bench '^BenchmarkFidelity$' \
  -benchmem -benchtime=1x -count=1 -cpu=4 -timeout=2m ./internal/gateway
```

This covers all 22 native-relay/gateway workload combinations: native unary,
native streaming, slow readers, large assets, translated unary and precise
pre-dispatch rejection at concurrency 1 and 8. Every request retains the
independent provider/client oracle and dispatch-count checks. Record output,
source revision, elapsed duration, outcomes and resource observations. Short
samples expose gross regressions and stalls; their percentiles are descriptive,
not stable tail estimates or an empirical performance qualification.

Keep resource correctness in ordinary and service validation. Representative
existing checks include `TestStreamsUsePerEventLimitNotUnaryResponseLimit`,
`TestRealtimeBoundedWriterStopsSlowPeer`,
`TestRealtimeBoundedWriterPropagatesParentCancellation`,
`TestMediaCapacityWithSlowReadersAndMassDisconnects`,
`TestGeminiLiveSlowReaderAppliesBackpressureAndReleasesSession`, and the
continuation actionability/recovery tests. These supplement the full required
`make check`, `make test-race` and `make integration` validation; a timing smoke
does not replace them. Investigate unexpected failure, dispatch, unbounded
growth, stalls and clearly material regressions before shipping.

## Optional studies and historical results

The [quality study](quality-plan.md) remains unexecuted and empirical quality
remains unknown. Live evaluations need the approvals specified there. The
[performance record](../../evidence/fidelity-performance/README.md) retains
frozen baseline/budget bytes and historical failed/invalid outcomes. Repeated
paired-source captures, detailed latency/tail/jitter surveys and statistical
studies are optional; no new study version is required to ship. Any later
empirical claim still needs valid scoped evidence under its predeclared
criteria. Semantic defects and functional regressions remain release blockers.
