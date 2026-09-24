# Qualification status receipt, version 1

This is a historical receipt. The September 24, 2026
[release-criteria amendment](release-validation.md) requires bounded performance
smoke and resource correctness instead of completion of long statistical
studies. Every feature and functional gate remains required. The historical G6
disposition below uses the earlier criteria; no failed/invalid study is now a
pass, and live empirical quality remains unknown. Current candidate validation
must be recorded separately.

This status belongs to the [compatibility matrix](compatibility-matrix-v1.json)
for locked product source `bc325da50c575803c771531dc4e3aab1ac203a53`.
It keeps three units separate: matrix rows, scripted request outcomes, and
timed benchmark workloads. The selected [public/SDK receipt](row-evidence-v1.md)
ran at ancestor `66a3ccb3fc737010a345d309ee280a541d4176f5`; all 38
pinned test-source hashes remain identical at the locked product, but the
receipt does not claim an exact-head rerun. No unqualified row was promoted.
No paid provider calls or empirical model-quality trials were performed.
Later benchmark revisions provide scoped performance evidence without
turning a failed frozen control into a pass.

| Compatibility-row outcome | Frozen 47 | Additive 45 | Total 92 |
| --- | ---: | ---: | ---: |
| Direct native behavior | 8 | 23 | 31 |
| Qualified dialect/resource mapping | 0 | 19 | 19 |
| Admitted and positive (`native` or `qualified`) | 8 | 42 | 50 |
| Pre-dispatch refused (`incompatible`) | 0 | 2 | 2 |
| Incomplete qualification (`unavailable` or `unknown`) | 39 | 1 | 40 |
| Ambiguous as a compatibility status | 0 | 0 | 0 |

The two refused rows are the OpenAI Chat reasoning-budget request against
Anthropic Messages and a nonempty unowned Gemini Live resumption handle. Both
were executed through the public route with **zero provider work**. The 40
unqualified rows remain visible in the denominator; 36 have unknown evidence
and four have no complete exposed contract. Nineteen of the fifty positives
qualify bounded dialect or owner-scoped upstream-to-local resource-ID mappings;
they are not raw native identity. These positives count
specific operation/profile/mode/client combinations, not 50 live inference
successes or provider-wide support. The 47 frozen rows themselves contain
eight scoped positives; adding another profile cannot turn a frozen row green.

Accepted-work ambiguity is not a compatibility status. Three named classes
were exercised as negative controls and counted **zero** positive completions:
an upstream file acceptance before the full client upload was sent
(`TestStrictFileEarlyProviderAcceptanceNeverLooksComplete`), and an accepted
continuation whose response was lost before ready state
(`TestContinuationConnectionLossDoesNotInventAcceptedWork` and the before-ready
process-loss case), and a realtime provider's normal WebSocket close after a
partial response but before `response.done` (`TestStrictRealtimeNormalCloseContracts`).
None may trigger fresh provider inference or publish a completed resource.
The selected public suite also exercises runtime contract violations
separately, including wrong accepted batch input identity, four late
continuation terminal/signature/event corruptions, and that premature realtime
close. Those six named
violation cases are refused or surfaced as failures; they are not included in
the 50 successful row completions. The repo's full test suite contains other
negative controls, so six is the count of these explicitly enumerated cases,
not a global number of possible protocol violations.

The full 47-row fixed inventory, all 92 matrix tuples, the seven independent
counterexamples, and the original benchmark workloads remain unchanged.
Selected public row checks passed under `-race`, including real pinned
JavaScript/Python SDK serialization and public media/resource/duplex behavior.
The final repository-wide integration/browser/CI run and two-axis code review
are distinct release gates; this receipt does not treat a passing row validator
or the existence of a fixture as their execution.

## Performance and quality limits

The original [v1 source-layer comparison](../../evidence/fidelity-performance/oif-source-v1/README.md)
recorded a failed slow native-relay c1 inter-event-gap p99 control:
3,943 µs against its unchanged 3,544 µs limit. A later clean
[`bc1e28ff` services-off capture](../../evidence/fidelity-performance/replacement-source-bc1-v1/README.md)
retained all 22 workloads and 66 repetitions with exact outcomes and sample
floors, but **still failed that same relay control**: 3,567 µs >3,544 µs. All
gateway metrics passed. The host's one-minute load rose from 0.63 to 3.29;
the capture does not prove whether scheduling caused the 23 µs miss. The
frozen source-v1 comparison therefore remains open. The encrypted
[barrier-v1 comparison](../../evidence/fidelity-performance/barrier-v1/README.md)
retains earlier failed candidate captures. The later clean
[`517455c3` candidate](../../evidence/fidelity-performance/replacement-517-v1/README.md)
completed all 288 translated two-turn workflows and 576 exact provider
dispatches with zero rejects, but **failed five unchanged small-history c1
limits**: wall and CPU per workflow plus workflow p50/p95/p99. Its load rose
from 1.07 to 2.48 during capture; the miss is retained without claiming a
source-causal regression. Two later clean
[continuation barrier captures](../../evidence/fidelity-performance/replacement-continuation-v2/README.md)
also completed every required workflow with zero rejects against those same
budgets. The `fa6e42f8` request-summary revision measured lower small/c1 CPU
and allocations than `517455c3`, but failed three wall/latency limits. The
isolated `66fb41f9` combination with an authorized one-statement submission
lookup failed four limits, including large/c8 sampled heap, and remains
unmerged. These historical failed captures remain part of the denominator;
the newer versioned paired barrier result below does not rewrite their
unchanged v1 budgets. A one-iteration semantic smoke or a complete workload
count is not a timed pass.

The separate native [lifecycle-stress-v1 reference](../../evidence/fidelity-performance/lifecycle-stress-v1/README.md)
has a frozen 4 MiB input/1 MiB slow content/64-duplex-event workload and
predeclared numeric budgets. Its strict attempt stopped at route activation
because that frozen fixture used an unversioned media profile; the frozen
`lifecycle-v1` strict attempt separately stopped at its legacy durable-ID
oracle. Neither old strict fixture is relabeled as a pass or rewritten. The
separately versioned
[`lifecycle-stress-v2` registered-profile comparison](../../evidence/fidelity-performance/replacement-383-v2/README.md)
froze a native reference and budgets first, then **passed every stated strict
and legacy workload and numeric limit** at product source `38358293`. That is
scoped scripted media/duplex evidence; it does not replace either old v1
strict attempt, the original slow-relay control, or the failed encrypted
continuation barrier. Rejected, incomplete and ambiguous workflows remain
visible outside successful timing samples.

The [locked `bc325da5` final-source
record](../../evidence/fidelity-performance/final-bc325-v2/README.md) adds
four successful **scoped** local comparisons without changing those old
results. Strict native lifecycle v2 passed 48 repetitions, 1,152 exact
dispatches, 288 independent mapping/retrieval checks and 12 wrong-owner
zero-dispatch negatives. Registered-profile stress v2 passed separately in
strict and legacy modes, each with 36 repetitions, 288 workflows, 576 exact
dispatches and three zero-dispatch negatives. Paired encrypted-barrier attempt
2 passed its committed B-only reference and all candidate bounds over 128
blocks, 18,432 B and 6,144 C workflows, 49,152 exact dispatches and zero
rejects. The original barrier-v1 failures still fail their old limits.

The exact-source [paired source r6
attempt](../../evidence/fidelity-performance/source-paired-v2/README.md) is
**invalid**, with zero complete paired blocks and an incomplete reservation
journal. Its generic first-C-subrun oracle/effect/count error does not identify
a cause or establish a source-cost pass. The registered attempt remains visible
without selecting another timing sample. Thus G6 remains **open**, even though
the lifecycle, stress and barrier v2 scopes passed.

All 92 rows have `empirical_quality: unknown`. The
[preregistered study](quality-plan.md) requires approved same-serving-identity
direct/direct and randomized direct/OLP trials before any intelligence parity
claim. Local scripted providers establish deterministic wire and effect
behavior, not live quality, latency, billing or provider stability. Missing
or inconclusive trials remain unknown rather than passing by default.

## #218 completion-gate crosswalk at the locked product

The [specification's G1–G7 gates](../../../OLP-implementation-spec.md) are
broader than a positive compatibility row. This crosswalk states what is
actually recorded at `bc325da5` and what remains for the final release audit.

| Gate | Recorded scope | Disposition |
| --- | --- | --- |
| G1 conservation | [Frozen fixtures](baseline-validation.md), [source conservation](oif-source.md), and [public preserve-or-reject rows](row-evidence-v1.md). | Scoped evidence integrated; final counterexample audit pending. |
| G2 complete interaction | [Native SDK](native-sdk.md), [negotiated continuation/recovery](continuation.md), and [paired barrier](../../evidence/fidelity-performance/paired-barrier-v2-attempt2/README.md). | Public next-turn and actionability evidence recorded; final exact-head audit pending. |
| G3 operation breadth | [Unary operations](unary-operations.md), [strict media](media-strict.md), [durable resources](durable-lifecycle.md), and [realtime](strict-realtime.md). | Scoped positives; 40 matrix rows remain unavailable or unknown, and final breadth audit is pending. |
| G4 configuration/security | [Provider profiles](../../provider-profiles.md), [native configuration storage](native-configuration-storage.md), and [route migration](strict-route-migration.md). | Targeted authority, policy, egress and migration evidence; final integrated audit pending. |
| G5 usable control plane | [Configuration editors](console-configuration.md) and [inspector/playgrounds](console-interaction.md), including reviewed screenshots. | Targeted UI/API evidence; exact-head browser and CI checks pending. |
| G6 quality/performance | [Final-source v2 captures](../../evidence/fidelity-performance/final-bc325-v2/README.md), [invalid source r6](../../evidence/fidelity-performance/source-paired-v2/README.md), immutable v1 failures and the [quality plan](quality-plan.md). | **Open:** no passing source r6 result; frozen v1 failures remain and empirical model quality is unknown. |
| G7 sustainable delivery | [Extension demos](extension-demos.md), [mixed-version migration](strict-route-migration.md), the [release inventory](../../../deploy/release-inventory.json), and generated contracts. | Functional scope recorded; exact-head integration, SDK, recovery, browser/CI and two-axis review pending. |

[#218](https://github.com/tyk-swe/olp/issues/218) remains open. The selected
31-case public/SDK run was at ancestor `66a3ccb3`; final exact-head repository,
service, recovery, SDK and browser checks, inventory reconciliation and the
mandatory two-axis review must be reported separately. A passing matrix
validator or one positive tuple does not complete a specification gate.
