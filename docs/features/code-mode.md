# Code mode

Code mode is the subscription-account forwarding contract for coding clients.
It uses a route-specific URL such as `https://olp.example/code/team-coding` and
keeps the native model in the request. Ordinary inference routes continue to
use OLP route slugs as model names.

**Qualification status:** management, durable ledger and raw transports are wired
into OLP. The pinned official Codex CLI, Claude Code and OpenCode have
controlled-fixture qualification. This is not yet a release claim for a real
ChatGPT, OpenCode Go or GLM Coding Plan subscription. See the
[support matrix and release gates](../qualification/code-mode.md) before rollout.

## Adapters and clients

A route serves one subscription family, its **adapter**, which OLP derives from
the plugin profiles of the provider connections the published revision froze.
The adapter decides which paths the route serves, how the upstream credential
is placed and which clients OLP generates configuration for. A pool holds
accounts of one adapter; management refuses a pool or publication that mixes
them. Every other path refuses before upstream dispatch.

| Adapter | Plugin and profiles | Paths under `/code/{slug}/` | Upstream | Clients (default first) |
| --- | --- | --- | --- | --- |
| Codex | [`codex`](../../plugins/codex/README.md): `codex-subscription` | `responses`, `responses/compact`, WebSocket `responses` | `https://chatgpt.com/backend-api/codex` | Codex CLI |
| OpenCode Go | [`opencode-go`](../../plugins/opencode-go/README.md): `opencode-go` | `v1/chat/completions`, `v1/messages`, `v1/responses` | `https://opencode.ai/zen/go/v1` | OpenCode, Claude Code |
| GLM Coding Plan | [`zai-coding`](../../plugins/zai-coding/README.md): `zai-coding-plan`, `bigmodel-coding-plan` | `v1/messages`, `v1/chat/completions` | `https://api.z.ai/api/{anthropic,coding/paas/v4}` or the same paths on `https://open.bigmodel.cn/api` | Claude Code, OpenCode |

The Messages paths speak Anthropic Messages, the chat paths Chat Completions and
the responses paths the Responses API. OpenCode Go serves each model on one
endpoint: most on Chat Completions, MiniMax and Qwen models on Messages and
Grok and GPT models on Responses. OpenCode picks it per model; Claude Code
speaks only Messages, so on OpenCode Go it works only with Messages models.

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
   - Codex enrolls through the official device sign-in.
   - OpenCode Go and GLM Coding Plan enroll a pasted API key: the operator opens
     the vendor's key page, creates a key and pastes it into a masked field. OLP
     makes no upstream call; it stores the key encrypted and takes a SHA-256
     fingerprint of it, scoped to the profile, as the principal. The key never
     expires or refreshes. A different key is a different principal, so rotating
     a key means a new code account; conversations pinned to the old account
     must start again. Revoke the old credential version, because nothing
     retires it automatically.
2. Create a code account with `project_id`, `provider_id`, `credential_id`,
   `name`, `enabled` and an explicit `models` array. Account identity survives
   credential rotation only when both provider and principal remain the same.
3. Create a pool and assign accounts of one adapter and API keys explicitly. A `shared` pool
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
| Generate client configuration | `GET /api/v1/code/routes/{id}/client-config?gateway_url=...&client=...&model=...&small_model=...` |
| Inspect metadata | `GET /api/v1/code/{bindings,attempts,refusals,token-windows}` |
| Retire the root of a conversation tree | `POST /api/v1/code/bindings/{id}/retire` |

Lists support `project_id`, `cursor` and `limit`; diagnostics additionally support
the applicable `route_id`, `api_key_id`, `account_id` and `binding_id` filters.
Use collection results' ETags when updating resources. The API and console must
ship together; a backend-only deployment does not meet the complete product contract.

## Client configuration

Client configuration reads the published revision. `client` defaults to the
adapter's first client and `model` to the route's first model; `small_model`,
for background requests, defaults to `model`. A client the adapter does not
support, or a model the route does not admit, is a 422. No configuration ever
contains an OLP key: each reads it from `OLP_API_KEY`.

### Codex

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

### Claude Code

The controlled suite runs Claude Code **2.1.286**. OLP generates a POSIX shell
file to source before starting `claude`:

```sh
unset ANTHROPIC_API_KEY CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL='https://olp.example/code/team-glm'
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_OPUS_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_SONNET_MODEL='glm-5.3'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='glm-5.3-flash'
export CLAUDE_CODE_SUBAGENT_MODEL='glm-5.3'
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
```

Every model variable names a native model the route admits, so background,
subagent and classifier requests stay on the route; Claude Code otherwise
sends Anthropic model names the route refuses. `ANTHROPIC_AUTH_TOKEN` sends the
OLP key as a bearer token, and the file unsets a workstation `ANTHROPIC_API_KEY`. The
non-secret variables may instead go in the `env` block of
`.claude/settings.json`; keep the key in the environment. Z.ai's guide suggests
`API_TIMEOUT_MS` and `CLAUDE_CODE_AUTO_COMPACT_WINDOW`; a generation through OLP
is limited to ten minutes whatever the client timeout, and OLP does not know a
model's context window, so set the latter yourself if you need it.

Claude Code's `HEAD /api/hello` connectivity probe and `count_tokens` are not
served; it falls back to local estimates. Server tools such as web search and
claude.ai login features are outside code mode.

### OpenCode

The controlled suite runs OpenCode **1.18.34**. OLP generates an `opencode.json`
that overrides the base URL of OpenCode's own provider for the plan:
`opencode-go`, or `zai-coding-plan` for either GLM profile.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "opencode-go/kimi-k3",
  "small_model": "opencode-go/kimi-k3",
  "enabled_providers": ["opencode-go"],
  "provider": {
    "opencode-go": {
      "options": { "baseURL": "https://olp.example/code/team-go/v1", "apiKey": "{env:OLP_API_KEY}" },
      "whitelist": ["kimi-k3", "minimax-m3"]
    }
  }
}
```

OpenCode keeps choosing each model's endpoint and SDK from its bundled model
catalogue, so a route model the pinned release does not know is unavailable.
`enabled_providers` stops OpenCode loading other providers; `small_model` keeps
title generation on the route. Never run `opencode auth login` for the plan on a
developer machine. OpenCode asks the npm registry for its own dependencies at
startup; that is not model traffic.
## Conversations and authority

Clients name their conversations in headers OLP reads and forwards unchanged:

| Client | Conversation | Child conversation |
| --- | --- | --- |
| Codex | `thread-id` or `session-id` | `x-codex-parent-thread-id` with `x-openai-subagent` |
| Claude Code | `x-claude-code-session-id` | `x-claude-code-agent-id`, beneath `x-claude-code-parent-agent-id` or the session |
| OpenCode | `x-opencode-session-id` | beneath `x-opencode-parent-session-id` |

A Claude Code agent is scoped by its session, as `session/agent`, because
teammate agents reuse name-based identifiers. Identifier bytes outside letters,
digits, `.` and `-` are encoded as `:xx`, so OpenCode's `ses_…` appears as
`ses:5f…`. A request naming no conversation, or naming both clients', refuses.
Only the client headers of the route's adapter count: a Codex route ignores
Claude Code and OpenCode headers, and the coding plans ignore Codex's.

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
input and reasoning output are subsets of input/output totals. Anthropic
Messages input includes cache reads and writes, and its cached input is the
cache reads. Chat Completions reports usage only in a final chunk when the
client asks for it with `stream_options.include_usage`, as OpenCode does; a
stream without it leaves its consumption uncertain. A disconnect,
cancellation or missing final usage leaves durable uncertainty and does not
release a reservation as unused. A bound overrun is a `bound_violation` and must
block subsequent budgeted admission. Historical unknown usage prevents falsely
enabling a hard guarantee.

Provider-reported allowance is separate metadata with observation/reset times.
Only Codex reports allowance OLP reads; OpenCode Go and GLM Coding Plan accounts
show none, and their health comes from response status alone. A 429, such as
Z.ai's 5-hour limit or its concurrency limit, cools the account for a minute.
Primary and secondary windows are retained independently for each metered limit;
credits remain separate from those windows and from local budgets. Partial
observations merge by window and do not erase other windows.
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
`ChatGPT-Account-ID`, `Cookie`, `X-API-Key` and `X-Goog-API-Key`. A route's
Messages paths also accept the OLP key in `X-API-Key`, as Anthropic SDKs send
it, and refuse in Anthropic's error envelope. Only the adapter's own headers
replace them: Codex's `Authorization` and `ChatGPT-Account-ID`, a bearer
`Authorization` for GLM Coding Plan, and for OpenCode Go a bearer
`Authorization`, or `X-API-Key` on Messages as OpenCode's Anthropic SDK sends it.
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
binding/account identity, bounded refusal codes, reservation/usage metadata,
upstream status and outcome origin. Upstream completion/rejection, transport loss
and client cancellation remain distinct from whether usage is known. WebSocket
handshakes and prewarms alone do not establish serving health. Diagnostics exclude
prompts, outputs, reasoning, tool payloads, raw headers,
upstream auth and refresh material. Account names and operator labels are not
places to store secrets.

## Vendor terms

Z.ai restricts the GLM Coding Plan to its officially supported coding tools and
scenarios, and OpenCode Go has its own terms. OLP forwards the client's own
requests unmodified, including its user agent and session headers, but it
cannot guarantee that a vendor accepts a plan used through a gateway or shared
by several developers. Review each vendor's terms before pooling an account,
and prefer a personal pool per subscriber where they require it.

### End-user accounting

Keys may identify callers through the ordinary [end-user policy](../access.md#end-user-identity).
Code generations and admission refusals retain its project-scoped HMAC digest,
never the raw identifier. `GET /api/v1/code/attempts` and `/api/v1/code/refusals`
accept `end_user_digest=<digest>` or `end_user_digest=unidentified`; the console's
Attempts and Refusals tabs expose the same filter and identity metadata. These
fields share the existing code-ledger retention and project isolation boundaries.
They do not turn subscription allowances into USD spend. Changes that would
switch identity during admission refuse before dispatch with
`code_end_user_changed`; reconnect or retry using the updated key policy.

### Attribution policy

Subscription calls enforce the same required and pinned labels as other
inference requests. Attempts retain the resolved attribution object and display
it in diagnostics. The ledger checks current project/key policy before creating
a billable attempt; changed policy returns `code_attribution_changed`. See
[attribution requirements](../access.md#attribution-requirements-and-pinned-labels)
for configuration, refusal codes and retention.

### Route request sizes

The route editor's optional `max_body_bytes` is published with each subscription
route revision. It lowers the installation/protocol cap for both encoded and
decoded HTTP bodies and individual client WebSocket messages. Oversized requests
are refused before durable generation admission or provider dispatch. See
[per-route request sizes](../gateway.md#per-route-request-sizes).

Workload JWTs use the same explicit pool assignment and current project authority
as static principals. Their verified claim digest survives fresh-policy admission;
issuer changes before dispatch require a retry, and idle subscription sockets
close on the five-second expiry/authority check. See
[workload JWT identity](../access.md#workload-jwt-identity) for provisioning and
renewal behavior. No JWT or raw subject enters subscription ledgers.
