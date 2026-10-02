# Code mode

Code mode is the subscription-account forwarding contract for coding clients.
It uses a route-specific URL such as `https://olp.example/code/team-coding` and
keeps the native model in the request. Ordinary inference routes continue to
use OLP route slugs as model names.

**Qualification status:** management, durable ledger and raw transports are wired
into OLP. The pinned official Codex CLI has controlled-fixture qualification. This is not
yet a release claim for a real Codex subscription. See the
[support matrix and release gates](../qualification/code-mode.md) before rollout.

## Operator workflow

Code accounts, pools, routes and budgets belong to projects. A management session
or management token needs the existing configure authority and project-manager
or global authority to change them. Read access is project-scoped. Session
mutations require CSRF protection; use `Idempotency-Key` on creation, publication
and retirement, and `If-Match` on updates and publication.

1. Enroll the account through the provider's supported authorization flow. OLP
   stores encrypted grant material under a credential version and its observed
   principal. A generic OAuth plugin demonstration does not establish supported
   Codex account authorization. Do not paste an access token into a developer's
   CLI or mark a fixture account as production-qualified.
2. Create a code account with `project_id`, `provider_id`, `credential_id`,
   `name`, `enabled` and an explicit `models` array. Account identity survives
   credential rotation only when both provider and principal remain the same.
3. Create a pool and assign accounts and API keys explicitly. A `shared` pool
   has no owner. A `personal` pool names `owner_user_id` and accepts only keys
   issued by that owner. Being a project member does not itself grant pool use.
4. Create a route with a stable `slug`, `pool_id`, `models` and `enabled`, then
   publish it. The published revision freezes connection configuration. Adding
   an account backed by a previously absent provider requires republishing the
   route before that connection can serve it.
5. Give the developer an OLP inference key and the supported route configuration.
   Keep grant access/refresh tokens and account authorization in OLP. The generated
   configuration surface states the exact client version, model list and gaps.
   In the console's Code mode page, set the public gateway URL before copying it.
6. Inspect account eligibility, bindings, attempts, refusals and token windows.
   Retire a binding to stop its entire tree. A retired or unavailable conversation
   fails; the caller must explicitly start a new conversation to choose again.

All lifecycle actions must avoid synthetic inference, including enrollment,
account activation, refresh, publication, health checks and background maintenance.
Initial `health: unknown` and a current grant do not imply successful inference
qualification. Ordinary provider probe/certification workflows are not code-mode
activation checks.

### Management surface

| Operation | Endpoint |
| --- | --- |
| List/create accounts, pools, routes, budgets | `GET`/`POST /api/v1/code/{accounts,pools,routes,budgets}` |
| Update one resource | `PUT /api/v1/code/{collection}/{id}` |
| Publish an immutable route revision | `POST /api/v1/code/routes/{id}/publish` |
| Inspect route history | `GET /api/v1/code/routes/{id}/revisions` |
| Generate official client TOML | `GET /api/v1/code/routes/{id}/client-config?gateway_url=...&model=...` |
| Inspect metadata | `GET /api/v1/code/{bindings,attempts,refusals,token-windows}` |
| Retire the root of a conversation tree | `POST /api/v1/code/bindings/{id}/retire` |

Lists support `project_id`, `cursor` and `limit`; diagnostics additionally support
the applicable `route_id`, `api_key_id`, `account_id` and `binding_id` filters.
Use collection results' ETags when updating resources. The API and console must
ship together; a backend-only deployment does not meet the complete product contract.

## Official Codex configuration

The controlled suite runs the unmodified official Codex CLI **0.160.0**. This
example demonstrates its tested provider configuration, not a real-account
qualification or a substitute for OLP's generated configuration:

```toml
model = "gpt-5.4"
model_provider = "olp"

[model_providers.olp]
name = "OpenAI"
base_url = "https://olp.example/code/team-coding"
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
http_headers = { "X-OLP-Code-Model" = "gpt-5.4" }
```

Supply only the OLP key as `OLP_API_KEY`. No workstation ChatGPT login is used in
the controlled tests. `gpt-5.4` is a native model identifier, not the route slug
and not proof that a particular account is entitled to it. Select only a model
allowed by the published route and enrolled account.

The generated configuration retains Codex's default request and stream retries.
Controlled tests cover interrupted-stream retries, WebSocket reconnect and HTTP
fallback. OLP itself never retries or replays inference. Every client retry gets
fresh admission on the original account. A 503 can quarantine that account;
automatic retries then fail without dispatch until the cooldown expires.

`X-OLP-Code-Model` is an OLP-only selection hint for the initial WebSocket
handshake, before the native body arrives. It is removed upstream. It cannot
change an existing account pin or bypass the model check on each generation.
Generate configuration again with `model=<chosen-native-model>` when changing
models. Codex 0.160.0's `-m` changes the body but **does not update** this static
header; an override alone can pin an incompatible account. Arbitrary `-m`
overrides are not qualified for disjoint-model pools.

## Conversations and authority

The first admitted top-level conversation selects one eligible pool account and
atomically persists its binding. Concurrent first turns on different replicas
must resolve to that same binding. Resumes, reconnects, children, compaction and
client retries retain the root's account. A child with an unresolved parent is
refused instead of receiving a new account.

Fresh `codex exec review` is a child-first workflow. With the generated
WebSocket-enabled configuration, controlled tests observe a root handshake
establishing its pin before the review child, without parent inference. This
also works when the upstream rejects WebSockets with 426 and Codex falls back
to HTTP after OLP has established the root. Disabling WebSockets from the outset
leaves the parent unresolved and correctly returns `code_parent_unresolved`.
The official app-server can also review a thread after a real user task on that
thread, over either transport. Do not send a dummy inference merely to acquire
a parent pin. Parent establishment must reach OLP; an external proxy that blocks
the root handshake before OLP is not qualified by the fallback test.

Every generation rechecks key/project/route/pool permission, account eligibility,
grant state and limits. A WebSocket upgrade is not lasting authorization to
generate. Revocation, retirement, an unavailable account, exhausted allowance or
an unsupported model never triggers silent account switching. A credential
refresh may change authentication for the same principal; it must not change
the principal stored in the binding.

## Limits and accounting

Request rate, token rate and concurrency reuse shared admission. Those estimates
do not establish hard token bounds. Subscription traffic excludes ordinary
monetary spending limits; dollar estimates are informational.

Hard token budgets are optional. An enabled matching budget requires an
adapter-proven conservative bound before dispatch. Project-wide, route-scoped and
key-scoped budgets overlap; all applicable UTC daily/monthly windows reserve
atomically. An operation without a trustworthy bound returns
`code_token_bound_unavailable` only when a matching hard budget is enabled.
Disabling a budget does not erase previous consumption or uncertainty.

The current qualification has **no proven native Codex generation bound**.
`max_output_tokens`, model context metadata, prompt estimates, a test constant or
an operator-entered number cannot be treated as proof. Positive hard-budget
support remains a release gate rather than an always-reject implementation claim.

After dispatch, final provider usage settles reservations exactly once. Cached
input and reasoning output are subsets of input/output totals. A disconnect,
cancellation or missing final usage leaves durable uncertainty and does not
release a reservation as unused. A bound overrun is a `bound_violation` and must
block subsequent budgeted admission. Historical unknown usage prevents falsely
enabling a hard guarantee.

Provider-reported allowance is separate metadata with observation/reset times.
Never infer subscription allowance from a local budget, monetary estimate or a
successful fixture request. Stale allowance observations cannot replace newer ones.
Observed exhausted allowance refuses admission until its reported reset.
Temporary transport/quota failures impose a one-minute cooldown; the next
client request can test recovery on the same pinned account. OLP schedules no
synthetic health inference and never automatically replays the failed request.

## Fidelity and privacy

The forwarding contract preserves request bodies, response bytes, SSE content,
WebSocket message payloads/order, and end-to-end header values including repeated
values. Observation may parse copies in memory; it must never reserialize the
forwarded payload. Unknown fields on a qualified operation remain untouched.
Unqualified paths and operation types refuse before upstream dispatch.

Exceptions are upstream authentication replacing the OLP credential, OLP-only
controls, hop-by-hop headers and protocol-required framing/upgrade handling.
Request authentication fields consumed locally are `Authorization`,
`ChatGPT-Account-ID`, `Cookie`, `X-API-Key` and `X-Goog-API-Key`; only the
qualified adapter's `Authorization` and `ChatGPT-Account-ID` replace them.
All `X-OLP-*` fields are consumed. Hop-by-hop exclusions are `Connection`,
its named fields, `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`,
`Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding` and `Upgrade`.
Trailers retain end-to-end values through the downstream framing mechanism.
WebSocket keys, versions, accept values and extensions are negotiated on each
leg; subprotocol requests refuse and compression is disabled. Upstream authority
and HTTP framing are recomputed without reserializing bodies.
The contract does not claim identical TCP segmentation, WebSocket frame
fragmentation, TLS fingerprint, source IP or timing. It cannot guarantee that
an upstream provider accepts an account's use.

Diagnostics retain identifiers, route revisions, native models, operations,
binding/account identity, bounded refusal codes, reservation/usage metadata and
state. They exclude prompts, outputs, reasoning, tool payloads, raw headers,
upstream auth and refresh material. Account names and operator labels are not
places to store secrets.
