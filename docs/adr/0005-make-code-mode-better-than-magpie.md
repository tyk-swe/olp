# Make code mode better than Magpie for its chosen job

Accepted. This record authorized documentation only. A later specification, tickets and implementation each require separate approval.

OLP should beat Magpie for teams sharing subscriptions behind one gateway. Reliability and pooling come before coverage, onboarding and shared configuration. Success means fewer developer-visible quota and rate-limit failures. The [comparison](../research/magpie-vs-olp-code-mode.md) records the product differences.

## Reliability stage

Stage one combines reset-aware selection for new conversations with wider in-flight failover for Codex, the coding plans and OpenCode Go. Claude subscription and Antigravity are excluded. Native forwarding remains the default, with compatibility enabled only by explicit published route policy under [ADR 0001](0001-explicit-code-mode-compatibility.md).

New conversations use fresh proven [allowance observations](../../GLOSSARY.md#code-mode) to rank healthy accounts above unknown accounts and unknown accounts above low accounts. Exhausted accounts remain ineligible. The existing weighted score chooses within the best band. Soonest reset only breaks ties or supplies a refusal deadline. Adapters without proven signals keep weights and cooldown eligibility. The [quota research](../research/cliproxyapi-quota-signals.md) supports this choice.

Established conversations stay pinned. Quota exhaustion returns a clear error until reset or administrator action. Wider failover covers only enumerated, complete HTTP rejections with proven non-execution semantics and per-adapter qualification evidence under [ADR 0002](0002-state-aware-code-account-recovery.md). No pin migration or broader meaning of safe recovery is introduced.

Automated client journeys for touched adapters must be restored as a release gate before claiming the reliability win. Active Codex usage and MiniMax remains reads need pinned schemas, approved origins and live qualification under that gate before they can order accounts. Unqualified adapters retain passive signals.

The API and console ship together, with per-request selection and failover reasons visible in diagnostics under the content-free history rule. Vendor pooling risk remains documented without a new acknowledgement gate or guarantee of vendor acceptance.

## Alternatives and deferred work

Local single-developer parity and winning only one named capability were rejected. So were generic retries before the first byte, administrator-configured retryable errors, CLIProxyAPI parity without allowance ordering, and heuristic smart ordering.

Coverage, onboarding and Magpie Library-style shared configuration follow reliability. Observation freshness, band thresholds and per-adapter classifiers belong to the later specification. Existing glossary meanings remain unchanged apart from the added allowance observation term.

The original round completed with this accepted record, the glossary term and two cited research notes. It created no specification, tickets, runtime code, tests or tracker issue. This record remains that round's home until a separately approved stage creates its tracker home.
