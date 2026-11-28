# Concepts

OLP separates the model name a caller sends from the provider deployment that
serves it. That separation lets operators certify, replace or rebalance
providers without changing every client. [Getting Started](getting-started.md)
shows the publication workflow. [Compatibility](use/compatibility.md) defines
the supported operations and protocol translations.

## Routes and slugs

An ordinary request names a published route slug, usually in the `model` field
or the native request URL. Uploads and operations without a model use
`X-OLP-Route`. Follow-up calls for a retained resource resolve the route from
the gateway-owned resource ID. Client configuration can therefore remain stable
while operators change the providers behind a slug.

A route defines its operations, provider targets, overall deadline and attempt
budget. It also selects a fidelity policy. Strict fidelity preserves the
target's native invocation and observation contract. Transformed fidelity
permits declared protocol translation or redaction. Changing fidelity publishes
a new route revision, while resources created earlier retain the contract that
applied at creation. See [Routing](use/provider-routing.md#routes) for the
complete route model.

## Provider drafts, revisions and certification

An ordinary provider connection starts as a draft. Discovery produces candidate
models, but a listing or vendor capability declaration does not prove that
inference will work. Server-side certification makes bounded probes for the
exact provider, model, operation, surface and mode that a route wants to serve.

Activation publishes the completed connection settings, enabled certified models
and exact credential versions together. A semantic or network change invalidates
the affected certification. A rotated credential also needs validation before a
revised connection can be activated. See
[Provider lifecycle](use/provider-routing.md#provider-lifecycle) for the state
transitions and failure cases.

## Runtime generations and pinning

Publication compiles related route and provider revisions into one numbered
runtime generation with a digest. Each gateway installs a complete valid
generation atomically. If installation fails, it keeps serving the previous
valid generation instead of mixing partial configuration.

Every request pins the generation and policy revisions it admitted under, so its
execution stays coherent for the life of that request. Live revocation remains
independent of pinning. New key-authenticated work refuses to start once its key
authority has been stale for more than 60 seconds. The publication and authority
boundary is described in
[Architecture](develop/runtime.md#runtime-publication-and-authority).

## Organizations, projects and identity

Organizations group projects at one level so organization managers and viewers
can inherit access without duplicating project grants. Projects own providers,
routes, API keys, guardrails, MCP servers, toolsets, agents, prompts, retrieval
bindings and code-mode pools, keeping team workloads isolated. Resources left
unassigned are installation-wide, and unassigned API keys can call only
unassigned routes.

Interactive console members hold an installation role (`owner`, `operator`,
`developer` or `viewer`) paired with either global or assigned-project scope.
Members sign in through local password and MFA accounts, OIDC or SAML, and
directories can synchronize users and groups through SCIM. Automation calls the
management API with management tokens, while machine workloads can authenticate
to the gateway directly through registered workload JWT issuers instead of
static secrets. See [Access](use/access.md) for the complete identity and
project model.

## Keys, permissions, rotation and limits

API keys (`olp_`) are machine principals separate from interactive console
sessions, and OLP stores only purpose-separated digests of their one-time
secrets. Each key carries one or more independent consumer scopes chosen from
`inference`, `models_read`, `guardrails`, `tools`, `agents`, `traces_write` and
`delegate`. `inference` admits model, retrieval and code-mode calls, while
`models_read` admits gateway model discovery. `guardrails` admits standalone
policy evaluation at `/v1/guardrails/apply`, `tools` admits published MCP
toolsets, `agents` admits published A2A agents, `traces_write` admits OTLP
trace ingestion at `/v1/traces`, and `delegate` lets the key mint short-lived
delegated tokens for approved workload agents. Project ownership, explicit route
lists, route groups, toolset allowlists, agent allowlists and CIDR allowlists
narrow access further.

Overlap rotation creates a new secret version for the same accounting principal
while optionally keeping up to eight previous secrets valid for a bounded
overlap window of up to 86,400 seconds without fragmenting rate counters or
usage history. Pausing a key stops every current and overlap secret from
admitting new work, new delegation exchanges and new agent callbacks once
serving replicas reach the pause authority generation, and resuming restores
secrets that remain unexpired and unrevoked. Revoking or expiring a key denies
every version of that principal permanently.

Every applicable budget and limit boundary must pass before admission. Monetary
spend budgets stack across the installation, organization, project, budget
group, attribution label pair, end-user, named-route (`route_limits`) and API
key tiers, alongside supply-side caps on provider connections and credential
slots. Consumer and hierarchy spend budgets measure either provider `cost` or
consumer rate-card `charge` according to `cost_basis`, while supply caps always
measure provider cost. Monetary day, week and month windows follow the
configured budget time zone, and temporary budget increases can extend an active
window without changing the permanent cap. Rate and token windows use UTC
minutes, and provider connections and credential slots can enforce separate
`input_tokens_per_minute` and `output_tokens_per_minute` ceilings alongside
combined `tokens_per_minute`. A known exhausted balance refuses work with 429,
and an unknown balance refuses with 503 until reconciliation establishes it.

Admission limits protect the gateway from unbounded work, but they are not exact
invoice caps. Estimates can be heuristic, a price or final usage record can be
missing, and a provider can accept work before its usage is known. See
[Access](use/access.md) for key policy and
[budget recovery](operate/operations.md#spend-budget-reconciliation) for
reconciliation.

## End users and attribution

Projects and keys can enforce attribution policies using
`required_attribution_keys`, `attribution_defaults` and
`allowed_attribution_keys` so callers attach bounded machine labels without
overriding pinned defaults. Projects can attach spend caps to specific label and
value pairs through attribution budgets, including the reserved
`delegation_pair` key that caps combined spend for a delegating principal and an
agent.

Keys can also identify callers' end users through `X-OLP-End-User` in `header`
mode or through provider request fields in `native` mode. OLP stores only an
installation- and project-separated HMAC digest in ledgers and policies so
operators can apply end-user blocks, rate limits and spend limits without
persisting raw end-user identifiers. See
[End-user identity](use/access.md#end-user-identity) for matching rules and
digest lookups.

## Attempts, usage and pricing

One client request can make more than one provider attempt through retry or
fallback. Each attempt keeps its own identity, pinned revisions, observed usage
and outcome. Attempt facts are the accounting authority, while metrics are
operational summaries derived from them.

Execution follows the state that is actually known. A stream that has committed
output never restarts on another provider. A create request with an ambiguous
upstream outcome is not blindly repeated. Cancellation releases local leases,
but it cannot erase usage that a provider already accepted.

Cost estimates what the provider charges the installation. Charge applies the
consumer's rate card. Missing usage remains incomplete and missing pricing
remains unpriced instead of being converted to zero.
[Pricing](operate/pricing.md) defines the exact calculation rules, and
[FinOps](operate/finops.md) covers exports and invoice reconciliation for
OpenAI, Anthropic, Bedrock, Vertex AI and Azure OpenAI connections.

## Guardrails

Guardrails inspect prompts, responses and tool content under project-owned
immutable definitions and ordered policies. Evaluators cover built-in detectors,
external HTTP guardrail services, model evaluators and confined WebAssembly
guardrail plugins.

Policies attach additively at installation, organization, project, route,
toolset or API key scope and record content-free decision evidence across input,
output, streaming and tool phases. Operators can preview effective policy chains
before dispatch and test sample inputs interactively without storing them, while
callers with the `guardrails` scope can invoke standalone evaluation at
`/v1/guardrails/apply`. See [Guardrails](use/guardrails.md) and
[Guardrail integrations](use/guardrail-integrations.md) for setup and bounds.

## Agents, toolsets, prompts, MCP and workflows

OLP governs agentic traffic through project-owned publications and certified
upstream MCP servers. Operators register remote (`streamable_http` or legacy
`sse`) or approved experimental `stdio` MCP servers, certify their immutable
tool catalogs, and compose certified tools into published toolsets alongside
versioned prompts (with `staging` and `production` labels for expansion on
transformed routes) and published A2A agents.

Transformed routes with gateway tool execution run bounded model-and-tool loops
inside the gateway, enforcing run ceilings on iterations, tool calls, duration,
concurrency and run cost. Whenever the model invokes a tool whose classification
is not `read_only`, OLP pauses the workflow and issues a sealed `olpa_`
continuation token for explicit human approval through `olp_approval`.

Published agents proxy A2A `0.3.0` and `1.0.0` traffic at `/a2a/{agent}` with
verified agent-card digests, background task monitoring and short-lived `olpt_`
callback capability tokens restricted to the agent's pinned route and toolset
revisions. OAuth-backed MCP servers and remote agents require per-user personal
grants or Entra on-behalf-of exchanges and never fall back to static
credentials. Approved workload agents can exchange a `delegate`-scoped API key
or a one-use member consent code (`olpc_`) for a short-lived delegated token
(`olpd_`) limited to the shared permissions of both principals.

Every gateway tool workflow and A2A send persists content-free runs, steps,
approvals and correlated trace spans, which operators can link into saved
investigations. Before dispatching bounded agent or tool work, OLP records
durable provisional exposure in `execution_debits` across every applicable
budget owner and holds uncertain outcomes (`outcome_unknown`) apart from settled
usage until reconciled or explicitly resolved. See [Agents](use/agents.md) and
[MCP registration](use/mcp-servers.md) for the full contracts.

## Retained provider resources, retrieval bindings, media jobs and batches

Provider-retained resources such as files, asynchronous batches, cloud batch
jobs, vector stores and retained Responses objects stay owned by the creating
API key and project and pin the provider revision, credential slot, credential
version and route fidelity selected at creation. Follow-up calls resolve through
the gateway-owned identifier and never fail over to another provider target.

Asynchronous video creation records durable media jobs that track execution
state and background reconciliation separately from inference history. Operators
can inspect status, trigger immediate refreshes, stream downloadable output
variants or delete stored provider output without rendering media inline in the
console.

Cloud retrieval bindings expose query-only access to operator-managed Amazon
Bedrock Knowledge Bases, Vertex RAG Engine corpora and Azure AI Search indexes
at `/v1/retrieval/{binding_id}/query`. Each binding pins a single transformed
route target, explicit API key grants and mapped metadata filters while keeping
cloud resource identifiers and write operations hidden from callers. See
[Retrieval bindings](use/retrieval.md),
[Durable media jobs](use/providers/media.md#durable-media-jobs) and
[Files and batches](use/compatibility.md#files-and-batches).

## Response and prompt caching

A route can opt into exact response caching or semantic response matching stored
encrypted with the installation key ring in a dedicated Valkey instance. Cache
entries isolate keys by `key`, `project` or `route` scope, incorporate effective
route, provider and guardrail revisions in their identity, meter zero provider
cost on hits and never write response bodies to PostgreSQL.

Transformed routes can also coordinate provider-side prompt caching, including
Anthropic prompt-cache breakpoints and OpenAI prompt cache keys. See
[Caching](use/caching.md) for configuration, bounds and invalidation rules.

## Plugins

Plugins are operator-installed extensions that add provider authentication and
hosting around built-in dialects, custom guardrail evaluators or route selector
predicates. Confined WebAssembly modules run on `wazero` with SHA-256 digest
identity and explicit owner approval of every declared network origin. An
experimental unconfined tier can also run permitted native provider executables
when a deployment enables it explicitly. See [Plugins](use/plugins.md) for
installation, review and confinement limits.

## What is stored and what never is

Request history, audit records and traces retain bounded identifiers, timing,
status, usage, pricing provenance and permitted attribution labels. They exclude
prompts, responses, reasoning content, tool payloads, uploaded bytes, raw
headers and credentials. Operator-entered names and attribution labels therefore
also need to stay free of secrets and request content.

Some features need content under a narrower contract. Uploads can pass through
temporary spools. Provider-retained files, jobs and responses keep OLP ownership
mappings. Native continuations and paused approval workflows use sealed
dependencies, cached responses use encrypted storage, and owner-enabled external
capture can send bounded in-memory payloads to a configured sink.

Capture has its own safety rules. An empty local redactor list means unredacted
capture. If configured local redaction fails, OLP drops the capture. A capture
marker means the payload was queued rather than delivered. See
[Security](develop/security.md), [Caching](use/caching.md) and
[Observability](operate/observability.md#payload-capture) for the boundaries
that apply to each case.

## Code accounts and conversation trees

[Code mode](use/code-mode.md) serves coding clients through `/code/{slug}` with
native model names and enrolled subscription accounts. A conversation tree pins
each model to one account, with at most one account for each subscription
family. Child conversations, resumes and reconnects retain those pins across
replicas and restarts.

An unavailable or revoked pin refuses new work. OLP does not replay inference on
another account or silently move account-owned state. Routes can also opt into
explicit paid API targets (`paid_targets`) for direct metered dispatch or
subscription-first paid fallback (`paid_fallback_targets`) per native model
using active metered provider connections and slots in the same project, with
current pricing, separate cost and charge, and durable uncertainty holds. Paid
targets never activate unless explicitly published.

A code-mode route can also expose opt-in research tools bound to an enrolled
Z.ai global account at `/code/{slug}/mcp/{service}` (`search`, `reader` and
`zread`), proxying bounded MCP traffic independently of the route's inference
models and allowances.

Subscription usage, observed provider allowance and optional hard token budgets
are separate ledgers with different meanings. Unknown final subscription usage
remains uncertain. A budgeted operation needs a proven native bound, and
controlled client fixtures do not qualify a real subscription. The evidence
boundary is set out in
[Qualification](develop/qualification.md#code-mode-qualification).

## Request lifecycle

An ordinary request proceeds through a fixed sequence. OLP authenticates the
caller and checks current scope, project and route authority. It pins runtime
policy, applies admission and guardrails, selects eligible targets and
credential slots, and executes inside the shared deadline and attempt budget. It
then observes usage, settles each attempt, releases leases and persists the
content-free request facts.

Code mode deliberately follows a separate path. It uses native forwarding and
its own generation-admission boundary so subscription account ownership and
conversation state are not weakened by ordinary route fallback rules.
