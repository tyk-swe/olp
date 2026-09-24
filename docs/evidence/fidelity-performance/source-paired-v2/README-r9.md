# Prospective C-only source-cost method r9

R9 is a single prospective C-only study. It **reuses the exact passing B8
reference**, measured at M8
`5ad7cda6f2864fc760b277f22cd47ce067c60f65` and committed together with
its journal at E8
`274e95a21cc78731bd32b64959cd083eeee0e227`. Its [B8 JSON](baseline-r8.json)
and [journal](baseline-r8.json.journal.jsonl) have SHA-256
`f537f5a936eccdaaf123707689898c8fd88d197900c9fa351c53dc79a590678f`
and `b6941932994aaebe3a900de79f02abe051efdf9e670677b8405b373bb17858fb`.
The runner validates B8 under the **original [R8 manifest](manifest-r8.json),
comparator, and journal** before C9. It requires a fresh B binary with B8's
same hash, the same B8 source revision, seed, order, block count and immutable
v1 oracle. There is **no B9 capture**, replacement baseline, or B selection.

The prior results remain visible. R6 stopped before a complete C block; R7 B
failed its old absolute reference envelope and had no C observation. B8
passed 254/254 signed B controls. The sole [C8 candidate](paired-r8.json)
failed its semantic gate before any timed block because the strict fixture
omitted a versioned provider profile; its [terminal journal](paired-r8.json.journal.jsonl)
remains unchanged, with SHA-256
`64f5a68119af5c00720c1e7eb740b78f84fc238199c3a94a3dad75e89ab052e6`
and `ddc2909c8443ab0230e2af1665b1216cf792ca430b527c62c7cbb670ec1845a5`.
The diagnostic-only retained C binary subsequently passed all 22 frozen
workload names at 64 samples each with explicit profiles (1,280 dispatches,
128 intentional zero-dispatch rejections). That does **not** rescore or retry
C8. R9 freezes the exact C provider overlay before new data: native
`compatible-chat/1`, translated and rejected `anthropic-messages/1`, alongside
the exact explicit strict route overlay. Omitted, different, or extra profile
fields fail before binary builds.

The frozen v1 baseline, budgets, runner, provider-bound request/complete
response/SSE oracle, exact asset/authentication, and 22 workload names are
unchanged. All **224 path and 30 added-latency numeric margins** stay
`M = old absolute v1 limit L − historical B median`. R9 uses the same B8
sealed seed and balanced 480-block schedule: 128 blocks for
`native_slow_stream_64/c1`, 32 for each other group. The fixed all-22
semantic gate precedes timed blocks; its durations never enter metric
vectors. Every C semantic or provider-effect failure is an adverse one-shot
candidate result recorded with structured details.

B8's original signed two-B sham remains a **formal** control under R8's
bounds, and contemporary B in the paired C9 capture must pass those same
bounds: `d_(23)<M` and `d_(10)>−M` at 32 blocks, `d_(78)<M` and
`d_(51)>−M` at slow/c1 128 blocks. The two-B sign flip checks order and
stability, not a full placebo. Only the paired **C9-minus-B primary** is
tightened: at 32 blocks `d_(24)<M`, with relay lower `d_(9)>−M`; at slow/c1
128 blocks `d_(80)<M`, with relay lower `d_(49)>−M`. The one-sided primary
sign bounds have alpha 0.0035001833 and 0.0029626033 respectively. Original
B and C relay old-L medians are descriptive with failures visible. All **120
contemporary C gateway medians** must separately be `<=` their unchanged old
v1 L; one excess fails C9 even if its paired interval passes. This absolute
budget gate is separate from the sign confidence bound.

R6, R7 and C8 produced no complete C numeric vector. Conservatively counting
their original or unused per-metric numeric opportunities anyway gives
`0.0250512299 + 0.0100308035 + 0.0100308035 + 0.0035001833 =
0.0486130202`, below the prospectively declared 5% **one-metric,
four-opportunity** target. There is no across-metric familywise claim and no
R9 retry after a failed, invalid, incomplete, or inconclusive C9 capture.
Passing scripted source cost would not prove production latency, isolated
process RSS, or live-model quality.

C9 must descend from the pushed failed-C8 evidence head
`042cf96e5d3abc38c274611388f39095ea69791d`, retain E8 as a strict
ancestor, and have production Go blobs byte-identical to locked product
`bc325da50c575803c771531dc4e3aab1ac203a53`. The measured B worktree
stays at exact M8 with its original two untracked B8 reservations; the
runner refuses any other B root, B binary hash, or output. C9 alone reserves
fixed `paired-r9.json` and its append-only journal. The unchanged host rule
requires 13 quiet samples over 60 seconds before reservation and invalidates
the whole capture for one >=2 external-core block or two consecutive >=0.75
blocks. No block is dropped or retried. Use the same host/runtime conditions
with owned PostgreSQL/Valkey and unrelated builds stopped; no paid inference.

Freeze M9 and review it **before** any C9 data. The only capture command is
paired C9; there is no R9 `record-B` or `compare-B` action:

```sh
node scripts/fidelity-paired-r9.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r9.json
node --test scripts/fidelity-paired-r9.test.mjs
node scripts/fidelity-paired-r8.mjs compare-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
export OLP_FIDELITY_BENCH_PROVIDER_CONTRACT='{"native":{"profile_id":"compatible-chat","profile_revision":"1"},"translated":{"profile_id":"anthropic-messages","profile_revision":"1"},"rejected":{"profile_id":"anthropic-messages","profile_revision":"1"}}'
node scripts/fidelity-paired-r9.mjs record-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r9.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r9.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" /home/ubuntu/.cache/olp-source-r9-B8-confirm.test /path/to/measured/B8-at-M8 /home/ubuntu/.cache/olp-source-r9-C9.test "$PWD"
node scripts/fidelity-paired-r9.mjs compare-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r9.json" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r9.json "$PWD" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json"
```

The frozen output/journal paths are write-once. Offline comparison rechecks
original B8 evidence, E8 and C8 provenance, product lock, route/profile
contract, full semantic gate and schedule, host rule, old B sham, tightened C
primary and relay bounds, and all 120 hard gateway L limits. No R9
qualification claim exists until the sole capture and comparator pass.
