# Exact-source scripted performance at `bc325da5`

These write-once captures measured the final production source
`bc325da50c575803c771531dc4e3aab1ac203a53` on 2026-09-23. Each copied
artifact is byte-identical to its original capture. They use local scripted
providers and the predeclared v2 methods and criteria; they make no claim about
paid-provider intelligence, WAN/TLS latency, or isolated gateway RSS.

| Capture | SHA-256 | Offline result |
| --- | --- | --- |
| [Strict native lifecycle](../lifecycle-v2/strict-candidate.jsonl) | `cc22b2ff1f2c760ba1f1f9b782e1ad6fbf081abd6427aef89f369f736369a375` | Pass |
| [Strict registered-profile stress](stress-strict.json) | `e172116d960f7a7e3c17f36fd3c1839235a2b35441173022dfbc9b14d54ec0df` | Pass |
| [Legacy registered-profile stress](stress-legacy.json) | `2975425365c11e8104cc63cedca95a2e220a1746f7e4323b65f61155882997b1` | Pass |
| [Paired encrypted barrier](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json) | `7fe2b9a6e1f1faabcc4c779b2dd8b052c7bb2fcbb8ad1ff87719135f275b7e8c` | Pass |
| [Barrier reservation journal](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json.journal.jsonl) | `515f55e17261bd13ec33b64dc33be4447e9c2c0ab9b09dcbfb0778c43eceaceb` | Complete |
| [Source paired r6 attempt](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json) | `33f5efb6dc681b2be570e5e2352e53ea35f01d8adfbaff836a05083a8bf1f023` | Invalid |
| [Source r6 reservation journal](https://github.com/tyk-swe/olp/blob/16769b6abee5008d33279542ccd96088254a6c82/docs/evidence/fidelity-performance/source-paired-v2/paired-r6.json.journal.jsonl) | `25f76f751721fa4252d4969e5c0583ea354a1deecd34eeb13e0953552c41bdde` | Incomplete |

The strict native lifecycle comparison passed every frozen v1 and prospective
v2 limit. Its 48 repetitions contained 1,152 complete native dispatches, 288
independent durable mappings and public retrievals, 12 wrong-owner negative
controls, and no negative dispatch or ambiguous outcome. The registered-profile
stress strict and legacy comparisons each passed their separately frozen v2
limits with 36 repetitions, 288 successful workflows, 576 dispatches, and
three zero-dispatch local negatives. Strict and legacy are separate results.

The paired encrypted barrier attempt 2 passed its committed B-only reference,
absolute controls, and prospective candidate criteria. Its 128 paired blocks
retained 18,432 B workflows and 6,144 C workflows, 49,152 exact provider
dispatches, 466,944 native events, 49,152 fixture actions, 6,144 ready reads
on each arm, and zero rejected workflows. The candidate capture and journal
remain separate, hash-bound files. Its 60-second quiet preflight passed; the
artifact records the host observations throughout the run rather than
discarding later high-load blocks.

The source paired r6 attempt is **invalid**, with zero completed paired blocks.
The capture reports `C: oracle, effect count or fixed sample count failed`
before numeric analysis. The runner's offline comparator also rejects its
incomplete reservation journal. That generic error does not establish which
oracle, effect, or count failed, nor a source-cost result. Both write-once files
remain visible; no subrun is selected or retried here.

**G6 remains open.** These scoped v2 passes do not turn the immutable frozen
v1 failures into passes. The source r6 attempt supplies no passing paired
source-cost result, and empirical live-model quality remains unknown without
authorized trials. No numeric criterion, fixture, original runner or failed
artifact was edited to obtain the v2 passes.

The following commands belong to the historical source above. Reproduce the
complete record in that archived checkout: the paired-barrier runner and raw
captures now live in the [immutable archive](../archive.md). The retained
lifecycle/stress comparators remain available in the active tree. Use the
[current release criteria](../../../qualification/fidelity/release-validation.md)
for candidate validation; the original G6 disposition above is historical.

```sh
node scripts/fidelity-lifecycle-v2-benchmark.mjs compare
node scripts/lifecycle-stress-v2-benchmark.mjs compare-strict docs/evidence/fidelity-performance/final-bc325-v2/stress-strict.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
node scripts/lifecycle-stress-v2-benchmark.mjs compare docs/evidence/fidelity-performance/final-bc325-v2/stress-legacy.json docs/evidence/fidelity-performance/lifecycle-stress-v2/replacement-budgets.json
node scripts/continuation-barrier-paired-v2-attempt2.mjs compare docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json
```
