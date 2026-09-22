# Continuation interaction qualification

This evidence covers the `chat-anthropic-tools-v1` negotiated OpenAI Chat to
direct Anthropic Messages tool workflow and strict native retained Responses.
It is scoped to the pinned public profile, controls, SDK versions and scripts;
it is not a provider-wide compatibility claim. No paid inference was used.

The translated workflow sends the frozen native reasoning-enabled first
request, consumes all 19 native events with thinking/signature, text before and
after two parallel tool calls, then sends the exact frozen native next request
with both ordered tool results. JavaScript OpenAI SDK 7.4.0 and Python OpenAI
SDK 3.8.0 each assembled and serialized their actual next request through the
public gateway; the independent provider fixture accepted exactly two requests
per client. Both clients rejected a missing carrier before dispatch, verified
ordered observations and no exposed signature, read a committed recovery
delivery, replayed the same submission without another Attempt and completed
the next turn. The helpers reject unqualified SDK versions before sending.
The versioned client extension retains exact native usage categories; standard
Chat usage includes cached-read detail, while cache-write and TTL categories
remain in `olp.native_usage`. Unknown usage categories fail the strict runtime
guard rather than disappearing from a successful translated response.
An additional official JavaScript SDK test consumed the committed first stream
inside its transport, raised a connection error, and let the SDK automatically
retry: both proxy HTTP requests carried the same submission identity, while
the provider accepted once and accounting recorded one Attempt.

The resource authority stores bounded complete native dependencies encrypted
under the existing KeyRing. An API key must explicitly allow provider state.
The gateway atomically commits the owner-unique submission claim and dispatch
journal before provider Do, and commits complete ready payload and state in one
later transaction before emitting ordinary tool chunks. An authenticated
recovery GET can independently decrypt that ready state. An incomplete accepted
request is outcome-unknown and cannot become new inference through an SDK or
network retry. A ready replay never dispatches provider work or debits a new
Attempt. Handles are scoped to the owner, route, historical provider/slot,
serving identity and contract; current key, provider and network credential
authority is checked on every use. Branches retain separate complete children,
and expiry and key revocation refuse lookups synchronously.

Public PostgreSQL tests blocked the ready commit while the provider streamed
and observed no actionable tool bytes until another reader saw committed
ciphertext. Fault tests removed the terminal event, changed stop reason, lost
the late signature and overflowed event bounds; none produced a ready replay
or duplicate dispatch. Real gateway processes were killed before ready and
after ready but before a handle was consumed. After restart, accepted unknown
work remained unknown and committed work recovered without inference. Canceled
pre-send and incomplete ingress uploads left no journal; cancellation after
provider acceptance stayed non-replayable. Eight concurrent resource claims
selected one creator, and a failed encryption rolled back the combined
dispatch journal. A compatible provider revision publication and fresh gateway
instance kept the historical endpoint/defaults for the active handle; revoking
that historical credential then refused recovery. Public branch, cross-key,
cross-route, tamper, expiry and live key-policy/revocation checks passed.
An active handle created over a real mTLS connection with a private trust root
also refused GET recovery, same-submission replay and a child turn after its
historical network credential was revoked; the provider accepted no new work.
Native strict public results separately preserved citation boundaries,
refusal structure and ordered Gemini candidates.
Translated requests asking for multiple candidates, citation projection,
structured output, an unqualified parallel-tool switch or a foreign reasoning
budget each returned a precise pre-dispatch reason with zero provider calls
and zero continuation claims.
The no-inference inspector showed the negotiated carrier, 4 MiB dependency
bound and encrypted actionability obligation to an authorized key; it rejected
a state-disabled key and unsupported carrier without a provider call or claim.

Focused validation after the combined claim/journal change: public workflow,
barrier, fault, branch, policy, revision and JS/Python SDK tests passed in
18.527 s; relevant race tests passed in 18.280 s; fresh binary process loss
and accepted-cancellation tests passed in 3.493 s; failed-encryption zero-call
and eight-claim journal tests passed in 2.524 s. The recovery helper update
passed both official SDK journeys in 8.130 s. Actual automatic JavaScript SDK
retry passed in 1.750 s and its race check in 4.511 s. These are focused checks; the
repository-wide release gates still run on the integrated PR branch.
The mTLS handle-revocation case passed in 1.429 s and under race detection in
4.017 s; the five public pre-dispatch rejections passed in 1.192 s and their
combined network/rejection race run in 5.779 s.

The [frozen native encrypted barrier](../../evidence/fidelity-performance/barrier-v1/README.md)
was not changed. The first clean [translated candidate capture](../../evidence/fidelity-performance/barrier-v1/candidate-failed-4e086098.json)
at source `4e086098` completed all 12 repetitions: 288 workflows, 576 exact
provider dispatches, 5,472 native events, 3,744 first-turn and 576 final-turn
observations, 576 enabled actions, 288 authenticated ready reads and zero
rejections. Its unchanged frozen comparison **failed six small-history
metrics**: concurrency 1 `ns/op`, CPU/op and workflow p50/p95/p99; concurrency
8 workflow p50. All other matched metrics, including every large-history and
action-ready metric, passed. The artifact remains in the repository for review.
The later one-resolution and combined claim/journal optimizations preserve
semantics but do not erase the recorded failure. A second clean
[candidate capture](../../evidence/fidelity-performance/barrier-v1/candidate-failed-7f333ee7.json)
at `7f333ee7` completed the same full semantic inventory and **failed 13
frozen metrics**, mainly small-history workflow/CPU/latency and one large c1
workflow p50. Its source, storage and hardware identities passed the runner's
strict checks. Ambient host load before capture was 2.60 (one minute) versus
0.63 before the first candidate; the worsening across all four workloads is
not evidence that the code change caused a regression, nor is it a passing
result. Both retained artifacts use candidate oracle version 1. Candidate
oracle version 2 also verifies exact native usage categories; the earlier
failures remain reviewable at their recorded source revisions. The
optimization needs a controlled same-condition differential and further
hot-path diagnosis. A new clean capture must pass the unchanged frozen
limits before the performance portion of qualification can be called complete.
The scoped functional continuation implementation can be integrated for
T02/T03/T07 review; final G6 performance qualification remains a separate
open gate and no passing claim is made for either failed capture.
