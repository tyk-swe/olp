# Paired encrypted-barrier experiment, attempt 2

Status: **method and criteria only; no attempt-2 B or C timing has been
collected.** This separately named attempt follows the retained
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
