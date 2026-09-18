# Go gateway: providers, native protocols, and routing

The Go gateway serves the OpenAI, Anthropic, and Gemini protocols through
one executor, including bounded media uploads, image and audio operations,
and durable video jobs. Operators configure a connection, certify its models,
publish a route, and issue a key; SDK clients use the route slug as the model
name. Distributed limits, accrued-cost budgets, exact pricing, durable
accounting, and policy-constrained routing apply across every surface.
Installation and identity are covered in
[Go installation and access control](go-access.md).

## What is available

| Area | Available now |
|---|---|
| OpenAI | Chat Completions, Responses, Responses input counting, embeddings, moderation, images, audio, durable videos, and gateway-owned models under `/v1` |
| Anthropic | Messages, streaming, counting, and gateway-owned models under `/anthropic/v1` |
| Gemini | Generation, streaming, counting, and gateway-owned models under both `/gemini/v1` and `/gemini/v1beta` |
| Providers | OpenAI, compatible vendors, Anthropic, Gemini, Azure OpenAI, Vertex AI, and Bedrock; native/cloud authentication and custom endpoints |
| Routing | Installation, route, key, and request policies; priority/order tiers; weighted, price, latency, and throughput strategies; credential pools and shared previews |
| Limits | Key, connection, and slot request/token/concurrency limits and daily/monthly accrued-cost budgets across replicas |
| Accounting | Durable requests/attempts, exact prices, completeness, history/reports, retention, and recovery |
| Media | Image generation, editing, and variation; speech and transcription; durable video jobs with polling, content, and deletion; bounded multipart and spool admission |
| Observability | Private `/health/live`, `/health/ready`, and `/metrics` listener, worker and media checkpoints, optional OTLP/HTTP tracing, and management health reads |

The [compatibility matrix](compatibility.md) specifies certified combinations
and translation refusals. Deterministic SDK/cloud/browser and build evidence is
recorded in [M5 qualification](roadmap/evidence/provider-and-routing-parity.md);
media failure-path and product evidence closes with M6-09 in the
[roadmap](roadmap/06-media-and-console-parity.md).

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials; enabled named pools require it. Mounted releases must contain the
published default-slot ID; republish older Go releases before enabling this mode.
`worker` publishes no public listener and runs only the accounting, media
reconciliation, and recovery plane described below, which `all` also runs; an
installation that serves traffic with `gateway` and `control` needs at least
one `worker` replica for accounting, budget reconciliation, retention, and
media job polling to happen at all. `GET /health/ready` on the private
listener reports an `authority` dependency alongside PostgreSQL and Valkey;
readiness fails while the authority snapshot is missing or stale.

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

Every variable also exists as a flag (`olp <mode> --help`). Bounds are
validated at startup.

| Variable | Default | Purpose |
|---|---|---|
| `OLP_DATABASE_URL` | required | PostgreSQL URL; `OLP_DATABASE_URL_FILE` reads it from a mounted file instead. |
| `OLP_DATABASE_MAX_CONNECTIONS` | `20` | PostgreSQL pool capacity (1–10000). |
| `OLP_VALKEY_URL` | unset | Shared limits, cooldowns, and the request metadata stream. Required by `worker`; without it a gateway refuses every key or target that carries a limit and records no durable accounting. `OLP_VALKEY_URL_FILE` reads it from a mounted file instead. |
| `OLP_VALKEY_TLS_CA_FILE` | unset | PEM trust roots for Valkey TLS; requires `OLP_VALKEY_URL`. |
| `OLP_LISTEN_ADDR` | `127.0.0.1:8080` | Public listener; must not overlap the observability listener. |
| `OLP_OBSERVABILITY_LISTEN_ADDR` | `127.0.0.1:9090` | Private health and metrics listener. |
| `OLP_PUBLIC_ORIGIN` | `http://127.0.0.1:8080` | Exact browser origin for OIDC and generated links. |
| `OLP_CONSOLE_DIR` | `console/build` | Static console directory. |
| `OLP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` for the JSON log stream. |
| `OLP_CONNECTOR_CONFIG_FILE` | unset | Shared providers-list configuration with restricted credential files; preserves published capabilities, quotas, and default-slot restrictions. |
| `OLP_TRUSTED_PROXY_CIDRS` | empty | Proxies whose `X-Forwarded-For` supplies the client address recorded in diagnostics. |
| `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` | empty | Destination networks exempt from the non-public egress denylist. |
| `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` | empty | Hostnames or IP literals whose endpoints may use plain HTTP. |
| `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` | `256` | Inference work admission (1–100000); excess requests receive `503`. |
| `OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS` | `32` | Management and console work admission (1–100000). |
| `OLP_HTTP_MAX_JSON_BODY_BYTES` | `2097152` | Largest JSON request body before and after gzip inflation (64 KiB–64 MiB). |
| `OLP_HTTP_MAX_MEDIA_BODY_BYTES` | `67108864` | Largest raw or multipart media request body (1 MiB–1 GiB); must stay within half of the spool capacity. |
| `OLP_MEDIA_SPOOL_DIR` | unset | Bounded media staging directory; defaults to the system temp directory. |
| `OLP_MEDIA_SPOOL_CAPACITY_BYTES` | `1073741824` | Media spool capacity (at least 256 MiB). |
| `OLP_PROVIDER_MAX_RESPONSE_BYTES` | `16777216` | Largest buffered unary provider response (1 MiB–256 MiB). |
| `OLP_PROVIDER_MAX_EVENT_BYTES` | `1048576` | Largest single streamed provider event (64 KiB up to the response cap). |
| `OLP_OTLP_TRACES_ENDPOINT` | unset | Complete OTLP/HTTP traces endpoint; unset disables tracing. |
| `OLP_OTLP_HEADERS_FILE` | unset | Mounted JSON object of OTLP exporter headers, read only when tracing is enabled. |
| `OLP_TRACE_SAMPLE_RATIO` | `1.0` | Sampling ratio from `0.0` through `1.0` for locally rooted traces. |
| `OLP_TRACE_PROPAGATE_UPSTREAM` | `true` | Inject the current W3C trace context into provider attempts. |
| `OLP_TRACE_ACCEPT_INBOUND` | `true` | Accept a valid inbound W3C trace context as the request parent. |
| `OLP_DEPENDENCY_REQUEST_TIMEOUT` | `2s` | Dependency request deadline (1ms–1m). |
| `OLP_STARTUP_TIMEOUT` | `10s` | Startup deadline (1ms–1m). |
| `OLP_SHUTDOWN_TIMEOUT` | `30s` | Shared shutdown deadline (1ms–10m). |

The mounted secrets (`OLP_AUTH_HMAC_KEY_FILE`, `OLP_MASTER_KEY_FILE`,
`OLP_BOOTSTRAP_TOKEN_FILE`) and the `migrate`, `doctor`, and `master-key`
commands are covered in
[Go installation and access control](go-access.md).

The browser and integration harnesses allow loopback egress and plain HTTP for
`127.0.0.1` so mock upstreams can be reached. Export the same two exceptions
before `make dev` to develop against a local mock; production deployments
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
authentication mode is `none`, `adc`, or `default_chain`, a credential. The console wizard then:

1. **Probes** the connection (`POST /providers/{id}/probe`), which lists
   upstream models or proves a configured deployment/model when that vendor
   has no model-list API, with bounded time, concurrency, and body size. Probe results
   store only a status, a timestamp, and a sanitized detail; upstream bodies
   never enter persistent diagnostics.
2. **Discovers** models (`POST /providers/{id}/discovery`), either from the
   upstream list or from up to 2000 declared identifiers. Discovered models
   start disabled with no capabilities.
3. **Reviews** capabilities (`PATCH /providers/{id}/models/{model_id}`),
   which records *declared* tuples of operation, surface, and mode.
4. **Certifies** each model (`POST /providers/{id}/models/{model_id}/certify`),
   which proves each operation/surface/mode tuple and marks successful tuples
   as *certified*. An OpenAI-surface generation capability proves both Chat
   and Responses unless the vendor profile explicitly translates Responses
   through Chat. Each probe has a 15-second budget, within a one-minute
   model-certification deadline. Only certified tuples of enabled
   models are published to the runtime and are eligible for routes.
5. **Activates** the draft (`POST /providers/{id}/activate`), which validates
   the configuration, requires current validation for each selectable
   credential slot and at least one enabled, fully certified model, writes
   an immutable revision, and publishes a new runtime generation.

Draft edits never change serving traffic: they mark the provider as having a
pending activation, and the runtime keeps using the active revision. Changing
transport or semantic details (kind, authentication mode, endpoint, cloud
addressing, credential headers, parameter defaults, model facts, vendor)
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
access. Applicable-model validation has a one-minute bound, with a 15-second limit
per upstream probe.

Evidence is tied to the credential version, transport configuration, and
allowed enabled model capabilities. Changing those inputs requires matching
validation before activation; edits made during a probe cause its result to
be rejected. Certification prefers an enabled default slot, then another
enabled usable slot; its evidence binds the actual selected credential.
Certifying all applicable models validates that selected slot. Newly added or rotated pool slots
must be validated separately. Disabled slots and slots with no allowed enabled
models cannot be selected and do not block activation. Connections using
`auth_mode: none`, `adc`, or `default_chain` do not require stored secrets.
A disabled default does not prevent an independently validated named slot
from serving.

Revoking a credential version (`POST /providers/{id}/credentials/{credential_id}/revoke`)
is authority state: gateways learn about it through the same five-second
authority poll as key revocation and stop selecting the version immediately,
even from retained releases, without waiting for a new provider activation.

## Routes

Route drafts carry a slug, allowed operations (default `generation`; explicit
`token_count`, `embeddings`, `moderation`, `image_generation`, `image_edit`,
`image_variation`, `speech`, `transcription`, `video_create`, `video_list`,
`video_get`, `video_content`, and `video_delete` are also supported), an overall
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

## Routing policies and evidence

`GET/PUT /api/v3/routing-policies/{scope}/{id}` owns installation (nil UUID),
route-draft, and API-key policy. Installation/key writes publish immediately;
route policy stays staged until activation and belongs to the immutable route
revision. ETags, scope permissions, audit, and idempotency apply.

Hard constraints intersect every scope, including defaults and request:
provider/vendor allow/ignore lists, regions, quantizations, price ceilings,
required parameters, data-collection denial, and zero-data-retention evidence.
Unknown facts fail affirmative requirements. Allowed strategies intersect;
preference precedence is request, key, route, installation.

Price ordering uses exact decimal rates and pins the selected revision or
explicit unpriced state. Successful persisted attempts from the last five
minutes supply latency/throughput after twenty qualifying samples. Streaming
latency starts at meaningful output; throughput excludes reasoning tokens and
needs twenty known output counts. Inputs refresh every ten seconds and expire
after sixty seconds without refresh. Unknown prices/measurements sort last;
equal or wholly unknown evidence falls back to weighted order.

Both simulators, playground, and execution share this selection engine.
Preview enumerates credential-slot attempts within the route budget and
explains constraints, strategy, prices, measurements, and current revocation.
Live cooldown/capacity can change after preview. See [provider routing](provider-routing.md)
for exact selector and preference semantics.

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
it is a safe token), a no-store cache policy, and permissive CORS headers so
browser SDKs can call the gateway. Bodies must be `application/json`,
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
operators.

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

`worker` and `all` processes run five tasks, each checkpointing its own liveness
into the worker health table:

- **Media reconciliation.** Claims durable video jobs in turn, polls their
  upstream provider through the pinned historical credential and connection,
  records completion or deletion evidence, and finishes the job's accounting.
  It checks the live credential-revocation authority before each upstream call
  and survives worker restarts by re-claiming pending jobs.
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
A clean drain closes the epoch against what was actually delivered. HTTP,
metadata, delivery, workers and trace flushing share `OLP_SHUTDOWN_TIMEOUT`
(30 seconds by default). Forced closure records uncertainty instead of
extending the deployment termination budget.

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
above.

The private observability listener (default `127.0.0.1:9090`) serves
`GET /health/live`, `GET /health/ready`, and `GET /metrics`. Readiness reports
each configured dependency — PostgreSQL, Valkey, the authority snapshot,
provider transport validity, worker checkpoints, and media reconciliation
gaps — and stays honest about stale or absent data rather than reporting a
healthy unknown. `olp health-probe` exits nonzero unless readiness passes.
When `OLP_OTLP_TRACES_ENDPOINT` is set, requests and provider attempts also
emit bounded OTLP/HTTP spans carrying identifier, classification, timing,
usage, and pricing attributes only; exporter headers come from the mounted
headers file and never reach providers. Keep this listener private.

## Local development and qualification

`make check` runs formatting, vet, Go unit/protocol suites, and console
checks. `make integration` builds the binary, runs real PostgreSQL/Valkey
and process scenarios, exercises all seven connector kinds and the retained
tuples and refusals including media, runs the official OpenAI, Anthropic, and
Google GenAI SDKs, and runs Chromium journeys at packaged and Vite origins.
Identity and provider responses are deterministic local fixtures, not paid
cloud qualification.

The browser journeys include accounting plus cloud configuration, bulk model
certification, grouped routes, credential pools, policy exclusions, preview,
publication, and playground execution. They use the existing local OpenAI and
Azure fixtures. [M5 evidence](roadmap/evidence/provider-and-routing-parity.md)
records pinned SDKs, qualification results, screenshots, and build/dependency
measurements.
