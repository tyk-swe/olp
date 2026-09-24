# R10 source candidate: host-invalid one-shot capture

The single prospective C10 attempt used the frozen [R10 method](manifest-r10.json),
the exact passing [B8 reference](baseline-r8.json), and the exact P10 product
commit `72b8f7bea4ee8800b71e82006b63a505e0818412`. The fresh B8 binary
matched the sealed B8 SHA-256
`6e899f78999bf5ff8464f204b02687fcb7b70834ead56b72e3656f0cea21d70a`;
the C10 binary SHA-256 was
`1629ec34ece573b4704dfb158ea309eb2e8fc6e84c4dbaf39b2d71e0f3379bb4`.
The full 60-second host preflight and all 44 fixed C/B semantic-gate entries
passed. The strict route and versioned provider overlays matched the frozen
manifest. The run started at 2026-09-24 07:20:13 UTC and stopped at 08:05:07
UTC.

The [C10 JSON](paired-r10.json) records `status=invalid` after **255/480**
scheduled blocks. The frozen host rule invalidated the whole attempt when two
consecutive blocks measured 1.0214408579 and 1.3252021658 external busy
cores, both at least the 0.75-core threshold. The preceding interval measured
0.4345715234. The [append-only journal](paired-r10.json.journal.jsonl) has a
header, all 44 semantic-gate entries, 255 complete blocks, and an invalid
terminal record. Its 258 lines and the JSON are retained unchanged. The
independent offline `compare-paired` command exited 1 with
`reservation journal lacks a complete terminal record`, as required for this
incomplete invalid capture. **No partial C10 numeric margins are assessed,
no C10 block or attempt is retried, and G6 remains open.**

Exact SHA-256 is
`ff93b3c8e5b28a22ee43db1af1a553db44bfcc51749196206640076c5018126a`
for JSON (47,618,231 bytes) and
`b39836f3141cafbf3327fcd2cf479ec5fceeb0e2a1324ddb597c60f38f9d76af`
for journal (20,555,147 bytes). The original B8 reference remains at method
revision `5ad7cda6f2864fc760b277f22cd47ce067c60f65`; no B10 reference
was captured. Earlier failed or invalid source captures and the original
frozen v1 outcomes remain unchanged. This invalid C10 attempt establishes no
R10 cost qualification or broader latency or live-model claim.
