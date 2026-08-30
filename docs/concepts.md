# Concepts

This document describes the model OpenLLMProxy presents to the people who
use it: what a route is, what a key is allowed to do, how a provider change
reaches traffic, and what the installation records about a request. It is
the user-facing counterpart to [Architecture](architecture.md), which
records the internal boundaries and runtime contracts, and to
[Configuration](configuration.md), which records the install-time settings
of the binary and its deployment.

## Routes and slugs

The `model` field of every gateway request is a route slug, never a provider
model identifier. Direct provider/model addressing is intentionally
unavailable: an installation publishes stable names, and the mapping from a
name to providers and upstream models stays configuration that operators
change without touching callers.

A slug is at most 63 bytes of lowercase ASCII letters, digits, and single
internal hyphens. It starts and ends with a letter or digit, and it never
contains two consecutive hyphens. Uppercase letters, underscores, dots, and
slashes are rejected, which is why a provider model identifier is not a
valid slug by accident.

A route binds its slug to the operations it serves and to one or more
targets. A target names a provider, the upstream model to call on that
provider, a priority, a weight, and its own timeout. The route itself
carries an overall timeout that no target timeout may exceed, and a bounded
maximum attempt count that cannot exceed the number of targets. A route with
no targets, a zero timeout, or a duplicated target is refused at
configuration time rather than at request time.

Visibility is default-deny. A capability is advertised only when the
endpoint policy exposes it and the calling key holds the matching route
permission, and model listing applies the same policy. A provider or model
that is configured but not explicitly made visible cannot become an
accidental discovery or routing path.

The slugs a key may use are listed by `GET /v1/models`, filtered by that
key's permissions, and in the console under Routes.

## Providers: drafts, revisions, certification

A provider is edited as a draft, and draft edits never affect the revision
that is serving traffic. Activation turns the draft into an immutable,
numbered revision holding the endpoint and cloud context, the credential
version, the enabled models, and the certified capabilities as they stood at
that moment. Between two activations, nothing a running request sees about a
provider changes.

Certification is server-owned. Native-provider activation requires a
certification for every exact provider/model/operation tuple, and a tuple
becomes certified only when a bounded probe through the production
connectors, deadlines, encoders, streaming decoders, and response codecs
succeeds. Probes store no prompt and no response content. Only an exact
successful probe earns a certified source and a certification timestamp;
generic OpenAI-compatible providers keep reviewed tuples as merely declared
until a per-model certification succeeds. At most 64 reviewed tuples and
eight concurrent probe requests are allowed, and unsafe media, video,
asynchronous, and cross-protocol claims fail closed.

Re-reviewing a set of tuples is a per-tuple diff: removed tuples lose their
evidence, unchanged tuples keep it, and newly added tuples start as
declared. Only transport edits — endpoint, region, project, deployment, API
version, authentication mode — reset every tuple to declared and clear the
stored probe. Renaming a provider and rotating its credential do not,
although a rotation still requires a fresh probe before the next activation.

## Runtime generations and pinning

Activating a change compiles the installation's configuration into a
byte-stable generation with a digest. Gateways verify that digest and
replace their snapshot atomically, so a half-applied generation is never
observable, and a repeated publication is harmless.

Each request pins exactly one generation and one credential revision when it
is admitted and keeps both for its entire lifetime, including a stream that
outlives several activations. A configuration change therefore never lands
mid-stream: it governs requests admitted after it, while a request already
in flight finishes against the configuration it started with.

## Keys, permissions, expiry, limits

Gateway keys belong to the installation. The secret is shown once, when the
key is created; the installation stores only its hash, so a lost secret is
replaced rather than recovered, and a revoked key is refused from then on.

Two scopes exist. `inference` permits the operations that call a provider:
generation, embeddings, moderation, image generation, editing and variation,
speech, transcription, token counting, and the video operations.
`models_read` permits listing and retrieving models. The mapping from
operation to required scope is exhaustive and positive, so a new operation
never inherits authorization merely by not being a model read, and neither
scope implies the other.

A key may carry a route allowlist. An empty allowlist places no route
restriction on the key; a non-empty one refuses any request naming a slug
outside it. A key may also carry an expiry, after which it is refused.

Hard limits are optional and independent of each other: requests per minute,
tokens per minute, and a maximum number of requests in flight at once. A key
with none of them is admitted without consulting the shared limit store at
all.

Enforcement uses the shared limit store's server time — never a gateway
process clock — for the windows, for lease expiry, and for the `Retry-After`
hint returned with a rejection. The request and token windows are fixed UTC
minutes, so a burst straddling a minute boundary is possible by design. The
token dimension is charged an estimate computed from the request before it
reaches a provider. A rejection consumes nothing: a request refused on one
dimension spends no part of any other. A key with at least one hard limit
fails closed when the limit store is unreachable or its state is malformed,
rather than being admitted unmetered. Concurrency leases are released
idempotently, and an abandoned lease expires on server time.

## Attempts, usage, pricing

Admission creates exactly one request identity. Every call to a provider is
an attempt with its own identity and a monotonic position within that
request, carrying the pinned generation, the provider revision, the model,
the operation, its deadline, and its outcome classification. Failover
appends the next attempt and never rewrites the previous one, so the record
of a request is the whole ordered sequence that was tried, not just the call
that happened to succeed.

Usage and cost attach to the exact attempt that produced them, which keeps
retries, provider changes, and partial streams auditable without storing any
request content. Missing upstream usage is recorded as incomplete and
unpriced, never as zero: a successful billable attempt with no observed
usage stays unpriced rather than free, and a failed attempt is retained
without inventing usage or cost for it.

Every request ends in exactly one terminal envelope, bounded independently
of how large the response was: status, timing, transport and error
classification, the full attempt list, usage completeness, and pricing
provenance. Cancellation takes the same terminal path — leases are released,
provider streams are closed, and the attempt facts already known are
persisted. A late provider callback cannot create a second terminal record,
because request and attempt uniqueness is enforced in storage.

## What is stored — and what never is

Durable request, attempt, and usage records contain identifiers, timing,
token or media units, status, error classification, and pricing provenance.
They are enough to answer what was called, when, how it ended, how much it
consumed, and what it cost.

They never contain prompts, responses, reasoning, tool arguments or results,
uploads, raw request headers, or credentials. Provider fields the canonical
model does not recognize stay in memory for the life of the request instead
of reaching storage, and certification probes store no prompt or response
content either. Nothing in the request or response body is recoverable from
an installation's own records after the request has finished.

## Request lifecycle

```text
   incoming request
        |
        v
  +-------------+  authenticate the key; check scope, route permission,
  |  admission  |  and expiry; reserve hard limits; enforce body caps
  +-------------+  -> one request identity, one bounded deadline
        |
        v
  +-------------+  pin one generation and one credential revision; order
  |  selection  |  the eligible certified targets by priority group, then
  +-------------+  by weight -> an attempt plan, capped by max attempts
        |
        v
  +-------------+  call the provider within the target timeout and the
  |  attempt 1  |  route's overall timeout; this attempt keeps its own
  +-------------+  usage, pricing, and outcome
        |
        |  failover appends attempts 2..n; it never rewrites an
        |  earlier attempt, and the plan bounds how many there can be
        v
  +-------------+
  |  attempt n  |
  +-------------+
        |
        v
  +-------------+  exactly one envelope, cancellation included: status,
  |  terminal   |  timing, error class, the full attempt list, usage
  +-------------+  completeness, pricing provenance -- never content
```
