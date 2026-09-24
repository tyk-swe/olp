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

## Current execution and gate crosswalk

The [September 24 execution receipt](release-execution-2026-09-24.md) records
committed code `3a3a3644ab06220478e07080e864f19e9e540c05`: `make check`,
`make test-race`, `make build`, contract/inventory/Helm checks, 50 selected
public/SDK race tests with zero skips, both native SDK suites and the bounded
strict smoke all passed. The [matrix](compatibility-matrix-v1.md) pins that
source and receipt; later evidence publication does not change executable
product or protocol-test code.

| Gate | Evidence and required validation |
| --- | --- |
| G1 Conservation | Independent frozen fixtures and mutation controls; source/oracle suites in `make check` and race checks; public conservation, strict native/translated and ambiguity tests. |
| G2 Complete interaction | Native and translated official SDK next turns, encrypted continuation and acceptance/actionability recovery; selected public races plus full recovery/process CI. |
| G3 Operation breadth | Registered unary storage/rerank/count/classification, native Cohere, audio translation, media/video, file/batch, Responses and Gemini durable streams, realtime; public boundary tests and the unchanged denominator. |
| G4 Configuration/security | Public configuration/profile/egress/policy suites, secret/identity negative controls, revocation and canceled-client commit races; full service CI. |
| G5 Usable control plane | 652 local console tests, clean Svelte/types/lint and reviewed screenshots; full CI browser journeys for configuration, inspection, operation playgrounds and migration. |
| G6 Honest quality/performance | All 22 bounded strict smoke paths passed; per-event limits, slow-reader backpressure, capacity, cancellation and overflow remain functional/race/service checks. Empirical quality remains unknown and historical failed/invalid studies remain visible. |
| G7 Sustainable delivery | Extension demos and forward/mixed-version migration in full CI, explicit legacy migration, generated contracts, release inventory, build, documentation and independent Standards/Spec reviews. |

The [final PR CI checks](https://github.com/tyk-swe/olp/pull/219/checks) must
pass on the published head before the PR is ready. They run `make check`,
`make test-race`, **full `make integration`** (disposable services, recovery,
mixed versions, SDKs and Chromium), dependencies, Helm, and amd64/arm64 native
image qualification. CI records its exact source and results separately;
neither an ancestor's green run nor the selected local tests substitute for
that final run. No feature gate is waived by the benchmark amendment.

The first [published-head CI run](https://github.com/tyk-swe/olp/actions/runs/35992739073)
passed its other jobs but failed one integration assertion in
`TestGeminiAcceptedResourceCommitsOutliveClientDisconnect`. The test had
observed a queued PostgreSQL status UPDATE, cancelled the client, released its
blocking row lock, then used a plain MVCC SELECT that could still see the old
committed state before the detached update finished. The assertion now uses a
row-locking read queued behind that observed writer. Production code and the
earlier execution receipt are unchanged; full CI must rerun on the corrected
head.

## Bounded performance smoke

Run the canonical local benchmark with a fixed iteration count and timeout:

```sh
OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}' \
OLP_FIDELITY_BENCH_PROVIDER_CONTRACT='{"native":{"profile_id":"compatible-chat","profile_revision":"1"},"translated":{"profile_id":"anthropic-messages","profile_revision":"1"},"rejected":{"profile_id":"anthropic-messages","profile_revision":"1"}}' \
go test -mod=readonly -run '^$' -bench '^BenchmarkFidelity$' \
  -benchmem -benchtime=64x -count=1 -cpu=4 -timeout=2m ./internal/gateway
```

The explicit overlays select strict routes and versioned compatible-chat and
Anthropic profiles; without them the harness selects legacy behavior. This
covers all 22 native-relay/strict-gateway workload combinations: native unary,
native streaming, slow readers, large assets, translated unary and precise
pre-dispatch rejection at concurrency 1 and 8, with 64 measured requests per
combination (1,280 successful dispatches and 128 intentional zero-dispatch
rejections, excluding warmup and benchmark calibration). Every request retains the
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
