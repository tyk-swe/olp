# R8 source candidate: failed semantic gate

The prospective [R8 method](manifest-r8.json) and passing [B8 reference](baseline-r8.json)
were committed before C8. The single write-once C8 candidate was locked to
`21c403b6ebfc5927250cd5e7dab840d77b3422b3`, with E8 as a strict
ancestor and production Go blobs unchanged from `bc325da5`. Its first launch
stopped before reservation because host preflight CPU pressure was 6.44%
against the fixed <5% limit; no C data was taken then. After the host quieted,
the identical method, seed, and strict route contract passed preflight and
reserved the fixed C8 journal.

That reserved attempt **failed during the all-22 semantic gate** at
`C/gateway native_unary/c1/gateway`, after `C/relay` and `B/relay` each
passed 64 requests and exact provider effects. It completed **zero timed
blocks**. No paired cost, paired B sham, or any of the 120 hard C gateway
old-L limits can be scored. The [failed JSON](paired-r8.json) is
`status=failed`; the [journal](paired-r8.json.journal.jsonl) has its header
and terminal failure record. SHA-256 is
`64f5a68119af5c00720c1e7eb740b78f84fc238199c3a94a3dad75e89ab052e6`
for JSON and
`ddc2909c8443ab0230e2af1665b1216cf792ca430b527c62c7cbb670ec1845a5`
for journal. Keep both bytes unchanged. **C8 cannot be retried, and G6 is
open.**

The structured error recorded `benchmark_setup_or_sample_count` with zero
benchmark iterations and zero provider effects for the failing entry. A
selected diagnostic-only run of the retained C binary exposed the underlying
setup error: `Strict execution requires an explicit versioned provider
profile.` The frozen gateway fixture supplied a strict route overlay but C8
recorded `C_provider_contract: null`; its provider had no `profile_id` or
`profile_revision`. `runtime.NewRelease` correctly rejected that snapshot
before dispatch. This is a measurement fixture omission and intended product
fail-closed behavior, not evidence of a production gateway defect. Diagnostic
provider-profile experiments after the failed capture do not change C8's
result.

The earlier [R6 invalid candidate](paired-r6.json), [R7 inconclusive
B-only reference](baseline-r7.json), frozen v1 failures, and passing B8
reference all remain visible. No source-performance parity or live-model
quality claim follows from this attempt.
