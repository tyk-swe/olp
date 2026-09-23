# Prospective source-cost method r8

R8 is a new, one-shot local scripted source-cost study frozen before any B8
or C8 timing. The frozen v1 source baseline, budgets, runner, fixture/oracle,
all prior method versions, and every failed or invalid capture remain unchanged.
The original 22 workload/path/concurrency names and all 224 path plus 30
added-latency numeric margins remain `M = old v1 absolute limit L − median of
three historical B repetitions`.

The history is visible in the method manifest. R5 B-only was invalidated by
external CPU load before C. R6 reached its first scheduled C relay arm but
stopped with a collapsed semantic/effect/sample error before a complete block;
its underlying cause remains unconfirmed. R7 completed all 384 B-only blocks
and passed its fixed 22-name semantic gate, but its contemporary B median
for `native_slow_stream_64/c1/relay` max inter-event-gap p99 was
**4,317.099 µs**, above old L **3,544 µs**. R7 was inconclusive and **no R7
C capture occurred**. The byte-identical R7 JSON and journal are pinned by
SHA-256 `73d4701f9684789296bf51d52e54a0fbe7fd759469cb7a36e9f8995fa2687b00`
and `3d10f864608e2b318fabaa83ff42037dd25d82940526d5abe38c6272ed0f5308`.
They are retained alongside the pinned R6 invalid evidence. R7 is never
retried.

R8 prospectively changes the estimand after the prior relay absolute-control
failure. It measures paired C-minus-B added source cost and checks a narrow
signed two-B order/stability control in both B-only and paired B. For every
path metric, the sham value in a block is `(B2 − B1)` multiplied by `+1` for
ABBA and `−1` for BAAB. For each added-latency metric it is the same signed
change in B gateway-minus-relay latency. This uses two B observations per
path and is **not** an exact four-arm placebo. All 224 path and 30
added-latency sham controls are formal: their lower and upper order
statistics must lie strictly inside `(-M, M)` in both B-only and paired B.
The original B old-L medians are reported with every failure, but are
**descriptive**, not the R8 B qualification gate.

The new sealed seed uses the same SHA-256 ranking and balanced ABBA/BAAB
schedule rule. `native_slow_stream_64/c1` has **128 blocks** in both B-only
and paired captures: 64 ABBA, 64 BAAB, and 32 relay-outer/32 gateway-outer
within each order. Each other group has 32 blocks, split 16/16 and 8/8.
There are 480 blocks total. For slow/c1, the formal upper bound is `d_(78)
< M` and lower control `d_(51) > −M`; for other groups they are `d_(23)
< M` and `d_(10) > −M`. The paired C-minus-B primary uses the same group
specific upper bound and unchanged M. Relay C-minus-B retains its lower
control. The exact one-sided sign coverage is 0.9916646329 for slow/c1
(alpha 0.0083353671) and 0.9899691965 elsewhere (alpha 0.0100308035).

R8 also adds a **separate hard candidate gate**: each contemporary C gateway
path median must be at or below its directly corresponding immutable old v1
absolute L. One miss fails the one-shot C8 attempt even if paired C-minus-B
passes. B and C relay old-L medians, and paired B old-L medians, remain
descriptive with all misses shown. The hard gateway median check is an
absolute budget comparison, not part of the sign-interval confidence bound.
Prior Pfinal v1 gateway passes were exploratory observations; they were not a
passing R8 result or used to change L. R8 makes no claim that old relay L
passes or that live model quality has been measured.

The 128-block sizing was chosen before R8 data from R7 reference observations
only. A B7-only stratified bootstrap gave an illustrative all-224-sham pass
estimate of about 97.1% at 128 slow/c1 blocks versus 93.1% at 96. For the
most exposed B7 lower-side relay-gap sham, 22 of 32 blocks exceeded `−M`;
an independent Bernoulli plug-in gives about 97.57% probability of at least
78 successes in 128 blocks. These are uncertain power-sizing illustrations,
not guarantees, confidence claims, or C8 observations. The bootstrap covered
224 path shams; it did not estimate joint passage of the 30 added-latency
shams or both B-only and paired B controls.

R6's original `d_(22)` one-sided alpha was 0.0250512299; R7 had no numeric
C look. R8's maximum alpha is 0.0100308035. Even conservatively counting
R7's unused 0.0100308035 opportunity, the three-opportunity upper sum is
0.0451128369, below the newly declared 5% target **for one metric across
attempts**. There is no across-metric familywise claim. R8 allows one
write-once B8 reservation and one write-once C8 opportunity, with no
selected block omission or retry after an inconclusive, failed, invalid, or
interrupted capture.

The C8 product is locked to
`bc325da50c575803c771531dc4e3aab1ac203a53`. C8 must descend from
that commit with byte-identical tracked production Go blobs; no artificial
hot-path change establishes chronology. Historical B8 stays at its measured
method commit M8, which descends from the direct-child R7 evidence commit E7
without changing historical product B. A separate normal evidence commit E8
must create B8 JSON and journal together directly from M8; E8 must be a
strict ancestor of locked C8 before paired timing. Prior R7 B-only data
cannot substitute for B8 because the seed, schedule, formal rule, method hash,
reservation paths, and block count differ.

The r7 structured failure protocol and bounded post-timer provider-completion
wait remain in force. Before timed blocks the runner reserves its fixed
append-only journal and performs a hashed, fixed all-22-name semantic gate.
Paired order starts with the same adverse R6 C relay name and tests C then B
for each name. Gate durations are excluded from timed vectors. A C semantic,
provider-effect, or sample-count error stops the one-shot candidate and
records stage, bounded/redacted detail, HTTP status when available, exact
counters, and process diagnostics. The frozen host rule still requires a
full 60-second quiet preflight; one block at 2 external busy cores or two
successive blocks at 0.75 invalidates the entire attempt. Both B and C are
freshly built, long-lived processes. Owned PostgreSQL/Valkey and unrelated
builds/tests must be stopped for capture. No paid inference is used.

Freeze this method and its manifest in M8 before any B8 data. Use distinct
root-disk binary paths and the exact fixed evidence paths:

```sh
node scripts/fidelity-paired-r8.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json
node --test scripts/fidelity-paired-r8.test.mjs
node scripts/fidelity-paired-r8.mjs record-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json /home/ubuntu/.cache/olp-source-r8-B.test "$PWD"
node scripts/fidelity-paired-r8.mjs compare-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json

# Copy B8's exact JSON and journal to a separate E8 worktree at M8.
# Commit both together as E8; merge E8 unchanged into C8 based on bc325.
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
node scripts/fidelity-paired-r8.mjs record-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" /home/ubuntu/.cache/olp-source-r8-B-confirm.test /path/to/measured/B-at-M8 /home/ubuntu/.cache/olp-source-r8-C.test "$PWD"
node scripts/fidelity-paired-r8.mjs compare-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r8.json" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json "$PWD" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json"
```

The runner refuses existing output or journal paths. The comparison
rechecks exact committed B bytes, E8 ancestry, C8 product blobs, complete
semantic gate, schedule, host rule, signed B controls, C gateway old-L gate,
paired margins, and all prior evidence pins. No R8 qualification is claimed
before the write-once captures and static comparison succeed.
