# M9: API surface completion

| Status | Depends on | Unlocks |
| --- | --- | --- |
| In progress | [M2](m02-provider-catalog.md); M4 authorization, M6 pricing and M7 inspection contracts for the relevant slices | [M10](m10-agent-gateway.md) retrieval tools; broader official-SDK coverage |

OLP already serves generation, counting, embeddings, rerank, moderation, media,
files, batches, realtime and retained generation resources in the
[compatibility matrix](../compatibility.md). M9 adds the endpoint families below.
It does not turn a connector registration into support for every API that vendor
exposes. New operations require a versioned contract, exact-tuple certification,
authorization, accounting and qualification before publication.

Serve official shapes natively; translate between declared shapes only on
transformed routes. Vendor operations without a modeled contract use the
separate governed pass-through route kind. The scope below is fixed to the
roadmap's pinned reference baseline. New upstream APIs require a reviewed scope
change, not an automatic expansion of this milestone.

## Outcome and present state

- A finite endpoint/provider ledger declares supported methods, schemas,
  streaming modes and explicit exclusions for the selected SDK versions.
- Files and batches work on the named providers, with one pinned target per job
  and idempotent settlement of each result's observed usage.
- Fine-tuning, retrieval and retained-resource lifecycles preserve ownership and
  historical provider/credential affinity.
- Governed realtime supports revocation and metering; pass-through has explicit
  limits and makes no modeled-fidelity claim.

M9 is **In progress**, not implemented. The endpoint ledger, legacy completions,
native Decisions and a restricted OpenAI-to-System-One mapping now have focused
local protocol/gateway, route-inspection and pinned SDK evidence. Completions
also cover M7 input/output and stream inspection, payload capture, health
probes, exact usage totals and cache-write usage. Decisions refuse token caps and
any USD caps that are not caller-paid exempt, both before dispatch and in route
inspection. System-One-to-OpenAI translation
is deliberately unavailable: the frozen System One response has no refusal
variant, and incompatible mappings must remain unavailable.

The M9.7 OCR and search slice adds the `mistral-ocr`, `perplexity-search`,
`tavily-search`, `exa-search` and `parallel-search` codecs with native
`/v1/ocr` and `/v1/search` endpoints, transformed search mappings, and
gateway, fixture, SDK, console and Chromium evidence. The additional
retained-resource families, batch-settlement worker, expanded realtime and
governed pass-through remain unimplemented. Existing
OpenAI/Azure files and batches are building blocks, not M9 completion evidence.
See the local evidence below for the exact boundary.

## Baseline

Planning-time comparison; current behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| Capability | Existing OLP contract | M9 addition |
| --- | --- | --- |
| Files and batches | OpenAI/Azure with gateway-owned IDs and pinned provider/slot/credential | Named provider expansion, managed translation and per-result settlement |
| Retained generation | Existing Responses/interaction/continuation contracts | Conversations, compaction and additional resource families |
| Realtime | OpenAI/Azure WebSocket and Gemini Live paths | Governed WebRTC plus the named duplex providers |
| Unary operations | Versioned operation/dialect registry, e.g. native TEI and Cohere | Legacy completions, OCR, search, retrieval and Decisions |
| Arbitrary vendor API | No general pass-through route | Explicit path/method allowlist on a distinct, uncertified route kind |

## Binding contracts across all slices

**Endpoint ledger.** The first implementation change adds a machine-readable
ledger under `tests/clients/` recording method/path, source dialect/version,
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
submission and result as duplicate inference. M6 owns pricing dimensions; if a
family needs new units, add them there before admitting priced traffic.

**Inspection and caching.** New codecs declare their M7 inspectable input/output
surfaces. Refuse enforcing guardrails with `422 guardrail_surface_unavailable`
where no complete inspection contract exists. Retained files, jobs, retrieval
state, realtime and pass-through do not gain response caching through a new
endpoint registration. Eligible unary inference can use M8 only after explicit
cache identity and output qualification.

## Scope

### M9.1 Legacy completions

`POST /v1/completions`, operation `generation`, serves unary and streaming native
OpenAI text completions. Initial native targets are OpenAI and the reviewed
vLLM, TGI, Fireworks and Together profiles that certify this dialect. An
OpenAI-compatible base URL alone is insufficient certification.

Transformed Chat Completions mapping accepts a single text prompt as one user
message. Refuse prompt arrays, `echo`, `best_of`, `logprobs` and `suffix` before
dispatch unless the selected target's mapping explicitly proves equivalent
semantics. Native `suffix` is admitted only for a certified supporting target.
Fixtures cover finish reasons, token usage, errors and stream termination.

### M9.2 Files and batches across providers

| Target | Planned native family | Translation/storage boundary |
| --- | --- | --- |
| Anthropic | Files and Message Batches, including results | Native `/anthropic/v1/files` and `/anthropic/v1/messages/batches`; pin required beta versions |
| Gemini | Resumable Files upload and batch generation | Native Gemini shapes; upload session URLs are gateway-owned handles, never arbitrary caller destinations |
| Groq, Together, Fireworks, Mistral | Their documented OpenAI-compatible files/batches | Each profile certifies its own accepted endpoints and lifecycle; resemblance to OpenAI does not enable missing methods |
| Vertex AI | Batch prediction | Operator-owned Cloud Storage input/output prefixes only |
| Bedrock | Batch inference | Operator-owned S3 input/output prefixes only |

Cloud storage references are restricted to configured account/project, region,
bucket and prefix, with least-privilege workload credentials. No gateway-managed
bucket or durable content staging is created. Reading results uses the same
reviewed credential/egress contract; redirects and returned URLs cannot expand
it. Resumable upload sessions bind owner, target, expected length and expiry;
chunks enforce monotonic offsets and total-size bounds, and cannot be replayed
against another session.

**Managed batches.** On transformed routes, accept OpenAI-format input and map
supported generation/embedding lines into one target's native batch format.
Every line uses the selected route/operation; reject mixed routes or operations,
duplicate `custom_id`, unsupported fields and incompatible referenced files.
Validate the entire bounded input before submission, retaining only the
submission manifest, line identifiers/digests and metering metadata durably.
Return results in the source format using `custom_id`; upstream completion order
does not change line identity. Unsupported line mappings fail the whole batch
before provider work, not after a paid partial submission.

**Settlement.** `batch_settlement` is a supervised, checkpointed worker using
PostgreSQL leases, bounded polling/backoff and restart-safe cursors. Reserve the
bounded estimated cost before submission using pinned output limits; refuse a
budget-constrained job when its maximum exposure cannot be bounded. Keep the
reservation until terminal settlement or explicit uncertain reconciliation,
not an ordinary HTTP lease timeout.

For each terminal result, use a unique ledger identity `(resource_id, custom_id,
result_kind)` and commit observed usage, pinned batch price/rate-card calculation
and reservation release atomically. Duplicate pages, workers and restarts are
idempotent. Conflicting duplicate results stop settlement with visible
uncertainty. Failed/cancelled batches still settle reported paid partial
results. Missing lines/usage stay billing-uncertain; never invent a zero-cost
result. Cost deduplication survives resource deletion and normal resource
expiry for the accounting retention period. Re-reading a result file does not
charge inference again.

**M9.2 implementation decisions.** These replace the planned bounded-cost
reservation and fix the settlement boundary:

- Batch creation refuses token and non-exempt cost budgets with
  `422 unbounded_work_budget`; this slice makes no durable reservation.
  Unsettled settlements project pending budget subjects, so budgets enabled
  after acceptance fail closed with `cost_accounting_incomplete` until closure.
  Durable reservations need PostgreSQL-backed provisional debits across every
  budget-owner kind, deferred to the shared provisional ledger M10 also requires;
  this replaces the planning text's reservation requirement.
- Fireworks is excluded: it has no OpenAI-compatible files/batches API, and
  its dataset batch results use provider-returned signed URLs forbidden by the
  egress contract.
- Managed OpenAI-format batches are limited to Gemini, Mistral and Together.
  Anthropic, Vertex and Bedrock remain native-only because managed requests
  would require durable content staging or gateway writes to operator storage.
- Non-OpenAI/Azure families are transformed-only. Strict routes refuse them
  before dispatch with `422 target_capability`.
- The ledger identity is `(resource_id, HMAC(custom_id), result_kind)`.
  Per-result usage events use deterministic IDs and `Batch:true`, so
  `batch_multiplier` applies; a missing multiplier remains unpriced. Missing
  lines create gap events, and a conflict visibly stops settlement.
- Persist submission intent before dispatch; identical retries during
  `submitting` or `submission_unknown` return `409`. Reconcile only from
  provider evidence: list-match by the gateway-owned input file, Gemini's
  `file_name`, Vertex's `olp_submission` label, or Bedrock's
  `clientRequestToken`. Anthropic and Gemini inline submissions remain unknown
  until `close_after`, then become uncertain.

### M9.3 Fine-tuning

Add operation `fine_tuning` with the OpenAI jobs family first: create, list,
retrieve, events, checkpoints and cancel. Azure follows only for methods in its
pinned native contract; Vertex tuning uses its native dialect. Pause/resume are
profile capabilities, never emulated by cancel/recreate. Unsupported methods
remain explicit exclusions in the ledger.

Training/validation files reuse M9.2 ownership and affinity. Job reservations,
ambiguous submission and terminal settlement use the same durable machinery,
with training units defined in M6 before serving. A completed job contributes a
declared candidate model to its owner's connection discovery. It becomes
servable only after exact-tuple certification and publication; job completion
never mutates a published route or grants model access to another project.

### M9.4 Vector stores and retrieval

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
score meaning without inventing cross-vendor calibrated scores. M10 consumes
this certified `retrieval` operation rather than accessing indexes directly.

### M9.5 Responses completion

Add OpenAI Conversations create/list/retrieve/update/delete and item lifecycle
methods defined by the pinned contract, all under `allow_provider_state` and
owner-scoped gateway IDs. Combining conversation, file, vector-store or prior
response references requires the same owner and compatible target. Never create
a local history copy or translate a conversation to a different provider.

`POST /v1/responses/compact` is native-only on OpenAI, then Azure if its profile
supports the same contract. It is priced as its own observed inference call and
may return opaque provider state. It cannot be silently replaced with a summary
prompt. Inspectable-surface requirements still apply to compaction input/output.

### M9.6 Realtime expansion

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
Opaque media is not represented as M7 text inspection.

### M9.7 OCR and search

| Operation and source endpoint | Native target | Transformed targets |
| --- | --- | --- |
| `ocr`, `POST /v1/ocr` using Mistral OCR shape | Mistral | Azure AI Document Intelligence; Vertex AI only for an explicitly selected document-processing profile |
| `search`, `POST /v1/search` using Perplexity Search shape | Perplexity | Tavily, Exa, Brave Search, Google Programmable Search, Parallel |

Certify mappings for pagination, filters, citations, time ranges and result
limits individually. A provider field without equivalent semantics returns 422.
OCR documents arrive inline or as provider-fetched URLs; the gateway never
becomes a general URL fetcher. URL submission is an explicit content-egress
choice, and gateway egress policy does not imply control of provider-side URL
fetching. Retrieval of cloud-batch outputs remains the distinct approved-storage
path in M9.2. Search uses priced request units; OCR needs its page/document units
in M6, not guessed token usage. Native vendor shapes can also be registered
under `/native/{dialect}` with the same operation contract.

**M9.7 implementation decisions.** The slice is implemented with these
binding decisions:

- `/v1/search` carries no model field; its route arrives in the required
  `X-OLP-Route` header and a missing header is refused
  `400 missing_required_parameter` before dispatch. `/v1/ocr` binds its
  route from the request body's `model`. Native paths stay
  `/native/{dialect}/models/{route}`.
- Pre-dispatch field and mapping refusals return HTTP 400
  (`unsupported_parameter`/`policy_conflict`) under the existing unary
  contract rather than the planning text's 422. OCR follows the Decisions
  unbounded-work-budget rules: token and non-exempt cost caps refuse
  `422 unbounded_work_budget`, waived only for caller-paid cost-exempt
  routes.
- The Perplexity surface is frozen to the reviewed ApiSearchRequest; the
  Tavily, Exa and Parallel mappings send only `query` and `max_results`
  (default 10), admit `search_type` absent or `web`, refuse token, locale
  and freshness controls, and map domain filters per target (Exa expands
  root domains with `*.d`; Parallel accepts `.tld` and domain/path entries
  verbatim; Tavily carries none). Projections mint `search_<hex>` ids,
  never reuse upstream ids, and inspect results fail closed: unrecognized
  fields pass only when null, number, boolean or empty containers.
- OCR documents are `https:`/`data:` `document_url`/`image_url` values only;
  `usage_info.pages_processed` meters as media units. Search carries no
  usage and charges the target's `per_request` price.
- Google Programmable Search is excluded: the provider discontinued new
  Custom Search JSON API enrollment and the API sunsets on 2027-01-01.
  Brave (GET-query transport), Azure Document Intelligence (bounded
  202/poll transport) and Vertex document processing (GCP-authenticated
  profile) stay pending in the ledger.

### M9.8 Provider-retained resource families

| Family / operation | Initial native scope | Boundary |
| --- | --- | --- |
| Containers / `container` | OpenAI, then Azure if supported: containers and container files | Lifecycle and file access only; no gateway shell execution |
| Evals / `eval` | OpenAI eval definitions and runs | Pin data-source resources; reserve and settle run costs |
| Skills / `skill` | Anthropic skills and version lifecycle | Files/versions are provider-retained; no local arbitrary-code execution |
| Context caches / `context_cache` | Gemini `cachedContents` lifecycle | Owner-bound IDs, expiry and explicit storage pricing |
| Managed agents / `agent_resource` | Gemini native agent resource lifecycle | Native resource management; A2A/tool execution belongs to M10 |

Each entry gets only its documented pinned methods, fields and versions.
Unsupported regional/profile methods stay excluded; a generic `/native` prefix
does not waive authorization, retention, accounting or ID rewriting. Async
runs reuse M9.2 submission/settlement infrastructure. Periodic storage charges
need an explicit M6 usage unit and reconciliation contract before enablement.

### M9.9 Governed pass-through

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

### M9.10 Decisions API

Add operation `decision`, unary-only, with `/v1/decisions` in the
[OpenAI Decisions format](https://developers.openai.com/api/docs/guides/decisions)
and `/v1/systemone` in the System One format. Native initial targets are OpenAI
for the former and TypeSafe Jev for the latter; transformed routes map between
these two declared shapes. Other reference providers require individual M2
profile/certification entries and do not inherit support from registration.

The first contract covers text input and `predicate`/`noul`, `choice` and `score`
questions. Preserve question identity/order, arbitrary supported choice values,
probabilities, score scale and refusals; never fabricate probabilities with a
chat prompt. Refuse image/box questions, streaming and unmappable state or
provider-only fields in this slice. These are explicit exclusions, not silent
parameter dropping. The ledger freezes schema bounds and fixtures from the
pinned reference/SDK version; newer fields on live documentation do not expand
that contract. Apply ordinary attempt pricing, usage completeness, M7 inspection
and route authorization.

## Delivery order and ownership

| Order | Owner / slice | Required evidence before the next dependent slice |
| --- | --- | --- |
| 1 | Gateway/SDK maintainers: endpoint ledger, operation authorization, source/destination codecs; M9.1 and M9.10 | Locked SDK fixtures, native byte-preservation and explicit transformed refusals |
| 2 | Resource/accounting maintainers: shared ID/affinity, durable intent, reservation and settlement; M9.2 | Concurrent/restart/crash tests and metadata-only storage inspection |
| 3 | Resource/connector maintainers: M9.3–M9.5 and M9.8 | Lifecycle/nested-ID isolation; per-provider method matrix and pricing units |
| 4 | Connector maintainers: M9.7 and query-only M9.4 bindings | Filter/citation fidelity, egress and paid-unit fixtures |
| 5 | Realtime maintainers: M9.6 | Browser and server-control fault tests with live-provider close/usage evidence |
| 6 | Routing/security maintainers: M9.9; release maintainers: integrated qualification | Path normalization corpus, configuration refusals, complete capability and exclusion ledger |

Independent codecs may proceed in parallel after step 1. Retained resources do
not ship before step 2; M10 retrieval integration waits for the certified query
contract in step 4. Each slice updates management/configuration schemas, SDKs,
simulation/capabilities, console controls and operator runbooks in the same
change. No dates or individuals are assumed; owning maintainers sign off the
listed evidence. New families remain disabled until their individual gates
pass, so rollback disables publication without deleting settlement state.

## Change map

| Change | Start here |
| --- | --- |
| Endpoints, native and translated codecs | `internal/gateway/`, `internal/protocols/`, `internal/operations/` |
| Operation authorization, pricing and capabilities | `internal/access/`, `internal/usage/`, `internal/connectors/` |
| Durable resources and settlement | `internal/resources/`, `internal/durablecontract/`, `internal/process/workers.go` |
| Realtime control | `internal/gateway/realtime.go`, `internal/realtimecontract/` |
| Pass-through route kind and validation | `internal/routing/`, `internal/routes/`, `internal/gateway/` |
| SDK and lifecycle qualification | `tests/clients/`, `tests/`, generated contracts and console resource views |

## Exit criteria

- [ ] The ledger covers every in-scope method/provider tuple and explicit
      exclusion; pinned official SDK fixtures pass, including pagination,
      errors, nested IDs and unary/streaming modes where defined.
- [ ] New operations enforce key scope, route permission, project ownership and
      provider-state policy at create and follow-up; foreign IDs/cursors produce
      the same response as missing ones with no provider dispatch.
- [ ] Native codecs preserve qualified payloads; transformed codecs either
      preserve all declared semantics or refuse before paid work. M7 rejects
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
      recorded opt-in live qualification. M1 regression/performance gates pass
      for the affected unary, streaming and retained-work paths.
- [ ] Compatibility, capabilities and parity rows reflect the qualified tuples
      and explicit exclusions. Unimplemented provider breadth remains `Partial`
      or `Gap`; no exclusion or connector registration is relabeled as parity.

## Local implementation evidence

The [machine-readable ledger](../../tests/clients/m9-endpoints.json) separates
implemented, excluded, baseline-only and pending tuples. It must not be used to
advertise pending methods. SDK locks were not changed.

| Slice | Local evidence | Remaining acceptance |
| --- | --- | --- |
| M9.1 | `internal/protocols/completions_test.go`, `internal/gateway/completions_test.go`, `internal/gateway/completions_probe_test.go`, `internal/connectors/completions_test.go`, `internal/interaction/completions_test.go`, `internal/routes/completions_decisions_test.go`, capture tests in `internal/protocols/capture*_test.go` and `internal/export/capture_redaction_test.go`, token exposure test, `tests/clients/m9sdk/`; TGI profile, stream guardrails, capture, probes, usage totals and cache-write usage | Console/browser qualification; each provider's live qualification; release performance gate |
| M9.10 | `internal/operations/decision/decision_test.go`, `internal/gateway/decisions_test.go`, `internal/operationplan/decision_test.go`, `internal/routes/completions_decisions_test.go`, `internal/routes/inspect_key_test.go`, `tests/clients/m9sdk/`; native and one-way mapped questions, typed labels, identity, usage, pricing, caller-paid budgets, inspection diagnostics and explicit refusals | Console/browser and configuration-promotion integration; per-provider live qualification |
| M9.7 | `internal/operations/ocr/` and `internal/operations/search/` codec/mapping tests, `internal/gateway/ocr_search_test.go`, `internal/operationplan/search_test.go`, `internal/routes/ocr_search_test.go`, `tests/clients/m9sdk/ocr_search_test.go`, `tests/fixtures/vendors/{mistral,perplexity,tavily,exa,parallel}/`, `tests/clientfixture/scripted/ocr_search.go`, console `nativeOperation.test.ts` and `tests/gateway/ocr-search.spec.ts`; strict and transformed dispatch, projected ids, fail-closed inspection, refusal paths and credential isolation | Per-provider live qualification, release performance gate and full `make integration`; the provider wizard cannot select a non-default operation profile (e.g. mistral-ocr, openai-decisions), so such providers are created through the management API or configuration apply |
| M9.2–M9.9 | Ledger records planned scope only | Implementation and all slice-specific acceptance, including durable settlement and provider control |

Focused SDK selection: `CLIENTS=m9-go tests/clients/run.sh`. This runs the locked
OpenAI Go SDK against the actual gateway and a scripted upstream. It exercises
unary and streaming completions, native Decisions, OpenAI-to-System-One
translation, incompatible-mapping refusal, and credential-safe dispatch. The
same selection drives raw-HTTP OCR and search coverage: native Mistral OCR,
native Perplexity search, the three transformed search routes, refusal
before dispatch, the required `X-OLP-Route` header, and credential isolation.

Console selection:
`pnpm --dir console test src/lib/features/inference/playground/PlaygroundPage.svelte.test.ts src/lib/features/inference/playground/nativeOperation.test.ts`.
Dependencies must already be installed. Chromium journey coverage lives in
`console/tests/gateway/ocr-search.spec.ts` (providers, routes and playground
driving against `mock-ocr-search.mjs`) alongside the existing route journey.
The dedicated M9 resource/accounting integration selection does not yet exist
and remains required; no exit criterion above is marked complete on the strength
of the implemented slices.
