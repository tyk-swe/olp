# Source paired v2 r5: invalid B-only attempt

The single r5 historical B-only attempt is **inconclusive** because material
unrelated host work appeared during collection. This decision was made around
block 111, before the B envelope or any candidate result was available. The
runner was then stopped gracefully as directed, without filtering or retrying
blocks. Its exclusive fixed paths remain reserved:

| Evidence | SHA-256 |
| --- | --- |
| [Partial B JSON](baseline.json) | 68f7bb88ce70bcc4b179179749577faf085838c0fd2696c7e3b2f9ba3b239b06 |
| [Append-only journal](baseline.json.journal.jsonl) | 5127ea75ec3688cbadf433eae8d021ce0d473b269f571fe03face683f7dd8295 |

The run used method commit 1447caf9f370e1e4d844a4c31f61a1c720857b74
and the frozen [r5 manifest](manifest-r5.json). It started at
2026-09-23 08:49:41 UTC and stopped at 09:00:52 UTC. The artifact has
status invalid; the journal has a header, 151 complete blocks and one
terminal failure record. These blocks contain 566 complete subruns, 36,224
measured requests, 33,792 successful provider dispatches and 2,432 intended
local rejections. The planned floor was 384 blocks and 90,112 measured
requests. No B envelope comparison was computed, and **no C process or C
timing was started**.

Host load began at 0.34 (one-minute average), rose above 10 during the
attempt, reached 12.66 in a recorded block diagnostic, and was 6.87 at
termination on an eight-logical-CPU host. Observation outside the benchmark
identified an unrelated engine.test process near 14.5% CPU, then
PacketCraftr rust-lld near 91%, multiple clippy-driver processes near
60–79% each, and packetcraftr near 99.6%, while the B process used about
190%. The runner's SIGTERM error records our deliberate stop after this
external interference; it is not a semantic failure of the gateway.

This artifact does not qualify historical B and cannot support a paired C
comparison. A future B attempt requires a separately named, prospectively
registered method/path and sequential rule; the r5 paths and evidence remain
unchanged.
