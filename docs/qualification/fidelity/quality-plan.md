# Preregistered quality study, version 1

This study is optional follow-up under the amended
[release criteria](release-validation.md). Its design and unknown result remain
unchanged; deferral does not authorize an empirical parity claim or a missing
feature.

Status: **unknown; not executed**. No live-provider credentials, cost ceiling,
privacy approval or serving-identity observations have been supplied for this
study. The fixture baseline establishes no intelligence-parity result. Paid
calls and production shadow inference require separate explicit approval.
Changes to this design require a new version timestamped before the affected
comparison results are collected; never tune margins after seeing a result.

## Admission and scope

Each study stratum is one exact inventory row plus provider account/hosting
identity, model/deployment binding, API revision, SDK revision, route revision,
client/state-carrier contract, operation, assets, controls and delivery mode.
Author and seal the direct-native request and OLP request independently before
execution. Use the same selected serving environment and effective settings;
record provider aliases, response fingerprints and observable version drift.
Any observed drift starts a separate stratum. Missing observable identity is
recorded as unknown and prevents a matching-identity parity claim. A matching
seed is not evidence of determinism.

Deterministic conservation is a prerequisite. Known semantic differences have
zero tolerance even when average quality is favorable. Failures to preserve
native bytes/representation, instruction hierarchy, tool identity, state,
assets or lifecycle cannot be excused by statistical noninferiority. Explicit
incompatibility is reported as such; refusal does not prove useful parity.

Use only synthetic or separately approved nonsensitive data. Pin task corpus,
assets, task labels, scoring implementation, independent direct/OLP templates
and randomization seed by SHA-256 in the approved study manifest. Never copy
production inference or log opaque thinking/state. Save only access-controlled
research artifacts under the approved retention policy.

## Sample design and analysis

1. Run a direct/direct variability pilot of 200 independent task units per
   stratum, with two independently issued native trials per unit. Measure task
   variance, nondeterminism, failures and identity drift. This pilot does not
   change the margins or main sample size. Identity instability, semantic
   mismatch or inadequate task labeling pauses that stratum as unknown.
2. The main design uses 2,500 independent task units per stratum. Run one
   direct-native and one OLP trial for each unit. Randomize order within each
   pair using a sealed PRNG seed and interleave pairs over time. Balance the
   direct-first/OLP-first assignment. Do not run all direct requests before all
   proxy requests. Analyze task units, not tokens or repeated samples, as the
   independent observations.
3. Collect the full prespecified sample without optional early stopping. Do
   not replace failed, rejected, truncated, timed-out or ambiguously accepted
   attempts with successful retries. Count them in the relevant completion/
   success denominator and report their categories separately. Provider-wide
   outages or invalid independent task labels can invalidate an entire
   stratum; retain the raw counts and report inconclusive, not a trimmed pass.
4. For each prespecified stochastic metric use the paired difference (OLP minus
   direct, reversing signs for error metrics). Report its estimate and a simultaneous
   one-sided 95% lower confidence bound. Use a task-cluster percentile
   bootstrap with 20,000 resamples and a sealed analysis seed. For K stochastic
   primary metrics in a stratum use the alpha = 0.05/K lower percentile (Bonferroni)
   and require **every** primary metric to pass. Also report two-sided 95%
   intervals for descriptive direct/direct and direct/OLP differences.
5. Noninferiority passes only when the lower bound is greater than the
   negative preregistered margin, all deterministic checks pass, all required
   identity evidence is present and all primary outcomes are measured. A
   confidence bound crossing the margin is inconclusive; failure to detect a
   difference is not equality. An unexplained significant regression blocks
   qualification of that stratum.

The fixed sample size may be insufficient for a rare-event metric or expensive
for a media workload. In either case keep the result unknown/inconclusive or
register a separately approved follow-up design before new comparison data.
Do not retroactively weaken the margin or omit an expensive workload.

## Primary metrics and noninferiority margins

Scores are normalized to [0, 1] unless explicitly stated. Each task suite must
pin the independent scoring rubric before the first pilot call.

| Workload | Primary metrics | Maximum accepted OLP decrease |
| --- | --- | --- |
| Generation reasoning/tasks | Task success; long-horizon workflow completion | 0.02 each |
| Tool use and continuation | Correct call/argument/result chain; completed workflow | 0.01; 0.02 |
| Structured generation | Schema-valid answer; independently labeled task success | 0.00; 0.02 |
| Embeddings/retrieval | Recall@10; nDCG@10 on fixed labeled documents | 0.01 each |
| Rerank | nDCG@10; pairwise ranking accuracy with stable document IDs | 0.01 each |
| Moderation/classification | Macro-F1; per-policy critical-category recall | 0.01 each |
| Transcription/translation | 1 − word-error-rate (clamped to [0,1]); task correctness | 0.01; 0.02 |
| Speech/image/video | Blind rubric task success; asset/timing requirement success | 0.02; 0.00 |
| Realtime | Completed interruption/tool scenarios; preserved VAD/media timing contract | 0.02; 0.00 |
| Durable jobs/batches/files | Successful item/lifecycle outcomes with stable identities | 0.00 |

Zero-margin conditions are deterministic gates, evaluated by the exact
conservation oracle rather than the bootstrap inequality. They require zero
violations over the full sample and are excluded from K. Stochastic evidence
alone cannot satisfy them. Native counts must match the declared native
counting contract; estimates are a distinct operation. Embedding dtype, dimensions, quantization,
packed layout and exact provider-native values are deterministic prerequisites,
not normalized away by a favorable retrieval score. Rerank native scores and
ties likewise remain exact contract requirements.

## Publication and denominator

Publish scoped identity/revision information, manifest hashes, approved budget,
counts, completion categories, pilot variability, point estimates, confidence
intervals, margins, drift events and the final status. Separate protocol
conservation, SDK/recovery qualification, empirical quality and performance.
Do not aggregate incompatible or unknown rows into a provider-wide badge.

Current totals: 0 approved live strata, 0 direct/direct trials, 0 direct/OLP
trials, 0 empirical parity qualifications. All 47 representative inventory rows
retain unknown empirical quality. No observation in this document is a live
model-quality result.
