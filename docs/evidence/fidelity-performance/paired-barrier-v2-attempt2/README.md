# Paired encrypted-barrier experiment, attempt 2

Status: **the write-once B-only reference and final-source C comparison both
passed their predeclared scoped criteria.** This separately named attempt follows the retained
[attempt-1 failure](../paired-barrier-v2/README.md#attempt-1-result-failed-before-a-b-only-baseline).
Attempt 1 ended after 76 of 768 historical B subruns, with no C observation.
The public gateway returned `upstream_unavailable` and then
`authority_unavailable` at about 60 seconds. Diagnostic replay reproduced the
failure: the old v2 test harness called `h.refresh()` only once, while
production refreshes key authority every five seconds and refuses admissions
when its last authority read is 60 seconds old. The [failed artifact and
journal](../paired-barrier-v2/) remain unchanged and visible.

Attempt 2 changes only the measurement lifetime: both the historical B and
production C command loops start the existing production `runtime.Manager`
poll/stop lifecycle after publishing the route and state-enabled key. Their
same frozen native provider request/event oracle, encrypted B barrier,
candidate projection oracle, loopback transport, storage authorities and exact
workflow counts remain required. A selected service-backed semantic test waits
beyond the 60-second stale boundary, checks the authority read advanced, and
executes a complete public native workflow. The historical B product remains
`29e18268ac23f7290d1881cbda3af2cc2ccab018`; its new overlay may change
only the measurement-only command-loop file. The original v1 reference,
budgets, attempt-1 runner/criteria and failed data are immutable.

Ordinary `make integration` runs both arm tests through required service
setup, then exits without reading stdin. A regression test keeps their stdin
pipe open to prove they cannot hang the default suite. Only the write-once
runner sets `OLP_PAIRED_BARRIER_COMMAND_MODE=1` to enable the long-lived
command loop. Explicitly selecting either arm without service configuration
still fails at setup; the >60-second public-workflow freshness test remains in
the ordinary integration suite.

[criteria.json](criteria.json) fixes a new sealed seed and 32 interleaved blocks
per stratum, exactly 16 of each palindromic orientation. Each block has two
24-workflow subruns for B relay, B gateway, B encrypted reference and, only
after the B-only baseline is committed, C translated continuation. It retains
all 12 B combinations and four C strata, every exact event/dispatch/tool/read
count, the original 11 directly corresponding C metrics, all B path controls,
the old frozen B envelopes and `M = old L - median(old three B repetitions)`.
No numeric margin or acceptance rule was fitted to attempt-1 outcomes.
The sorted 32-block paired difference `d_(22)` and absolute B-control
`a_(22)` must each be strictly below their corresponding M; the B 64-subrun
median must remain within its original frozen L. No block, rejection, outlier
or control excursion is dropped.

The preregistered sequence allows **this one replacement attempt only** for
the identified harness-lifetime bug. Attempt 1 made no C measurement, so no
C hypothesis was tested and its one-sided error allowance was not spent.
Attempt 2 retains the original 97.494877% one-sided median bound. If its B
envelope/control or its sole locked-source C comparison fails or is
inconclusive, this sequence ends without a passing barrier claim. Further
timing would need a separately justified experiment and explicit error-budget
plan; it cannot be selected merely for a favorable result.

The runner builds both arms into fresh outputs and records before/after
executable SHA-256, exact source tree, build/run commands and runtime. Before
launch it reserves the fixed artifact path with an exclusive fsynced journal,
then records every completed subrun/block and a terminal bound to the artifact.
An interrupted or failed run cannot be silently repeated. The bounded
non-measurement Go stdout/stderr tail is retained in a failed artifact so a
future semantic exit has an inspectable Go test reason. Journal I/O occurs
after each measured subrun and before the next checked warmup. The B-only
artifact and journal must be committed together before the candidate source
can be locked; their exact hashes and strict ancestry to the reviewed product
change are checked again offline.

After binary builds and **before journal reservation**, the runner samples
`/proc/loadavg`, `/proc/pressure/cpu` and aggregate `/proc/stat` 13 times at
five-second intervals across at least 60 seconds. Every sample must have
one-minute load strictly below 2 and CPU `some avg10` pressure strictly below
5%; missing counters fail closed without starting a timed attempt. Each block
then records raw host ticks/HZ, load and pressure plus runner and B/C process
CPU counters before and after its measured subruns. PostgreSQL is an owned
measured service in another process, so host CPU minus only B/C/runner is
**not** called unrelated work or used as an automatic rejection threshold.

If an operator observes a materially competing unrelated build, test or
compute process **during** collection, they immediately run
`flag-interference` with that process's PID/name and one of the frozen
materiality reasons. The active runner captures time, process, host load and
pressure in a fsynced journal event, aborts both arms, and marks the *whole*
attempt inconclusive before numeric analysis. A later classification cannot
select or discard favorable blocks. The accepted reasons are
`unrelated-build`, `unrelated-test` and `unrelated-compute` as defined in the
criteria. For example, while the B-only runner is active:

```sh
node scripts/continuation-barrier-paired-v2-attempt2.mjs flag-interference \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/baseline.json \
  unrelated-build 12345 rustc
```

```sh
node --test scripts/continuation-barrier-paired-v2-attempt2.test.mjs
OLP_PAIRED_B_ROOT=/tmp/olp-worktrees/paired-barrier-reference-attempt2 \
  node scripts/continuation-barrier-paired-v2-attempt2.mjs record-baseline \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/baseline.json
OLP_PAIRED_B_ROOT=/tmp/olp-worktrees/paired-barrier-reference-attempt2 \
  node scripts/continuation-barrier-paired-v2-attempt2.mjs record-paired \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/candidate.json \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/baseline.json
```

Both timed arms use the same local PostgreSQL 18.6/no-TLS server with separate
scratch databases, idle Valkey, Go 1.27.1 and the original Haswell-class host
conditions. Process resource figures include the local client/provider/oracle,
exclude PostgreSQL and are not isolated gateway RSS. This is local scripted
performance, not WAN/TLS or empirical live-model quality evidence.

## Captured B-only result

The clean method `9350d40e` with measurement-only historical B overlay
`55d9d591` produced the single write-once [baseline.json](baseline.json) and
[reservation journal](baseline.json.journal.jsonl) on 2026-09-23. The 13-sample
quiet preflight passed over at least 60 seconds (load1 0.46–0.81; CPU pressure
`some avg10` 0–0.55%). All 128 blocks and 768 B subruns completed: 18,432
two-turn workflows, 36,864 exact provider dispatches, 350,208 native events,
36,864 enabled fixture tool actions, 6,144 encrypted-reference ready reads,
and zero rejections. All 240 original B envelope metrics and all 240 absolute
same-source stability controls passed their unchanged frozen limits. The
offline comparator revalidated the complete journal, artifact, method/source
hashes and numeric result. No material unrelated-work event was reported.

The artifact SHA-256 is
`5cec1e3c2dbfc62c8511d5c236867ecbd7aaedbd7b2772ddeec44c36f9b999a9`;
the journal SHA-256 is
`96d4876b6c1ebb2dc7d8530ec7e4c783b6f8e3c837337ac72485a880db576b55`.
These B-only observations establish host/reference comparability, not a C
added-cost pass. The first failed attempt remains visible and no C process or
C timing was included in this capture.

## Captured final-source C result

Clean `bc325da50c575803c771531dc4e3aab1ac203a53` produced the write-once
[candidate](candidate.json) and [journal](candidate.json.journal.jsonl) after
the B evidence became a strict ancestor of the production resolver change.
Their SHA-256 values are respectively
`7fe2b9a6e1f1faabcc4c779b2dd8b052c7bb2fcbb8ad1ff87719135f275b7e8c`
and `515f55e17261bd13ec33b64dc33be4447e9c2c0ab9b09dcbfb0778c43eceaceb`.
Offline `compare` passed the complete journal, frozen B envelopes, controls
and all candidate bounds. The 128 blocks retained 18,432 B and 6,144 C
workflows, 49,152 exact provider dispatches, 466,944 native events, 49,152
fixture actions, 6,144 ready reads on each arm, and zero rejects. The host
preflight passed; all collected blocks, including later high-load observations,
remain in the artifact. This qualifies the scoped local encrypted-barrier
comparison, not the [full G6 gate](../final-bc325-v2/README.md).
