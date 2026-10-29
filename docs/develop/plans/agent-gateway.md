# Agent gateway plan

Status: **Planned**. Individual checklists record implemented pieces and the
remaining workflow and release work.
Existing [upstream registration](../../use/mcp-servers.md) and
[management MCP](../../operate/automation.md#management-mcp) have separate contracts.
This plan depends on existing access, accounting, pricing, observability and
guardrail owners; it extends them rather than creating parallel registries.
It owns roadmap RM4.4, RM7 and RM8. The [delivery checklists](#ordered-delivery-and-verification)
name the dependencies and evidence needed for each step.

## Shared implementation contract

These are implementation decisions, not claims about current behavior.

1. **Ownership.** Servers, toolsets, agents and prompts belong to one project.
   Management reads need `read` and project visibility; mutations need
   `configure` and project-change authority. Changes to identity trust additionally
   need installation `access`. Use contract-declared operations, observed ETags,
   explicit Idempotency-Key, transactional reauthorization and metadata audit.
2. **Publication.** Keep editable configuration separate from immutable
   revisions. A toolset pins server revision IDs and digests; a route pins its
   execution policy. One request/run pins the runtime generation, revisions,
   pricing and grant binding. Revocation remains live and overrides those pins.
   No discovery, schema compilation or configuration SQL occurs on ordinary
   inference admission. Retained task and approval lookups are explicit exceptions.
3. **Authority.** Add independent API-key scopes `tools`, `agents` and
   `traces_write`, plus opt-in `delegate`. Empty toolset/agent allowlists grant
   no access. Existing route
   groups keep their model-route meaning; they do not implicitly authorize
   toolsets. Effective rights intersect key, project, published resource and any
   delegation. Recheck before every outbound step and at most every five seconds
   during a long step; stale authority refuses after the existing 60-second
   freshness boundary.
4. **Accounting.** A run is a correlation record; each model/tool/agent dispatch
   has one attempt ID and its own immutable owner, revision, cost/charge basis,
   timing and outcome. Root totals are derived, never charged a second time.
   Reserve each step before dispatch, release undispatched reservations and
   settle from actual evidence. Unknown balances fail closed; unpriced work is
   explicit and never silently treated as free. Trace assertions cannot create
   or alter financial facts.
5. **Retries and uncertainty.** Read/discovery retries are bounded. Never
   automatically retry a tool call or agent send after dispatch might have
   occurred. A caller request ID deduplicates the local dispatch record; it
   cannot promise exactly-once upstream side effects. An ambiguous response is
   `outcome_unknown`, retains its accounting evidence and requires explicit
   caller reconciliation. Restart cannot silently replay it. Before dispatch,
   use the shared [RM4.2–RM4.3 accounting foundation](api-surface.md#rm4-shared-execution-foundations)
   to write a durable provisional-budget ledger entry with dispatch ID, run/step,
   immutable accounting owner/window, cost/charge basis, installation pricing
   currency and declared upper bound. Unknown outcomes retain that debit across
   restart and ordinary retention; no lease or TTL automatically releases it.
   Later settlement or an audited evidence-backed finding that no work was
   accepted resolves the entry idempotently, releases/replaces the bound and
   applies any correction exactly once. Reports separate unresolved bounds from
   observed provider cost; bounds are never presented as invoiced charges.
6. **Privacy and egress.** Arguments, results, rendered prompts, task messages,
   bearer tokens and raw subject claims stay in request memory. Store only
   configuration, scoped identifiers, bounded status/usage and purpose-separated
   digests. Every discovery, OAuth, call, reconnect and cancellation destination
   is operator-declared and egress-validated; refuse redirects and credential
   forwarding to another origin. Tool output cannot authorize a new destination.

## Scope

### MCP gateway

**Transport and certification.** Reuse `internal/mcpservers/` and its existing
`mcp_credential` seal purpose; do not create a second server registry. The first
consumer slice supports Streamable HTTP at MCP `2025-03-26`, `2025-06-18` and
[`2025-11-25`](https://modelcontextprotocol.io/specification/2025-11-25).
Legacy HTTP+SSE follows as an explicitly selected transport. Stdio is limited
to operator-installed executables in the experimental
[unconfined plugin directory](../../use/plugins.md#unconfined-plugins-experimental);
API callers cannot supply a path, command, environment or working directory.
This plan closes only after all three declared transports qualify. The legacy adapter
pins MCP `2024-11-05`; transport choice never silently downgrades a modern session.

Keep existing discovery limits: ten seconds, 16 pages, 128 tools, one MiB of
metadata, 64 KiB per input schema, depth 32 and 4,096 schema nodes. Compile object
schemas offline without network loaders. Runtime sessions certify against the
pinned catalog before becoming usable. A changed catalog leaves the published
definition visible but blocks calls to that server with
`tool_definition_changed` until a reviewed revision is published. Notifications
invalidate the session's certification. This detects definition drift; it does
not attest the remote implementation's behavior between checks.

**Toolsets and sessions.** Publish project toolsets referencing exact server
revisions and selected tool names. Use stable wire aliases
`t_<server UUID without hyphens>_<first 32 hex SHA-256(name)>`; reject a collision
at publication and retain the full name/digest mapping. Display names do not
change aliases. `/mcp` serves Streamable HTTP; `GET /v1/mcp/tools` and
`POST /v1/mcp/tools/call` expose the same authorization and call path as JSON.
Sessions bind installation/project/principal/toolset revisions and expire after
15 idle minutes, with at most 64 sessions per key. Shared session metadata may
live in Valkey; message bodies, credentials and result replay buffers may not.
Session initialization performs discovery before the session can enter the
ordinary call path; published schemas are already compiled. JSON calls use the
same bounded, pre-established upstream session pool.
Unsupported sampling, elicitation, roots and arbitrary resource access are
explicitly refused. Resource/prompt proxying requires a later scoped contract.

**Credentials.** Retain static bearer authentication and add operator-approved
static headers plus OAuth authorization-code/PKCE and device authorization
through the existing grant machinery. Reject hop-by-hop, Host and caller-routing
headers. Bind every per-user grant to server, client, audience, scopes, project
and either key UUID or project end-user digest. Browser enrollment uses the
existing authenticated one-use flow; supplying an end-user label alone cannot
enroll or adopt a grant. Missing, revoked or unrefreshable grants fail the call;
never fall back to a service credential. Refresh is single-flight and does not
hold a database mutation lock. Grant tokens never enter exported configuration.

**Calls.** Validate arguments against the pinned schema, run guardrail argument/result
policies, then call with a 30-second default deadline (120-second hard ceiling),
one MiB request/result bounds and key/server concurrency limits. Server defaults
are 60 calls/minute and eight concurrent calls, adjustable by operators within
installation limits. Oversize or invalid results fail explicitly, never truncate
JSON into an apparent success. Prices use an immutable server/tool `unit_price`
per dispatched call, including upstream failures; explicit zero is a known free
price. Discovery is not a billable tool call. Missing prices produce unpriced
facts and refuse a call when an enforced cost ceiling needs a finite bound.
All monetary bounds use the installation pricing currency; no implicit foreign
exchange conversion is permitted.
Required call IDs bind principal, revision and argument digest for 24 hours.
An exact repeat returns retained status/attempt identity, never re-executes; if
the transient result is gone, return `result_not_retained` instead of pretending
to replay it. A changed body under that ID returns a conflict.

### Gateway-executed tools

`tool_execution: gateway` is opt-in on transformed routes. The caller may narrow
the route's published toolsets. Strict routes never execute tools; native MCP
input passes through only on a certified provider tuple that supports it.
Transformed targets must be certified for model-emitted tool calls; targets
without that capability are ineligible rather than prompted to simulate it.

The gateway advertises pinned schemas, calls the model, serially validates and
executes requested tools, appends results, and repeats. Default bounds are eight
model iterations, 32 tool calls and 120 seconds total; hard ceilings are 32,
128 and 600 seconds. The effective limit is the smallest operator/key/caller
limit. A distinct optional `max_run_cost` bounds the whole run: every next step
must fit the remaining worst-case allowance, using pricing prices in the installation
pricing currency. Existing `max_cost` remains the per-attempt candidate filter
and applies independently to each model step. No unbounded output model or
unpriced tool can satisfy a finite run ceiling. Each model and tool step remains
a separate attempt, including refused or uncertain continuations.

Operators classify tools as read-only or approval-required; upstream annotations
are untrusted hints. Unclassified tools require approval. Guardrail deny rules override
approval. Responses emits `mcp_list_tools`, `mcp_call` and
`mcp_approval_request` where its certified contract supports them. Chat
Completions, Anthropic and Gemini expose only the final answer; when approval
is required they return a typed pause with an OLP continuation token, never a
fabricated native tool success. Intermediate tool output is not streamed on
these surfaces.

An approval expires in five minutes and binds the principal, project, run,
tool/route revisions, argument and transcript digests, deadline and cost
allowance. The client resubmits the transcript and arguments; OLP stores only
the one-use approval metadata under a dedicated digest/signing purpose. Resume
reauthorizes and verifies exact digests; expiry, changed input, replay or revoked
authority refuses. Approval consumes no new iteration allowance and releases
active concurrency while paused. Restart may resume only through that explicit
flow. Response caching is disabled for gateway-executed runs, so cache hits
cannot bypass tool effects, approvals or accounting.

### A2A agent gateway

An agent registration pins its card, endpoint, protocol, credential binding and
declared per-message price/maximum cost. Card fetching uses bounded egress and
requires explicit publication of changes. Support JSON-RPC over HTTP/SSE for
[A2A 0.3.0](https://a2a-protocol.org/v0.3.0/specification/) and
[1.0.0](https://a2a-protocol.org/v1.0.0/specification/); maintain separate schema
adapters and refuse undeclared bindings or versions. The rewritten card at
`/a2a/{agent}` advertises OLP's authentication and only implemented capabilities.
Initial methods cover send/stream, get/cancel and stream resubscription; task
listing is principal-scoped. Push notifications, gRPC and arbitrary artifact
URL fetching are outside this plan and are not advertised.
Agent credentials use a distinct `agent_credential` seal purpose and the same
server-bound OAuth grant lifecycle as MCP. Changing an endpoint requires
explicit credential replacement/removal and recertification; a card cannot
redirect credentials to another advertised interface.

Task mappings retain opaque upstream IDs behind OLP IDs, project, initiating
key/delegation, pinned revision, state and accounting references. Reads/cancels
require that same owner or an explicitly authorized project operator. They
never use a caller-supplied upstream ID. Store no message or artifact bodies;
reads retrieve authorized upstream content through egress policy. A disconnect
does not imply task success or cancellation.

Defaults are 60 sends/minute/key, four concurrent tasks/key, 32 observed
continuations/task and a ten-minute task deadline; operator hard ceilings are
128 continuations and one hour. Bound each stream event to one MiB. Limits
cover gateway-observed sends and OLP model/tool callbacks. OLP cannot count or
terminate hidden iterations inside a remote agent; the console states this
limit. A finite budget requires a declared finite remote-work bound in the
installation pricing currency.
Authenticated callbacks carry a gateway-issued task capability, not a trusted
caller-chosen attribution header; callback attempts are charged once to the
original owner. Remote fees and callback model costs are separate components.
OLP sends the capability to the remote agent in the `X-OLP-Task-Capability`
header. The agent presents it as a bearer token when calling chat completions,
Responses, Anthropic Messages, Gemini generation or the MCP tool endpoints.
The capability is limited to the agent revision's pinned routes and toolsets,
the original caller's current permissions and budgets, and the task's lifetime.

Disablement or revocation denies new sends/continuations and requests upstream
cancellation within the authority freshness bound. Confirmed cancellation,
completion, failure and `outcome_unknown` remain distinct. The kill switch
stops OLP-mediated work; it does not promise to stop an uncooperative remote
service or refund accepted work. Deadline/restart recovery follows the same
rule, with no automatic resend of accepted messages.

### Prompt registry

Prompts have project-scoped drafts, immutable revisions and ETag-protected
`production`/`staging` labels. Requests resolve a label once and record the exact
revision. Templates use `{{name}}` substitutions of schema-declared string
variables only. No sections, partials, expressions, recursive expansion, remote
includes or implicit escaping; JSON is serialized after string substitution.
Unknown/missing variables refuse before dispatch. Templates and schemas are
bounded to 64 KiB each; rendered messages inherit the route's input limit.

On transformed Responses routes, `prompt.id` beginning `olpp_` selects the OLP
registry. Other IDs retain native provider behavior. Chat Completions,
Anthropic and Gemini use consumed `olp_prompt` input. Chat requests supply a
publication `id`, exactly one of `label` or `revision_id`, and a `variables`
object of strings. Combining a registry prompt with explicit messages is
refused; merge ambiguity is not implicit.
Default parameters apply only when absent and cannot widen route constraints.
Strict routes refuse OLP rendering. All rendered messages run input guardrails.
Templates are durable configuration and promote without environment-local IDs;
variables and rendered messages are transient. Reports attribute model attempts
to prompt/revision without an extra billable prompt event.

### Agent traces

Add `/v1/traces` for OTLP/HTTP protobuf ingestion with `traces_write`, explicit
project access and existing key admission. Accept at most one MiB/256 spans per
batch, 60 batches/minute/key, and at most 32 allowlisted bounded attributes per
span. Derive project/key from authentication. Correlate only validated OLP
run/attempt IDs owned by that principal; unknown IDs cannot impersonate billing.
Drop content attributes, events, links and unknown fields before persistence;
span names become bounded operation categories. Do not accept arbitrary SQL or
OTLP endpoints from a caller.

Store timing/status/parentage in a feature-owned metadata table, with the
installation request-history retention (currently 30 days by default).
Ingestion IDs deduplicate telemetry only. Investigations save typed filters
(time range, run, duration, error and tool), at most 100 evidence references and
an operator-authored summary. Reads/export recheck project authority. No
automatic narrative stores source payloads or treats client-supplied spans as
verified execution. Observability exports expose this origin distinction.

### Agent identities

Extend the [workload issuer](../../use/access.md#workload-jwt-identity)
owner rather than inventing an independent identity store. Map exact approved
issuer/subject/client claims to a digest-only project agent principal; retain
the existing signature, audience, lifetime, JWKS and revocation checks.

Delegation uses a separate OLP-issued token obtained with both authenticated
agent identity and explicit user consent. Bind agent principal, delegating key
or member, project, resource allowlists, operation scopes and expiry (five
minutes by default, 15 maximum); prohibit further delegation. Native IdP
delegation claims may narrow this token but cannot mint OLP authority. Agent,
user and token permissions intersect, and revocation of either principal ends
the delegation. User claims alone cannot impersonate a member.
An API-key delegation keeps that key as the consumer/accounting owner. A member
consent names an existing consumer key in the same project that the member may
administer; member login alone creates neither inference scope nor an unbudgeted
consumer. Consent uses session CSRF and fresh authentication; key delegation
requires the key's explicit `delegate` scope, off by default.

Record both principal UUIDs and a stable pair digest on every attempt. Enforce
the user's existing budgets, agent budgets and a pair-specific limit together;
never replace them with one issuer-wide pool. Rotation/renewal preserves pair
ownership. Tokens/raw claims are transient; grant records retain only consent,
digests, bounds, expiry and revocation. The first external qualification target
is Entra agent identity; generic configured workload issuers remain supported.

## Ordered delivery and verification

The [roadmap](../../roadmap.md#remaining-outcomes) owns status and cross-plan
order. Shared accounting is implemented once in the API foundation; this plan
adds agent authority and execution contracts. Existing registration/discovery
code and management MCP are starting points, not completed consumer milestones.

### RM4.4: Agent contracts

**State:** In progress: contracts and console implemented; service-backed
qualification remains open. **Accountable:** access/agent maintainers, with runtime,
accounting and observability owners. **Entry:** existing access, pricing,
observability and operator automation registries. This work can proceed beside
API RM4.1–RM4.3, but no agent dispatch is enabled before durable accounting passes.

The management API and Agent publications console support project drafts,
immutable revisions, exact certified MCP tool pins, and production/staging prompt
labels. New key/workload scopes use explicit toolset/agent UUID allowlists;
empty lists deny and route groups grant no resource authority. Installed
revisions are immutable, while live key withdrawal, publication retirement and
MCP catalog/enablement changes revoke execution eligibility independently.
Agent destination/card/credential changes also require installation access.

Configuration export/plan/apply preserves draft, current publication and labels
separately. Destination revision pins require explicit rebinding; draft and
published credentials have independent sealed bindings and cannot follow a
changed endpoint. Run/step references and one-use approvals contain only IDs,
digests and allowances. Approval consumption commits with the matching fenced
dispatch. Neither publication nor approval creates a consumer execution endpoint.

Focused contract/deny/revocation/round-trip tests and console form checks pass.
Management, immutable-row, live-revocation and atomic approval/dispatch integration
scenarios compile but await the service-backed CI gate; the checklist remains
open until that qualification passes.
The packaged-console browser gate also includes a real prompt save/publish,
draft-versus-label, retirement and exposure-view journey. The complete gate has
not run locally. A separate packaged-console prompt rendering journey passed
locally with publication, label movement and Axe checks on both pages.

- [ ] **RM4.4 — Establish agent authority and publication:** add forward
  migrations, declared scopes/allowlists, immutable toolset/agent/prompt
  publication and configuration promotion, run/attempt references and one-use
  approval metadata. Reuse existing MCP, grant and workload-identity owners.
  Verify the deny matrix, revision pinning, revocation, configuration round trips
  and content-free storage; link dispatch references to the shared ledger.

**Exit:** the [shared contract](#shared-implementation-contract) has focused
authorization, isolation and privacy evidence. API families do not depend on
RM4.4; direct agent/tool execution depends on both RM4.4 and accounting RM4.3.

### RM7: Consumer MCP

**State:** Planned. **Accountable:** MCP/access maintainers.
**Entry:** RM4.3, RM4.4 and existing guardrail argument/result execution.
RM7.1 comes first; RM7.2–RM7.4 may follow independently, then RM7.5 verifies their
combined scope. Live vendor qualification in RM2 is not a prerequisite.

- [ ] **RM7.1 — Serve direct Streamable HTTP calls:** publish pinned toolsets,
  static credentials, bounded sessions and compiled schemas; serve `/mcp` and
  the JSON tool endpoints through one authorized call path. Verify aliases,
  catalog drift, argument/result policies, limits, priced attempts, timeout,
  restart and ambiguous dispatch. Repeated call IDs must not repeat effects.
- [ ] **RM7.2 — Add OAuth and per-user grants:** use existing enrollment and
  grant machinery. Test binding to server/client/audience/scopes/project/user,
  single-flight refresh, expiry/revocation and concurrent credential isolation;
  missing grants never fall back to a service credential.
- [ ] **RM7.3 — Add legacy HTTP+SSE:** implement the explicit `2024-11-05`
  transport and its fixtures without silently downgrading modern sessions.
- [ ] **RM7.4 — Add experimental stdio:** allow only operator-installed
  executables under the existing unconfined-plugin contract. Verify caller
  rejection of command/path/environment/working-directory selection and the
  same call, isolation and effect-accounting limits as HTTP.
- [ ] **RM7.5 — Qualify MCP interoperability:** run pinned reference conformance
  for every declared version/transport and auth mode. Retain credential,
  malicious-schema, drift, egress and failure evidence with exact versions;
  update support claims only for the qualified combinations.

**Exit:** all declared transports/auth modes pass the MCP [exit criteria](#exit-criteria)
and applicable candidate gates. A working HTTP-only slice does not close RM7.

### RM8: Agent workflows

**State:** Planned. **Accountable:** agent maintainers; specialist owners are
named below. **Entry:** RM7.1 and guardrail input/output execution for RM8.1;
existing prompt publications and input/output guardrails for RM8.2. Inference
delegation uses workload identity and has no MCP dependency. Static-credential A2A
in RM8.4 can begin independently. A2A OAuth uses RM7.2, tool callbacks use RM7.1,
and delegated tasks use RM8.3. RM8.5 follows those workflow contracts and existing
observability exports; RM8.6 qualifies the combined result.
Retrieval-backed tools additionally require certified [RM5.2 or RM5.3 retrieval](api-surface.md#rm5-retained-apis-and-retrieval)
for their selected backend, not completion of every retrieval adapter.

- [ ] **RM8.1 — Execute bounded tool loops** (gateway/protocol): implement the
  three native dialects plus Responses projections under the published
  transformed-route policy. Verify iteration/time/concurrency/cost bounds,
  per-step attempts, approvals, digest-bound continuation and revocation.
  Test cancellation/restart/unknown effects and prevent cache replay of runs.
- [x] **RM8.2 — Add prompt publication and rendering** (prompt/console):
  immutable revisions, labels and bounded string substitution use the same
  server rendering path for inference, playground and imported configuration.
  Gateway fixtures cover fixed in-flight revisions, variable and strict-route
  refusals, input guards and content-free persistence. The PostgreSQL-backed
  `TestPlaygroundRendersPublishedPromptLikeInference` compares rendered input
  across four request formats in unary and streaming mode, checks project
  access, refuses ambiguous and oversized raw requests before dispatch, and
  exercises import and label movement. With capture enabled, it also checks that
  playground variables and rendered text stay out of durable records and
  exports. The packaged-console journey covers save, publish, playground
  rendering, label movement and Axe checks. These are local fixture results,
  not live provider qualification.
- [x] **RM8.3 — Add delegated identities** (access): workload agents use
  explicit consent and scoped tokens with intersected rights and
  user/agent/pair budgets. Fixtures cover signed tool calls, audience refusal,
  replay, renewal and revocation. Live Entra agent identity qualification
  remains separate from generic workload issuer fixtures.
- [x] **RM8.4 — Serve A2A workflows** (agent/resource): local reference fixtures
  cover the pinned protocol adapters, retained owner-bound and delegated tasks,
  callbacks, accounting, cross-project denial and revocation. Unconfirmed remote
  effects remain uncertain. Live external interoperability remains unverified and
  is part of RM8.6 qualification. See [A2A qualification](../../use/compatibility.md#a2a-qualification).
- [ ] **RM8.5 — Add traces and operator views** (observability/console): after
  RM8.1–RM8.4, implement bounded OTLP ingestion, scoped evidence links and
  origin-labelled exports. Test content stripping, project isolation, retention
  and inability to forge financial facts; exercise accessible operator journeys.
  Saved investigations are implemented with typed filters, bounded references,
  an operator-authored summary and current project checks on reads. Referenced
  evidence remains subject to its own access and retention policies.
- [ ] **RM8.6 — Qualify the complete agent surface** (release): run service
  faults/restarts, authorization/isolation, pinned SDK/reference implementations,
  Chromium and applicable check/race/build/Helm/performance gates on one candidate.
  Publish runbooks for uncertain effects, remote cancellation limits, secret
  enrollment/recovery and upgrades, then review exact support claims.

**Exit:** every criterion below has evidence. Implementation checkboxes do not
establish live support; record remaining blockers and artifact links using the
[roadmap evidence rules](../../roadmap.md#updating-progress-and-evidence).

## Change map

| Change | Owner |
| --- | --- |
| Registry, discovery, toolsets and consumer MCP | Extend `internal/mcpservers/`; new execution owner `internal/mcp/` |
| Grants, scopes, approvals and agent delegation | `internal/access/`, existing grant/runtime owners |
| Loops, native projection, prompts | `internal/gateway/`, `internal/protocols/`; new `internal/prompts/` |
| A2A tasks and callbacks | New `internal/agents/`, existing retained-resource/accounting boundaries |
| Pricing, accounting and traces | `internal/usage/`, `internal/limits/`, `internal/telemetry/`, `internal/export/`; new trace-ingestion owner |
| Operator surfaces | `openapi/management.json`, `console/src/lib/features/agents/`, generated clients and provider repository |

## Exit criteria

- [ ] All declared MCP versions/transports pass pinned reference conformance;
      OAuth refresh/revocation and per-user separation survive concurrent calls.
- [ ] Drifted catalogs cannot execute, malicious schemas cannot fetch networks,
      and caller/tool output cannot change destinations or credential bindings.
- [ ] Multi-step tasks on OpenAI, Anthropic and Gemini fixtures honor iteration,
      time, concurrency, approval and cost bounds; every dispatched step has
      exactly one financial attempt, including cancellation and uncertainty.
- [ ] A2A 0.3.0/1.0.0 reference agents pass send/stream/read/cancel/resubscribe
      tests; cross-project task access fails, and local disablement stops OLP
      work within 60 seconds without claiming remote termination.
- [ ] Delegation cannot widen either principal, renew a revoked grant or reset
      pair spend; wrong audience/project/agent/subject and replay tests fail closed.
- [ ] Prompt rendering agrees across API, playground and promotion; label
      movement leaves in-flight revisions fixed and no variable values persist.
- [ ] Trace injection cannot cross projects, forge financial facts or persist
      content; investigations and exports enforce current access and retention.
- [ ] Service fault/restart tests, generated authorization/isolation sweeps,
      pinned SDKs, accessible Chromium journeys, race/check/build/Helm and
      applicable gateway regression gates pass on the same candidate.
- [ ] Runbooks explain uncertain effects, remote cancellation limits, secret
      enrollment/recovery and upgrades. Each agent/tool support claim changes only
      with its evidence; external qualification is distinguished from fixtures.

No execution or external qualification result is claimed by this plan.
See [MCP registration](../../use/mcp-servers.md) for the existing registration boundary.
