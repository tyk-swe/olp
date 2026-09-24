# R9 source candidate: complete capture, failed budgets

The single prospective C9 attempt used the frozen [R9 method](manifest-r9.json),
the exact passing [B8 reference](baseline-r8.json), E8 ancestry, and product
Go blobs locked to `bc325da5`. It passed the full 60-second host preflight and
all 44 fixed C/B semantic-gate entries, then completed all **480/480** paired
blocks without dropping or retrying one. The [C9 JSON](paired-r9.json) is
`status=complete`, and its [append-only journal](paired-r9.json.journal.jsonl)
has a complete terminal record. The recorded and independent offline
comparators both returned **`failed`**. **C9 cannot be retried; G6 is open.**

Exact SHA-256 is
`db0fd64596800f08f9e237bf74dcc28fb50225b99735f14793082e2dd3e67d64`
for JSON (89,510,106 bytes) and
`660b8a33823de6d3e496a5ad1d9ac289705aaa282333454f0aad42dbaa95a265`
for journal (38,126,445 bytes). The measured B revision is M8
`5ad7cda6f2864fc760b277f22cd47ce067c60f65`, B binary SHA-256
`6e899f78999bf5ff8464f204b02687fcb7b70834ead56b72e3656f0cea21d70a`;
C revision is `7445289f4adf3312ea64fbe7118caf6be62d4d5b`, C binary SHA-256
`282a2e2b45e74bc102ea7d0e481694b429dcc1eaebee3a68fd0af6637d902c83`.
The strict route and versioned provider overlays matched the frozen manifest.

The original B8 and contemporary paired B each passed all **254 signed
order/stability controls**. Both arms recorded exactly 106,496 timed provider
dispatches and 8,192 intentional zero-dispatch rejections; the gate added
1,280 dispatches and 128 intentional rejections per arm. There were no host
invalidations; one isolated block was elevated at 0.751653 external busy
cores, below the single-block 2-core rule and with no consecutive elevated
pair. All 224 B medians and C relay medians happened to stay within old v1 L
but are descriptive under R9.

Eight paired C-minus-B primary comparisons exceeded unchanged margins M.
The table gives B and C contemporary medians, the frozen upper order statistic
`d_(24)`, and M, in each metric's named unit:

| Workload/path | Metric | B median | C median | d_(24) | M |
| --- | --- | ---: | ---: | ---: | ---: |
| native_unary/c1/gateway | B/op | 62,856.5 | 211,563.5 | 149,308.5 | 46,724 |
| native_unary/c1/gateway | allocs/op | 930 | 1,271 | 342 | 296 |
| native_unary/c8/gateway | B/op | 67,332.5 | 215,030 | 148,142.5 | 46,641 |
| native_unary/c8/gateway | allocs/op | 944 | 1,284 | 341.5 | 296 |
| native_unary/c8/gateway | latency-p95-us | 3,342.8755 | 5,522.4735 | 2,350.889 | 2,249 |
| native_unary/c8/gateway | latency-p99-us | 4,351.521 | 6,891.3255 | 3,035.467 | 2,766 |
| native_unary/c8 added gateway-minus-relay | latency-p95-us | 1,283.074 | 3,651.4685 | 2,804.002 | 2,249 |
| native_unary/c8 added gateway-minus-relay | latency-p99-us | 1,745.25125 | 4,586.2595 | 3,667.111 | 2,766 |

Seven of the 120 separate **hard C gateway old-L medians** failed: the six
native_unary gateway path rows above plus `translated_unary/c8/gateway B/op`,
whose B median was 79,804 and C median 127,673.5 against old L 125,830.
That translated metric's paired upper value 48,113.5 was below its margin
52,870, so the absolute gate adds a real independent failure. No B sham or
relay control failed. The slow/c1 128-block group passed its tightened paired
bounds; the failures above are all 32-block groups.

The earlier [R6 invalid candidate](paired-r6.json), [R7 inconclusive
B-only reference](baseline-r7.json), and [C8 failed semantic-gate
candidate](paired-r8.json) remain visible. This result demonstrates exact
scripted semantics but does **not** qualify the prospective R9 cost budgets,
old frozen v1 replacement, production latency, or live-model quality.
