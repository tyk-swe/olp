# Negotiated continuation candidate correspondence

The frozen [native encrypted reference](README.md) remains the source of the
workload and limits. The candidate is a separately versioned measurement of the
production `chat-anthropic-tools-v1` carrier. It must not replace the native
reference or change its baseline, runner, hashes or budgets.

Candidate oracle version 2 requires the exact native usage projection as well
as first/next requests, events and tool structure. Earlier version 1 candidate
captures remain available at their recorded source revisions and retain their
failed results; they did not check the native usage extension and cannot
qualify the final carrier. The frozen native reference and budgets are shared
unchanged by both candidate oracle versions.

Both paths use the same scripted provider, original user history, 256 KiB
history variant, two native requests, 19 native events, native thinking and
signature, text before and after two parallel tool calls, and the two ordered
results. The independent provider fixture rejects any altered first or next
request. The candidate starts with an OpenAI Chat source; its declared provider
defaults create the native reasoning and output budget. Its Go client consumes
the same versioned chunk and assistant/tool structures qualified separately
through the actual pinned OpenAI JavaScript 7.4.0 and Python 3.8.0 SDKs. The
Go measurement includes no SDK child-process CPU or memory.
The candidate checks the terminal and final-turn `olp.native_usage` against
the frozen native event/result categories, including exact token counts;
separate regression tests cover cache-read, cache-write and TTL categories
that ordinary Chat usage cannot express and reject unknown native categories.

The native reference exposes a tool-use frame on the wire before its separate
reference-side ready transaction. Its `wire-tool` metric is intentionally
earlier than its `action-ready` metric. The candidate buffers ordinary tool
chunks until the gateway's encrypted ready transaction commits. At the first
projected ordinary tool chunk, the candidate performs a separate authenticated
public submission recovery read that decrypts the ready assistant. Only then
does it enable the two fixture actions. This is the candidate `action-ready`
metric and is compared with the frozen reference `action-ready` metric. The
candidate `tool-visible` metric describes its own post-commit observation and
has no native `wire-tool` equivalent.

The candidate's first projected observation differs from the reference's
first native event, so `first-event` is recorded but excluded from direct
budget comparison. The candidate's public recovery GET has an HTTP/auth layer
where the reference performs a direct separate PostgreSQL transaction; that
extra cost is part of the production carrier qualification. Whole workflow,
recoverable action availability, process CPU, allocations and sampled heap are
compared to the predeclared reference limits for the same size and concurrency.
The process resource envelope includes the local Go client, gateway, provider,
oracle and instrumentation, and excludes the PostgreSQL server process in both
cases. The per-phase claim/journal/ready timings from the native reference have
no candidate metric because production code does not expose those internal
timers.

The reference artifact pins shared fixture and harness files at its recorded
source revision. The production continuation candidate necessarily has a newer
`access_test.go` that installs encrypted gateway resources and a newer
`strict_generation_test.go` with native retained Responses positives. The
runner verifies the original source hashes from the reference Git revision,
then records and verifies the candidate's complete current shared-source hash
inventory separately. The frozen reference harness, runner and provider corpus
remain byte-for-byte unchanged. These current test setup and assertion changes
do not change the scripted provider or independent first/next request oracle.

Each candidate repetition requires 24 complete workflows, 48 exact provider
dispatches, 456 served native events, 312 ordered first-turn projected
observations, 48 final-turn text boundary observations, 48 enabled tool actions,
24 independently decrypted ready reads and zero rejected
requests. Three repetitions run for each small/large and concurrency 1/8
combination. The runner checks the baseline self-comparison, immutable source
hashes, complete counters and observation order before it compares candidate
medians with the unchanged reference budgets. A failed budget remains a failed
qualification; the budgets are not regenerated from candidate data.

This local reference does not measure WAN or inference TLS, separate gateway
RSS, real JavaScript/Python process cost, or live-model quality. These limits
remain visible in the candidate artifact and must not be inferred from a green
semantic fixture.
