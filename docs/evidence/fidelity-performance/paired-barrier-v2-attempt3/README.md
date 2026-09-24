# Paired encrypted-barrier final-source repeat, attempt 3

This is one separately named **C-only follow-up study** after the P10 product
change. It does not replace a failed numeric experiment: the committed
[attempt-2 C](../paired-barrier-v2-attempt2/candidate.json) already passed.
An unmerged, unmeasured draft at `25a75179` was abandoned before any attempt-3
reservation because its journal header used the attempt-2 ID while its artifact
used the new ID. This corrected method binds both to the new attempt-3 ID.
Its JSON and journal hashes are fixed at
`7fe2b9a6e1f1faabcc4c779b2dd8b052c7bb2fcbb8ad1ff87719135f275b7e8c`
and `515f55e17261bd13ec33b64dc33be4447e9c2c0ab9b09dcbfb0778c43eceaceb`.
The attempt-3 runner validates that earlier C under its original frozen method
before reserving any new evidence.

The method reuses the exact committed [attempt-2 B-only artifact](../paired-barrier-v2-attempt2/baseline.json),
its journal, [criteria](../paired-barrier-v2-attempt2/criteria.json), sealed
seed and balanced 128-block schedule. Every original B absolute envelope,
same-source control and 44 C paired margins stays unchanged. The same public
reference and translated candidate command loops retain the full request,
event, native dependency, action readiness and provider-effect oracles.
Attempt 3 builds fresh B and C binaries and interleaves them with the original
two checked 24-workflow subruns per arm in each block. It retains the original
60-second, 13-sample quiet preflight, per-block host diagnostics and prospective
material-interference invalidation. It never drops or retries a block.

The new C source must follow this method commit and the committed
[exact P10 product lock](../final-source-repeat-v1/README.md), while preserving
the old B2 and C2 evidence as strict ancestors. Freeze and commit the lock only
after P10 is integrated. Use the same isolated PostgreSQL 18.6/no-TLS server,
separate scratch databases, idle Valkey, Go 1.27.1 and host/runtime settings.
The old barrier-v1 baseline/budget and failed attempt-1 record stay visible.
No paid inference is used.

The sole timed command, after semantic-only checks and a quiet host preflight,
is:

```sh
OLP_PAIRED_B_ROOT=/tmp/olp-worktrees/paired-barrier-reference-attempt2 \
  node scripts/continuation-barrier-paired-v2-attempt3.mjs record-paired \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt3/candidate.json \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt2/baseline.json
node scripts/continuation-barrier-paired-v2-attempt3.mjs compare \
  docs/evidence/fidelity-performance/paired-barrier-v2-attempt3/candidate.json
```

The candidate JSON path and fsynced journal are exclusively reserved. A failed,
incomplete or inconclusive attempt remains at its registered path and cannot
be retried under attempt 3. Passing supports scoped local scripted barrier
cost only, not production latency or live-model quality.
