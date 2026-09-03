# Execution roadmap

Version 2.3.0 is published. This period closes the external evidence gaps
carried from the archived [September 2026 roadmap](archive/2026-09/README.md).
It does not reopen product scope. Work candidates stay trigger-gated in
[`deferred.md`](deferred.md).

## Priorities

| Priority | Meaning |
|---|---|
| P0 | Release or security blocker; interrupt other work |
| P1 | External correctness or release evidence |
| P2 | Operator and performance evidence |
| P3 | Maintenance with measured benefit |

## Schedule

| Week | Outcome | Items |
|---|---|---|
| 1 | Close release and provider-risk evidence | TEST-01, REL-07 |
| 2 | Reproduce operator-facing workflows | DOC-05, OTEL-05, SPEND-06, SPEND-07 |
| 3 | Measure tracing-disabled performance | OTEL-06 |

The active item definitions and status live in [`backlog.md`](backlog.md).

## Health signals

- Keep the consecutive-green `Required` streak visible; it was twelve inspected
  runs at the September archive cut.
- Record checkout-to-first-request elapsed time from a person who has not used
  this repository before.
- Keep the live-provider workflow green and close `provider-drift` issues only
  after a successful recovery run.

## Scope rules

1. Finish P1 evidence before starting P2 evidence.
2. Do not substitute mocks for a carry-over that explicitly requires a live
   service, Compose stack, browser, or fresh cluster.
3. Do not start a deferred candidate without its documented reopen trigger.
4. Record inaccessible credentials or infrastructure as a blocker with an
   owner; never weaken a gate to make it green.

## Archive

- [September 2026 final plan, score, and decisions](archive/2026-09/README.md)
- [September 2026 final backlog](archive/2026-09/backlog.md)
- [September 2026 immutable baseline](archive/2026-09/baseline.md)
- [September 2026 risk register](archive/2026-09/risks.md)
