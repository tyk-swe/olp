# Replacement qualification at source `38e8539b`

These are write-once local scripted-provider captures against unchanged frozen
reference artifacts and numeric budgets. They preserve both passes and failures;
none is a live-model quality comparison. The clean candidate worktree was based
on `38e8539ba07718e6f94df233afb0d6a572ba418e`, after strict media, durable
resources, realtime and the two duplex review fixes were integrated. PostgreSQL
18.6 and idle Valkey ran locally for the service-backed workloads. Captures
used Go 1.27.1, loopback hops, warmed pools and the runner-recorded hardware and
environment. Each JSON artifact carries its exact source, runner/harness,
reference and budget hashes, observations, counts, timestamps and host load.

| Frozen comparison | Capture | Result |
| --- | --- | --- |
| Encrypted [barrier-v1](../barrier-v1/README.md), earlier pushed `a23cdb40` | [candidate](barrier-a23-failed.json) | **Failed** five small/c1 limits; 288 complete workflows, 576 dispatches, zero rejects. |
| Encrypted barrier-v1, integrated `38e8539b` | [candidate](barrier-38e-failed.json) | **Failed** three small/c1 wall limits: median 22.001 ms/op > 21.186; workflow p50 21.953 ms > 21.360; p99 27.857 ms > 26.521. CPU, action-ready, c8 and large limits passed. |
| Encrypted barrier-v1, unmerged one-statement `ReadContract` experiment `bef4ac2e` | [candidate](barrier-bef4-rejected-experiment.json) | **Failed** four small/c1 wall limits and was worse than `38e8539b`; this optimization was rejected, not merged. |
| Native [lifecycle-v1](../lifecycle-v1/README.md), legacy | [candidate](lifecycle-v1-legacy-38e-passed.json) | **Passed** every frozen limit: 48 repetitions, 1,152 complete sessions/requests and dispatches. |
| Native [lifecycle-stress-v1](../lifecycle-stress-v1/README.md), legacy | [candidate](stress-v1-legacy-38e-passed.json) | **Passed** every frozen limit: 36 repetitions, 288 complete workflows, 576 dispatches, and three zero-dispatch negative controls. |
| Registered-profile [lifecycle-stress-v2](../lifecycle-stress-v2/README.md), legacy | [candidate](stress-v2-legacy-38e-failed.json) | **Failed** three c1 duplex added-latency limits; complete work and negative controls passed. |
| Registered-profile lifecycle-stress-v2, strict | [candidate](stress-v2-strict-38e-failed.json) | **Failed** ten duplex allocation, added-latency, jitter and gateway-tail limits; all media limits, complete work and negative controls passed. |

The v2 stress native reference was captured from clean **pre-#216** source
`d78be007` at load 0.69→0.96; its separate budgets were frozen and self-compared
before either v2 candidate. The strict candidate recorded 36/36 repetitions,
288 successful workflows, 576 exact provider dispatches, 6,144 duplex RTT and
6,048 jitter observations, and all three local rejections without provider
work or spool residue. Its host load was 0.89→0.97. The legacy v2 load was
0.94→1.04. The barrier `38e` load was 0.59→2.05 during its own 18.12-second
measurement; the one-minute rise is recorded, not removed from the result.

Two older frozen strict selections cannot be relabeled as passes. The
[lifecycle-v1 strict attempt](lifecycle-v1-strict-38e-failed.log) stopped at
its legacy `resp_` durable-ID oracle after the strict resource used its distinct
owner-scoped identity; no complete timed artifact was written. The
[stress-v1 strict attempt](stress-v1-strict-profile-failed.log) stopped at route
activation because its frozen video fixture has no versioned provider profile.
Both failures remain visible. The separately versioned stress-v2 registered
profile fixes only that fixture mismatch; it does not rewrite v1 history or
budgets. The [original 22-workload source comparison](../oif-source-v1/README.md)
also retains its slow native-relay c1 p99 control failure and is not superseded
by these service-backed results.

The frozen runners' `compare`/`compare-strict` commands produced the statuses
above; no workload, numeric limit, sample floor, accepted-work count, native
asset, event, or rejection was excluded or rebased. Empirical intelligence
parity, WAN/TLS inference, isolated gateway RSS and partial batch item timing
remain unknown or outside these workloads. **G6 remains open** pending safe
product optimization, new clean frozen comparisons and the original relay
control qualification.
