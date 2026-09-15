# Go gateway: providers, routes, keys, and OpenAI inference

The Go gateway serves the OpenAI Chat Completions and Responses endpoints, the
model list, and the console playground against OpenAI and OpenAI-compatible
connections. Operators configure a connection, certify its models, publish a
route, and issue a key; SDK clients then use the route slug as the model name.
Shared limits, accrued-cost budgets, exact pricing, and durable usage
accounting are enforced and recorded; Anthropic and Gemini surfaces, other
provider kinds, media, and routing policies arrive later.
Installation and identity are covered in [Go installation and access
control](go-access.md).

## What is available

| Area | Available now | Deferred |
|---|---|---|
| Surfaces | `POST /v1/chat/completions`, `POST /v1/responses`, `GET /v1/models`, `GET /v1/models/{model}` | `/anthropic/v1/*` and `/gemini/*` return `501` until M5 |
| Provider kinds | `openai`, `openai_compatible` (`api_key`, `headers`, or `none` authentication) | Other kinds return `422 provider_kind_unavailable` |
| Operations | `generation` on the `openai` surface, unary and streaming | Other operations return `422 operation_unavailable`; media in M6 |
| Routing | Priority tiers with deterministic weighted selection, attempt budgets, deadlines | Routing policies read as defaults; writes return `501` until M5 |
| Limits | Key, connection, and credential-slot request/token/concurrency limits and daily/monthly cost budgets, enforced across replicas through Valkey | `olp_limits_fail_open_total` is counted in process but not exported until the metrics listener in M6 |
| Accounting | Durable requests and attempts, priced usage facts, request history, usage reports, completeness, pricing revisions, and retention | Media unit prices can be configured, but media operations arrive in M6 |

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only and answers `/v1/*` with `404`. `gateway` serves
inference only; it still needs the database, `OLP_AUTH_HMAC_KEY_FILE`, and
`OLP_MASTER_KEY_FILE` because it reads key authority and decrypts provider
credentials itself. `worker` publishes no public listener and runs only the
accounting and recovery plane described below, which `all` also runs; an
installation that serves traffic with `gateway` and `control` needs at least
one `worker` replica for accounting, budget reconciliation, and retention to
happen at all. `GET /health/ready` on the private listener reports an
`authority` dependency alongside PostgreSQL and Valkey; readiness fails while
the authority snapshot is missing or stale.

`GET /api/v3/auth/capabilities` answers `limits_enforced` and
`retention_enforced` from that same composition: both are true where
`OLP_VALKEY_URL` is configured, because admission needs the shared state and
the accounting plane starts only with it. Both report how this installation is
configured, not what is running right now — a `control` process cannot observe
whether a `worker` replica is alive — so an installation that configures shared
state and then runs no `worker` or `all` replica still reports
`retention_enforced` as true while nothing applies retention. The console uses
the flags to say whether stored limits and retention policies bind at all.

## Configuration

Every variable also exists as a flag (`olp --help`). Bounds are validated at
startup.

| Variable | Default | Purpose |
|---|---|---|
| `OLP_VALKEY_URL` | unset | Shared limits, cooldowns, and the request metadata stream. Required by `worker`; without it a gateway refuses every key or target that carries a limit and records no durable accounting. |
| `OLP_TRUSTED_PROXY_CIDRS` | empty | Proxies whose `X-Forwarded-For` supplies the client address recorded in diagnostics. |
| `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` | empty | Destination networks exempt from the non-public egress denylist. |
| `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` | empty | Hostnames or IP literals whose endpoints may use plain HTTP. |
| `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` | `256` | Inference work admission (1–100000); excess requests receive `503`. |
| `OLP_HTTP_MAX_JSON_BODY_BYTES` | `2097152` | Largest JSON request body before and after gzip inflation (64 KiB–64 MiB). |
| `OLP_PROVIDER_MAX_RESPONSE_BYTES` | `16777216` | Largest buffered unary provider response (1 MiB–256 MiB). |
| `OLP_PROVIDER_MAX_EVENT_BYTES` | `1048576` | Largest single streamed provider event (64 KiB up to the response cap). |

The browser and integration harnesses allow loopback egress and plain HTTP for
`127.0.0.1` so mock upstreams can be reached. Export the same two exceptions
before `make go-dev` to develop against a local mock; production deployments
should leave both empty.

## Provider egress

Endpoints must be absolute `https://` URLs without credentials, query strings,
or fragments; plain `http://` is accepted only for hosts listed in
`OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS`. Probes and inference validate the
endpoint against the current policy and use the same normalized base URL.
Before every probe and inference
attempt the host is resolved, every answer is checked against the denylist of
loopback, private, link-local, multicast, and other non-public ranges, and the
connection is pinned to the resolved addresses so a later DNS answer cannot
redirect traffic. Redirects are refused. Operators can exempt specific
networks with `OLP_PROVIDER_EGRESS_ALLOW_CIDRS`. Transports use TLS 1.2 or
newer, bounded dial and handshake timeouts, and a 32 KiB header limit.

## Provider lifecycle

A provider is created as a draft with its configuration and, unless the
authentication mode is `none`, a credential. The console wizard then:

1. **Probes** the connection (`POST /providers/{id}/probe`), which lists
   upstream models with bounded time, concurrency, and body size. Probe results
   store only a status, a timestamp, and a sanitized detail; upstream bodies
   never enter persistent diagnostics.
2. **Discovers** models (`POST /providers/{id}/discovery`), either from the
   upstream list or from up to 2000 declared identifiers. Discovered models
   start disabled with no capabilities.
3. **Reviews** capabilities (`PATCH /providers/{id}/models/{model_id}`),
   which records *declared* tuples of operation, surface, and mode.
4. **Certifies** each model (`POST /providers/{id}/models/{model_id}/certify`),
   which sends a minimal generation request per tuple and marks the tuples
   that succeed as *certified*. Each probe has a 15-second budget; the
   certification request allows 45 seconds for both probes and management
   work, including saving the evidence. Only certified tuples of enabled
   models are published to the runtime and are eligible for routes.
5. **Activates** the draft (`POST /providers/{id}/activate`), which validates
   the configuration, requires current validation for each selectable
   credential slot and at least one enabled, fully certified model, writes
   an immutable revision, and publishes a new runtime generation.

Draft edits never change serving traffic: they mark the provider as having a
pending activation, and the runtime keeps using the active revision. Changing
transport details (kind, authentication mode, endpoint, credential headers)
invalidates certification evidence and slot validation, so tuples must be
certified again before the next activation. Revisions can be listed, read,
compared, and restored as a new draft; restoring copies the recorded models
and evidence but never a historical credential.

Disabling a provider (`POST /providers/{id}/disable`) publishes a generation in
which the provider is not selectable; streams that already started keep the
snapshot they were admitted with.

## Credentials and slots

Credentials are write-only. Each provider has a default slot and up to 64
slots in total; a slot carries a priority, a weight, an enabled flag,
optional model, route, and key allowlists, and the request, token, and
concurrency limits enforced for that slot. Rotation
(`POST /providers/{id}/credentials`) validates the new secret against the
upstream model list and the default slot's allowed enabled model capabilities
before storing a new version and selecting it for the draft.
The active revision keeps the version it was activated with until the provider
is activated again. Slot validation
(`POST /providers/{id}/credential-slots/{slot_id}/validate`) probes every allowed
enabled model capability with that slot's credential, including unary and
streaming generation. Model-list access alone does not validate generation
access. Validation and rotation requests allow 45 seconds overall, with a
15-second limit per upstream probe.

Evidence is tied to the credential version, transport configuration, and
allowed enabled model capabilities. Changing those inputs requires matching
validation before activation; edits made during a probe cause its result to
be rejected. Certifying all required models with the current default
credential also validates the default slot. Newly added or rotated pool slots
must be validated separately. Disabled slots and slots with no allowed enabled
models cannot be selected and do not block activation. Connections using
`auth_mode: none` do not require credential evidence.

Revoking a credential version (`POST /providers/{id}/credentials/{credential_id}/revoke`)
is authority state: gateways learn about it through the same five-second
authority poll as key revocation and stop selecting the version immediately,
even from retained releases, without waiting for a new provider activation.

## Routes

Route drafts carry a slug, the allowed operations (`generation`), an overall
deadline, a maximum attempt count, and ordered targets with priority, weight,
and per-attempt timeout. Every target must reference a published model with
certified support for each allowed operation; validation and activation reject
unknown, inactive, unpublished, or uncertified targets. Drafts are versioned
with ETags, so stale edits return `412` and the console offers a reload.

Activating a draft writes an immutable route revision, makes it the latest
revision of the route named by the slug, and publishes a runtime generation.
Revisions can be compared and restored as new drafts. The simulation endpoints
(`POST /route-drafts/{id}/simulate` and `POST /routing/simulate`) explain the
deterministic attempt order for a given seed or key without contacting any
provider.

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

Requests authenticate with `Authorization: Bearer <key>`; the key needs the
`inference` scope for generation and `models_read` for model listing, and a
route allowlist restricts both. The `model` field must be a published route
slug; the model list and retrieval expose only routes the key may use.

Each request receives an `X-Request-Id` (a client-supplied value is kept when
it is a safe token), a no-store cache policy, and permissive CORS headers so
browser SDKs can call the gateway. Bodies must be `application/json`,
optionally gzip-compressed, and within the JSON body limit before and after
inflation. Uploads have a 15-second read deadline; incomplete bodies receive
`408 request_timeout` and release their admission slot. The upload deadline
ends when the body is read, before the route's inference deadline starts.
The optional `X-OLP-Routing` header accepts
`{"strategy":"weighted","max_attempts":N}`; other strategies and preferences
return `400` until M5.

Attempts follow the route's priority tiers and deterministic weighted order,
seeded by the key so the same key sees a stable order. Each attempt consumes
the budget, uses the target's timeout within the route's overall deadline,
injects the selected slot's credential, and rewrites the model to the
upstream identifier while preserving unknown request fields. Failover to the
next eligible attempt happens only before any response bytes have been sent
to the client and only for connect, timeout, rate-limit, credential, and
upstream server failures; upstream client errors, protocol errors, and
cancellations are terminal. A committed stream never restarts on another
provider: a later failure is reported in-band as an error event and the
stream ends without `[DONE]`. Stream forwarding retains no cumulative output or
tool arguments. Distinct chat-choice tracking is bounded to
`max(1, OLP_PROVIDER_MAX_EVENT_BYTES / 16)` entries per stream.

Provider health is tracked per gateway: five counted failures within 30
seconds open a provider's circuit for 30 seconds, a credential rejection
cools the slot for 60 seconds, and a rate limit cools it for the upstream
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
default, times `n`) — and reserves the key's requests-per-minute,
tokens-per-minute, and concurrency windows and its cost budgets together. The
lease is sized by the route's overall deadline; it is the backstop for a replica
that dies mid-request, not the request deadline. An estimate larger than the
key's tokens-per-minute limit is refused immediately with
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

Rate-limit and credential failures also record a cooldown for the credential
version and for the slot in Valkey, so other replicas skip a target the upstream
just rejected instead of each learning it alone. The per-gateway circuit and
cooldowns described above still apply. An unreadable cooldown is treated as
absent.

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
process, but no metrics endpoint exports the count yet.

## Shared state in Valkey

Every key is prefixed with the installation namespace
`olp:go:v1:<installation>:`, so installations sharing one Valkey service never
read, acknowledge, or reconcile one another's state.

| Key | Contents |
|---|---|
| `<prefix>limits:{<lookup>}:rate` | Request and token windows for one lookup. |
| `<prefix>limits:{<lookup>}:concurrency:v2` | Concurrency leases for one lookup. |
| `<prefix>limits:{<api key>}:cost:day` and `:cost:month` | Accrued spend and unpriced attempt counts for the current UTC windows. |
| `<prefix>limits:provider-cooldown:<scope>` | Credential-version and slot cooldowns. |
| `<prefix>request-metadata` | The request metadata stream, read by consumer group `olp:persistence`. |

A lookup is the key's lookup identifier, `pc_<provider uuid>` for a connection,
or `ps_<slot uuid>` for a credential slot; the braces are the cluster hash tag,
so one key's dimensions stay on one slot. Cost keys are tagged by the API key
itself, so every lookup of one key meets the same balance.

## Accounting and the worker plane

Every request an API key owns produces one content-free metadata event;
playground traffic has no key and is not accounted for. Inference
processes buffer up to 8192 events and write them to the installation stream;
the buffer never blocks a request, and an overflow is counted as loss rather
than paid for in latency. Events carry identifiers, timing, token counts, and
per-attempt evidence only — never prompts, outputs, tool data, or headers.

`worker` and `all` processes run four tasks, each checkpointing its own liveness
into the worker health table:

- **Request metadata consumer.** Reads the stream on its own Valkey connection,
  replays its own pending entries before reclaiming another consumer's idle
  deliveries, and acknowledges and deletes an entry only after the event is
  durable. A duplicate delivery is recorded as a duplicate and stored once. An
  event whose wire version this build does not support stays pending, never
  acknowledged or deleted. Malformed payloads, invalid events, and deliveries
  whose payload is gone are recorded once as explicit gaps and then drained. A
  Valkey acknowledgement is not an fsync guarantee.
- **Gateway epoch detection.** Turns a replica that stopped without closing its
  epoch into a recorded, visible gap after two confirming passes.
- **Maintenance.** Every 60 seconds, under an advisory lock on one dedicated
  session, rolls usage facts additively into hourly buckets before purging them
  so spend reconstruction stays exact, then purges requests, receipts, audit
  rows, gap rows, epochs, sessions, invitations, replays, and expired OIDC flows
  according to the stored `retention.*` settings.
- **Cost reconciliation.** Every 60 seconds, rebuilds current daily and monthly
  totals from durable facts and publishes them to Valkey. It never lowers a
  valid counter and never initializes a window its snapshot does not match. One
  leader holds a session advisory lock between passes; other replicas report a
  skipped pass. An error, cancellation, or the 120-second pass deadline drops
  and closes that session rather than returning a locked connection to the pool.

Management processes serve the results: the usage summary, breakdown,
time series, and completeness endpoints under `/api/v3/usage/`, request listing
and detail under `/api/v3/requests`, pricing revisions under
`/api/v3/pricing/revisions`, and gateway epochs and their acknowledgement under
`/api/v3/request-metadata/gateway-epochs`. Reports mark a partial boundary
bucket as approximate and report what they excluded, and carry gap evidence and
consumer health so incompleteness stays visible after aggregation.

Shutdown stops the listeners and drains their handlers first, then closes
metadata intake and gives the writer a bounded opportunity to flush the buffer.
Only afterwards are delivery and worker contexts cancelled. An expired flush
budget records undelivered events as loss; a forced HTTP shutdown leaves the
gateway epoch open for detection because handlers may still emit metadata.
A clean drain closes the epoch against what was actually delivered. Each stage gets the
`OLP_SHUTDOWN_TIMEOUT` budget (5 seconds by default, 10 minutes at most); a
stage that outlives it is logged and left to its own bounded cleanup.

## Diagnostics

Every request ends in exactly one terminal envelope logged as
`inference request`: request ID, actor (key or playground user), route and
revision, release sequence, family, mode, outcome, status, whether the
response was committed, timing, observed usage when the upstream reported it,
and one record per attempt with target, provider revision, slot, credential
version, status class, and timing. Prompts, outputs, tool data, headers, and
credentials are never included. `GET /api/v3/provider-health` summarizes the
same facts per provider for the requested window. The same envelope is the
source of the durable request, attempt, and priced usage records described
above.

## Local development and qualification

`make go-check` runs formatting, vet, the Go unit suites (egress corpus,
OpenAI codecs, runtime selection, and gateway lifecycle scenarios), and the
console checks. `make go-integration` builds the binary and runs the
PostgreSQL-backed scenario that walks from an empty installation to SDK
traffic, the official OpenAI SDK smoke checks against the Go fixture, and the
browser journeys at the packaged and Vite origins. The integration suites
include the Valkey limit scenarios, accounting and consumer recovery, and the
two-replica and two-installation process scenarios; the browser journeys include
the accounting journey, which prices real gateway traffic and reads back the
request explorer, usage reports, and a key's accrued spend. Both browser
journeys use `console/tests/gateway/mock-openai.mjs` as their upstream.
