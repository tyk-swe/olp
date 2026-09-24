# Concepts

Routes give callers stable model names; provider revisions determine where
requests run. This guide introduces publication, authorization, accounting, and
privacy. See [compatibility](compatibility.md) for supported operations and
[configuration](configuration.md) for installation settings.

## Routes and slugs

Generation requests name a published route slug in the `model` field or native
URL path. File uploads use `X-OLP-Route`; retained-resource operations resolve
the route through their stored mapping. Callers never select an upstream
provider/model directly. Operators can change that mapping without changing the
caller's model name.

A slug contains at most 63 bytes of lowercase ASCII letters, digits, and single
internal hyphens. It starts and ends with a letter or digit. Uppercase,
underscores, dots, slashes, and consecutive hyphens are rejected.

A route defines allowed operations, an overall deadline, an attempt budget, and
targets with provider models, priorities, weights, and timeouts. Target timeouts
cannot exceed the overall deadline. Credential pools can produce several
attempts per target, so the attempt budget may exceed the target count. See
[route configuration](provider-routing.md#routes) for bounds and publication.

A route revision may declare a **fidelity** contract. Omitted contracts remain
legacy. Explicit `transformed` routes declare intentional model-significant
changes; `strict` routes admit only interactions that preserve execution,
observation, permitted continuation and effects relative to the target's native
invocation. A published slug keeps its strict or non-strict identity, so moving
across that boundary publishes a reviewed draft under a new slug. See
[route fidelity](gateway.md#runtime-publication-and-authority).

Keys can use only routes in their own project; a key without a project can use
only routes without one. Scope and route allowlists further restrict access.
`GET /v1/models` lists the routes visible to the calling key, not upstream
provider models. Configured but unpublished models are not discoverable.

## Providers: drafts, revisions, certification

Provider edits remain drafts until activation publishes an immutable revision:
connection settings, enabled models, certified capabilities, and exact
credential versions. Requests keep the revision they started with.

Certification belongs to the server. A successful bounded probe earns evidence
for an exact provider/model/operation/surface/mode tuple; a declaration or
successful model listing alone does not certify inference. Probes use production
connectors and codecs and retain no prompt or response content.

Transport and semantic edits invalidate certification and credential-slot
validation. Renaming a provider or rotating a credential preserves model
certification, but rotation still requires validation before activation. See
[provider lifecycle](provider-routing.md#provider-lifecycle) for the full probe,
discovery, review, certification, and activation workflow.

## Runtime generations and pinning

Activation compiles the installation configuration into a numbered runtime
generation with a digest. Gateways verify it and replace their snapshot
atomically. A failed installation leaves the previous complete generation
active.

Each request pins one generation and its provider/credential revisions, even
when a stream outlives later activations. Key and credential revocation refresh
independently of generation installation; a failed activation cannot preserve
revoked authority. See
[runtime publication and authority](gateway.md#runtime-publication-and-authority).

## Keys, permissions, expiry, limits

An API-key secret is shown once; only its HMAC digest is stored. Replace a lost
secret. Revocation reaches gateways through authority refresh: polls run every
five seconds and new admissions stop when authority is 60 seconds old. Already
admitted ordinary streams may finish; realtime sessions recheck the key during
the session. See [production contracts](production-guarantees.md).

`inference` authorizes provider operations, including generation, token
counting, embeddings, rerank, moderation, media, batches, and realtime.
`models_read` authorizes model discovery. Neither scope implies the other. An
empty route allowlist permits all routes within the key's project boundary; a
non-empty list narrows that set. Expired keys are refused.

Optional limits cover requests per minute, tokens per minute, concurrency, and
daily/monthly accrued cost. A key can also join one **budget group** in the same
project, sharing daily/monthly spend thresholds across its member keys. Group
and individual limits both apply. Changing membership does not reassign past
usage: accounting retains the group captured for the request. Manage groups
through [Access](access.md#projects-and-budget-groups).

Valkey server time defines fixed UTC minute, day, and calendar-month windows,
lease expiry, and rejection hints. Bursts across boundaries are possible. Tokens
are estimated before dispatch and reconciled against reported usage; concurrency
leases release on completion and expire after abandoned work.

Cost limits compare previously attributed spend with the threshold. Concurrent
accepted work can exceed it. Exhausted windows return HTTP 429
`budget_exhausted` with `Retry-After` to the UTC boundary. Missing, malformed,
or wrong-window spend state returns HTTP 503 until an authoritative PostgreSQL
snapshot initializes it. A key or group cost budget always fails closed.
Unpriced attempts accrue no money and remain visible separately.

For rate/concurrency-only keys, an explicit `fail_open` setting can bypass
limits during a configured Valkey outage. See
[gateway limits](gateway.md#limits-and-budgets) and
[spend recovery](spend-budget-recovery.md) for enforcement and initialization.

## Attempts, usage, pricing

Every admitted request has one identity; each provider call has an ordered
attempt identity with its pinned revisions, deadline, and outcome. Failover
appends attempts rather than replacing earlier failures.

Usage and cost attach to the attempt that produced them. Missing usage or a
missing applicable price stays incomplete or unpriced; the gateway never invents
usage or cost. Unpriced work contributes zero to accrued budgets, so incomplete
pricing coverage still requires provider-side quotas.

The terminal envelope records status, timing, cancellation, attempts, usage
completeness, and pricing provenance. Cancellation closes upstream work and
releases leases through the same completion path. Storage uniqueness prevents
duplicate request/attempt facts. Stored background responses settle final usage
once it is observed; see
[stored response accounting](gateway.md#stored-response-accounting).

Caller attribution labels can help filter and group usage. Keys explicitly allow
label names, and values must be bounded machine tokens. Labels never grant
access or change routing; see [request attribution](gateway.md#request-path).

## What is stored — and what never is

Durable diagnostics contain identifiers, timing, token/media units, status,
error classification, pricing provenance, and allowed attribution labels. They
exclude prompts, outputs, reasoning, tool payloads, uploads, raw request
headers, and credentials. Keep secrets out of operator names and labels too.

Uploaded media may occupy a bounded temporary spool during execution. Files,
batches, and opt-in stored Responses may retain content at the upstream
provider; OLP stores their ownership mappings and accounting metadata. This is
separate from telemetry privacy. See
[provider-retained state](compatibility.md#files-batches-realtime-and-provider-retained-state).

[Content policies](gateway.md#content-policy) inspect supported text in memory.
Only `{rule_id, phase, action, outcome}` decisions persist, without matched
text, offsets, patterns, or replacement strings. Unsupported surfaces refuse a
policy rather than bypassing it.

## Request lifecycle

1. Authenticate and check scope, project, route access, and expiry. Pin runtime
   configuration and enforce admission limits before calling a provider.
2. Select eligible targets and credential slots under the published policy and
   attempt budget.
3. Execute within target and route deadlines. Record each attempt; never fail
   over a committed stream or ambiguously created resource.
4. Complete accounting, release leases, and close streams, including on
   cancellation. Persist metadata without request or response content.
