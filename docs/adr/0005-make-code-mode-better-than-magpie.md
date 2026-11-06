# Make code mode better than Magpie for its chosen job

Status, Final. The user accepted this record explicitly when opening the separate spec stage, and accepting this record does not by itself approve tickets or runtime code.

Magpie is currently better than OLP code mode on the job the user has in mind, and the factual comparison lives in the [Magpie compared with OLP code mode research note](/home/ubuntu/code/olp/1/olp/docs/research/magpie-vs-olp-code-mode.md). This record holds each settled decision as the interview settles it and keeps every open branch visibly unresolved.

## Settled

Better means winning the team job. OLP should beat Magpie for a team sharing subscriptions behind one gateway, and borrow Magpie ideas only where they strengthen that job. Beating Magpie for the individual developer on their own machine and beating it on one named capability while conceding the rest were considered and not picked.

Native forwarding stays the default, and compatibility behavior stays explicit published route policy. The rule in [ADR 0001 on explicit code mode compatibility](/home/ubuntu/code/olp/1/olp/docs/adr/0001-explicit-code-mode-compatibility.md) stands, so parity work widens what opt ins can do and never enables compatibility silently.

The primary win axis is reliability and pooling, ahead of coverage and onboarding and ahead of shared config. Success on this axis means fewer developer visible quota and rate limit failures when a team pools accounts behind one route.

The first reliability stage does both mechanisms together, meaning smarter reset aware selection for new conversations and wider in flight failover under the proof rules in [ADR 0002 on state aware code account recovery](/home/ubuntu/code/olp/1/olp/docs/adr/0002-state-aware-code-account-recovery.md).

The validation bar is a restored qualification gate, meaning automated client journey lanes for the touched adapters run as a release gate before the reliability win can be claimed.

Stage one covers Codex plus the coding plans and OpenCode Go. The Claude subscription adapter and Antigravity stay out of stage one.

A pinned conversation stays hard pinned on quota exhaustion and refuses with a clear quota error until reset or admin action. No pin migration is introduced, so the glossary meaning of safe recovery does not widen.

Wider failover covers enumerated proven rejections only, meaning per adapter complete HTTP rejections with proven not executed semantics, each added only with qualification evidence. Generic pre byte retry and admin configured retryables were considered and not picked.

This interview finishes at docs only, meaning the accepted version of this record plus any glossary change plus the research notes. A spec, tickets, and runtime code are later stages, and each later stage needs its own approval.

Pooling decisions must be inspectable in stage one of any later implementation, meaning console diagnostics would show the per request selection and failover reason within the content free history rule, and the API and console would ship together.

The vendor terms posture stays document risk only, keeping the current warning that OLP does not guarantee a vendor accepts pooled use, with no new acknowledgement gate for wider failover.

No tracker issue is created from this round. Later stages get their tracker home when a later stage is approved, and until then this record in the repo is the only home.

New conversation ordering uses proven bands with weights inside, grounded in the [CLIProxyAPI quota signals research note](/home/ubuntu/code/olp/1/olp/docs/research/cliproxyapi-quota-signals.md). Exhausted accounts stay ineligible, fresh proven observations place known healthy accounts above unknown accounts and unknown accounts above known low accounts, and the existing weighted score picks inside the best band. Soonest reset is a tie break and a refusal deadline only, adapters without a proven signal stay on weights plus cooldown eligibility, and established pins never move. CLIProxyAPI parity with no allowance ordering and Magpie style smart ordering filled with heuristics were considered and not picked.

Active quota reads may feed ordering per adapter only after qualification. The Codex usage endpoint and the MiniMax remains endpoint each need a pinned schema, an approved origin, and live qualification under the restored gate before their observations can order accounts, and unqualified adapters stay on passive signals.

The glossary now carries allowance observation as the canonical term for a vendor reported window state, in the [OLP glossary](/home/ubuntu/code/olp/1/olp/GLOSSARY.md). Code mode, adapter, model pool, conversation affinity, and safe recovery keep their existing meanings because no settled decision widened them.

## Scope contract for this round

The artifact boundary for this round is this record, the allowance observation term in the [OLP glossary](/home/ubuntu/code/olp/1/olp/GLOSSARY.md), and the two research notes. A follow on spec, tickets, runtime code, tests, and tracker issues are out of scope for this round.

Done means exactly four things. This record reaches Final through explicit user acceptance, the glossary term has landed, both research notes have landed with their primary source links clean, and no spec, tickets, code, or tracker issue was created. Nothing outside those four items is a completion dependency for this round.

Later stages are deferred proposals, so accepting this record never approves them. A spec for stage one, its tickets, and its implementation each return for their own approval, and words like go or do it all authorize only the boundary above. Freshness rules for an allowance observation, exact band thresholds, and per adapter classifier details belong to that later spec stage.

Coverage and onboarding work and shared config work in the Magpie Library shape stay deferred behind the reliability stage. The Claude subscription adapter and Antigravity stay out of stage one, and a local single developer mode stays rejected rather than deferred.
