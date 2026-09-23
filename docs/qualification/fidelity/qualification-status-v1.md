# Qualification status receipt, version 1

This status belongs to the [compatibility matrix](compatibility-matrix-v1.json)
assessed at `44c88077888cc1274dd3d3687a7dca48f084ac91`. It keeps three
different units separate: matrix rows, scripted request outcomes, and timed
benchmark workloads. The test receipts are [row-evidence-v1.md](row-evidence-v1.md)
and the per-row source/run hashes in the JSON artifact. No paid provider calls
or empirical model-quality trials were performed.

| Compatibility-row outcome | Frozen 47 | Additive 44 | Total 91 |
| --- | ---: | ---: | ---: |
| Admitted and positive (`native` or `qualified`) | 7 | 41 | 48 |
| Pre-dispatch refused (`incompatible`) | 0 | 2 | 2 |
| Incomplete qualification (`unavailable` or `unknown`) | 40 | 1 | 41 |
| Ambiguous as a compatibility status | 0 | 0 | 0 |

The two refused rows are the OpenAI Chat reasoning-budget request against
Anthropic Messages and a nonempty unowned Gemini Live resumption handle. Both
were executed through the public route with **zero provider work**. The 41
unqualified rows remain visible in the denominator; 36 have unknown evidence
and five have no complete exposed contract. Forty-eight positives count
specific operation/profile/mode/client combinations, not 48 live inference
successes or provider-wide support. The 47 frozen rows themselves contain
seven scoped positives; adding another profile cannot turn a frozen row green.

Accepted-work ambiguity is not a compatibility status. Two named classes
were exercised as negative controls and counted **zero** positive completions:
an upstream file acceptance before the full client upload was sent
(`TestStrictFileEarlyProviderAcceptanceNeverLooksComplete`), and an accepted
continuation whose response was lost before ready state
(`TestContinuationConnectionLossDoesNotInventAcceptedWork` and the before-ready
process-loss case). Neither may trigger fresh provider inference or publish a
completed resource. The selected public suite also exercises runtime contract
violations separately, including wrong accepted batch input identity and four
late continuation terminal/signature/event corruptions. Those five named
violation cases are refused or surfaced as failures; they are not included in
the 48 successful row completions. The repo's full test suite contains other
negative controls, so five is the count of these explicitly enumerated cases,
not a global number of possible protocol violations.

The full 47-row fixed inventory, all 91 matrix tuples, the seven independent
counterexamples, and the original benchmark workloads remain unchanged.
Selected public row checks passed under `-race`, including real pinned
JavaScript/Python SDK serialization and public media/resource/duplex behavior.
The final repository-wide integration/browser/CI run and two-axis code review
are distinct release gates; this receipt does not treat a passing row validator
or the existence of a fixture as their execution.

## Performance and quality limits

The original [v1 source-layer comparison](../../evidence/fidelity-performance/oif-source-v1/README.md)
still records a failed slow native-relay c1 inter-event-gap p99 control:
3,943 µs against its unchanged 3,544 µs limit. The encrypted
[barrier-v1 comparison](../../evidence/fidelity-performance/barrier-v1/README.md)
retains two earlier failed candidate captures. A quiet a23 revision candidate
also failed five frozen small-history c1 limits (wall/CPU and workflow
p50/p95/p99); its write-once capture is retained outside the repository at
`/tmp/olp-spec-context/candidate-v2-a23-20260923.json`. Current compiled-plan
and database optimizations need a new final-source timed capture against the
**same** frozen criteria. A passing one-iteration semantic smoke is not a
timed comparison.

The separate native [lifecycle-stress-v1 reference](../../evidence/fidelity-performance/lifecycle-stress-v1/README.md)
has a frozen 4 MiB input/1 MiB slow content/64-duplex-event workload and
predeclared numeric budgets. Its strict replacement comparison has not passed;
the first correctly configured strict semantic attempt failed at route
activation because the frozen fixture used an unversioned media profile. The
fixture and budget cannot be silently rewritten to make a strict workload
positive. The original native lifecycle and encrypted barrier references are
also separate from this matrix. Rejected, incomplete and ambiguous workflows
must remain visible outside successful timing samples.

All 91 rows have `empirical_quality: unknown`. The
[preregistered study](quality-plan.md) requires approved same-serving-identity
direct/direct and randomized direct/OLP trials before any intelligence parity
claim. Local scripted providers establish deterministic wire and effect
behavior, not live quality, latency, billing or provider stability. Missing
or inconclusive trials remain unknown rather than passing by default.

## Gate disposition at this assessed revision

G1 conservation and G2 complete-interaction have selected public positives,
preserve-or-reject negatives, official next-turn clients and recovery checks.
G3 breadth has scoped unary, media, durable and Gemini duplex positives, while
direct OpenAI translation/realtime and strict streaming background remain
unavailable in the fixed inventory. G4 configuration/security and G5 control
plane have targeted public and browser evidence in their own receipts. G6
performance remains open at the failed/pending frozen comparisons above;
empirical model quality remains unknown by design without approved trials. G7
requires the final integrated checks, additive inventory reconciliation,
migration/rollback, and mandatory two-axis review. A gate is not marked passed
merely because one positive tuple exists in this matrix.
