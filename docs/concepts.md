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

## Providers: drafts, revisions, certification

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
[Architecture](develop/architecture.md#runtime-publication-and-authority).

## Keys, permissions, expiry, limits

API keys are machine principals. They are separate from interactive console
sessions, and OLP stores only digests of their one-time secrets. The `inference`
and `models_read` scopes are independent. Project membership, explicit route
lists and route groups narrow access further.

Rotation creates a new secret version for the same accounting principal.
Revoking or expiring a key denies every version of that principal. This
distinction lets an operator replace a leaked or aging secret without
fragmenting usage history.

All applicable budget boundaries must pass before admission. Rate and token
windows use UTC minutes. Monetary day, week and month windows use the configured
budget calendar. A known exhausted balance refuses work with 429. An unknown
balance refuses with 503 until reconciliation can establish the balance.

Admission limits protect the gateway from unbounded work, but they are not exact
invoice caps. Estimates can be heuristic, a price or final usage record can be
missing, and a provider can accept work before its usage is known. See
[Access](use/access.md) for key policy and
[budget recovery](operate/operations.md#spend-budget-reconciliation) for
reconciliation.

## Attempts, usage, pricing

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

<a id="what-is-stored--and-what-never-is"></a>

## What is stored and what never is

Request history, audit records and traces retain bounded identifiers, timing,
status, usage, pricing provenance and permitted attribution labels. They exclude
prompts, responses, reasoning content, tool payloads, uploaded bytes, raw
headers and credentials. Operator-entered names and attribution labels therefore
also need to stay free of secrets and request content.

Some features need content under a narrower contract. Uploads can pass through
temporary spools. Provider-retained files, jobs and responses keep OLP ownership
mappings. Native continuations can require sealed dependencies, cached responses
use encrypted storage, and owner-enabled external capture can send bounded
in-memory payloads to a configured sink.

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
another account or silently move account-owned state. Subscription usage,
observed provider allowance and optional hard token budgets are separate ledgers
with different meanings.

Unknown final subscription usage remains uncertain. A budgeted operation needs a
proven native bound, and controlled client fixtures do not qualify a real
subscription. The evidence boundary is set out in
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
