# Go gateway: providers, routes, keys, and OpenAI inference

The Go gateway serves the OpenAI Chat Completions and Responses endpoints, the
model list, and the console playground against OpenAI and OpenAI-compatible
connections. Operators configure a connection, certify its models, publish a
route, and issue a key; SDK clients then use the route slug as the model name.
Distributed limits and durable usage accounting arrive in M4; Anthropic and
Gemini surfaces, other provider kinds, media, and routing policies arrive later.
Installation and identity are covered in [Go installation and access
control](go-access.md).

## What is available

| Area | Available now | Deferred |
|---|---|---|
| Surfaces | `POST /v1/chat/completions`, `POST /v1/responses`, `GET /v1/models`, `GET /v1/models/{model}` | `/anthropic/v1/*` and `/gemini/*` return `501` until M5 |
| Provider kinds | `openai`, `openai_compatible` (`api_key`, `headers`, or `none` authentication) | Other kinds return `422 provider_kind_unavailable` |
| Operations | `generation` on the `openai` surface, unary and streaming | Other operations return `422 operation_unavailable`; media in M6 |
| Routing | Priority tiers with deterministic weighted selection, attempt budgets, deadlines | Routing policies read as defaults; writes return `501` until M5 |
| Limits | Key, connection, and slot limits are stored as policy | Enforcement, pricing, and history in M4 |

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only and answers `/v1/*` with `404`. `gateway` serves
inference only; it still needs the database, `OLP_AUTH_HMAC_KEY_FILE`, and
`OLP_MASTER_KEY_FILE` because it reads key authority and decrypts provider
credentials itself. `GET /health/ready` on the private listener reports an
`authority` dependency alongside PostgreSQL and Valkey; readiness fails while
the authority snapshot is missing or stale.

## Configuration

Every variable also exists as a flag (`olp --help`). Bounds are validated at
startup.

| Variable | Default | Purpose |
|---|---|---|
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
   the configuration, requires a usable credential and at least one enabled,
   fully certified model, writes an immutable revision, and publishes a new
   runtime generation.

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
optional model, route, and key allowlists, and stored (not yet enforced)
limits. Rotation (`POST /providers/{id}/credentials`) validates the new secret
against the upstream model list before storing a new version and selecting it
for the draft; the active revision keeps the version it was activated with
until the provider is activated again. Slot validation
(`POST /providers/{id}/credential-slots/{slot_id}/validate`) checks that one
slot's credential works without affecting sibling slots.

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
| 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `unsupported_parameter`, `unsupported_stateful_reference` | Request envelope problems, including unsupported Responses state references. |
| 401 | `invalid_api_key` | Missing, unknown, expired, or revoked key. |
| 403 | `permission_denied`, `route_forbidden` | Scope missing or route outside the key's allowlist. |
| 404 | `route_not_found`, `not_found` | Unknown route slug or endpoint. |
| 408 | `request_timeout` | Request body not received within 15 seconds. |
| 413 / 415 | `request_too_large`, `unsupported_media_type`, `unsupported_content_encoding` | Body limits and content negotiation. |
| 429 | `upstream_rate_limit` | Every attempt was rate limited; `Retry-After` is forwarded. |
| 502 | `upstream_unavailable`, `upstream_rejected`, `upstream_authentication_failed`, `upstream_permission_denied`, `provider_protocol_error` | Upstream or transport failures after the budget is spent. |
| 503 | `authority_unavailable`, `request_admission_overloaded`, `upstream_unavailable` | Stale authority, admission limit, or no eligible target. |
| 504 | `gateway_timeout` | Route deadline reached before commitment. |

## Diagnostics

Every request ends in exactly one terminal envelope logged as
`inference request`: request ID, actor (key or playground user), route and
revision, release sequence, family, mode, outcome, status, whether the
response was committed, timing, observed usage when the upstream reported it,
and one record per attempt with target, provider revision, slot, credential
version, status class, and timing. Prompts, outputs, tool data, headers, and
credentials are never included. `GET /api/v3/provider-health` summarizes the
same facts per provider for the requested window. M4 adds durable delivery,
pricing, and history.

## Local development and qualification

`make go-check` runs formatting, vet, the Go unit suites (egress corpus,
OpenAI codecs, runtime selection, and gateway lifecycle scenarios), and the
console checks. `make go-integration` builds the binary and runs the
PostgreSQL-backed scenario that walks from an empty installation to SDK
traffic, the official OpenAI SDK smoke checks against the Go fixture, and the
browser journeys at the packaged and Vite origins. The browser journey uses
`console/tests/gateway/mock-openai.mjs` as its upstream.
