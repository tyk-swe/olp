# API surface completion plan

Status: **In progress**. [Compatibility](../../use/compatibility.md) owns current
endpoint support and refusals. The [endpoint ledger](../../../tests/clients/m9-endpoints.json)
records implemented, excluded, baseline-only and pending tuples. Legacy
completions, text Decisions, the named file/batch families and the initial
OCR/search slice have local evidence; remaining qualification is in
[Qualification](../qualification.md#api-surfaces). This plan describes unfinished
scope against the [frozen reference](../../roadmap.md).

## Shared retained-resource dependency

The current batch implementation refuses token and non-exempt cost budgets with
`422 unbounded_work_budget`; it makes no durable reservation. Pending subjects
make budgets enabled after acceptance fail closed with `cost_accounting_incomplete`
until settlement. New bounded asynchronous families require a PostgreSQL-backed
provisional-debit ledger across every budget owner, shared with the
[agent plan](agent-gateway.md#shared-implementation-contract). Ordinary leases
must not release unresolved exposure across restart or retention.

Retain submission intent before dispatch, immutable target/slot/credential and
price affinity, provider-evidence reconciliation of ambiguous creates, and
idempotent per-result settlement. Do not widen managed batches beyond Gemini,
Mistral and Together without a reviewed storage/translation contract. Anthropic,
Vertex and Bedrock are native-only; Fireworks is declined because its result
URLs violate the destination contract. Current non-OpenAI/Azure file/batch
families are transformed-only. See the canonical
[file and batch contract](../../use/compatibility.md#files-and-batches).

## Binding contracts across all slices

**Endpoint ledger.** Extend the existing machine-readable
ledger under `tests/clients/`, recording method/path, source dialect/version,
operation, provider profile/version, mode, SDK/version and expected refusal for
unsupported tuples. Use the repository's locked SDK versions, updating lockfiles
only in a separate reviewed compatibility change. There is no wildcard promise
that every present or future SDK method works. Beta APIs need an explicit pinned
beta header/version and fixture; unlisted methods remain unavailable.

**Authorization and ownership.** Calls use the existing `inference` key scope
plus an allowed published route and operation. Model discovery retains
`models_read`. New retained-content families additionally require
`allow_provider_state`; this does not retroactively alter existing file/batch
permissions. Creation without a model selector requires `X-OLP-Route`.
Follow-ups derive their route from the gateway resource ID and recheck current
key/route authority. A foreign, expired or deleted ID is indistinguishable from
missing. Nested IDs, result-file IDs and pagination cursors get the same checks;
provider IDs never become caller-authorized handles.

**Affinity and lifecycle.** Creation pins the release, route/provider revisions,
selected target/model, slot, credential version, dialect contract and price/rate
card revisions. One resource or batch has one target; no splitting, migration or
fallback after creation. Related resources must share owner and compatible
provider affinity before dispatch. Revoked credentials return
`provider_resource_credential_unavailable`; replacement credentials are not
substituted. Current revocation still denies user access to historical resources.
Workers may reconcile already accepted costs after a key is revoked, but may
not initiate new model work on its behalf.

**Content and identity.** Reuse the existing strict/transformed durable-resource
contracts. Persist only required identifiers, lifecycle/accounting metadata and
explicitly permitted sealed native envelopes. Do not copy file bytes, training
sets, batch lines, retrieval documents, SDP or model outputs into PostgreSQL,
logs or traces. Upload/translation buffers use the existing bounded spool
mechanism and are deleted on success, error, cancellation and process recovery.
New resource kinds define identifier rewriting for every nested response and
error field; unrecognized ID-bearing fields fail contract qualification rather
than leaking upstream identifiers.

**Retries and errors.** Read-only polling may retry within deadline/backoff.
Mutating operations retry only with a provider idempotency contract and a stable
submission identity. Persist submission intent before dispatch. An ambiguous
create timeout remains `submission_unknown` until reconciled; never blindly
create a second paid job. Pre-dispatch schema/affinity errors use a typed 4xx;
unsupported route/surface combinations use the existing 422 incompatibility
contract; unknown paths remain unavailable. Provider errors preserve the source
shape only after credential and identifier sanitization. No semantic field may
be silently dropped by a transformed codec.

**Accounting.** Charge every observed paid attempt once, including failed and
cancelled work. Never turn missing usage or a missing price into zero cost.
Control-plane polling and creation are request-accounted; apply a provider
`per_request` charge only when that action is explicitly priced. Delayed model
work uses a durable reservation and settlement record instead of charging its
submission and result as duplicate inference. Pricing owns usage dimensions; if a
family needs new units, add them there before admitting priced traffic.

**Inspection and caching.** New codecs declare their guardrail-inspectable input/output
surfaces. Refuse enforcing guardrails with `422 guardrail_surface_unavailable`
where no complete inspection contract exists. Retained files, jobs, retrieval
state, realtime and pass-through do not gain response caching through a new
endpoint registration. Eligible unary inference needs explicit cache identity and output qualification
before response caching.

## Fine-tuning

Add operation `fine_tuning` with the OpenAI jobs family first: create, list,
retrieve, events, checkpoints and cancel. Azure follows only for methods in its
pinned native contract; Vertex tuning uses its native dialect. Pause/resume are
profile capabilities, never emulated by cancel/recreate. Unsupported methods
remain explicit exclusions in the ledger.

Training/validation files reuse the shared ownership and affinity contracts.
Job reservations,
ambiguous submission and terminal settlement use the same durable machinery,
with training units defined in pricing before serving. A completed job contributes a
declared candidate model to its owner's connection discovery. It becomes
servable only after exact-tuple certification and publication; job completion
never mutates a published route or grants model access to another project.

## Vector stores and retrieval

OpenAI and Azure receive native vector-store create/list/retrieve/update/delete,
file attach/list/retrieve/delete, file-batch create/retrieve/cancel/list and
search methods present in each pinned contract. New operation `retrieval`
authorizes search; `vector_store` authorizes resource lifecycle calls. All
objects are owner-scoped and require provider-state permission.

Native Responses rewrites gateway vector-store IDs inside `file_search` and
result references. A store can attach only files with matching ownership and
provider affinity. Deleting a store follows provider semantics; it does not
implicitly delete separately owned files or erase settlement evidence.

Bedrock Knowledge Bases, Vertex AI RAG Engine and Azure AI Search provide
transformed `/v1/vector_stores/{id}/search` through operator-configured,
project-owned resource bindings. This is query-only: OLP does not create, ingest
or synchronize those indexes. Bindings have gateway IDs and pinned connections;
operators explicitly authorize which keys may search them. Mapping includes
query, result limit, supported filters, document text, scores and citations.
Reject filters/ranking controls without a faithful mapping; retain provider
score meaning without inventing cross-vendor calibrated scores. the agent gateway consumes
this certified `retrieval` operation rather than accessing indexes directly.

## Responses completion

Add OpenAI Conversations create/list/retrieve/update/delete and item lifecycle
methods defined by the pinned contract, all under `allow_provider_state` and
owner-scoped gateway IDs. Combining conversation, file, vector-store or prior
response references requires the same owner and compatible target. Never create
a local history copy or translate a conversation to a different provider.

`POST /v1/responses/compact` is native-only on OpenAI, then Azure if its profile
supports the same contract. It is priced as its own observed inference call and
may return opaque provider state. It cannot be silently replaced with a summary
prompt. Inspectable-surface requirements still apply to compaction input/output.

## Realtime expansion

**Governed WebRTC.** OpenAI is the first qualification target; Azure follows
only after independently proving session control and metering. The selected
flow is server-mediated session creation (`POST /v1/realtime/calls` for an
OpenAI contract), with a pinned route/key/configuration and a server control
channel established before admission completes. This keeps media direct while
allowing OLP to observe usage and close the provider session. The
[official WebRTC guide](https://developers.openai.com/api/docs/guides/realtime-webrtc)
documents server-mediated and ephemeral-token flows; choosing the mediated flow
here is an OLP governance decision.

Do not expose raw provider `client_secrets` as a substitute: expiry restricts
connection initiation and does not by itself prove revocation, one-session
limits or control of an already connected session. That endpoint remains an
explicit first-release exclusion unless the provider can prove those controls.
Reserve request/concurrency and bounded session cost before creating a session;
recheck key validity at most every five seconds, close on revocation/budget or
one-hour session expiry, and release reservations on close. Loss of the control
channel triggers a close attempt and billing-uncertain reconciliation. A failed
close is not evidence that upstream stopped: retain the exposure reservation
until bounded provider-session expiry or reconciled completion, and refuse new
work against that unresolved exposure. Providers without a trustworthy close/control contract
remain unavailable for governed WebRTC.

**Additional duplex profiles.** xAI realtime, Vertex AI Live and Bedrock Nova
Sonic are native-only, separately certified protocols. Transcription-only
sessions are admitted only on profiles that explicitly support them. No
cross-provider reconnect/fallback occurs after session creation. Session and
transport identifiers are owner-scoped; audio, SDP and event payloads are not
logged. Terminal usage is settled once; missing final usage remains uncertain.
Opaque media is not represented as guardrail text inspection.

## Provider-retained resource families

| Family / operation | Initial native scope | Boundary |
| --- | --- | --- |
| Containers / `container` | OpenAI, then Azure if supported: containers and container files | Lifecycle and file access only; no gateway shell execution |
| Evals / `eval` | OpenAI eval definitions and runs | Pin data-source resources; reserve and settle run costs |
| Skills / `skill` | Anthropic skills and version lifecycle | Files/versions are provider-retained; no local arbitrary-code execution |
| Context caches / `context_cache` | Gemini `cachedContents` lifecycle | Owner-bound IDs, expiry and explicit storage pricing |
| Managed agents / `agent_resource` | Gemini native agent resource lifecycle | Native resource management; A2A/tool execution belongs to the agent gateway |

Each entry gets only its documented pinned methods, fields and versions.
Unsupported regional/profile methods stay excluded; a generic `/native` prefix
does not waive authorization, retention, accounting or ID rewriting. Async
runs reuse the shared retained-resource foundation submission/settlement infrastructure. Periodic storage charges
need an explicit pricing usage unit and reconciliation contract before enablement.

## Governed pass-through

A separate route kind binds exactly one connection and explicit method/path
patterns. It is neither strict nor transformed and advertises `uncertified`.
Use `/native/{dialect}/routes/{route}/...` as the gateway namespace; it cannot
shadow a modeled endpoint. The operation is `passthrough`; allowed keys must
name the route, and its configuration requires normal project change authority.

Patterns are anchored path-segment matches; `*` matches one nonempty segment.
Normalize once and reject encoded separators, dot segments, absolute URLs,
fragments and alternate authorities before matching. Query names and forwarded
headers have explicit allowlists; strip hop-by-hop headers and all caller
credentials, then inject only the bound credential. Disable redirects. Request
and response sizes use configured gateway bounds and the route deadline.
Streaming HTTP bodies use those bounds; WebSocket upgrades are excluded.

No fallback, mirroring, translation, response caching or guardrails attach to
this route kind; reject such configurations at plan and publication. Admit a
priced per-request attempt when configured and otherwise mark it unpriced.
Operator acknowledgement is required for opaque provider state or unmetered
work; pass-through does not claim hard token/spend enforcement it cannot
observe. Do not add implicit gateway resource handles or background jobs to an
opaque response. Pass-through is an escape hatch, not qualification of a modeled
provider family.

## Remaining OCR, search and Decisions breadth

Brave search needs GET-query transport; Azure Document Intelligence needs bounded
202/poll transport; Vertex document processing needs its GCP-authenticated profile.
Each remains pending in the endpoint ledger. Certify pagination, filters,
citations, time ranges and result limits independently, refusing unmappable
fields before paid work. Provider-fetched OCR URLs remain an explicit content-egress
choice; the gateway does not become a general URL fetcher. Search prices request
units and OCR prices observed pages/documents, never invented token usage.

Google Programmable Search remains excluded under the recorded enrollment and
2027-01-01 sunset decision. Review any scope change separately. Decisions remains
text-only (predicate/noul, choice, score); image/box questions, streaming and
unmappable state are excluded from the initial contract. System-One-to-OpenAI
translation remains unavailable because the frozen System One response has no
refusal variant. Preserve question order/identity, supported choice values,
probabilities, scales and explicit refusals; chat prompts cannot fabricate these.

## Delivery order and ownership

| Order | Owner | Gate |
| --- | --- | --- |
| 1 | Gateway / SDK maintainers | Extend the endpoint ledger, operation authorization and codecs; native byte preservation, transformed refusals and locked-SDK fixtures |
| 2 | Resource / accounting maintainers | Qualify current files/batches; implement shared provisional debits before enabling bounded paid async work; concurrent/restart/crash and metadata-only storage evidence |
| 3 | Resource / connector maintainers | Fine-tuning, vector stores, Conversations and retained families; nested-ID isolation, provider methods and pricing units |
| 4 | Connector maintainers | Remaining OCR/search and query-only retrieval; filters/citations, egress and paid-unit fixtures |
| 5 | Realtime maintainers | Server-control and browser fault tests plus live close/usage evidence before each new transport |
| 6 | Routing / security / release maintainers | Pass-through path corpus, policy refusals, complete exclusions and integrated candidate qualification |

Independent stateless codecs may proceed after the shared ledger/authorization
gate. The agent gateway consumes certified retrieval, never direct index access.
Update management/configuration schemas, generated clients, simulation,
capabilities, console controls and runbooks in the same slice. Rollback disables
new publication while retaining settlement history. No dates are promised.

## Exit criteria

- [ ] The ledger covers every in-scope method/provider tuple and explicit
      exclusion; pinned official SDK fixtures pass, including pagination,
      errors, nested IDs and unary/streaming modes where defined.
- [ ] New operations enforce key scope, route permission, project ownership and
      provider-state policy at create and follow-up; foreign IDs/cursors produce
      the same response as missing ones with no provider dispatch.
- [ ] Native codecs preserve qualified payloads; transformed codecs either
      preserve all declared semantics or refuse before paid work. Guardrails reject
      unsupported inspection; new endpoints do not accidentally enable caching.
- [ ] Parallel workers, repeated result pages, process crashes before/after
      commits, cancellation and key revocation produce one settlement per paid
      result, bounded reservations and visible uncertainty for incomplete usage.
- [ ] Lost create acknowledgements cannot duplicate jobs; cloud storage,
      resumable sessions, returned URLs and spool cleanup pass isolation/egress
      tests. No content appears in durable diagnostics.
- [ ] Fine-tuned models cannot serve before certification and publication;
      retrieval/retained resource IDs cannot cross owner or provider affinity.
- [ ] WebRTC live evidence proves admission, five-second revocation checks,
      provider close, bounded session lifetime and exactly-once observed usage;
      missing usage/control produces uncertainty and no continued admission.
- [ ] Pass-through normalization, header/query allowlists and redirect refusal
      pass adversarial fixtures; modeled endpoints cannot be shadowed and
      conflicting route policies cannot be published.
- [ ] Each new provider tuple has local contract evidence and separately
      recorded opt-in live qualification. Gateway regression/performance gates pass
      for the affected unary, streaming and retained-work paths.
- [ ] Compatibility, capabilities and roadmap outcomes reflect the qualified tuples
      and explicit exclusions. Unimplemented provider breadth stays explicitly incomplete; neither an
      exclusion nor connector registration establishes support.
