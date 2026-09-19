# Gateway execution

The Go gateway serves the OpenAI, Anthropic, and Gemini protocols through
one executor, including bounded media uploads, image and audio operations,
and durable video jobs. Operators configure a connection, certify its models,
publish a route, and issue a key; SDK clients use the route slug as the model
name. Distributed limits, accrued-cost budgets, exact pricing, durable
accounting, and policy-constrained routing apply across every surface.
Installation and identity are covered in
[installation and access control](access.md).

The [compatibility matrix](compatibility.md) owns endpoint support and translation
limits. [Provider connections and routing](provider-routing.md) covers onboarding,
credential pools, publication, and selection policy.

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials; enabled named pools require it. Mounted releases must contain the
published default-slot ID; republish older Go releases before enabling this mode.
`worker` publishes no public listener and runs only the accounting, media
reconciliation, and [recovery plane](operations.md#replicated-worker-health),
which `all` also runs; an installation that serves traffic with `gateway` and
`control` needs at least one `worker` replica for accounting, budget reconciliation, retention, and
media job polling to happen at all. Gateway readiness on the private
`GET /health/ready` listener fails while the key-authority snapshot is missing
or stale.

## Configuration

Use the [configuration reference](configuration.md) for flags, defaults, bounds,
egress, and mounted connectors, and [access control](access.md) for secret files
and maintenance commands. Local mock providers need explicit loopback and HTTP
[egress exceptions](configuration.md#test-and-harness-variables).

## Runtime publication and authority

Every provider or route activation compiles the complete set of active
providers and latest route revisions into one snapshot, validates it, records
its SHA-256, and stores it as a numbered runtime release. Gateways install the
newest release only when it validates and every referenced credential can be
decrypted; a release that fails either check is skipped and the previous one
stays active. Repeated publication is harmless.

Key authority (API keys, expiry, revocation, and revoked credential versions)
is polled every five seconds independently of release installation. Authority
older than 60 seconds, measured from the start of the last successful read,
is stale: new requests are rejected with `503 authority_unavailable` while
requests already admitted keep the snapshot and policy they were pinned to.

## Request path

Requests authenticate with `Authorization: Bearer <key>`, Anthropic
`X-Api-Key`, or Gemini `X-Goog-Api-Key` (the Gemini query-key form is also
accepted). Every inference operation needs `inference`; model reads need
`models_read`. A route allowlist restricts both. The body model, or Gemini
URL model, must be a published route slug. Model list/get expose only those
routes the key may use.

Each request receives an `X-Request-Id` (a client-supplied value is kept when
it is a safe token), a no-store cache policy, and CORS headers for explicitly
allowed browser origins (`OLP_GATEWAY_CORS_ALLOWED_ORIGINS`). Bodies must be
`application/json`,
optionally gzip-compressed, and within the JSON body limit before and after
inflation; the media endpoints also accept raw and `multipart/form-data`
bodies bounded by `OLP_HTTP_MAX_MEDIA_BODY_BYTES` and the spool's per-endpoint
reservations. Uploads have a 15-second read deadline; incomplete bodies receive
`408 request_timeout` and release their admission slot. The upload deadline
ends when the body is read, before the route's inference deadline starts.
The optional `X-OLP-Routing` header accepts one JSON object, for example
`{"strategy":"price","allow_fallbacks":false,"max_attempts":1}`.
It can narrow constraints and attempts within published policy, never expand
access or deadlines. Unknown controls, invalid selectors, and budget increases
are refused. Raw header values are never forwarded or persisted.

Attempts follow priority and preferred-order tiers, then the selected strategy.
Weighted ties are seeded by the key so the same key sees a stable order. Each attempt consumes
the budget, uses the target's timeout within the route's overall deadline,
injects the selected slot's credential, and rewrites the model to the
upstream identifier. Native calls preserve unknown request fields; translation
refuses semantic extensions it cannot represent. Failover to the
next eligible attempt happens only before any response bytes have been sent
to the client and only for connect, timeout, rate-limit, credential, and
upstream server failures; upstream client errors, protocol errors, and
cancellations are terminal. A committed stream never restarts on another
provider: a later failure is reported in-band as an error event and the
stream ends without a success marker. Translation and native tool validation
bound retained text/tool state by the event-size limit. Distinct native Chat
choice tracking is bounded to `max(1, OLP_PROVIDER_MAX_EVENT_BYTES / 16)`
entries. Bedrock advertised event lengths are checked before SDK allocation,
and the SDK still verifies event CRCs.

Provider health is tracked per gateway: five counted failures within 30
seconds open a provider's circuit for 30 seconds. One half-open probe may
proceed; credential-only failure releases it without penalizing siblings.
A credential rejection cools that credential version for 60 seconds; a rate
limit cools the logical slot across rotation for the upstream
`Retry-After` (10 seconds when absent, at most 60 seconds). Client
cancellation and disconnects close the upstream request and release admission
once. Unary response writes and individual stream frames have a 30-second
write deadline. Failed unary writes or flushes are recorded as cancellation;
successful delivery is recorded only after the response has been flushed.

| Status | `error.code` | Meaning |
|---|---|---|
| 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `unsupported_parameter`, `unsupported_stateful_reference`, `request_exceeds_token_limit` | Request envelope problems, including unsupported Responses state references and an estimate larger than the key's tokens-per-minute limit. |
| 401 | `invalid_api_key` | Missing, unknown, expired, or revoked key. |
| 403 | `permission_denied`, `route_forbidden` | Scope missing or route outside the key's allowlist. |
| 404 | `route_not_found`, `not_found` | Unknown route slug or endpoint. |
| 408 | `request_timeout` | Request body not received within 15 seconds. |
| 413 / 415 | `request_too_large`, `unsupported_media_type`, `unsupported_content_encoding` | Body limits and content negotiation. |
| 429 | `rate_limit_exceeded`, `budget_exhausted`, `upstream_rate_limit` | The key's requests, tokens, or concurrency limit was exceeded; the key's daily or monthly cost budget is exhausted; or every attempt was rate limited upstream. `Retry-After` carries whole seconds. |
| 502 | `upstream_unavailable`, `upstream_rejected`, `upstream_authentication_failed`, `upstream_permission_denied`, `provider_protocol_error` | Upstream or transport failures after the budget is spent. |
| 503 | `authority_unavailable`, `request_admission_overloaded`, `distributed_limits_unavailable`, `upstream_unavailable` | Stale authority, admission limit, limits that cannot be enforced, or no eligible target. |
| 504 | `gateway_timeout` | Route deadline reached before commitment. |

## Media and durable video jobs

The media endpoints — `/v1/images/generations`, `/v1/images/edits`,
`/v1/images/variations`, `/v1/audio/speech`, `/v1/audio/transcriptions`, and
the `/v1/videos` family — share the same authentication, route-slug, limits,
and accounting contracts as generation. Multipart and raw uploads reserve
capacity inside `OLP_HTTP_MAX_MEDIA_BODY_BYTES` and per-endpoint fractions of
the spool; oversized bodies, excessive parts, and insufficient capacity fail
predictably, and cancellation releases the reservation and its files. Restart
cleanup never removes live work, and uploaded content stays inside its bounded
operational lifetime — it never enters persistent request diagnostics.

Video creation is asynchronous: an admitted request writes a durable job
record pinning the route, provider, slot, credential, and price revisions, so
a client disconnect cannot erase upstream work and an ambiguous create never
triggers a blind retry or duplicate. Jobs cannot cross API-key ownership
boundaries. The worker plane's media reconciler polls claimed jobs to
completion, resolves the pinned historical credential through rotation and
provider changes, obeys explicit revocation guards, and finishes accounting.
`GET /v1/videos`, `GET /v1/videos/{video_id}`,
`GET /v1/videos/{video_id}/content`, and `DELETE /v1/videos/{video_id}` expose
the durable record, which the management `/api/v3/media-jobs` reads mirror for
operators. The console provides metadata-only list/detail views, filters, and
manual refresh. It has no automatic polling, content download/delete controls,
video cancellation workflow, or media playground controls. Content and deletion
remain API-key-owned inference operations; request cancellation and backend job
reconciliation are separate from these console controls.

## Limits and budgets

Enforcement needs `OLP_VALKEY_URL`. Every counter lives in Valkey and is
evaluated by a Lua script on the server's clock, so replicas share one decision
rather than each keeping its own. Without it there is no admission backend at
all: a key that bounds nothing is served as before, a key that carries any
limit is refused with `503 distributed_limits_unavailable`, and a target with a
configured quota is skipped rather than used unmetered.

A request is admitted once it is authenticated, parsed, and routed, and before
any provider is called. The gateway estimates the tokens the request may use —
text at four characters per token across messages, tool calls, and tool schemas,
a flat charge per inline image or media part, plus the largest reply the caller
allowed (`max_completion_tokens`, `max_tokens`, or `max_output_tokens`, 4096 by
default, times `n`) — for every attempt the request permits, and reserves the
key's requests-per-minute, tokens-per-minute, and concurrency windows and its
cost budgets together. The lease is sized by the route's overall deadline; it
is the backstop for a replica that dies mid-request, not the request deadline.
An estimate larger than the key's tokens-per-minute limit is refused immediately with
`400 request_exceeds_token_limit` rather than sent to retry into a window it can
never fit.

Each attempt then reserves the provider connection quota and the credential
slot quota, with concurrency leases covering the remaining overall route
deadline. An attempt's first-byte or idle timeout does not bound a stream's
total lifetime. A rejected slot refunds the
connection reservation it already took, the attempt is recorded as a rate-limit
failure with its `Retry-After`, and failover continues to the next target. When
a request ends, a reservation that dispatched nothing is refunded in full;
otherwise the token reservation is reconciled against the usage the upstream
reported and the concurrency lease is released. Settlement ignores client
cancellation, so a caller that hangs up still returns its slot.

Credential failures record a version-scoped cooldown in Valkey; rate limits
record a logical-slot cooldown that survives rotation. Shared cooldown state
is authoritative when available, and successful credential validation clears
it. Without shared coordination, the gateway uses its local cooldowns. An
unreadable shared cooldown is treated as absent.

Cost budgets are accrued-spend, not reserved-spend: daily and monthly windows
use UTC boundaries and exact decimals, concurrent admitted work can exceed a
threshold, and an attempt nobody could price accrues nothing — which is what
`budget_exhausted` says. A budget is enforced only against an authoritative
snapshot published by the worker plane. Missing, malformed, or wrong-window
spend state is unknown spend, not zero, and answers
`503 distributed_limits_unavailable` until the next reconciliation pass
publishes the window. See [spend-budget initialization and
recovery](spend-budget-recovery.md).

When Valkey is configured but unreachable, the `limits.valkey_unavailable`
installation setting decides what happens to keys limited only by rate or
concurrency: `fail_closed` (the default) answers
`503 distributed_limits_unavailable`, and `fail_open` admits them without those
dimensions and logs a warning per request. A key with any cost budget always
fails closed. The setting is read once before the listener binds and polled
every 15 seconds, and an unreadable setting keeps the policy already in force.
A target whose configured quota cannot be consulted is skipped so a sibling can
serve; a request that exhausts every target that way ends
`503 distributed_limits_unavailable`. Fail-open admissions are counted in
`olp_limits_fail_open_total` on the private `/metrics` listener.

## Diagnostics

Every request ends in exactly one terminal envelope logged as
`inference request`: request ID, actor (key or playground user), route and
revision, release sequence, family, mode, outcome, status, whether the
response was committed, timing, observed usage when the upstream reported it,
and one record per attempt with target, provider revision, slot, credential
version, status class, and timing. Prompts, outputs, tool data, headers, and
credentials are never included. `GET /api/v3/provider-health` summarizes the
same facts per provider for the requested window, `GET /api/v3/health/ready`
serves the cached readiness snapshot, and `GET /api/v3/media-jobs` lists
and reads durable video jobs. The same envelope is the
source of the durable request, attempt, and priced usage records described
in [accounting delivery](operations.md#accounting-delivery-and-shutdown).

Readiness, worker checkpoints, metadata delivery, tracing, and shutdown are
documented in the [operations runbook](operations.md). Keep health and metrics
on the private listener. `olp health-probe` exits nonzero unless readiness passes.

See [tests/README.md](../tests/README.md) for deterministic protocol, SDK,
service, and browser checks. Paid provider qualification is separate.
