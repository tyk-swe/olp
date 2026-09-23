# Qualification status receipt, version 1

This status belongs to the [compatibility matrix](compatibility-matrix-v1.json)
for product source `66a3ccb3fc737010a345d309ee280a541d4176f5`, with a
docs-only evidence anchor at `2426aec0c6f6c39c1fcb48fdc8924b58f198862c`.
It keeps three
different units separate: matrix rows, scripted request outcomes, and timed
benchmark workloads. The test receipts are [row-evidence-v1.md](row-evidence-v1.md)
and the per-row source/run hashes in the JSON artifact. No paid provider calls
or empirical model-quality trials were performed. This matrix was reassessed
through the selected [public/SDK receipt](row-evidence-v1.md); no unqualified
row was promoted. Benchmark revisions below update only scoped performance
evidence and do not turn a failed frozen control into a pass.

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
unmerged. Neither capture qualifies G6; source changes after `fa6e42f8`
require another final-source comparison. A one-iteration semantic smoke or a
complete workload count is not a timed pass.

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
visible outside successful timing samples. Later product revisions require
their own appropriate frozen comparison before an exact-head performance pass.

All 92 rows have `empirical_quality: unknown`. The
[preregistered study](quality-plan.md) requires approved same-serving-identity
direct/direct and randomized direct/OLP trials before any intelligence parity
claim. Local scripted providers establish deterministic wire and effect
behavior, not live quality, latency, billing or provider stability. Missing
or inconclusive trials remain unknown rather than passing by default.

## Gate disposition for the assessed matrix and later scoped benchmarks

G1 conservation and G2 complete-interaction have selected public positives,
preserve-or-reject negatives, official next-turn clients and recovery checks.
G3 breadth has scoped unary, media, durable, direct OpenAI/Azure realtime and
Gemini duplex positives, while audio translation and strict Responses
streaming background remain unavailable in the fixed inventory. G4
configuration/security and G5 control
plane have targeted public and browser evidence in their own receipts. G6
performance remains open at the failed original-v1 and encrypted-barrier
comparisons, despite the scoped registered-profile stress-v2 passes above;
empirical model quality remains unknown by design without approved trials. G7
requires the final integrated checks, additive inventory reconciliation,
migration/rollback, and mandatory two-axis review. A gate is not marked passed
merely because one positive tuple exists in this matrix.
