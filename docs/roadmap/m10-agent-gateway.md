# M10: Agent gateway

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | [M4](m04-tenancy-identity.md), [M5](m05-observability.md), [M6](m06-cost-management.md), [M7](m07-guardrails.md), [M11.2](m11-operator-ecosystem.md#m112-infrastructure-as-code) | Governed model, tool and agent execution |

Extend OLP's published configuration, scoped authority and attempt accounting to
tools, agents and prompts. Upstream MCP **registration and certification already
exist**; inference-time tool execution, A2A, prompt rendering, trace ingestion
and delegated agent identities do not. The existing management MCP endpoint
administers OLP and is a separate product surface.

## Outcome and baseline

- Keys discover and call approved MCP tools through a consumer endpoint.
- Transformed model routes can run bounded, governed tool loops.
- Registered A2A agents have scoped tasks, admission limits and accountable cost.
- Applications reference immutable prompts; operators investigate metadata-only
  agent traces and authorize explicitly delegated agent principals.

| Boundary | Current code and evidence | Remaining work |
| --- | --- | --- |
| MCP registration | [Project-owned registrations](../mcp-servers.md), immutable certified catalogs, static bearer seals, conditional promotion and Terraform/OpenTofu lifecycle | Consumer calls, toolsets, OAuth, legacy transports and runtime publication |
| Management MCP | [Contract-generated `/api/v1/mcp`](../operator-cli.md#management-mcp), token authorization and secret-redacted responses | No inference execution belongs here |
| Shared execution | M4 principals/budgets, M5 accounting/export, M6 prices and M7 tool policies | Extend their typed contracts and runtime owners to tool/agent events |
| Agents, prompts, traces | Workload JWTs and outgoing OTLP exist | A2A, registry rendering, incoming OTLP, investigations and delegation |

The [parity matrix](parity.md#agents-and-tools) remains `Gap` for execution
capabilities until their own exits pass. Registration alone closes none of them.

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
   write a durable provisional-budget ledger entry with dispatch ID, run/step,
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

### M10.1 MCP gateway

**Transport and certification.** Reuse `internal/mcpservers/` and its existing
`mcp_credential` seal purpose; do not create a second server registry. The first
consumer slice supports Streamable HTTP at MCP `2025-03-26`, `2025-06-18` and
[`2025-11-25`](https://modelcontextprotocol.io/specification/2025-11-25).
Legacy HTTP+SSE follows as an explicitly selected transport. Stdio is limited
to operator-installed executables in the experimental
[unconfined plugin directory](../plugins.md#unconfined-plugins-experimental);
API callers cannot supply a path, command, environment or working directory.
M10 closes only after all three declared transports qualify. The legacy adapter
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

**Calls.** Validate arguments against the pinned schema, run M7 argument/result
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

### M10.2 Gateway-executed tools

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
must fit the remaining worst-case allowance, using M6 prices in the installation
pricing currency. Existing `max_cost` remains the per-attempt candidate filter
and applies independently to each model step. No unbounded output model or
unpriced tool can satisfy a finite run ceiling. Each model and tool step remains
a separate attempt, including refused or uncertain continuations.

Operators classify tools as read-only or approval-required; upstream annotations
are untrusted hints. Unclassified tools require approval. M7 deny rules override
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

### M10.3 A2A agent gateway

An agent registration pins its card, endpoint, protocol, credential binding and
declared per-message price/maximum cost. Card fetching uses bounded egress and
requires explicit publication of changes. Support JSON-RPC over HTTP/SSE for
[A2A 0.3.0](https://a2a-protocol.org/v0.3.0/specification/) and
[1.0.0](https://a2a-protocol.org/v1.0.0/specification/); maintain separate schema
adapters and refuse undeclared bindings or versions. The rewritten card at
`/a2a/{agent}` advertises OLP's authentication and only implemented capabilities.
Initial methods cover send/stream, get/cancel and stream resubscription; task
listing is principal-scoped. Push notifications, gRPC and arbitrary artifact
URL fetching are outside this milestone and are not advertised.
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

Disablement or revocation denies new sends/continuations and requests upstream
cancellation within the authority freshness bound. Confirmed cancellation,
completion, failure and `outcome_unknown` remain distinct. The kill switch
stops OLP-mediated work; it does not promise to stop an uncooperative remote
service or refund accepted work. Deadline/restart recovery follows the same
rule, with no automatic resend of accepted messages.

### M10.4 Prompt registry

Prompts have project-scoped drafts, immutable revisions and ETag-protected
`production`/`staging` labels. Requests resolve a label once and record the exact
revision. Templates use `{{name}}` substitutions of schema-declared string
variables only. No sections, partials, expressions, recursive expansion, remote
includes or implicit escaping; JSON is serialized after string substitution.
Unknown/missing variables refuse before dispatch. Templates and schemas are
bounded to 64 KiB each; rendered messages inherit the route's input limit.

On transformed Responses routes, `prompt.id` beginning `olpp_` selects the OLP
registry. Other IDs retain native provider behavior. Chat Completions,
Anthropic and Gemini use consumed `olp_prompt` input. Combining a registry
prompt with explicit messages is refused; merge ambiguity is not implicit.
Default parameters apply only when absent and cannot widen route constraints.
Strict routes refuse OLP rendering. All rendered messages run input guardrails.
Templates are durable configuration and promote without environment-local IDs;
variables and rendered messages are transient. Reports attribute model attempts
to prompt/revision without an extra billable prompt event.

### M10.5 Agent traces

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
verified execution. M5 exports expose this origin distinction.

### M10.6 Agent identities

Extend the [M4.4 workload issuer](m04-tenancy-identity.md#m44-workload-identity)
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

| Slice | Prerequisites | Concrete exit |
| --- | --- | --- |
| 1. Shared contracts | Existing M4/M5/M6 owners and M11 registry | Forward migrations, scopes, attempt/run types, one-use approvals, resource publication and promotion; no second MCP or identity owner; deny-matrix and privacy fixtures pass |
| 2. Direct MCP calls | Slice 1 and M7.6 | Streamable HTTP toolsets, pinned-schema validation, static credentials, limits and priced attempts through both consumer endpoints; drift/timeout/restart/ambiguous-dispatch tests pass |
| 3. MCP interoperability | Slice 2 | OAuth/per-user grants, legacy SSE and experimental stdio; versioned conformance and credential-isolation tests pass |
| 4. Tool loops and prompts | Slice 2; M7 input/output execution | Three native dialects plus Responses, bounded continuations/approvals, prompt API/playground/import equivalence and no cache replay of effects |
| 5. Delegation and A2A | Slices 1–3 | Both A2A adapters, owner-bound retained tasks, authenticated callbacks, pair budgets and kill-switch behavior under a lost upstream connection |
| 6. Traces and operator views | Slices 1, 4–5; M5 export | Bounded OTLP ingestion, project-safe evidence links, origin-labelled exports and accessible console workflows |
| 7. Release qualification | All prior slices | Full gates below, pinned external reference implementations, runbooks and parity review |

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
      applicable M1 regression gates pass on the same candidate.
- [ ] Runbooks explain uncertain effects, remote cancellation limits, secret
      enrollment/recovery and upgrades. Each agent/tool parity row changes only
      with its evidence; external qualification is distinguished from fixtures.

No M10 execution or external qualification result is claimed by this plan.
Existing registration evidence remains in [M11](m11-operator-ecosystem.md) and
the [MCP registration guide](../mcp-servers.md).
