# API surface completion plan

Status: **In progress**. [Compatibility](../../use/compatibility.md) owns current
endpoint support and refusals. The [endpoint ledger](../../../tests/clients/m9-endpoints.json)
records implemented, excluded, baseline-only and pending tuples. Legacy
completions, text Decisions, the named file/batch families and the initial
OCR/search slice have local evidence; remaining qualification is in
[Qualification](../qualification.md#api-surfaces). This plan describes unfinished
scope against the [frozen reference](../../roadmap.md) and owns the API work in
roadmap RM3–RM6. The [delivery checklists](#delivery-order-and-ownership) separate
current qualification, shared implementation and new families.

## Shared retained-resource dependency

The current batch implementation refuses token and non-exempt cost budgets with
`422 unbounded_work_budget`; it makes no durable reservation. Pending subjects
make budgets enabled after acceptance fail closed with `cost_accounting_incomplete`
until settlement. New bounded asynchronous families require a PostgreSQL-backed
provisional-debit ledger across every budget owner, shared with the
[agent plan](agent-gateway.md#shared-implementation-contract). RM4.2–RM4.3 below
own this shared accounting work; agent-specific contracts are RM4.4, not a
second ledger. Ordinary leases must not release unresolved exposure across
restart or retention.

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

The retained implementation exposes the declared OpenAI/Azure create, list,
get, cancel, event and checkpoint methods and Vertex create, list, get and
cancel. Native `trained_tokens` and Vertex billable training tokens settle
terminal jobs, including paid usage from failures and cancellations. Missing
usage preserves liability. Supervised and DPO training are accepted where the
native provider supports them; reinforcement billing units, pause/resume and
Vertex distillation, preference and external evaluation sources are refused.
Vertex supervised datasets must fall under the connection's operator-configured
`options.tuning_dataset_uri_prefixes`; an empty list grants no dataset access.
Vertex creation correlation uses native labels, and terminal intervals use the
authoritative native `endTime`.

Native `/v1/files` uploads for `fine-tune`, `evals`, `assistants` and `user_data`
use the corresponding route operation, a bounded multipart upload and an
`Idempotency-Key`. Result files discovered through an owned tuning job retain
that job's connection and credential authority. Unknown upload acceptance never
authorizes redispatch. These native file uploads have immediate request
accounting; billable vector storage belongs to the store settlement owner.

## Vector stores and retrieval

OpenAI and Azure receive native vector-store create/list/retrieve/update/delete,
file attach/list/retrieve/delete, file-batch create/retrieve/cancel, nested batch-file listing and
search methods present in each pinned contract. New operation `retrieval`
authorizes search; `vector_store` authorizes resource lifecycle calls. All
objects are owner-scoped and require provider-state permission.

Native Responses rewrites gateway vector-store IDs inside `file_search` and
result references. A store can attach only files with matching ownership and
provider affinity. Deleting a store follows provider semantics; it does not
implicitly delete separately owned files or erase settlement evidence.

Nested `/file_batches/{batch_id}/files` lists are implemented. A batch filters
the store attachment collection, so its returned file identifiers and cursors
remain children of the store and work with the store's file methods. Attachment
updates and attachment-content methods outside the declared registry are
explicitly unsupported.

Bedrock Knowledge Bases, Vertex AI RAG Engine and Azure AI Search provide
transformed `/v1/vector_stores/{id}/search` through operator-configured,
project-owned resource bindings. This is query-only: OLP does not create, ingest
or synchronize those indexes. Bindings have gateway IDs and pinned connections;
operators explicitly authorize which keys may search them. Mapping includes
query, result limit, supported filters, document text, scores and citations.
Reject filters/ranking controls without a faithful mapping; retain provider
score meaning without inventing cross-vendor calibrated scores. The agent gateway consumes
this certified `retrieval` operation rather than accessing indexes directly.

The query handler also accepts `POST /v1/retrieval/{binding_id}/query`.
Cloud binding IDs are UUIDs; the vector-store search alias accepts the same
binding UUID. Manage bindings with revisioned `/api/v1/retrieval-bindings`
CRUD and revision reads. Writes require configuration permission and project
change authority, an idempotency key, and an observed ETag for updates or
retirement. The connection identity is derived from the selected target and
persisted with the binding revision. Queries check the explicit key grants,
owning project and current published route, target, model and credential
authority before dispatch.

Use the dedicated `bedrock-knowledge-bases`, `vertex-rag-engine` or
`azure-ai-search` provider profile, with the native Knowledge Base ID, full
RAG corpus name or search index name as the target model. The Azure provider
kind is `azure_ai_search`, using API version `2025-09-01`; its Entra scope is
`https://search.azure.com/.default`. Existing credential pools and egress
policies apply. Metadata certification performs a cost-free native resource
read; live qualification must also establish query permissions.

Queries accept `query` and `max_num_results` from 1 to 100 (default 10).
Bedrock and Azure accept `eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `in` and `nin`
metadata filters with `and`/`or` composition, only for explicitly mapped
logical fields. Azure string values are escaped as OData literals. The pinned
Vertex contract supports `vector_distance_threshold` or
`vector_similarity_threshold`; arbitrary metadata filters are refused.
Ranking models, index mutation and controls without a faithful mapping are
refused before dispatch. Results preserve text, native score precision and
score meaning (`score` or `distance`), citations and attributes. These
bindings charge the immediate `retrieval` request through normal request
accounting. Configuration promotion resolves the destination provider,
route target and exact project-local API-key names; absent or ambiguous
grants block promotion.

Native query contracts are pinned to [Bedrock Retrieve](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Retrieve.html),
[Vertex retrieveContexts](https://cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations/retrieveContexts)
and [Azure document search](https://learn.microsoft.com/en-us/rest/api/searchservice/documents/search-post?view=rest-searchservice-2025-09-01).

## Responses completion

Implement OpenAI Conversations create/retrieve/update/delete and item lifecycle
methods defined by the pinned contract (root listing is unsupported), all under `allow_provider_state` and
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

Implementation reading recorded with RM6.4: reservation and settlement run on
the shared realtime lease and cost-hold path that WebSocket realtime already
uses, with the session record held by the serving gateway process. The
gateway has no `execution_debits` intent wiring today, so this slice does not
claim durable debit-ledger settlement. Leases and cost holds live in the
shared limits store and outlive a gateway process until their one-hour bound,
but a process crash loses the control loop for its live sessions. Those
sessions then end only by provider expiry or peer disconnect, and their
settlement never reaches the usage sink. Wiring WebRTC sessions into durable
debit intents with restart recovery remains follow-up work, and the missing
piece is recorded in the qualification record rather than papered over. The
capability tuple is `realtime` on the `openai` surface in the distinct
`webrtc` mode, compiled at publication like the WebSocket contract. A
certified WebSocket realtime tuple never implies the WebRTC tuple.

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

Containers expose the declared create/list/get/delete and file
create/list/get/delete/content methods. File uploads accept a bounded multipart
file or an owned JSON/multipart `file_id`; downloads return bounded binary bytes. Memory
tiers are pinned at creation. Native lifecycle observations do not create
session charges from elapsed time; verified billing evidence owns those charges.

Evals expose custom definitions, name/metadata updates, runs, cancellation,
deletion and output items. Runs accept owned JSONL files or inline file content.
Every generation and model-based grader names an explicitly authorized model
on the same connection, route and credential slot, with separate pinned pricing.
Model-based runs require the route's `generation` operation and certified
generation capabilities for every sampling and grader model.
Native logs and stored-completions sources, implicit models and testing-criteria
updates are refused. Dynamic item-reference trajectories also require a
per-item resource authority contract and are refused. Terminal per-model usage settles incurred work even when
the run fails or is cancelled; absent model quantities remain unresolved.

Gemini caches persist their native model, immutable token quantity, creation
time and authoritative expiration. PATCH accepts only expiration controls.
Generation binds a gateway cache identifier before unary or streaming dispatch;
raw provider IDs and incompatible model or content transformations are refused.
Verified remote expiry can settle the exact immutable token quantity over its
authoritative lifetime. Early deletion or disappearance cannot establish usage
coverage by itself and retains the outstanding accounting owner.

Native family methods have focused fixture coverage. The endpoint ledger keeps
live qualification required for every provider/profile/model tuple; these local
checks do not establish service, SDK-service or live-provider qualification.
The locked OpenAI SDK v3.78.0 has no Evals client; Evals uses the frozen REST
fixtures documented in `internal/resources/testdata/retained/README.md`.

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

Brave search runs on the reviewed GET-query unary transport, where the
lowered request document becomes bounded query parameters on a bodyless GET.
The same transport shape leaves room for a bounded submit-then-poll shape
beside it, and Azure Document Intelligence still needs that 202/poll
transport, so it remains pending in the endpoint ledger. Vertex document
processing serves through its GCP-authenticated Google Cloud Document AI
profile, with GCS document locations bounded by the connection's configured
document URI prefixes. Certify pagination, filters,
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

The [roadmap](../../roadmap.md#remaining-outcomes) owns cross-plan order and
status. IDs below identify deliverables, not a global serial queue. Preserve the
existing `M9.*` identifiers in the endpoint ledger when adding roadmap references.
Run RM3 qualification independently of new RM4–RM6 implementation. RM4.1 is the
endpoint-ledger/authorization gate; it is distinct from the durable budget ledger
in RM4.2–RM4.3. Stateless work needs the former, not all asynchronous machinery.

### RM3: Implemented API qualification

**State:** Implemented; qualification open. **Accountable:** protocol/resource/SDK
maintainers. **Entry:** current ledger tuples, candidate, disposable services and
locked clients. RM3.1–RM3.4 can run independently; RM3.5 qualifies the resulting
candidate. Keep current managed-batch and text-only limitations throughout.

- [ ] **RM3.1 — Complete legacy-completions acceptance** (protocol/SDK/console):
  run locked SDK fixtures and console/browser journeys, preserving native and
  transformed refusals, inspection and token/usage behavior.
- [ ] **RM3.2 — Complete text-Decisions acceptance** (protocol/console): verify
  browser and configuration-promotion workflows alongside labels/order,
  pricing, caller-paid budgets and inspection fixtures; preserve unsupported
  image/box, streaming and reverse-mapping refusals.
- [ ] **RM3.3 — Complete OCR/search acceptance** (connector/console): execute
  raw-HTTP process and Chromium journeys with the documented route headers,
  credential isolation and management/configuration path for nondefault profiles.
  No locked official SDK exists for these APIs; do not imply SDK coverage.
- [ ] **RM3.4 — Complete files/batches acceptance** (resource/accounting/SDK):
  execute resource/accounting and full batch/provider integration, database-backed
  official SDKs and console/Chromium journeys. Verify uploads, managed/native
  jobs, ambiguous submission, cleanup and content-free settlement; the client
  fixture without a database cannot close this item.
- [ ] **RM3.5 — Qualify current tuples for release** (release/provider): run
  full integration and applicable candidate gates, each claimed live provider
  tuple and release performance. Record separate local/live results using
  [API qualification](../qualification.md#api-surfaces) and update compatibility.

**Exit:** all current in-scope tuples have the evidence required by the
qualification matrix; missing live accounts or hardware remain open gates.

### RM4: Shared execution foundations

**State:** In progress: implementation and focused unit checks exist; service,
crash/recovery and restore qualification remain open.
**Accountable:** gateway/SDK maintainers for RM4.1; resource/accounting maintainers
for RM4.2–RM4.3. **Entry:** the binding ownership, pricing and runtime contracts
above. RM4.1 and RM4.2 may proceed in parallel; RM4.3 verifies RM4.2 before new
bounded asynchronous work is enabled.

Operation declarations use `execution-foundations-1` in the endpoint ledger;
they do not register codecs or enable pending tuples. The `limits` owner holds
the shared PostgreSQL intent/allocation/evidence ledger and nonexpiring,
versioned Valkey projection. `usage.PersistDebitResultTx` commits observed facts
and settlement together, checking dispatch/release/provider/credential/price
affinity. Incomplete receipts retain exposure until verified coverage resolves
them; coverage events contain only new accounting deltas. Recovery reclaims only
undispatched, fenced preparations. Accepted uncertainty never expires or resends.

Focused tests cover declaration isolation, authority, evidence identity,
projection commands and metadata references. Integration scenarios cover
competing dispatchers, projection loss, uncertain acknowledgements, transaction
rollback, duplicate/conflicting evidence, approval fencing, and replacement
restore. These scenarios compile; they have not run locally. The checklist stays
open until the database/Valkey and process acceptance gates pass in CI.

- [ ] **RM4.1 — Extend API contracts per operation:** before each new codec,
  extend the existing endpoint ledger, operation authorization and declared
  inspection surface. Define native byte-preservation, transformed-refusal and
  locked-client cases, including cross-owner/route and nested-ID refusals; run
  the existing authorization/refusal checks. Execute the new codec cases in its
  RM5/RM6 slice. Track this gate per operation so unrelated pending tuples do not
  block a ready family or become supported implicitly.
- [ ] **RM4.2 — Implement durable provisional debits:** persist submission
  intent and finite exposure across all budget owners before dispatch, with
  immutable target/credential and price affinity. Share the reservation and
  idempotent settlement mechanism with agent dispatch; retain unknown outcomes
  across restart/retention and separate bounds from observed charges.
- [ ] **RM4.3 — Verify recovery and settlement:** test concurrent workers,
  crashes before/after dispatch and commit, ambiguous acknowledgements, repeated
  results, cancellation and revocation. Prove no automatic redispatch after
  ambiguous acceptance, no duplicate settlement, no TTL release of unresolved
  exposure, and metadata-only storage.
  Verify reconciliation and backup/restore preserve outstanding debits.

[RM4.4 agent contracts](agent-gateway.md#rm44-agent-contracts) can develop in
parallel and reuse this ledger. They are not prerequisites for API families.
**Exit:** RM4.1 gates each operation; RM4.2–RM4.3 establish safe bounded async
accounting. Existing batch budget refusals remain until the corresponding
implementation and qualification explicitly support changing them.

### RM5: Retained APIs and retrieval

**State:** Done (implementation); service and live qualification remain separate. **Accountable:** resource/connector maintainers, with
accounting and SDK owners. **Entry:** RM4.1 per operation, owner/credential
affinity and defined priced units. Bounded asynchronous jobs/runs additionally
require RM4.2–RM4.3; query-only retrieval does not wait for those jobs.

Paid retained families use durable pre-dispatch settlement owners, immutable
price/connection/credential pins and one atomic provider/operator evidence ledger.
Unbounded work refuses applicable consumption caps; delayed training/evals always
refuse TPM caps. Immediate compaction and retrieval use request accounting.
Zero-cost housekeeping remains available for owner-bound resources.
See [retained operations](../../operate/operations.md#retained-work-and-evidence)
for evidence, recovery and provider-retirement constraints.

- [x] **RM5.1 — Add fine-tuning:** implement the OpenAI jobs family first,
  followed by the pinned Azure and native Vertex methods in the [contract](#fine-tuning).
  Verify file affinity, unknown-create recovery, priced training settlement and
  certification/publication before any resulting model becomes servable.
- [x] **RM5.2 — Add native vector stores:** implement the pinned OpenAI/Azure
  lifecycle, file attachments/batches and search. Test nested IDs, pagination,
  provider-state permissions and Responses `file_search` identifier rewriting.
- [x] **RM5.3 — Add certified query-only retrieval:** implement project-owned
  Bedrock Knowledge Bases, Vertex AI RAG Engine and Azure AI Search bindings.
  Verify filters, limits, text, scores, citations, egress and pricing without
  creating or synchronizing indexes. Retrieval-backed tools use the certified
  `retrieval` operation for their selected RM5.2 or RM5.3 backend; they do not
  wait for every backend or access indexes directly.
- [x] **RM5.4 — Complete Conversations and compaction:** implement pinned
  lifecycle/items and native compact methods. Test cross-resource owner/target
  affinity, inspection and independently priced compaction; do not retain a local
  conversation history or substitute a summary prompt.
- [x] **RM5.5 — Add containers:** implement the declared OpenAI and supported
  Azure lifecycle/file methods; verify owner isolation without gateway shell execution.
- [x] **RM5.6 — Add evals:** implement native OpenAI definitions/runs; test
  data-source affinity, durable run reservations and terminal settlement.
- [x] **RM5.7 — Add skills:** implement native Anthropic skills/versions;
  verify retained file/version ownership without local code execution.
- [x] **RM5.8 — Add context caches:** implement Gemini `cachedContents`;
  verify owner-bound IDs, expiry and explicit storage pricing/reconciliation.
- [x] **RM5.9 — Add managed-agent resources:** Gemini v1beta create/list/get/delete
  with owner-scoped IDs, immutable affinity, submission fences and console
  workflows. The pinned SDK does not expose PATCH. Native environment references
  remain unsupported; resource control refuses configured execution pricing.
  Service integration scenarios compile; execution and live qualification remain
  required. RM8 A2A/tool execution remains separate.

Focused implementation checks pass and generated clients are current.
Database/process and restoration scenarios compile; their execution and the
SDK-service/browser/live gates remain open in the [qualification record](../qualification.md#rm5-retained-apis-and-retrieval).

**Release exit:** each family passes the [shared exit criteria](#exit-criteria), including
local lifecycle/failure evidence and separately recorded live qualification.
The [family contracts](#provider-retained-resource-families) govern methods and
versions; neither implementation nor a generic native prefix widens that scope.

### RM6: Protocol and provider expansion

**State:** Planned remaining breadth. **Accountable:** connector maintainers
for RM6.1–RM6.3, realtime maintainers for RM6.4–RM6.5 and routing/security
maintainers for RM6.6. **Entry:** RM4.1 per operation and its transport, pricing,
egress and inspection contract. These tasks may progress independently;
unresolved realtime exposure additionally requires RM4.2–RM4.3.

- [x] **RM6.1 — Add Brave GET-query transport:** verify pagination, filters,
  citations, time ranges, limits and request pricing; refuse unmappable fields.
- [ ] **RM6.2 — Add Azure Document Intelligence polling:** implement bounded
  202/poll transport; test deadline/cancellation, observed page/document pricing,
  output fidelity and provider-fetched URL egress boundaries.
- [x] **RM6.3 — Add Vertex document processing:** implement its GCP-authenticated
  profile; verify credential isolation, field fidelity, egress and observed units.
- [x] **RM6.4 — Add governed WebRTC:** implement OpenAI server-mediated session
  creation/control first. Pass browser faults and live admission, five-second
  revocation, close/usage, session-expiry and lost-control exposure checks before
  considering Azure's independently qualified contract. Implemented for OpenAI.
  Live admission evidence and the Azure tuple remain open qualification work.
- [x] **RM6.5 — Add native duplex profiles:** independently implement and
  qualify xAI realtime, Vertex AI Live and Bedrock Nova Sonic. Test owner-scoped
  sessions, declared transcription support, cancellation and terminal usage;
  no reconnect/fallback may move a session to another provider.
- [x] **RM6.6 — Add governed pass-through:** implement the separate route kind
  and anchored namespace. Test adversarial path normalization, query/header
  allowlists, redirect/credential isolation, bounds and policy-publication
  refusals; verify per-request pricing or explicit unpriced accounting.

**Exit:** local fixtures and exact live tuples satisfy the [shared exit criteria](#exit-criteria).
Retain all exclusions in the contracts above, including raw realtime client
secrets, pass-through WebSockets and conflicting guardrail/cache policies.

### Completing a slice

Update management/configuration schemas, generated clients, simulation,
capabilities, console controls and runbooks in the same slice. Run its focused
fixtures before service/SDK/browser and live checks; use the applicable
candidate gates before claiming **Shipped**. Record blockers and artifacts under
the [roadmap evidence rules](../../roadmap.md#updating-progress-and-evidence).
Rollback disables new publication while retaining settlement history.
No dates are promised.

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
