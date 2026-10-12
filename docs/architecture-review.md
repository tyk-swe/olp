# Architecture review and implementation ledger

Baseline: `e5a87b859fe25aa7b3287111899a7f4074d1ada5`, initially clean worktree;
Go 1.27.2, Node 26.11.1, pnpm 11.24.0 match repository pins. Initial `make test`
passed Go and benchmark harness tests; console setup failed because ignored
generated `requirements.ts` had not been generated. `make api` restores that
prerequisite. No dependency or toolchain pins changed.

Production dependencies are separated from test/fixture imports in the review.
The modular monolith and feature-owned transactions remain the architectural
model. Serving releases are request-pinned; live authority is independently
refreshed. PostgreSQL owns durable facts; Valkey coordinates admission and
metadata delivery. Protocol owners validate native sources; gateway owns
transport, deadlines, admission, settlement and downstream commitment. Process
owns composition and task/dependency lifetimes. OpenAPI owns management consumer
facts; console drafts own logical mutations and temporary secret input.

## Coverage and working plan

| Area / representative flow | Review owner | Reviewed boundary / qualification |
| --- | --- | --- |
| Process modes, startup failure, readiness/drain, listeners, workers | Process/runtime | A1; focused race and service tests |
| Releases, live authority, credentials, external resolution, replicas | Process/runtime | B1–B3; constructor, progress, pool/revocation tests |
| Ordinary strict/transformed generation, retry/fallback, unary, OIF | Primary | C4–C5, E1; planning/dispatch/memory; native corpus and benchmarks |
| Pinned files/batches/responses/Interactions, accounting and delivery | Primary | C1–C6; transport/body/delivery; native terminal, trusted metadata and durable background usage |
| Realtime / Gemini Live duplex lifetimes and native observations | Primary | D1–D2; cancellation race tests and parsing benchmark |
| Durable media, historical pins, claim fencing/tombstones/recovery | Primary | F1; exact historical pins; race/service reservation refusal and recovery tests |
| Raw code HTTP/SSE/WebSocket, binding, hard-token uncertainty | Primary | Existing separate lifecycle; integration/official client qualification |
| Budgets/calendar, raw/hourly retention, usage commit/ack/recovery | Process/runtime | G1–G3, C3–C4; per-attempt ownership, calendar/raw/hourly and durable redelivery service checks |
| Management access, providers/routes, configuration export/plan/apply/replay | Contracts | H1–H3, G2; shared transaction invariants, grant fences and promotion tests |
| Console forms, sessions, permissions, logical retries, enrollment | Console | I1–I6, G3; mounted regressions, typecheck and browser suite |
| OpenAPI generation, SDK/CLI/MCP, copied official SDK examples | Contracts | J1–J5; generated parity and actual SDK snippet qualification |
| Plugin ABI/host/SDK/reference, egress and confinement | Contracts | Review pinned ABI/cancellation/evidence; existing qualification |
| Persistence/migrations, Compose/Helm, CI/build/release/scripts | Primary | Final contract/build/integration gates; no incidental format churn |
| Operations, security, configuration, compatibility and feature docs | Primary | Update actual ownership/contracts; record limits of evidence |

## Validated debt

The initial review recorded 31 IDs; source-scoped retained identity adds F2,
bringing this ledger to 32. The count is evidence, not a finding quota.
All entries below have high confidence from concrete source/caller evidence.
Tests, dispositions and precise validation results are added as slices close.
Priority orders consequence and dependency, rather than change size.

Source findings have focused evidence and independent review, including the B2
release/grant publication regression found by the integrated service gate and
the C3 observation and C6 metadata boundaries found by the final source review.
The final source review closed C1/C3, F2 and I1/I2 siblings after focused
counterexamples, passing checks and independent rereads. Validated native
terminal evidence, accepted-work retry policy, local-refusal health and deferred
HTTP observations retain separate owners. All areas in the coverage matrix have
been reviewed and the final normal whole-source qualification passed. An earlier
complete run exposed fixture assumptions about constructor aliases and native
terminal frames, corrected without production changes. Rejected hypotheses
and deliberately retained contracts have explicit dispositions below.

Verification dependencies follow the affected state owners. B1–B3 jointly
protect release pins and fresh credential authority; E1 and F1/F2 depend on those
pins. C1/C2/C4/C5 share transport, native completion and delivery evidence;
C3/F2/G2/C6 jointly protect original identity, financial ownership and trusted
pending metadata. D1/D2 combine joined tasks with protocol-owned observations.
H1–H3 share provider transaction and credential-boundary operations. I1/I2/I4–I6
protect draft, principal and resource ownership, including I3 enrollment. J1–J3
share generated contract facts; J4/J5 preserve caller-owned mutation intent.
A1, G1 and G3 have no new cross-finding implementation prerequisite.

| ID / priority | Evidence, fragile invariant and consequence | Selected repair / dependency | Status |
| --- | --- | --- | --- |
| A1 / critical | `process.Run`: tasks start before bind; early returns cancel without joining, reader closes before worker cancellation, runtime wait escapes drain deadline. Dependency closure can race owned tasks. | One registered, bounded cleanup path for partial and full startup; forced unblock on expiry. | Closed. Focused race and scratch-service tests verified; final whole gates passed |
| B1 / high | `runtime.NewRelease` retains caller snapshot and shallow credential buffers. Constructor aliases can mutate a validated serving release. Production readers themselves showed no mutation. | Constructor-time deep freeze; defensive public credential copies and explicit immutable serving borrow, without per-request snapshot copies. | Closed. Focused race and independent review verified; final whole gates passed |
| B2 / high | Refresh obligations share a sequential polling budget; slow release/remote reads starve authority progress; older completion can win. Integrated review also exposed first-release publication before its grant cache: served generation 1 appears as generation 0 to health, and later rotation loses its refresh notification. | Independent deadlines; coherent initial grant seeds and atomic release/cache publication; preserve newer generations and retry candidates whose installed base changed. Reuse unchanged immutable pairs; check cancellation after serialization. Authority and last-good release stay separate. | Closed. Fixed; two independent reviews, repeated unit race tests and original/fenced service regressions verified; final whole gates passed |
| B3 / high | Historical external Secret resolves remotely inside a PostgreSQL transaction. Small pools block authority/revocation progress. | Commit authorized pinned-reference read before remote I/O; recheck current revocation after resolution; retain bounded cache/single flight. | Closed. Focused race and scratch-service tests verified; final whole gates passed |
| C1 / critical | `pinnedDo`, multipart and Bedrock duplicated lifecycle: unconditional dispatched errors, success/commit at headers, dropped usage on mapping/delivery failure, unjoined multipart writers. Native Responses discarded validated terminal evidence on a failed client write. Gemini SSE classified delivery/read cancellation as protocol failure and advanced visibility before Flush; unary paths omitted bounded writes/Flush and claimed first byte. | Reuse attemptState/upstream evidence; validate native completion/identity before effects; join writers and emit once. Preserve native phase/consumption independently of request cancellation. Use the original request's causal error without masking malformed frames or live-caller route deadlines. Bound Gemini writes and check Flush before first byte/client completion. Preserve known active-generation native faults through cancellation; successful retained reads retain their operation semantics. Fence accepted POST retries before headers, including with response metadata disabled. | Closed. Implemented focused repairs; eight original Responses writer cases plus three retry controls passed race in 28.748s, with gateway/OpenAI controls in 1.496/1.042s. Gemini writer before-cases reproduced false 502 in 9.182s; four read-cancel cases reproduced it in 11.519s. Sixteen read/write/flush, malformed-frame and live-parent timeout controls passed race in 30.562s before the final unary repair. Four buffered unary before-cases proved absent Flush/deadline and false first byte; completed accepted POST also lacked its retry fence. The final ten-case native failure/unary delivery matrix passed race in 19.531s, with pinned unit controls in 1.214s and clean independent review. Final unchanged-source sixteen read/write controls passed race in 31.140s; six repeated before-first-frame cases passed race in 12.943s with accepted native evidence. Suspected retained SSE phase loss was disproved by finalization restoring settled evidence. Latest whole gates passed |
| D1 / high | Both duplex coordinators receive only one result after control-triggered exit, leaving another task mutating final observations. | Cancel, unblock peers and join all remaining application tasks before returning usage/state. | Closed. Focused cancellation race tests and independent review verified |
| D2 / medium | Gemini Live parses one bounded provider frame repeatedly across validation, usage and terminal helpers. | Protocol-owned typed observation from one OIF parse; gateway retains socket and session semantics. | Closed. Verified; 127→36 allocations/frame; ~13.6→3.55 KiB/frame in local benchmark |
| E1 / medium | Prepared strict/profile/unary plans retain heavyweight native documents per eligible target into long streams; route/source changes manually clear selected caches. | Measure retained memory, separate admission receipts from dispatch documents, and replace preparation owner on transitions. | Closed. Verified race eviction/source tests; 16-target strict retained memory ~16.9→1.14 MiB for 1 MiB prompt |
| H1 / high | Promotion network binding bypasses provider transport-fingerprint invalidation, retaining stale certification after destination mTLS changes. | Shared provider-owned transaction-aware draft configuration mutation after destination binding. | Closed. Eight adjacent race service regressions and independent review verified; final whole gates passed |
| H2 / high | Promotion deletes/reinserts slots without advancing collection ETag; unchanged pending enrollment is unnecessarily cascaded. Portable no-op credential binding also bypasses parent ETag. | Provider-owned staged inventory and credential transitions preserve unchanged slots, advance parent/collection identities and clean omitted enrollment secrets. | Closed. Eight adjacent race service regressions and independent review verified; final whole gates passed |
| I1 / high | Code-mode and ordinary idempotent helpers mint identity per invocation. Commit followed by response loss cannot be safely retried; refresh failure can repeat a successful write. | Draft-owned identity, immutable effective payload/ETag/expiry, stable retries, changed-input rules and write/observation separation; migrate current callers. Retire frozen secret-bearing receipts on cancellation/artifact/resource replacement. | Closed. Mounted before/after replacement/secret/retry proofs, 59 panel and 22 configuration/mutation tests, types/lint and independent reread verified; final integration/browser gates passed |
| I2 / high | Code-mode local project/tab changes unmount dirty forms; stale callbacks and unauthorized member inventory query violate draft/permission boundaries. | Shared unsaved-change lifetime including local transitions, stale completion fences, authorized reference queries and preserved unavailable selections. Refuse competing row/input replacement during saves; pin initial ETag through refresh; fence principal/mount and delayed file revisions. | Closed. Mounted local-transition/conflict/permission, delayed-row/409/file-read counterexamples and independent reread verified; final integration/browser gates passed |
| I3 / high | Three grant consumers independently poll/continue; failed one-use submission retains pasted secret; completion waits for refresh. | One shared enrollment lifecycle, explicit completion hooks and input clearing at one-use boundary; preserve onboarding differences. | Closed. Mounted expiry/cancellation/one-use/stale-completion tests and independent review verified; final browser gates passed |
| F1 / high | Media SQL interprets serialized release internals; reconciliation accepts a credential from any sibling slot and reservation does not verify supplied revision. Malformed durable identity can reconstruct another target. | Shared pure release/target/slot/version decoder; feature transactions retain current new-work policy and exact native configuration comparison. | Closed. Unit/race and real PostgreSQL refusal tests verified; no schema change |
| F2 / high | Retained native IDs are opaque and account-scoped, but the unique key and lookup omit actor/account/selected-source dimensions. Before-fix race proof (3.629s): both POSTs reach the correct accounts, share one alias, and B's GET returns A's output. | Feature-owned typed source identity covers kind/key/provider/revision/route/revision/selected route-target/slot/credential-version/native ID in all publication, lookup and previous/output paths. Forward 0062 adds an immutable scalar and uniquely proven historical backfill, preserving local IDs/ciphertexts; unproven sources refuse dispatch. | Closed. Implemented; four strict/transformed × unary/SSE modes, caller/credential rotation/history/previous echo passed race in 7.88s; Batch/Response lifecycle passed in 3.61/2.32s (package 15.042s). Earlier adjacent 16 and seven backfill cases passed; gateway/database/resource unit race passed. Independent schema/caller/DTO review clean; latest whole gates passed |
| G1 / high | Aggregate deltas charge all attempts to the final provider; reconstruction omits route-project fallback; budget SQL relies on literal text replacement. Rebuild and displayed ownership diverge for keyless/shared-provider work. | One per-attempt key→provider→route project rule; fixed owner/query bindings and authoritative event-fact deltas inside idempotent transaction. | Closed. Broad race calendar/pricing/exemption/raw→hourly/rebuild service tests verified; final whole gates passed |
| G2 / high | Physical budget-group deletion can remove the immutable original financial subject before accepted background or queued usage commits; a new pending FK alone blocks upgrades with prior orphan templates. | Access-owned policy-free retirement; hide and reject new use, preserve existing dependency refusals, forward-only orphan identity backfill without invented authority, then install pending FK. | Closed. Public PostgreSQL/Valkey and 0059 upgrade race checks plus independent review verified |
| G3 / medium | Monetary form copy describes UTC reset while production admission, reconstruction and reports use installation budget timezone. | Correct monetary copy and explain known pending usage versus recorded accrual; raw-code hard-token UTC remains separate. | Closed. Actual production calendar source and full console suite verified |
| C2 / critical | Native Bedrock relay could complete truncated streams with partial initial usage and expose native failure credentials. Gemini failed unary/SSE projections exposed applied credentials for creation/retrieval; active POST native server failures were labeled success and delivery cancellation erased their known fault. | Operation-owned terminal observation; preserve partial consumption uncertainty, distinguish native error/delivery failure, scrub native exception and base64 error documents. Reuse bounded OIF redaction for Gemini before local projection. Classify active generation's native fault independently of delivery while preserving successful failed-resource retrieval. Keep protected pending/HTTP-finalization separation and immutable financial receipts. | Closed. Implemented/verified. Bedrock truncated/terminal/failing-writer/credential-echo race proofs passed. Four Gemini leak before-cases failed in 5.383s; two native-fault/writer siblings failed in 2.692s, and original persisted SSE health incorrectly counted success. Final ten public cases passed race in 19.531s: credential sanitation, member order/numeric lexemes/extensions/framing, exact original usage, one call, known native fault through failed delivery, truthful Flush visibility, metadata-disabled no-retry fences and successful failed-resource reads. Actual original POST SSE health, including failed writer, records attempts/success/server/transport 1/0/1/0 through the existing finalizer. Pending financial observations remain UNKNOWN until that bounded finalizer; no receipt/schema/outbox change. Independent review found no further actionable issue in this boundary; latest whole gates passed |
| C3 / critical | Pending Gemini background result is treated terminal; retained final usage lacks original durable accounting; expired pending templates are blindly deleted. Review also found partial replay/counter and unchecked terminal boundaries; deferred templates recorded fake 200/committed for actual 502/uncommitted delivery. | Operation-owned grammar/identity and typed usage precede effects, counters and overlays. Lock original accounting, fence settled replay and retain scoped pending-cost proof. Internal optional HTTPOutcomePending persists unfinished observations as null; detached two-second finish locks the resource and patches its pending template or receipt-qualified persisted observations, preserving financial identities/evidence/receipt and no early history. | Closed. Implemented; unary/stream identity, malformed terminal, returned-counter and accepted-work retry/health proofs verified. Seven PostgreSQL race HTTP ordering/cleanup/replay/conservation tests passed in 23.190s, including actual 502/uncommitted assertions. Gemini projection refusal/replay and 11 adjacent cases passed race; independent final source review clean. Latest whole gates passed |
| I6 / high | Provider workflows repeat confirmed phases before unresolved receipt replay, conflate writes with failed observation, continue after retirement, or use newer parent ETags with stale reviewed model facts. | Provider-specific phase progress and refresh-only recovery retain exact requests. Draft/principal/selection epochs fence chains; model snapshots bind rows to one page version and advance only through confirmed own receipts. Probe/certification API helpers preserve body plus exact ETag separately. | Closed. Fixed; wizard28, detail34, pool/catalog15, bulk28 and affected caller105 + HTTP receipt10 slices passed; final types0/0, formatting/lint and recursive independent reviews passed. Final integrated gates passed after the sibling repairs |
| C4 / critical | Ordinary native streams retain intermediate counters after truncation, but generic fact recording marks every nonnil usage complete. Unknown consumption can disappear from durable facts and mixed-attempt settlement. | Protocol-owned usage completeness before projection/delivery, paired dispatched request bounds, one fact usage owner, per-attempt uncertainty floor. | Closed. Actual priced dispatch, six-family native refusal/delivery, exact Cohere counters and controlled rollback verified; full protocol/gateway races pass |
| C5 / high | Pinned HTTP SSE/EventStream omit canonical write deadlines; buffered unary writes can finish success before Flush, and native stream first-byte can precede failed Flush. | Apply existing per-frame deadline, check full writes/Flush before delivery, retain independently completed upstream usage. | Closed. Public stalled/buffered SSE/unary/replay/recovery, silent/positive WebSocket delivery race tests verified; independent final review passed |
| C6 / high | Opaque native Batch `pending_usage` is copied into generic metadata; accounting consumers assign expiry, index subjects or try to persist/delete it as trusted original-generation evidence. An unsupported extension becomes undeletable and a valid-shaped forged event enters financial projections. | Trust pending accounting only for response/strict_response/interaction in Go and SQL, before parsing/casting. Keep native Batch fields opaque and unchanged; guard new migration backfills and orphan extraction. | Closed. Fixed; public namespace race tests, baseline-0059 upgrades, controlled rollback and fresh independent review passed; final whole gates passed |
| H3 / high | Grant challenge/exchange pins slot/profile but not current credential destination; destination edits can retain secrets or commit remote tokens across the boundary. | Feature-owned credential-boundary identity, cancel unclaimed challenges, locked post-exchange boundary fence. | Closed. Pre-fix failures reproduced; eight adjacent race service regressions verified |
| I4 / high | Delayed route/provider callbacks manually populate a new principal cache after the old editor is disposed. | Full principal/authority partition fence before manual cache writes; retain same-principal old-resource updates. | Closed. Independent reproduction and 51 mounted tests verified |
| I5 / high | Passive same-user authority changes and cached provider A→B navigation retain typed secrets, dirty guards or delayed enrollment results in a replacement owner. | Key protected content by full authority partition and provider detail by resource ID; retain explicit late-result/cache fences and clear one-time secrets. | Closed. Pre-fix mounted failures reproduced; CSRF/permission/resource/secret regressions, types/lint verified |
| J1 / high | Registry discards scalar parameter type facts; Go integer query values are rejected and unsupported shapes accepted. | Generated bounded scalar contract, lossless Go/CLI encoding and explicit unsupported-shape generation failures. | Closed. Focused race, generator, exact official JS/Python snippet qualification verified |
| J2 / high | MCP omits declared parent-ETag transitions needed for subsequent safe mutations. | Contract-derived allowlisted metadata shared with Go client; redaction unchanged. | Closed. Focused race and independent contract/consumer review verified |
| J3 / medium | HTTP-method/name inference labels SCIM search POST destructive although GET uses identical read implementation. | Explicit reviewed read-only OpenAPI extensions and generated effect facts. | Closed. Focused race and independent contract/consumer review verified |
| J4 / high | SDK auto-creates a write key and never returns it; CLI inherits unreplayable ambiguity. | Require explicit caller-owned logical mutation key under 0.x; current SDK/CLI help/tests/docs updated. | Closed. Focused race and independent contract/consumer review verified |
| J5 / high | CLI configuration apply replans before sending the retained mutation. A committed apply with a lost response changes the destination and is refused before its original receipt can be replayed. | Version-2 saved intent preserves origin, original document/plan and external-reference consistency, including exact native order/numbers; send original apply/key directly. Server owns authorized replay and destination preconditions. | Closed. Real HTTP/PostgreSQL pre-fix failure reproduced; fixed recovery plus stale-first-attempt service race tests passed in 11.533 s, proving one apply audit record and revoked-token refusal; unit race/native-order guards and independent server-order review passed; final whole gates passed |

## Deliberate distinctions and rejected hypotheses

- Feature-specific generation, unary, media, durable, realtime, Gemini and raw
  code-mode contracts remain separate; shared mechanics do not imply identical
  acceptance, retry or terminal obligations.
- Published release access remains an immutable borrow. No production mutation
  was found; opaque per-request copies would add cost without fixing the actual
  construction alias. Construction and credential buffers are strengthened.
- Installation mutation serialization remains: authorization, publication,
  replay and audit are transaction invariants, not a cosmetic contention target.
- Native code-mode hard-token bounds are intentionally unavailable where the
  official contract provides no conservative total-token bound. Controlled
  fixtures do not establish live-provider qualification.
- Metadata delivery retains its bounded explicit loss policy. Durable commit
  precedes acknowledgment; this review does not claim exactly-once delivery or
  add an unmeasured outbox.
- Authoritative provider configurations and release snapshots are PostgreSQL
  `json`, deliberately preserving native values. The suspected `jsonb` numeric
  normalization defect is disproved. Exact configuration comparison excludes
  only live quota limits.
- Group monetary ownership uses the original event's group, not current key
  membership. A stale review note suggested the latter; source and queries
  disproved it. Named accounting routes remain distinct from pinned and actual
  fallback/delegate legs.
- Valkey retains an ordinary lease's initial token allowance, including when
  native terminal usage is exactly 26 tokens or zero. `Lease.Reconcile` only
  increases that allowance; the proposed refund requirement is disproved by
  source, documentation and the real-Valkey fixture. Native usage completeness
  and monetary facts remain independent of this admission policy. C4 preserves
  each uncertain attempt's estimate floor when summing several attempts.
- Response metadata cannot inject `pending_usage`: its existing five-field
  allowlists exclude that field. The evidenced opaque metadata defect was Batch
  C6; it does not justify a second Response metadata mechanism.
- Ordinary native caller idempotency has no qualified upstream contract here:
  strict preparation rejects those headers and transformed dispatch does not
  forward them. Source-scoped retained aliases do not add upstream replay support.
- Configuration secret inputs render for a blocked plan. A clean plan removes
  those inputs before Apply, so the suspected editable password during an apply
  was not evidenced. Artifact abandonment and delayed file/409/principal
  boundaries were evidenced and repaired under I1/I2.

## Representative flow outcomes

The dependency map distinguishes production imports from tests, fixtures and
generated clients. These outcomes describe the ownership contract; final gates
below qualify the integrated implementation.

| Flow | Decisions and state owners | Refusal, failure and concurrent change |
| --- | --- | --- |
| Strict/transformed unary and streamed generation | Runtime pins release/route; preparation owns effective source/receipts; attempts owns slots, reservations, caps, health and progression; native protocol owns completion/usage; gateway owns delivery. | Live authority rechecks exclude revoked material without spending attempts. Only declared safe precommit failures retry/fallback. Accepted ambiguity and partial consumption retain uncertainty; downstream failure preserves observed usage. Route/source transitions replace preparation. Crashes leave lease recovery and metadata completeness evidence. |
| Provider/route edit and publication | Feature transaction reauthorizes under installation lock, validates current state/ETag, stages configuration, publishes and audits atomically. Runtime validates/freezes installation and independently refreshes authority. | Stale preconditions/replays remain scoped. Failed publication rolls back; failed activation retains last good release. Rotation/revocation cannot change a request's configuration pin or let older refresh completion replace newer material. |
| Retained files/batches/responses/media | Resource/media features own caller- and source-scoped native aliases, exact historical target/slot/credential pins, mapping/claim transactions, tombstones and cleanup. Operation owner distinguishes accepted submission, native completion and durable attachment. | Account-local native collisions produce distinct owned aliases. Unproven historical targets refuse dispatch. Accepted create plus failed attachment remains explicit unknown; no generic cross-target failover. Bounded detached commits preserve accepted identities; restart/reclaim is fenced. Original pending usage survives delete/expiry; poison owners retain evidence without starving unrelated cleanup. |
| Realtime and Gemini Live | Session owns both relay tasks, cancellation, peer unblocking and final observations; protocol owns bounded native grammar/terminal counts. | Revocation, heartbeat, context and either forwarding failure cancel/unblock/join both tasks before final facts. Native close meaning and pending terminal obligations remain separate. Full/partial provider usage survives failed downstream forwarding. |
| Raw code HTTP/SSE/WebSocket | Code transport/authorizer/ledger own native dispatch, account/principal matching, binding identity and per-generation durable admission. Gateway owns socket/HTTP transport. | MarkDispatched precedes native work; no translation or gateway retry/failover. Reported totals settle once; interrupted/conflicting consumption remains uncertain. Unknown parent/account and unavailable conservative hard-token bounds refuse. Reconnection/new generation reauthorizes the pinned binding. |
| Console/management mutation | Session owns fresh CSRF/authority partition; form owns payload/identity/ETag; handler owns authorization/replay; feature transaction owns audit/state; observer owns refresh. | Same-intent ambiguous retry retains exact wire input/key. Changed input is new intent. Confirmed writes are not repeated for failed refresh. Local navigation participates in dirty-state lifetime; scope/resource replacement retires secrets/drafts and fences late cache writes. Conflicts retain inputs rather than fetching a fresh ETag to overwrite. |
| Configuration export/plan/apply/replay | Configuration orchestrates portable intent and destination-owned bindings; providers/access/routes own their transactional policies. Reviewed fingerprint identifies the plan. | Stale plan/authority refuses. Secrets/restrictions remain destination-owned; transport changes invalidate evidence and pending enrollment boundary. No-op and explicit staging remain deliberate. Failure rolls back publication/audit/state; replay disclosure reauthorizes. |
| Usage/delivery/retention/reconstruction | Bounded emitter records losses; consumer commits deduplicated PostgreSQL facts before acknowledgment; owner rules/calendar govern deltas, retention and Valkey rebuild. Resource owns deferred original financial template; gateway finish owns original HTTP observation. | Native terminal usage settles before delivery overlays. Duplicate/redelivered events do not repeat deltas. Detached finish updates the locked pending template or receipt-qualified existing observations; failed/crashed finish leaves null HTTP metadata. Unsupported envelopes stay owned. Monetary admission fails closed on incompleteness/known scoped pending work; nonbillable retrieval can finish the original owner. |
| Startup/readiness/drain | Process acquires dependencies, registers task/resource ownership, binds listeners and advertises readiness; runtime/feature loops retain local responsibilities. | Every partial-start failure uses producer-first shared cleanup. Authority remains available during producer drain, then metadata/delivery/consumers stop before dependencies. One deadline bounds joins/closures; forced unblock reports incomplete work rather than extending the deadline. |

## Measurements and validation scope

The prepared-plan memory characterization keeps eight executions reachable
across GC with 1 MiB prompts. Strict 16-target retention falls from about
16.9 MiB/request to 1.14 MiB in selected-attempt state. Eviction/revisit tests
compare actual native bytes and estimates. Planning allocations change slightly;
no speed claim is inferred from noisy planning samples.

Gemini Live's one bounded observation reduces a representative frame from
127 to 36 allocations and about 13.6 to 3.55 KiB. Native Bedrock's strengthened
duplicate/Unicode-safe grammar and count/terminal observation costs more than the
initial sparse parser: aligned six-sample four-frame relay median 44.26→54.42 µs,
19,176→26,048 B and 259→325 allocations. This measured correctness tradeoff is
retained; no speculative second fast parser weakens OIF. These are local
microbenchmarks without networking, not live-provider or production capacity
qualification. Existing integrated hot-path gates remain separately required.

For 10,001 pending mappings, indexed subject/recorded-owner lookup replaces two
repeated scans measured at 8.92/15.63 ms with 1.20/2.04 ms and 24–50 execution
buffers in initial race-enabled shared-service measurements. The fourth full
run exposed a redundant global existence probe after projection churn: before
vacuum it scanned 314 pages while the recorded-owner index probe used two.
Removing that probe and preserving indexed installation occupancy reduced the
deterministic 10,001-row churn proofs to 22–73 pages against an 18-page baseline,
within the unchanged 64 additional-page bound; installation occupancy used 21
pages. Empty-scope and malformed-owner behavior, original ownership/backfill and
native-Batch forward upgrades passed race checks and independent review. These
characterize this query boundary, not arbitrary-bloat or production latency
guarantees.

Warm strict chat dispatch was compared with identical added instrumentation at
the initial commit and current checkout: small and 1 MiB prompts, unary and
64-frame streaming, aligned functions and ten alternating 500 ms samples.
The initial small-stream slowdown did not reproduce in twenty confirmation
samples (2.865 versus 2.866 ms); the unchanged 10% regression check passed.
Added allocation cost is approximately 1.1 KiB per request across both prompt
sizes, with small unary allocations 448→462. One small credential copy belongs
to the fixture's defensive accessor; production cached reads borrow immutable
bytes. This fixed bookkeeping cost is retained within the existing policy.
These in-memory warm-client results do not establish network or production
latency. The existing repository-wide gate is separate.

## Decisions and migration implications

Constructor-time freezing plus immutable serving borrows removes construction
aliases without copying releases or authority on every request. Preparation
receipts preserve admission proof while bounding heavyweight dispatch plans;
deterministic reconstruction trades some retry work for lower stream retention.
Shared attempt evidence and structured task joining preserve each operation's
own acceptance, retry and terminal rules. A universal workflow or job model
would obscure those differences and has no demonstrated requirement.

Promotion now calls cohesive provider transaction operations instead of owning
another implementation of credential/certification transitions. Installation
serialization remains the shared authorization/publication boundary. Console
drafts and SDK/CLI callers own logical mutation identity because a transport
helper cannot decide whether an ambiguous retry represents the same intent.
The management SDK and CLI therefore require an explicit idempotency key for
operations that declare one. This is an intentional 0.x consumer change; the
provider protocols, official SDK behavior and plugin ABI are unchanged. CLI saved
configuration plans now use format 2 with a local whole-intent consistency digest.
Regenerate earlier plan artifacts. The digest preserves opaque native member
order and numeric lexemes, accepts indentation changes, and grants no authority;
the server retains authorization, exact replay and stale-destination decisions.

Apply forward-only migrations 0060, 0061 and 0062 in sequence using the ordinary
single-version upgrade procedure. Migration 0060 derives indexed pending
subject and recorded-owner projections from the original accounting template;
it creates neither a second event owner nor a guessed provisional charge. Only
Response, strict Response and Interaction resources own this namespace; opaque
native fields on other kinds create no financial projection or expiry. It also
sets previously expiry-less accounting owners to creation plus 24 hours.
Older mappings can become immediately eligible for conservative uncertain
settlement; expiry does not fabricate provider usage.

Migration 0061 retires deleted budget groups as hidden, policy-free identities,
preserving the original financial UUID. Previously orphaned pending references
are backfilled as retired shells with unknown creator/project left null before
the pending-group foreign key is installed. Active groups still require known
creator and policy. Reusing a retired name creates a new UUID. Known historical
creator/project foreign keys remain retained provenance and can constrain their
later deletion; erasing or guessing that authority is not part of retirement.

Migration 0062 scopes the retained native namespace to kind, API key, provider
and revision, route slug and revision, selected route-target UUID, slot,
immutable credential-version row UUID and native ID. The target is not a
provider-model UUID; the full historical pair binds provider and model. Its
nullable scalar backfills only one valid historical model/slot/target match;
unknown or ambiguous sources stay null and refuse unprovable dispatch. Existing
local IDs and ciphertexts are preserved. The index avoids mutable metadata
expressions and unbounded native model strings, retaining HOT eligibility for
accounting/HTTP updates. Explicit management and native DTOs do not expose the
internal target. Typed identity propagates through all five insert and seven
lookup producers, including Batch output and unbound previous-ID echo.
Deferred HTTP finalization requires no new outbox or schema migration.
No toolchain, dependency, configuration version or plugin ABI pin changed.


## Integrated gate results

Final normal whole-source qualification PASSED. The implementation freeze began
on 2026-10-12 at 02:40:54 UTC: 2,605 source entries, 62 migration checksums and
manifest SHA `a69d7962cd5299879e2168900aad7fa63964b83a41f566a4df26b0916f3f7aa1`.
Independent audits verified unchanged bytes, modes, symlinks, path inventory,
four generated outputs, baseline HEAD and the empty actual index through the
complete run and cleanup. Closing documentation changes are confined to this
ledger and two CLI examples that now supply their required idempotency keys.
Qualified production, test, migration and generated files remain unchanged.

| Gate | Final independently verified evidence |
| --- | --- |
| `make check GO_TEST_ARGS='-race -count=1'` | PASS, 02:40:54–02:44:57 UTC: formatting/vet, 85 tested Go packages (80 root plus five benchmark-harness), lint/types, 147 console files / 1,228 tests and 91 script tests; no failures; console/script skip, cancellation and todo counters zero |
| Contract reproducibility | PASS, 02:45:18–02:45:23: isolated expected-generated index for exactly four outputs, including ignored console outputs; actual index unchanged |
| Full console qualification | PASS: 1,228 unit/component tests; 37 normal feature browser tests (34 packaged, two Vite, one Vite-MFA); six fresh hosted journeys plus the exact restored journey. The fresh restored-only test intentionally phase-skipped and then passed in the restored installation |
| Existing benchmark gate | PASS, 03:19:51–03:32:23: exact initial commit `e5a87b859fe25aa7b3287111899a7f4074d1ada5`, 93 shared / 26 added / zero removed, six alternating 200 ms samples, unchanged 10% policy and default exclusions. The sole initial 10,000-connection flag cleared ordinary 12-sample-per-side confirmation: 32.91→32.92 ns, p=.809, zero bytes/allocations |
| Supplementary measurements | PASS, 03:33:08–03:34:38: identical-fixture, function-aligned strict/Responses comparisons, six names × six samples per side, no flags. Identity eight cases × six and usage four × six are head-only measurements, without original-baseline or production-throughput claims |
| Normal complete `make integration` | PASS, 03:37:51–04:54:51, exit 0 / 4,620s. All 719 compiled public names ran/passed exactly once in disjoint 61/170/166/322 cohorts; main CodeCLI, eight internal packages, trusted extension, three official SDK surfaces and all ten pinned client/framework suites passed; every normal phase ran |
| Release build | PASS, 02:45:31–02:45:57: release binary and static console; binary version 0.1.0 verified |
| Helm/schema/version checks | PASS: version/release/binary guards 02:46:02–02:46:04 and deployment guards 02:46:08–02:46:15; default/production Helm, schema/confinement and restricted egress. Go 1.27.2 / Node 26.11.1 / pnpm 11.24.0 pins unchanged |

The normal integration's Go portions passed 841 top-level and 1,975 nested tests
with no failures, skips, races or panics. Nine Node summaries passed 363 tests
with no failures, skips, cancellations or todos. Native Anthropic continuation
produced 19 events, two tools and two verified dispatches. Public cohort durations
were 1,078.933 / 709.250 / 858.441 / 1,289.611s. Hosted fresh tests passed six in
1.3 minutes; the same restored-only journey passed in 7.5s, with historical
request count greater than zero and increased from 6 to 9 after inference.
Independent cleanup found no owned process identities, process groups, watcher,
Compose containers/volumes/networks, main or browser restore containers, TLS,
scratch or backup storage. Browser name/ID and backup ownership were captured;
successful cleanup could not pass with missing browser evidence.

Unrelated work in another workspace interrupted earlier benchmark launches.
Those datasets were discarded and retained separately. A reviewed `/tmp`-only
collector then buffered original invocations and could retry a slot only for
prospectively observed external CPU overlap, never for a numerical result.
Its 20 fake controls covered exact bytes, argv, environment, working directory,
umask, failure status, PID reuse, detached children, functional failures and
bounded lock/cancellation/cleanup. The repository gate, real pinned Go, filters,
arguments, sample order/count, threshold and ordinary confirmation were unchanged.
Final collection accepted 74 quiet invocations with zero rejected attempts:
two original precompiles and 72 measurements. Both sides used the same 50 ms
observer and 0.5s cooldown. These are paired local measurements with monitoring
overhead, not production throughput. Earlier measurements of the same production change retained
a real below-threshold malformed-authentication cost (69.69→74.41 ns, +6.77%,
p=.002, zero bytes/allocations); correctness costs and memory benefits above
remain explicit tradeoffs.

Historical attempts remain separate evidence. The earlier 00:42:05 freeze passed
its non-integration gates, but normal integration at 01:04:25–02:14:14 exited 2
in five video fixtures and one Bedrock supply fixture after 719 public passes.
It did not reach extension/SDK/client/browser phases. The four-file fixture
repair preserved constructor freezing and native terminal obligations: media
setup now persists and publishes a fresh release while retaining historical
pins (five top-level / 18 nested race passes, 15.038s), and Converse emits
`messageStop` before metadata (six top-level / 33 nested race passes, 1.642s).
Production, migrations and generated outputs were unchanged. The separately
audited 668s downstream preflight omitted exactly four public commands and the
already-completed main CodeCLI command; it was component evidence only. The
final normal run above repeated every phase without omissions.

Earlier integrated review also repaired initial grant publication, original
financial-observation assertions, JSON Content-Type, exact consumed MFA replay
and Valkey minute boundaries. Strict refusals, one-use replay and conservation
assertions were preserved. No required gate remains unrun and no environmental
blocker remains. All 32 evidenced material findings are closed with implemented
repairs, focused controls, independent review and the final normal qualification.
The documented retained limitations and tradeoffs remain deliberate;
this does not establish universal absence of design debt, live-provider
qualification or external exactly-once effects.
