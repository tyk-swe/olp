# Prospective C-only source-cost method r10

R10 is one new write-once local scripted candidate study, registered after the
complete [failed C9 capture](README-r9-failed.md) and before any C10 timing.
Five pre-data direct-child method drafts are **abandoned**. The initial commit
`4875e86bfd669eb0a9161a2813ec8d2ad5a26311` bound only selected journal
header fields and omitted Go test files compiled into the C source lock. Its
replacement `a8f84804b1f9fad71a9cfbf0a44f36a86b2c3a3c` fixed those gaps but
incorrectly compared M10's Go blobs with earlier product revision `bc325da5`
instead of the pinned direct parent `670408ee`; that parent already changed a
benchmark adapter `_test.go` file. The next draft
`7ea650b75457c0d570d1f12d994917f8abb5a307` fixed that comparison but
checked only M10 and P10's immediate parent. A Go test change and revert in
the intervening history could therefore pass. The fourth draft
`ab3856525fa8bb35f2c798631d8bc1901abf6018` checked every introduced Go
tree, but omitted tracked build inputs such as `tests/fixtures/`, `go.mod`,
embedded SQL and OpenAPI. It also allowed unrelated paths into P10, or a
connector mode change without new code. The fifth draft
`95e3c3ed993ffac293f22aa0ed2a357552d80380` closed those source and product
checks but parsed Git's quoted text tree output. A tracked
build input with a tab or newline in its filename could evade the prefix lock.
This replacement reads NUL-delimited tree records and preserves their exact
blob OIDs and modes. None of these drafts was merged or pushed, and no C10
binary was built, journal reserved, or timed request collected under any draft.
This final replacement is one new normal direct-child method commit from the
same pinned base, with the seed, schedule, margins and numeric gates unchanged.
It reuses the **exact sealed B8 reference**: measured at M8
`5ad7cda6f2864fc760b277f22cd47ce067c60f65`, committed with its journal
at E8 `274e95a21cc78731bd32b64959cd083eeee0e227`, and checked under the
original [R8 manifest](manifest-r8.json), comparator and journal. B8 passed all
254 signed two-B controls. There is **no B10 capture**, baseline selection,
new seed, altered schedule, or revised margin.

The C9 JSON and journal remain immutable at SHA-256
`db0fd64596800f08f9e237bf74dcc28fb50225b99735f14793082e2dd3e67d64`
and `660b8a33823de6d3e496a5ad1d9ac289705aaa282333454f0aad42dbaa95a265`.
C9 passed 44/44 semantic-gate entries and completed 480/480 timed blocks, but
failed eight paired primary comparisons and seven separate hard C gateway
old-L limits, with no B sham/control failure. C9 is not retried or re-scored.
R5's externally invalid B-only capture, R6's zero-block invalid C, R7's failed
old native-relay reference envelope, C8's zero-block strict-profile fixture
failure, and the original frozen v1 failures remain visible under their own
paths. The C9 outcome motivates a **real connector hot-path product change**;
it does not authorize changing the v1 fixture, oracle, baseline, budgets or
numeric limits.

The [R10 manifest](manifest-r10.json) pins the same 22 names, 480 balanced
blocks (128 slow/c1 and 32 per other group), sealed B8 seed/order, 64 fresh
requests per subrun, 224 path and 30 added-latency margins
`M = old v1 absolute limit L − historical B median`, complete source/response/
SSE and effect oracle, and exact zero-dispatch rejections. The first all-22
semantic gate uses C then B per name and is excluded from timed vectors.
The explicit strict route overlay and versioned provider-profile overlay are
byte-for-byte the R9 contracts. The original R8 signed B-only and paired-B
sham bounds remain formal (`d_(23)<M`, `d_(10)>−M` for 32 blocks;
`d_(78)<M`, `d_(51)>−M` for slow/c1). Every one of the 120 contemporary C
gateway medians must still be `<=` its corresponding old absolute L as a
**separate hard gate**. B and C relay old-L medians remain descriptive, with
misses reported. No difficult or rejected workload is omitted.

Only the paired C10-minus-B primary tightens. At 32 blocks it requires
`d_(25)<M`, with relay lower control `d_(8)>−M`; at slow/c1 128 blocks it
requires `d_(82)<M`, with relay lower `d_(47)>−M`. The one-sided upper sign
alphas are respectively `0.0010512007866` and `0.000931234262`. Counting
even unused prior numeric opportunities conservatively gives
`0.0486130202 + 0.0010512008 = 0.049664221 < 0.05` for **one metric across
five opportunities**. There is no across-metric familywise claim. The old
B-sham interval, hard gateway L gate and all frozen M values are unchanged.
A failed, invalid, interrupted or inconclusive C10 is final for R10; no block
or attempt is selected, dropped or repeated after data.

Chronology is part of the method. E8 and the failed-C9 evidence commit
`39ad40cb704812b656a1c3be61f019c100c79848` must be strict ancestors
of M10. M10 must be a **normal direct child** of the pinned pre-method PR
source `670408ee58e36e677571006b25107f0a4730f8f2`, creating this
runner and manifest together without changing **any tracked Go source, test or
build-input blob/mode relative to that direct parent**. The earlier `bc325da5`
product revision is historical evidence, not M10's source parent. Every commit
newly introduced into the parent history of P10 after M10, including
side-branch commits and merge results, must retain the complete M10 tracked
build-input inventory: all files under `internal/`, `cmd/`, `openapi/`, `tests/`
and `vendor/`, plus root `go.mod`, `go.sum` and `go.work*`. This rejects a Go
source, embedded fixture or dependency change even if later reverted before
P10, while permitting the docs/scripts-only service-method merge
`c3f054c46df341e5888ac6bf8f7d9a7e5bf37967`. After M10, one
new normal P10 commit must change the blob OID of at least one of
`internal/connectors/defaults.go`, `internal/connectors/operations.go` or
`internal/connectors/profiles.go`. P10 may modify only those three files and
`internal/connectors/profiles_test.go`, retaining regular-file modes.
Rename detection is disabled when checking P10, so moving another path into
the allowed set still exposes and rejects the deleted source path. A prepared
side-branch commit predating M10 does not count merely because it is merged
afterward. The timed C10 checkout must be
**exactly P10**: the runner rejects every intervening documentation, script,
build, deployment, test or code commit. It additionally checks the complete
non-test product Go inventory and **all tracked Go blobs, including
`_test.go` files**, against P10. An offline comparison from a later
documentation-only descendant still evaluates the recorded P10 revision
and cannot manufacture or erase chronology. The C9 JSON/journal creation
commit and M10 method blobs must remain unchanged in that ancestry.
Reviewers must also assess whether P10 is a real optimization; a changed
Go blob alone proves only the machine-checkable chronology.

The fixed `paired-r10.json` and append-only
`paired-r10.json.journal.jsonl` are write-once paths; the
runner refuses either existing path. A complete artifact must bind the
**entire pre-timing reservation header** (preflight, host rule, conditions,
hardware/toolchain/runtime, route/provider overlays, B8 and schedule hashes,
source provenance and diagnostics), all 44 semantic entries, every scheduled
block and terminal record. Changing the final JSON alone, or changing
the header while merely updating the final JSON journal hash, fails. The
write-once journal itself remains the reservation record. The same physical
host, Go toolchain, runtime conditions and frozen
host rule apply: 13 quiet samples across 60 seconds before reservation, then
whole-attempt invalidation for one block at two external busy cores or two
consecutive at 0.75. Both B8 and C10 binaries are freshly built into distinct
unused root-disk paths. Owned PostgreSQL/Valkey and unrelated builds/tests
must be stopped for the quiet capture. No paid inference is used.

Freeze M10 before P10 and before any candidate data:

```sh
node scripts/fidelity-paired-r10.mjs freeze-manifest docs/evidence/fidelity-performance/source-paired-v2/manifest-r10.json
node --test scripts/fidelity-paired-r10.test.mjs
node scripts/fidelity-paired-r8.mjs compare-B "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r8.json

# Commit final corrected runner, tests, this README and manifest together as M10.
# Merge M10 unchanged, then make a new normal connector optimization commit
# P10. Time from a clean detached checkout of EXACT P10.
export OLP_FIDELITY_BENCH_ROUTE_CONTRACT='{"native":{"fidelity":{"mode":"strict"}},"translated":{"fidelity":{"mode":"strict"}},"rejected":{"fidelity":{"mode":"strict"}}}'
export OLP_FIDELITY_BENCH_PROVIDER_CONTRACT='{"native":{"profile_id":"compatible-chat","profile_revision":"1"},"translated":{"profile_id":"anthropic-messages","profile_revision":"1"},"rejected":{"profile_id":"anthropic-messages","profile_revision":"1"}}'
node scripts/fidelity-paired-r10.mjs record-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r10.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r10.json "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" /home/ubuntu/.cache/olp-source-r10-B8-confirm.test /path/to/measured/B8-at-M8 /home/ubuntu/.cache/olp-source-r10-C10.test "$PWD"
node scripts/fidelity-paired-r10.mjs compare-paired "$PWD/docs/evidence/fidelity-performance/source-paired-v2/paired-r10.json" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json" docs/evidence/fidelity-performance/source-paired-v2/manifest-r10.json "$PWD" "$PWD/docs/evidence/fidelity-performance/source-paired-v2/baseline-r8.json"
```

A passing R10 comparison would support only its preregistered local paired
source-cost claim. It would not convert old failed v1 or r5–r9 results into
passes or prove production latency, isolated gateway RSS, or live-model
quality. Until C10 is captured and its offline comparator passes, G6 remains
open.
