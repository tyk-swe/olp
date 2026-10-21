# Concepts

OLP separates the model name a caller uses from the providers that serve it.
[Getting Started](getting-started.md) walks through publication;
[Compatibility](use/compatibility.md) defines supported operations.

## Routes and slugs

An ordinary request names a published route slug, usually in `model` or the native
URL. Uploads and model-less operations use `X-OLP-Route`; retained-resource
follow-ups resolve the route from their gateway-owned ID. Operators can change
providers behind a slug without changing client configuration.

A route sets operations, targets, an overall deadline and an attempt budget.
**Strict** fidelity preserves the target's native invocation and observation
contract. **Transformed** fidelity permits declared translation or redaction.
A new revision changes fidelity; historical resources retain creation contracts.
See [Routing](use/provider-routing.md#routes).

## Providers: drafts, revisions, certification

Ordinary providers start as drafts. Discovery lists candidate models;
server-side certification proves exact provider/model/operation/surface/mode
tuples with bounded probes. A model listing or capability declaration does not
certify inference. Activation publishes connection settings, enabled certified
models and exact credential versions together. Semantic/network edits invalidate
certification; credential rotation requires validation before activation.
See [Provider lifecycle](use/provider-routing.md#provider-lifecycle).

## Runtime generations and pinning

Publication compiles one numbered runtime generation with a digest. Gateways
install complete valid generations atomically; a failed install retains the
previous generation. Each request pins configuration for its lifetime, while live
revocation remains independent. New key-authenticated work refuses after 60 seconds
of stale authority. See [Architecture](develop/architecture.md#runtime-publication-and-authority).

## Keys, permissions, expiry, limits

API keys are machine principals, separate from console sessions. Their one-time
secrets are stored as digests. `inference` and `models_read` are independent;
projects, route lists and route groups further bound access. Rotation preserves
the principal's accounting; revocation/expiry denies all its versions.

Every applicable budget boundary must pass. Rate/token windows use UTC minutes;
monetary day/week/month windows use the configured calendar. Known exhausted
balances refuse with 429; unknown balances refuse with 503 until reconciliation.
Heuristic estimates, missing prices/usage and later provider work mean admission
limits are not invoice caps. See [Access](use/access.md) and
[budget recovery](operate/operations.md#spend-budget-reconciliation).

## Attempts, usage, pricing

One request can make several attempts, including retries and fallbacks. Each
retains its own identity, pinned revisions, usage and outcome. A committed stream
never restarts elsewhere and an ambiguous create never blindly repeats.
Cancellation releases leases but cannot erase already accepted provider usage.

Missing usage or prices remain incomplete/unpriced. **Cost** estimates provider
billing; **charge** applies the consumer's rate card. Attempt facts, not metrics,
are accounting authority. See [Pricing](operate/pricing.md) for exact rules and
[FinOps](operate/finops.md) for exports and invoice reconciliation.

## What is stored — and what never is

Request history, audit and traces retain bounded identifiers, timing, status,
usage, pricing provenance and allowed attribution. They exclude prompts,
responses, reasoning, tool payloads, uploads, raw headers and credentials. Keep
operator names and labels secret-free too.

Other content has explicit storage contracts: temporary upload spools;
provider-retained files/jobs/responses with OLP ownership mappings;
sealed native continuation dependencies; encrypted responses in a dedicated
cache; and owner-enabled external capture from bounded memory. Empty capture
redactor lists are unredacted; failures with configured local redactors drop the
capture. A capture marker means queued, not delivered. See
[Security](develop/security.md), [Caching](use/caching.md) and
[Observability](operate/observability.md#payload-capture).

## Code accounts and conversation trees

[Code mode](use/code-mode.md) uses `/code/{slug}` with native model names and
enrolled subscription accounts. A conversation tree pins each model to an account,
with at most one account per subscription family. Children, resumes and reconnects
retain those pins across replicas/restarts. Unavailable or revoked pins refuse;
OLP never replays inference or switches accounts silently.

Subscription usage, observed provider allowance and optional hard token budgets
are separate ledgers. Unknown final usage remains uncertain. Budgeted operations
need a proven native bound; controlled client fixtures do not qualify real
subscriptions. See [Qualification](develop/qualification.md#code-mode-qualification).

## Request lifecycle

Authenticate and check current scope/project/route authority; pin runtime policy;
apply admission and guardrails; select eligible targets and credential slots;
execute within the shared deadline/attempt budget; observe usage and settle each
attempt, release leases and persist content-free facts. Code mode uses its
separate native forwarding and generation-admission boundary.
