# Code mode

Code mode lets authorized coding clients use enrolled subscription accounts, or
a metered API target that a route explicitly selects, behind OLP access controls.
Each code route has a URL such as `https://olp.example/code/team-coding`.
Requests keep the client's native model name by default, unlike ordinary
inference routes where the published route slug is the model name.

One code route can also mix subscription families. An OpenCode or Claude Code
session might plan with an OpenCode Go model and implement with a GLM Coding Plan
model while each request reaches its own account. See
[Mixing subscriptions](#mixing-subscriptions) for the account and conversation
rules.

Native forwarding is the default because coding clients depend on exact protocol
fields and stream ordering. A published route policy can explicitly enable the
Messages and Chat translations described in this guide. Compatibility behavior
never turns on silently.

Code mode management, its durable ledger and raw transports are implemented.
The fixture qualification gate installs lockfile-pinned official client versions
from `tests/qualification/clients/package.json` (`@anthropic-ai/claude-code@2.1.330`,
`@openai/codex@0.171.0` and `opencode-ai@1.19.6`) through
`scripts/qualification-clients.sh` and runs one HTTP journey per client against
controlled fixtures. Fixture qualification is not a release claim for a real
ChatGPT, Claude, Antigravity, OpenCode Go or other coding-plan subscription.
Review the [support matrix and release gates](../develop/qualification.md) before
rollout.

### Qualification status

Reliability claims in this guide name adapters and profiles, never code
mode as a whole. A mechanism counts as live proven for an adapter and
profile only when the matching live lane in
[qualification](../develop/qualification.md#live-lanes) has run green
and its claims summary names that pair. Until then the console and this
guide keep the no proven signal wording for that pair.

Eligible accounts are selected using weights and cooldowns. Codex inference
responses can supply passive observations, although OLP does not assume which
models they cover. Active Codex reads and both MiniMax M Plan remains reads are
held pending live qualification. Existing active observations do not influence
admission or ordering.

GLM, Kimi, Qwen and MiniMax quota envelopes can set a cooldown, and they do
not establish that inference did not execute and cannot authorize replay.
The Codex rejection envelopes remain the controlled assumptions documented
under portable conversation recovery. OpenCode Go has no qualified replay
proof, and its reported `allowance_charge` stays an observation, never an
ordering input.

Cooldown and failover behavior claim live evidence only for the pairs a
green live run summary names. No adapter or profile claims live proven inference
or failover from fixture evidence alone.

## Adapters and clients

Each code account belongs to a subscription family, its **adapter**, which OLP
derives from the plugin profile of the account's provider connection. The
adapter decides which paths the account serves, where upstream they go, how the
upstream credential is placed and which clients OLP generates configuration
for. A route serves the paths of every adapter its published revision froze,
and each request reaches an account whose adapter serves its path. Every other
path refuses before upstream dispatch.

| Adapter | Plugin and profiles | Paths under `/code/{slug}/` | Upstream base and paths | Clients (default first) |
| --- | --- | --- | --- | --- |
| Codex | [`codex`](../../internal/codemode/codexauth/codex/README.md) (`codex-subscription`) | `responses`, `responses/compact`, WebSocket `responses` | `https://chatgpt.com/backend-api/codex` (`responses`, `responses/compact`) | Codex CLI |
| Claude subscription (fixture only) | [`claude`](../../internal/codemode/claudeauth/claude/README.md) (`claude-subscription`) | `v1/messages` | `https://api.anthropic.com` (`v1/messages`) | Claude Code (live qualification pending) |
| Antigravity (fixture only) | [`antigravity`](../../internal/codemode/antigravityauth/antigravity/README.md) (`antigravity-subscription`) | `v1/chat/completions` (when `chat_to_antigravity` is enabled) | `https://daily-cloudcode-pa.googleapis.com` (`v1internal:generateContent`, `v1internal:streamGenerateContent`) | OpenCode (live qualification pending) |
| OpenCode Go | [`opencode-go`](../../internal/codemode/codeplans/opencode-go/README.md) (`opencode-go`) | `v1/chat/completions`, `v1/messages`, `v1/responses` | `https://opencode.ai/zen/go/v1` (`chat/completions`, `messages`, `responses`) | OpenCode, Claude Code |
| GLM Coding Plan | [`zai-coding`](../../internal/codemode/codeplans/zai-coding/README.md) (`zai-coding-plan`, `bigmodel-coding-plan`) | `v1/messages`, `v1/chat/completions` | `https://api.z.ai/api` or `https://open.bigmodel.cn/api` (`anthropic/v1/messages`, `coding/paas/v4/chat/completions`) | Claude Code, OpenCode |
| MiniMax M Plan | [`minimax-plan`](../../internal/codemode/codeplans/minimax-plan/README.md) (`minimax-plan`, `minimax-plan-cn`) | `v1/messages`, `v1/chat/completions` | `https://api.minimax.io` or `https://api.minimax.cn` (`anthropic/v1/messages`, `v1/chat/completions`) | Claude Code, OpenCode |
| Kimi Code | [`kimi-code`](../../internal/codemode/codeplans/kimi-code/README.md) (`kimi-code`, `kimi-code-cn`) | `v1/messages`, `v1/chat/completions` | `https://api.kimi.ai/coding` or `https://api.kimi.com/coding` (`v1/messages`, `v1/chat/completions`) | Claude Code, OpenCode |
| Qwen Token Plan | [`qwen-token-plan`](../../internal/codemode/codeplans/qwen-token-plan/README.md) (`qwen-token-plan`, `qwen-token-plan-cn`) | `v1/messages`, `v1/chat/completions` | `https://token-plan.ap-southeast-1.maas.aliyuncs.com` or `https://token-plan.cn-beijing.maas.aliyuncs.com` (`apps/anthropic/v1/messages`, `compatible-mode/v1/chat/completions`) | Claude Code, OpenCode |

The Messages paths speak Anthropic Messages, the chat paths speak Chat
Completions and the responses paths speak the Responses API. OpenCode Go serves
each model on one endpoint, using Chat Completions for most models, Messages for
MiniMax and Qwen models, and Responses for GPT and Grok models. OpenCode picks
that endpoint per model, while Claude Code speaks only Messages and therefore
uses only Messages models on OpenCode Go. For MiniMax M Plan, generated OpenCode
configuration sends requests through Anthropic Messages.

### Exact model aliases

Enable **Rewrite model identifiers using exact aliases or model pools** in the route editor,
then enter one `alias=native-model` per line in **Exact model aliases**. Each
alias must target one of the route's listed native models. Save and publish to
include the aliases in authenticated model discovery and generated client setup.
An alias uses its target's protocol and client capabilities, even when its name
has no vendor prefix.

The management route write uses `model_rewriting: true` and an `aliases` object,
for example `"aliases": {"team-code": "native-model"}`. This opt-in is independent
of Messages translation and identity compatibility. Native identifiers pass through unchanged unless their public name has an
explicit model pool. Alias requests replace only the protocol's model
identifier, and replies expose the public alias. Prompt content and unrelated
payload fields remain unchanged.

Alias chains, native-name collisions and targets outside the native model list
are rejected. An unavailable target is omitted from discovery and refused for
inference. Without an explicit model pool, an alias always names one exact model and
recovery keeps that exact target.

Repointing an alias and publishing changes the target for new conversations.
Existing conversations retain their durably stored target and account while
that target remains published, eligible and authorized. Removing the old target
or its access refuses the continuation instead of moving it to the new model.
Existing native-model pins are preserved.

### Explicit model recovery pools

Model changes require a published opt-in. In **Model recovery pools**, enter
`public-model=primary,alternate` on each line. The public name can be a native
model or an exact alias. Its first target must match normal exact resolution,
and every target must be distinct and listed under **Native models**. Enable
model rewriting, choose one or two **Recovery attempts**, then save and publish.
Zero attempts disables automatic recovery, including configured model pools.

The management route write carries `model_pools`, for example
`"model_pools": {"team-code": ["native-main", "native-alternate"]}`. Discovery
and client setup expose the ordered targets as policy metadata. Availability
and protocol compatibility are checked again for each selected account.
Configuration and publication do not send inference requests.

Recovery first tries another eligible account for the currently pinned exact
model, then considers the configured targets in order. Substitution requires a
positive nonexecution result and positively portable input. Accepted work,
partial output, uncertain dispatch and opaque input are never replayed. The
existing single-pin root bound still applies. Child-bearing trees, multiple
pins and socket-owned state refuse recovery. Current key and pool authority,
account eligibility, protocol support and applicable budgets are checked before
execution. A missing proven token bound refuses a request with a hard token
budget. An unavailable alternate never bypasses these checks. Paid participation
and paid fallback have separate policies.

The client keeps requesting the same public name. Replies retain that name,
and the recovered exact target and account are stored on the conversation pin.
Later requests retain that target while it remains published, eligible and
authorized. Repointing an alias changes new conversations. Recovery for an
existing pin still tries its stored exact target first, then the currently
published pool order. Removing a pool restores exact recovery for that pin
without moving it back to an earlier target. Attempts record the public
`requested_model` separately from the selected exact `model`. Historical
attempts have an unknown requested model rather than an inferred value.

### Messages through Codex

Enable **Translate Messages through Codex** in the route editor, or set
`messages_to_codex: true` in the management route write, then publish the draft.
The policy is disabled by default and pinned to the published revision. A route
with Codex accounts can then generate Claude Code setup using the same native
model identifiers. Codex remains the default client for those accounts.

The policy translates HTTP `v1/messages` text and tool requests into the Codex
Responses transport. Tool names, call IDs, JSON arguments and text results are
preserved across turns, including fragmented argument streams. Explicit
`output_config.effort` values `low`, `medium` and `high` map to the same
`reasoning.effort` values. `thinking.type: adaptive` is accepted with one of
those explicit effort values. This mapping follows the
[pinned translator](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/translator/codex/claude/codex_claude_request.go#L407-L453)
reviewed on 2026-10-30. Both unary Messages replies and Messages event streams are supported.
System text is preserved in developer input, and the caller's output-token cap
is preserved. Admission, selected-account authorization and usage settlement use
the existing code-mode ledger. Native Messages accounts continue to forward
native payloads unchanged.

The documented Claude `?beta=true` transport flag is accepted and removed before
the Codex request is sent. Signed thinking, opaque reasoning state, numeric
reasoning budgets, error-marked tool results, media, beta headers, unknown query
fields, stop sequences and other unsupported required semantics are refused.
Cancellation and partial upstream failures end the attempt without replay or a
successful Messages terminal event. Missing terminal usage remains unknown in
the ledger. Translation alone does not enable identity compatibility, and prompt
compatibility remains disabled. This translated
path does not establish complete Claude Code qualification. CLI context and beta
requirements and durable opaque state need separate qualification.
Live Codex acceptance, including output-token caps, remains unqualified.

### OpenCode Chat through Codex

Enable **Translate Chat through Codex** in the route editor, or set
`chat_to_codex: true` in the management route write, then publish the draft.
This independent policy is disabled by default and frozen in the published
revision. Generate OpenCode setup through the existing route client-config
endpoint. It declares each offered model explicitly under `olp-codex`, with the
Chat SDK and the route's `/v1` base URL. It does not require those models to be
present in OpenCode's bundled catalog. Exact model names and published aliases
remain the public selection, including planning and small models.

HTTP `v1/chat/completions` text and function tools translate through the Codex
Responses transport. System and developer text, function names, call IDs, JSON
arguments, text results and output-token caps are preserved. Explicit
`reasoning_effort` values `low`, `medium` and `high` map to the same Responses
effort. Unary and SSE replies share the existing Responses codec and native
Codex usage accounting. OpenCode session and parent headers retain code-mode
conversation ownership across tool results, child sessions and cold resumes.
Portable initial requests can use the existing bounded recovery policy after
proven nonexecution. Accepted, partial and ambiguous execution is never replayed.

Signed or opaque reasoning, server-owned continuation state, media, named
messages, unsupported generation controls, beta headers, query parameters and
unknown required fields are refused. Unsupported upstream state fails the
projection while preserving observed usage. Cancellation emits no successful
terminal marker, and missing terminal usage remains unknown. Native Chat,
Messages and Responses forwarding retain their wire behavior. Translation does
not enable the independent Codex identity policy or change prompts. Controlled
codec and SDK fixtures do not establish current OpenCode client acceptance or
live Codex subscription acceptance. Both remain unqualified.

### Codex CLI identity compatibility

This independent route option is disabled by default. Before enabling it, review
its exact behavior and account-suspension risk. For Codex subscription HTTP and
WebSocket requests only, it supplies `Originator: codex_cli_rs` when all supplied
Originator values are absent or blank. It preserves every explicit nonblank
Originator value, User-Agent, other headers and all body content, including
instructions, tools, reasoning and conversation state. It does not add a client
version or platform, change prompts, enable translation or alter required OAuth
authentication signals. Other subscription adapters and paid API targets retain
their native identity behavior.

An existing route administrator can select **Enable Codex CLI identity
compatibility**, save the draft and publish it, or set `codex_cli_identity: true`
through the existing route API and publish. This authorizes the policy for
eligible accounts in the route pool without an additional account-contributor
approval. The editor, API contract and publication confirmation describe the
mutation and warn that using this compatibility identity may violate provider
terms and risk account suspension. Turning the option off also requires
publication. Draft changes leave earlier publications unchanged, and
configuration export and apply preserve draft and published choices separately.

Attempt diagnostics expose `codex_cli_identity_enabled` from the attempt's
frozen route revision. This means the policy was enabled, not that the attempt
used Codex or that a header changed. Diagnostics do not record client identity
header values or prompts. Publication history shows the same frozen policy.

The reviewed behavior is narrower than the pinned compatibility implementation
at [CLIProxyAPI commit 90c447d3](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/runtime/executor/codex_executor_request.go),
which supplies a subscription Originator fallback. The value used here was
reviewed on 2026-10-30 against the official Codex
[default client at commit 5e769a1c](https://github.com/openai/codex/blob/5e769a1cdae3fd93efd5e7b9df71e39efb2223ab/codex-rs/login/src/auth/default_client.rs).
These source references establish the reviewed header policy, not live provider
acceptance. No prompt transformation or live subscription qualification is
claimed.

### Paid API targets

A paid API target is an existing metered provider and credential slot, separate
from a coding account or subscription adapter. Enable **Permit paid API targets**
in the route editor, select a provider and slot for each native model that should
use it, then publish. The management API uses `paid_participation: true` and a
`paid_targets` object keyed by exact native model, with `provider_id` and
`slot_id` for each entry. The selected provider must be active in the same
project and support the native protocol without request defaults, model
overrides, caller credentials, plugin adaptation or hosting wrappers.

A model mapped in `paid_targets` selects its paid target directly even when a
subscription account serves the same model. Without an explicit
`paid_fallback_targets` entry, subscription failures never activate paid targets.
Aliases resolve an existing conversation pin before provider, credential and
pricing selection, provided the selected native model and target remain
authorized. Removing access or retiring the binding still refuses continuation.
Generic affinity identifiers cannot adopt another conversation through a
response reference.

Subscription-first paid fallback uses a separate `paid_fallback_targets` map
with the same native-model, `provider_id` and `slot_id` fields. Enable **Permit
paid API targets** and bounded recovery (`initial_recovery_attempts` of one or
two), select a **Paid fallback** provider and slot in the route editor, then
publish. Entries in `paid_targets` remain direct selection and take precedence,
and a native model cannot appear in both maps.

With `paid_fallback_targets` published, subscription accounts remain first. A
new root with complete portable input can fall back before any subscription
dispatch when durable admission proves that no eligible subscription destination
exists and no subscription or paid binding already owns it. An existing
conversation can fall back only after a subscription adapter proves an HTTP
authentication or quota rejection did not execute and recovery finds no eligible
subscription destination. The complete request must be portable. Same-model paid
fallback needs no model pool, while substituting an alternate native model also
needs model rewriting and an explicit model pool containing that alternate.
Child conversations, multiple model pins, sockets, opaque continuation state,
partial output and ambiguous execution cannot migrate.

A paid fallback attempt enforces current caller and pool authority, frozen
provider and credential selection, known pricing and every applicable monetary
limit. Requested and selected models remain distinct in diagnostics and
accounting. Later requests retain the paid owner, while previous subscription
response references retain their original owner. Removing permission or retiring
the original subscription root prevents continuation. Missing usage keeps
durable monetary exposure against limits until complete paid accounting is
available.

Generated companion models must all use subscription protocols or all use paid
API protocols. Setup refuses mixed selections because subscription and paid
conversations keep separate ancestry. Paid companions may use different
authorized providers and native protocols.

Paid targets currently support native HTTP Responses generation, Messages and
Chat Completions. Compaction, prewarming, WebSockets and hosting that requires a
different request body are refused before dispatch. Generated coding-client
setup requires certified streaming support, while a unary-only target remains
available through its native HTTP endpoint. Generated Codex setup disables
WebSockets for paid models. Discovery marks paid models with `paid` and setup
returns `paid_models`, without inventing subscription adapters.

Publication freezes the provider revision and slot. Admission checks the current
key, project membership, pool assignment, personal ownership, provider,
credential, slot restrictions and monetary policy again. Known pricing is
required before dispatch. Cost and customer charge can differ through the
selected rate card. Key, group, route, project, installation, provider and slot
policies retain their applicable rate, token and spend checks. A hard
coding-token budget still requires a proven bound, which a native request does
not provide merely by specifying an output estimate.

Paid usage records retain the requested and selected model, target ownership,
native token counters, cache or media dimensions, pinned price and rate card.
Subscription allowance remains separate. Public paid attempts have null
subscription `account_id` and `binding_id`, with `paid_binding_id`,
`paid_provider_id`, `paid_slot_id` and `paid_monetary_state` identifying their
paid lifecycle. The binding filter accepts either ownership type.

#### Paid target runbook

After publishing, check the model catalog and generated client setup before
sending a request. A missing target usually means its active revision,
credential, slot restrictions or native protocol no longer matches the
publication. Republish after an intentional provider or slot change. A pricing
refusal requires a current matching price and usable rate card. A spend refusal
requires reviewing the applicable cost or charge policy and recorded
consumption, rather than changing the request model to bypass the policy.

An interrupted request, missing usage, missing monetary dimension or failed
accounting handoff leaves a durable hold for the affected owners after temporary
leases expire. Complete measured provider cost can resolve provider and slot
exposure while caller cost and charge remain distinct. Known token totals do not
make unknown monetary usage free. Review standard usage evidence and
accounting-loss records. There is currently no code-specific manual
usage-resolution endpoint, so unresolved exposure remains blocked when
definitive evidence is unavailable.

HTTP error documents and recognized identity-encoded SSE error events are
inspected with the bounded native error scrubber and the credentials actually
applied by authentication. Error values are redacted and credential-bearing
member names are refused. Successful unary responses and encoded streams retain
native delivery without additional error-envelope classification. An oversized
or malformed stream frame ends this bounded classification while delivery
continues unchanged. Native error observation never persists prompt content or
credential material.

### Mixing subscriptions

A pool may hold accounts of several adapters, and a route may list the models
of all of them. Each request reaches an account that lists its model and whose
adapter serves the model on the request's path, using that account's credential
at that subscription's upstream. A model that no adapter or paid target serves on
the path is refused with `code_operation_unsupported` before any account is
chosen. A client can use every route model its own protocols reach. With native
forwarding, a model no subscription serves on the client's protocol stays out of
that client's configuration.

| Client | Paths it uses | Models a mixed route can give it |
| --- | --- | --- |
| Claude Code | `v1/messages` | Claude subscription, GLM Coding Plan, MiniMax M Plan, Kimi Code and Qwen Token Plan models, OpenCode Go Messages models (MiniMax and Qwen), Codex models when `messages_to_codex` is enabled, and streaming paid Messages targets |
| OpenCode | `v1/chat/completions`, `v1/messages`, `v1/responses` | Every coding-plan model, every OpenCode Go model (including GPT and Grok), Codex models when `chat_to_codex` is enabled (`olp-codex`), Antigravity `gemini-*` models when `chat_to_antigravity` is enabled (`olp-antigravity`), and streaming paid targets (`olp-paid-responses`, `olp-paid-messages`, `olp-paid-chat`) |
| Codex CLI | `responses`, `responses/compact` | ChatGPT Codex subscription models (HTTP and WebSocket), and streaming paid Responses targets over HTTP (with WebSockets disabled in generated setup) |

To plan with a GPT model and implement with GLM in OpenCode, put an OpenCode Go
account listing `gpt-6.2-sol` and a GLM Coding Plan account listing `glm-5.4` in
one pool, publish a route with both models, and generate the OpenCode
configuration with `model=glm-5.4` and `plan_model=gpt-6.2-sol`. Claude Code can
plan the same way with MiniMax or Qwen using native forwarding, or with Codex
accounts when `messages_to_codex` is published. Codex accounts become available
to OpenCode only with explicit `chat_to_codex` translation, and Antigravity
accounts become available to OpenCode only with explicit `chat_to_antigravity`
translation.

When several accounts list a model, the conversation's own accounts come first.
See [Conversations and authority](#conversations-and-authority). A conversation
uses one account of each adapter, so the planning and background models must be
ones it reaches with the main model whichever account it pins first. Two
OpenCode Go accounts listing different models cannot serve one conversation.

Every request carries the conversation so far, so each subscription a
conversation mixes receives the turns the others produced. No live vendor is
yet qualified to accept another vendor's reasoning and tool calls in that
history, and mixed conversations are qualified against scripted vendors only.

## Import Codex credentials once

In the code account editor under **Code mode -> Accounts**, select a provider
connection using the Codex subscription profile, paste its `auth.json` into
**Codex auth.json**, and choose **Import Codex credentials**. Save the account
to associate the new credential. This is a one-time migration. OLP stores
encrypted credentials and owns refresh after import. Stop using the original
local credentials after a successful import. OLP does not read, watch or
synchronize local files.

The supported JSON document contains a `tokens` object with string fields
`access_token`, `id_token`, `refresh_token` and `account_id`. The token account and
user claims must agree with each other and with `account_id`. Optional top-level
fields are `auth_mode` set to `chatgpt`, `OPENAI_API_KEY` set to `null`, and
`last_refresh` as an RFC3339 timestamp. Other fields, API keys, duplicate JSON
fields and conflicting identities are rejected. Expired local access and identity
tokens are accepted only when refresh verifies the same account and user.

The management API accepts `POST /api/v1/providers/{provider_id}/codex-import`
with the current provider `If-Match` header and a JSON body containing
`credentials`, the document above encoded as a string. Optional `slot_id` selects
an existing credential slot. Otherwise import uses the default slot. OLP verifies
the refresh token through the pinned Codex profile and the provider's approved
network controls before storing it. An occupied slot requires the same verified
principal. The response contains credential metadata and never tokens.

Import preserves account and conversation pins. Replacing an account's credential
requires saving that account with the returned credential identifier. Import and
refresh do not send inference requests or establish subscription eligibility.
This path has controlled-fixture coverage only. Real subscription qualification
remains subject to the release gates linked above.

## Import Claude credentials once

In the code account editor under **Code mode -> Accounts**, select a provider
connection using the Claude subscription profile, paste the supported
`.credentials.json` document into **Claude .credentials.json**, and choose
**Import Claude credentials**. Save the account to associate the new credential.
OLP encrypts the imported grant and owns refresh afterward. Stop using the
original local credentials after import. This is a one-time operation. OLP does
not read, watch or synchronize local files.

The supported document contains only a `claudeAiOauth` object with opaque string
fields `accessToken` and `refreshToken`, an `expiresAt` Unix timestamp in
milliseconds, and a `scopes` array containing `user:profile` and `user:inference`.
Optional fields are `refreshTokenExpiresAt` and `subscriptionTypeVerifiedAt` as
positive Unix timestamps in milliseconds, `subscriptionType` and `rateLimitTier`
as strings or `null`, `subscriptionTypeSource` set to `sign_in` or `profile`, and
`clientId` matching the supported subscription client's identifier. This format
was checked against the official Claude Code 2.1.314 credential writer. See the
[official authentication documentation](https://code.claude.com/docs/en/authentication)
for local sign-in. API keys, other credential stores, unknown fields, duplicate
JSON fields and unsupported client identifiers are rejected.

The management API accepts `POST /api/v1/providers/{provider_id}/claude-import`
with the current provider `If-Match` header and a JSON body containing
`credentials`, the supported document encoded as a string. Optional `slot_id`
selects an existing credential slot. Otherwise import uses the default slot.
OLP first authenticates the pasted access token against the fixed subscription
profile endpoint. It then refreshes through the pinned Claude plugin and verifies
the same account and organization again, using the provider's approved network
controls. Credentials whose original identity cannot be verified, including
expired or revoked access tokens, require a fresh local sign-in before import.
OLP does not infer identity from opaque token contents or local metadata.

An occupied slot requires the same verified principal. Account or organization
changes are rejected without modifying the existing account or its conversation
pins. The response contains credential metadata and never tokens. Same-identity
replacement preserves pins after the account is saved with the returned
credential identifier. Import and refresh do not send inference requests or
establish subscription eligibility. Real subscription qualification remains
subject to the release gates linked above.

## Import Antigravity credentials once

In the code account editor under **Code mode -> Accounts**, select a provider
connection using the `antigravity-subscription` profile, paste the narrow
migration JSON into **Antigravity migration JSON**, and choose **Import
Antigravity credentials**. Save the account to associate the new credential. OLP
encrypts the imported client secret and refresh token together and owns refresh
afterward. Stop using the original local credentials after import. This is a
one-time migration, and OLP does not synchronize local files or read raw IDE
authentication stores.

The supported JSON envelope requires `type` equal to `"antigravity"`,
`access_token`, `refresh_token`, `client_id` and `client_secret`. The `client_id`
and `client_secret` must belong to the original Google desktop OAuth client that
issued the refresh token, and `client_id` must match the provider's configured
public `client_id` option. Optional fields are `expired` as an RFC3339 timestamp
in the future and no more than ten years away, positive integer `expires_in` and
`timestamp`, string `email` and string `project_id`. A supplied `project_id` must
match the authenticated Cloud Code Assist project. Other fields, duplicate JSON
keys, service account credentials and raw IDE stores are rejected.

The management API accepts
`POST /api/v1/providers/{provider_id}/antigravity-import` with the current
provider `If-Match` header and a JSON body containing `credentials`, the
envelope above encoded as a string, plus optional `slot_id`. OLP authenticates
the pasted access token against the fixed Google OpenID userinfo and Cloud Code
Assist `loadCodeAssist` endpoints, then refreshes through the pinned Antigravity
plugin and verifies that the same Google subject and project remain unchanged.
An occupied slot requires the same verified principal. See the
[Antigravity plugin guide](../../internal/codemode/antigravityauth/antigravity/README.md)
for the full enrollment and import contract.

## Research tools

Research tools use a pool account independently of the coding model. A route
serving only GPT can use a Z.ai global GLM Coding Plan account to search
documentation, read a webpage and inspect a public repository. GLM does not need
to appear in the route's model list.

In the existing route editor, enable **Research tools**, select an eligible
Z.ai global account from the route's pool, and choose **Search**, **Web reader**
and/or **Repository research**. The sole qualifying account is preselected.
Inference health cooldowns and exhausted coding allowances do not remove an
otherwise enrolled research account, and management reports `research_eligible`
separately from inference `eligible`. Save and publish, then open **Client
configuration**. Existing keys with the `inference` scope, route access and pool
membership can use the enabled services without model selection, conversation
binding or separate MCP registration.

![Research tools enabled on a GPT-only route](../images/code-research-route.png)

The management route write accepts an optional `tools` binding:

```json
{
  "tools": {
    "account_id": "11111111-1111-4111-8111-111111111111",
    "services": ["search", "reader", "zread"]
  }
}
```

Omitting `tools` or setting it to `null` disables research in the next
publication. Draft changes take effect only after publication. Account and
service selections are frozen with the route revision, alongside its provider
connections. The account must remain enabled, enrolled in the same project and
assigned to that pool. Only `zai-coding-plan`, the global profile, qualifies.

| Service | Route endpoint | Upstream service | Exposed tools |
| --- | --- | --- | --- |
| Search | `/code/{slug}/mcp/search` | `web_search_prime` | `webSearchPrime` |
| Web reader | `/code/{slug}/mcp/reader` | `web_reader` | `webReader` |
| Repository research | `/code/{slug}/mcp/zread` | `zread` | `search_doc`, `get_repo_structure`, `read_file` |

Upstream endpoints are fixed at
`https://api.z.ai/api/mcp/{upstream_service}/mcp` and use the account's enrolled
grant through the published connection's egress, proxy and TLS controls.
Vision, client roots, sampling and local-file forwarding are outside this
surface. The management MCP endpoint and project-owned MCP registrations are
separate features.

### Client setup

Set `OLP_API_KEY` in the client environment. Merge generated entries into
existing files, preserving other MCP servers, tools and settings. Server names
include the route slug and service. Users choose which tools to invoke in their
client, and generation does not disable built-in tools.

For Codex, the generated TOML includes `[mcp_servers."olp_<slug>_<service>"]`
tables with a route endpoint and `bearer_token_env_var = "OLP_API_KEY"`. For
OpenCode, generated `opencode.json` includes remote `mcp` entries with
`oauth: false` and `Authorization: "Bearer {env:OLP_API_KEY}"`. For Claude Code,
source the generated shell setup as before and merge the companion `.mcp.json`
into the project, whose HTTP entries use `Authorization: "Bearer ${OLP_API_KEY}"`.
The management response returns this companion in optional `additional_files`,
and the console displays and copies every required file. Approve project MCP
servers through Claude's normal flow.

These formats follow the official [Codex MCP configuration](https://developers.openai.com/codex/mcp),
[OpenCode remote MCP configuration](https://opencode.ai/docs/mcp-servers/) and
[Claude Code environment expansion](https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcpjson).
Vendor credentials stay on the gateway.

### Authorization and transport

Every POST, GET stream and DELETE checks the live key, end-user policies, route
publication, pool membership, account and grant. OLP translates upstream session
IDs to opaque IDs bound to the key, route revision, service, account, credential
and grant generation. Stateless upstreams also receive an OLP session fence,
without forwarding an invented session ID to the vendor. Initialize first and
send the returned OLP session ID on subsequent requests. Republishing or
rotating credentials requires initialization again, and another key cannot reuse
a session. Rotation reclaims obsolete sessions across all research services for
the route. State is shared across gateway replicas, expires after one idle hour
or 24 hours total, and is limited to 32 sessions per key and 10,000 per
installation. Session creation reclaims expired metadata. SSE resume IDs are
authenticated to the OLP session and credential, so an event ID from another key
or session cannot resume a stream, including on stateless upstreams. OLP
translates valid resume IDs back to native upstream IDs. Session state is
tentative until a valid initialization result for the request reaches the
client. Failed, malformed or interrupted handshakes release their session slots,
including JSON delivery failures and client cancellation. A delayed handshake
cannot retire sessions from a newer publication or credential generation. SSE
response headers are flushed immediately, including for idle streams that
initially contain only heartbeat comments. Upstream heartbeat comments also
reach the client while waiting for data, and write deadlines apply while sending
data without shortening idle HTTP/2 streams.

The proxy answers upstream [MCP pings](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/ping)
locally after rechecking live authorization and the session's credential
generation, including end-user blocks added while the stream is open. Ping
replies use the stream's existing admission, with at most 32 replies per stream
and five seconds per reply. Other server callbacks remain disabled.

The proxy supports [MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports),
covering initialization, paginated discovery, calls, notifications,
cancellation, JSON, SSE event IDs and session termination. Discovery exposes
only the listed tools, while native schemas, structured results, source
references and tool errors pass through. Each request is bounded to two minutes,
1 MiB of input, 8 MiB of upstream output and 1 MiB per SSE event, and
installation and route body limits can lower the input limit. Key and connection
request and concurrency limits also apply. GET notification streams use separate
concurrency counters per service at the key, group, route, end-user and provider
boundaries. They do not occupy tool-call slots, and request-rate counters remain
shared with other research requests. Session-scoped lifecycle and progress
notifications retain request-rate limits without needing another work slot. This
lets initialized notifications complete an open SSE handshake and cancellation
stop an active tool call. Research has no inference token usage, token-rate
reservation or hard token reservation.

Published research uses the route's frozen provider connection. Staging a new
provider draft does not change dispatch until the route is published again.

Dispatched calls are never replayed or moved to another account. Service errors
and tool allowance errors do not change inference account health or affect other
services. Arguments, results and credentials are never written to diagnostic
ledgers, and retained session rows contain only transport and authorization
metadata. Live subscription and real-client research qualification remain
release gates.

## Operator workflow

Code accounts, pools, routes and budgets belong to projects. A management session
or management token needs the existing configure authority and project-manager
or global authority to change them. Read access is project-scoped. Session
mutations require CSRF protection. Use `Idempotency-Key` on creation, publication
and retirement, and `If-Match` on updates, drain changes and publication.

1. Enroll the account through the provider's supported authorization flow. OLP
   stores encrypted grant material under a credential version and its observed
   principal. A generic OAuth plugin demonstration does not establish supported
   account authorization. Do not paste an access token into a developer's CLI or
   mark a fixture account as production-qualified. Codex enrolls through the
   official device sign-in. Claude uses headless browser sign-in where you open
   the authorization URL, sign in upstream, and paste the returned `code#state`
   or full HTTPS callback into OLP, and the profile endpoint verifies account
   and organization identity on enrollment and every refresh (see the
   [Claude plugin guide](../../internal/codemode/claudeauth/claude/README.md)).
   Antigravity uses headless Google PKCE enrollment with an operator-supplied
   Google desktop OAuth `client_id` on the provider, where you open the returned
   authorization URL, copy the loopback callback URL, and submit
   `{"callback_url":"...","client_secret":"..."}` in the masked grant input so
   OLP verifies Google OpenID subject and an existing Cloud Code Assist project
   and encrypts the client secret with the refresh token (see the
   [Antigravity plugin guide](../../internal/codemode/antigravityauth/antigravity/README.md)).
   OpenCode Go and the coding plans enroll a pasted API key from the vendor's
   key page without making any upstream call, storing the key encrypted and
   taking a profile-scoped SHA-256 fingerprint as the principal. Pasted keys
   never expire or refresh, and rotating a key creates a new principal and
   requires a new code account and revoking the old credential version because
   conversations pinned to the old account must start again.
2. Create a code account with `project_id`, `provider_id`, `credential_id`,
   `name`, `enabled` and an explicit `models` array. Account identity survives
   credential rotation only when both provider and principal remain the same.
   Set **Maximum simultaneous generations** (API field `max_concurrency`) to a
   positive integer to cap the account across all gateway replicas, routes and
   keys, or leave it blank or send `null` for no account cap. Finish upgrading
   every gateway replica before enabling the cap. For Kimi Code accounts, you
   can set **Extra Usage** (API field `extra_usage_enabled` as `true`, `false`
   or `null`) to record whether the member account has paid Extra Usage enabled
   beyond included plan quota so paid overflow is never read as spare included
   allowance. For Qwen Token Plan accounts, you can set **Subscription cycle
   anchor** (API field `cycle_anchor` as a `YYYY-MM-DD` date or `null`) to
   record the monthly quota reset day anchor used when Qwen reports exhausted
   quota. Both adapter-specific settings are rejected on other adapters.
3. Create a pool and assign accounts and API keys explicitly. Accounts of
   several adapters may share one pool. A `shared` pool has no owner. A
   `personal` pool names `owner_user_id` and accepts only keys issued by that
   owner. Being a project member does not itself grant pool use. In the console
   pool editor, set each selected account's weight, or send `account_weights`
   keyed by account ID in the pool API. Weights are integers from 1 to
   2147483647, and omitted accounts default to weight 1. Larger weights give
   eligible accounts a larger share of new conversations. Accounts must still
   support the exact model, protocol and transport and be available. Weight
   changes affect new account selections, while existing bindings and model pins
   keep their accounts across resumes, reconnects and child conversations.
4. Create a route with a stable `slug`, `pool_id`, `models` and `enabled`, then
   publish it. Each model needs an enrolled pool account that lists it or an
   enabled paid target. The published revision freezes connection configuration.
   Adding an account backed by a previously absent provider requires
   republishing the route before that connection can serve it.
5. Give the developer an OLP inference key and the supported route
   configuration. Keep grant access and refresh tokens and account authorization
   in OLP. The generated `client-config` JSON reports `"client_version": "latest"`
   alongside the model list and qualification gaps, while qualification run
   records capture exact client binary versions. In the console's Code mode
   page, set the public gateway URL before copying the configuration.
6. Inspect account eligibility, bindings, attempts, decisions, refusals and
   token windows. Retire a binding to stop its entire tree. A retired or
   unavailable conversation fails, and the caller must explicitly start a new
   conversation to choose again.

All lifecycle actions must avoid synthetic inference, including enrollment,
account activation, refresh, publication, health checks and background maintenance.
Initial `health: unknown` and a current grant do not imply successful inference
qualification. Ordinary provider probe and certification workflows are not
code-mode activation checks.

### Management surface

| Operation | Endpoint |
| --- | --- |
| List or create accounts, pools, routes and budgets | `GET`/`POST /api/v1/code/{accounts,pools,routes,budgets}` |
| Update one resource | `PUT /api/v1/code/{collection}/{id}` |
| Start or resume routing on an account drain | `PUT /api/v1/code/accounts/{id}/drain` |
| Request an active allowance read for a qualified account | `POST /api/v1/code/accounts/{id}/allowance-reads` |
| Import local credentials once for Codex, Claude or Antigravity | `POST /api/v1/providers/{provider_id}/{codex,claude,antigravity}-import` |
| Publish an immutable route revision | `POST /api/v1/code/routes/{id}/publish` |
| Inspect route history | `GET /api/v1/code/routes/{id}/revisions` |
| Generate client configuration | `GET /api/v1/code/routes/{id}/client-config?gateway_url=...&client=...&platform=...&model=...&plan_model=...&small_model=...` |
| Inspect diagnostics and metadata | `GET /api/v1/code/{bindings,attempts,decisions,refusals,token-windows}` |
| Retire the root of a conversation tree | `POST /api/v1/code/bindings/{id}/retire` |

Lists support `project_id`, `cursor` and `limit`. Diagnostics additionally
support the applicable `route_id`, `api_key_id`, `account_id` and `binding_id`
filters. `attempts` and `decisions` additionally accept `attempt_id`, and on
`decisions` the `account_id` filter matches `chosen_account_id`. `attempts` and
`refusals` additionally accept `end_user_digest`, taking a 64-character hex
lookup digest or `unidentified`. A binding matches `account_id` through its
first account or any of its tree's pins, and `attempts` matches `binding_id`
across both `binding_id` and `paid_binding_id`. Use collection results' ETags
when updating resources or toggling drain. The API and console must ship
together, because a backend-only deployment does not meet the complete product
contract.

## Client configuration

Client configuration reads the published revision and the accounts of its pool.
`supported_clients` lists the clients the route's subscriptions or paid targets
support, in the order of the route's models, and `client` defaults to the first.
`native_models` are the route models the client can reach, meaning models listed
by an account or streaming paid target that supports the client, and `model`
defaults to the first. `adapters` names the subscriptions they come from.
`plan_model`, for planning, and `small_model`, for background requests, default
to `model`. Codex takes neither. A client no subscription or paid target
supports, or a model the client cannot reach, returns 422. No configuration ever
contains an OLP key, because every generated snippet reads it from `OLP_API_KEY`.

Choose Linux, macOS or Windows in the console before copying the setup. API
callers pass `platform=linux`, `platform=macos` or `platform=windows` to
`GET /api/v1/code/routes/{id}/client-config`. An omitted platform defaults to
Linux for existing callers. The response includes `platform`, `instructions`
and the destination in `file`. Linux and macOS use a POSIX shell. Windows uses
PowerShell, including `$env:OLP_API_KEY`. Choose Linux when running inside WSL.

Treat the generated configuration as a fragment. For Codex, update the root
`model` and `model_provider` settings and replace only `model_providers.olp` in
the user configuration. The path is `$CODEX_HOME/config.toml`, defaulting to
`$HOME/.codex/config.toml`. In PowerShell, these paths are
`$env:CODEX_HOME\config.toml` and `$HOME\.codex\config.toml`. Do not put the
provider in a project-local `.codex/config.toml`, where Codex ignores provider
and authentication settings. Merge MCP tables by name without duplicating TOML
keys or tables, preserving unrelated settings and servers.

For OpenCode, merge the generated fields into the project's `opencode.json`,
or its existing `opencode.jsonc`. Update the selected models and provider list,
including `agent.plan.model` when planning uses the main model again. Preserve
other plan-agent settings while merging provider, agent and MCP entries by name,
and retain other tools and settings. Keep environment references intact. For Claude Code, save the shell
setup as `olp-claude.sh` on Linux or macOS, or `olp-claude.ps1` on Windows.
Source it with `. ./olp-claude.sh` or `. .\olp-claude.ps1`, then launch `claude`
in the same shell. The setup changes the process environment without writing
`settings.json`. Merge companion MCP entries into the project `.mcp.json` and
approve the project servers in Claude Code.

These paths and merge rules follow the official
[Codex configuration reference](https://developers.openai.com/codex/config-reference/),
[Claude Code settings](https://code.claude.com/docs/en/settings),
[Claude Code environment variables](https://code.claude.com/docs/en/env-vars) and
[OpenCode configuration](https://opencode.ai/docs/config/) documentation, checked
on 2026-10-30. Generated platform setup does not establish native client startup
on Windows or macOS, or live subscription qualification on any platform.

### Codex

Use the OLP CLI to set up one published route in the user configuration
without merging TOML by hand. Set `OLP_MANAGEMENT_URL` and
`OLP_MANAGEMENT_TOKEN_FILE` as for other management commands. Use the route
ID from the console and pass the public gateway URL:

```sh
olp code-setup preview --route "$ROUTE_ID" --gateway-url "$GATEWAY_URL"
olp code-setup apply --route "$ROUTE_ID" --gateway-url "$GATEWAY_URL" --revision "$REVISION_ID"
olp code-setup status
olp code-setup detach
```

Preview shows the destination, endpoint, model, and published revision without
writing. Copy the revision ID from preview to make apply refuse a newer
publication. Add `--model MODEL` to both commands to select a model rather than
using the route's default. Apply only manages the top-level `model` and
`model_provider` settings and the `[model_providers.olp]` table. It saves the
previous settings in a comment in the same file. Keep that comment for
detach. Apply and detach leave other settings alone. MCP servers and tool
permissions are never changed by the CLI. Merge optional MCP configuration
separately if you need it.

Status and detach work offline. If a managed setting was edited, both name the
conflicting setting and detach leaves the entire file untouched. Restore the
installed setting before retrying detach, or remove the setup comment and manage
the file by hand. Unsupported shapes, including inline OLP provider tables and
custom keys in the OLP provider, are refused without writing.

The fixture qualification gate installs the lockfile-pinned official Codex CLI
(`@openai/codex@0.171.0` from `tests/qualification/clients/package.json`) and
runs one HTTP `responses` journey against a scripted upstream, while the
`client-config` API response still reports `"client_version": "latest"`. This
example shows the generated provider fragment rather than a real-account
qualification or a substitute for OLP's generated configuration:

```toml
model_provider = "olp"
model = "gpt-6.2-sol"

[model_providers.olp]
name = "OpenAI"
base_url = "https://olp.example/code/team-coding"
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
http_headers = { "X-OLP-Code-Model" = "gpt-6.2-sol" }
```

Supply only the OLP key as `OLP_API_KEY`. No workstation ChatGPT login is used
in the fixture lane. `gpt-6.2-sol` is an example native model identifier, not
the route slug and not proof that a particular account is entitled to it. Select
only a model allowed by the published route and enrolled account.

The generated configuration retains Codex's default request and stream retries.
OLP forwards each inference once by default. An explicitly published
[initial recovery policy](#portable-conversation-recovery) can permit bounded
additional attempts after proven nonexecution and portable initial state. Every
client retry gets fresh admission on its durable conversation account. A 503 can
quarantine that account, and subsequent retries fail without dispatch until its
cooldown expires.

`X-OLP-Code-Model` is an OLP-only selection hint for the initial WebSocket
handshake, before the native body arrives. It is removed upstream. It cannot
change an existing account pin or bypass the model check on each generation.
Generate configuration again with `model=<chosen-native-model>` when changing
models. Because `X-OLP-Code-Model` is static in the generated TOML, passing `-m`
changes the request body without updating that header and can pin an
incompatible account in a disjoint-model pool. Arbitrary `-m` overrides are not
qualified for disjoint-model pools.

### Claude Code

The fixture qualification gate installs the lockfile-pinned official Claude Code
CLI (`@anthropic-ai/claude-code@2.1.330` from
`tests/qualification/clients/package.json`) and runs one HTTP `v1/messages`
journey against a scripted upstream, while the `client-config` API response
still reports `"client_version": "latest"`. OLP generates a POSIX shell file for
Linux and macOS, or a PowerShell file for Windows, to source before starting
`claude`. The POSIX setup matches this format:

```sh
: "${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_BASE_URL='https://olp.example/code/team-glm'
export ANTHROPIC_MODEL='glm-5.4'
export ANTHROPIC_DEFAULT_OPUS_MODEL='glm-5.4'
export ANTHROPIC_DEFAULT_SONNET_MODEL='glm-5.4'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='glm-5.4'
export CLAUDE_CODE_SUBAGENT_MODEL='glm-5.4'
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC='1'
```

With a `plan_model` other than `model`, the file sets `ANTHROPIC_MODEL` to
`opusplan` and `ANTHROPIC_DEFAULT_OPUS_MODEL` to the planning model. Claude Code
then plans on the planning model in plan mode and uses
`ANTHROPIC_DEFAULT_SONNET_MODEL`, the native model, for everything else. Pass
`small_model` when you want `ANTHROPIC_DEFAULT_HAIKU_MODEL` to target a
different published native model.

Every model variable names a native model the route admits, so background,
subagent and classifier requests stay on the route instead of sending Anthropic
model names that the route refuses. `ANTHROPIC_AUTH_TOKEN` sends the OLP key as
a bearer token, and the file unsets a workstation `ANTHROPIC_API_KEY`. The
non-secret variables may instead go in the `env` block of
`.claude/settings.json`, while keeping the key in the environment. Z.ai's guide
suggests `API_TIMEOUT_MS` and `CLAUDE_CODE_AUTO_COMPACT_WINDOW`. A generation
through OLP is limited to ten minutes regardless of the client timeout, and OLP
does not know a model's context window, so set the auto-compact window yourself
if you need it.

Claude Code's `HEAD /api/hello` connectivity probe and `count_tokens` are not
served, so the client falls back to local estimates. Vendor-native server tools
and claude.ai login features are outside code mode. Optional
[research tools](#research-tools) are exposed through the companion MCP setup.

### OpenCode

The fixture qualification gate installs the lockfile-pinned official OpenCode
CLI (`opencode-ai@1.19.6` from `tests/qualification/clients/package.json`) and
runs one HTTP `v1/chat/completions` journey against a scripted upstream, while
the `client-config` API response still reports `"client_version": "latest"`. OLP
generates an `opencode.json` that configures providers pointing at the route's
`/v1` base URL. Subscription providers are `opencode-go`, `zai-coding-plan` for
either GLM profile, `minimax-coding-plan` for either MiniMax profile,
`kimi-code-plan-global` for either Kimi profile, `alibaba-token-plan` for either
Qwen profile, `olp-codex` when `chat_to_codex` is enabled and `olp-antigravity`
when `chat_to_antigravity` is enabled. Streaming paid API targets use
`olp-paid-responses`, `olp-paid-messages` and `olp-paid-chat`.

Each model is listed under the provider of the subscription or paid target that
serves it, with both `whitelist` and explicit `models` definitions naming the
AI SDK package (`@ai-sdk/openai`, `@ai-sdk/anthropic` or
`@ai-sdk/openai-compatible`), and every provider points at the route. The
provider selects the endpoint OpenCode uses for a model, so when several
subscriptions offer the same model, OLP picks providers with which one
conversation reaches the main, planning and background models together. With a
`plan_model` other than `model`, OpenCode's plan agent uses the planning model
and its build agent uses `model`. For a route mixing OpenCode Go and GLM Coding
Plan:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "zai-coding-plan/glm-5.4",
  "small_model": "zai-coding-plan/glm-5.4",
  "agent": {
    "plan": {
      "model": "opencode-go/gpt-6.2-sol"
    }
  },
  "enabled_providers": [
    "opencode-go",
    "zai-coding-plan"
  ],
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "https://olp.example/code/team-mix/v1",
        "apiKey": "{env:OLP_API_KEY}"
      },
      "whitelist": [
        "gpt-6.2-sol"
      ],
      "models": {
        "gpt-6.2-sol": {
          "id": "gpt-6.2-sol",
          "name": "gpt-6.2-sol",
          "provider": {
            "npm": "@ai-sdk/openai"
          }
        }
      }
    },
    "zai-coding-plan": {
      "options": {
        "baseURL": "https://olp.example/code/team-mix/v1",
        "apiKey": "{env:OLP_API_KEY}"
      },
      "whitelist": [
        "glm-5.4"
      ],
      "models": {
        "glm-5.4": {
          "id": "glm-5.4",
          "name": "glm-5.4",
          "provider": {
            "npm": "@ai-sdk/openai-compatible"
          }
        }
      }
    }
  }
}
```

Generated OpenCode settings define each reachable model and its native SDK,
including models absent from the client's bundled catalogue.
`enabled_providers` stops OpenCode loading other providers, and `small_model`
keeps title generation on the route. Never run `opencode auth login` for the
plan on a developer machine. OpenCode asks the npm registry for its own
dependencies at startup, and that traffic does not go through the model route.

`GET /code/{slug}/models` and `GET /code/{slug}/v1/models` return the published
models currently served by eligible pool accounts or paid targets, their native
protocol paths and transports, and supported clients. Authenticate with the same
OLP inference key used for requests. Key revocation, pool membership, account
availability and credential eligibility apply to every discovery request.
Generated setup uses the same catalog and returns it in `models`, and discovery
does not contact upstream services or report account identities. The catalog is
a current view, not a guarantee that a later request will succeed.

## Conversations and authority

### Generic protocol access

An authorized client can send complete native Responses, Messages or Chat
Completions requests to the paths its published route serves. An OLP key with
the `inference` scope, route permission and pool membership is sufficient.
Private coding-client identity headers are optional. For example, set
`OLP_API_KEY` to your project key and `OLP_NATIVE_MODEL` to a model the route
allows, then send a complete Responses request to a Codex-backed route:

```sh
curl --fail-with-body https://olp.example/code/team-coding/responses \
  -H "Authorization: Bearer ${OLP_API_KEY}" \
  -H 'Content-Type: application/json' \
  --data "{\"model\":\"${OLP_NATIVE_MODEL}\",\"input\":\"Explain what this function does\",\"store\":false}"
```

Use `/v1/responses`, `/v1/messages` or `/v1/chat/completions` with the matching
native body for other adapters. Native forwarding, model names and explicit
translation or model-alias policies apply as usual. OLP preserves native bytes
and unknown extensions and supplies the selected account's credential. It adds
a Codex identity fallback only when the separate policy is enabled and published.
Provider access rules still apply, and accepting a generic request locally does not establish live provider
qualification.

Without affinity, each HTTP request starts an independent conversation. Send
the complete text and function-tool history needed for that request. To keep
the same account binding across requests, add `X-OLP-Code-Affinity` with a stable
identifier of at most 256 bytes. The header must contain one nonempty value
without surrounding whitespace. OLP consumes it locally and scopes the binding
by route and API key, so equal values from different keys or projects do not
share a conversation. The value selects no recognized client's parent or child.
Generic WebSocket Responses require this header on the handshake.

With explicit affinity, `previous_response_id` may continue a response OLP
observed for that key and conversation, subject to its producing account and
model ownership. Unknown, ambiguous or differently owned references refuse.
An affinity cannot adopt another affinity's conversation or a recognized
client's tree through a response reference. Without affinity, response
references refuse. Generic requests also refuse opaque or signed thinking
history, turn state, compaction, stored prompts, stored item/file references,
server tools and other unsupported stateful content. Complete
function-call arguments and tool results remain native input, including fields
that happen to have names such as `signature`. OLP never converts opaque state
into a stateless prompt. Every admitted generation uses the same live access,
limits and accounting checks described below.

### Recognized client lineage

Recognized clients name their conversations in headers or Codex request metadata
fields that OLP reads and forwards unchanged.

| Client | Conversation | Child conversation |
| --- | --- | --- |
| Codex | `thread-id`, `session-id` or `session_id` header, or `client_metadata.thread_id` body field | `x-codex-parent-thread-id` header or `client_metadata["x-codex-parent-thread-id"]` body field, paired with `x-openai-subagent` header or `client_metadata["x-openai-subagent"]` body field |
| Claude Code | `x-claude-code-session-id` | `x-claude-code-agent-id`, beneath `x-claude-code-parent-agent-id` or the session |
| OpenCode | `x-opencode-session-id` | beneath `x-opencode-parent-session-id` |

When both a Codex header and the matching `client_metadata` field are present,
they must agree or the request refuses with `code_identity_ambiguous`. Sending
`x-openai-subagent` without a parent thread identifier refuses with
`code_parent_unresolved`.

A Claude Code agent is scoped by its session, as `session/agent`, because
teammate agents reuse name-based identifiers. Identifier bytes outside letters,
digits, `.` and `-` are encoded as `:xx`, so OpenCode's `ses_...` appears as
`ses:5f...`. Mixing recognized client families or combining their identities
with `X-OLP-Code-Affinity` refuses. The Codex paths accept Codex identities, and
the `v1/` paths accept Claude Code or OpenCode identities. Recognized identities
keep their existing conversation and parent lineage.

The first admitted top-level conversation selects one eligible pool account and
atomically persists its binding. The first admission of each model in the
conversation tree pins one account to that model. A tree uses at most one
account of each subscription family, so a new model takes the tree's first
account, or else another account the tree already uses, when it lists the model
and serves the request's path. Otherwise it takes the first available eligible
account of a family the tree does not use yet. Another account of a family the
tree already uses never joins it, which keeps a vendor's conversation state on
one account. When a republish puts two of a tree's accounts in one family, the
account the tree used first keeps serving and the other refuses its pinned
models. Each pin records its account's family, so a family stays the tree's
after a republish drops that account. Concurrent first turns on different
replicas must resolve to the same binding and pins. Resumes, reconnects,
children, compaction and client retries keep each model's pinned account. A
child with an unresolved parent is refused instead of receiving a new account.
Bindings list their tree's pins.

Translated Messages requests use the same durable bindings and pins. Reusing a
session and agent identifier after a gateway restart resumes that agent's
account ownership. Send the complete portable text and tool history, or an
explicit text summary after client compaction. OLP stores identity and account
metadata rather than prompts or history. Signed thinking, redacted reasoning
and opaque compaction blocks that cannot be represented refuse with
`code_translation_unsupported_semantics` before dispatch. Use a native Messages
account when those blocks are required.

A `previous_response_id` belongs to the account that produced it, scoped to the
route and API key. It cannot continue on another account in the same conversation
tree. A conflicting owner returns `code_response_account_conflict`. Unknown or
ambiguous ownership returns `code_parent_unresolved`. Legacy references remain
usable when the tree's dispatched attempts prove a single producing account.
Otherwise, start a new conversation with full context and no response reference.

Fresh `codex exec review` is a child-first workflow whose review child requires
an established parent pin at OLP without parent inference. With the generated
WebSocket-enabled configuration, the root WebSocket handshake establishes its
pin at OLP before the review child starts, including when the upstream rejects
the WebSocket with 426 and Codex falls back to HTTP after OLP has bound the
root. Disabling WebSockets from the outset leaves the parent unresolved and
returns `code_parent_unresolved`. An external proxy that blocks the root
handshake before it reaches OLP prevents parent establishment as well. The
official app-server can review a thread after a real user task on that thread
over either transport. Do not send a dummy inference merely to acquire a parent
pin.

Every generation rechecks key, project, route and pool permission, account
eligibility, grant state and limits. A WebSocket upgrade is not lasting
authorization to generate. Revocation, retirement, an unavailable account,
exhausted allowance or an unsupported model never triggers silent account
switching, and a cooling account that the tree would take for a new model
refuses that model too. A credential refresh may change authentication for the
same principal, and it must not change the principal stored in the binding.

### Thread status from the coding client

A developer can check whether their own thread can continue without sending a
generation. Send `GET /code/{slug}/status`, or `GET /code/{slug}/v1/status`,
with the same OLP inference key and the same conversation identity headers
the client already sends, as listed under
[Recognized client lineage](#recognized-client-lineage). A generic client
sends its `X-OLP-Code-Affinity` value instead. For a Codex thread, send a
request like this:

```sh
curl --fail-with-body https://olp.example/code/team-coding/status \
  -H "Authorization: Bearer ${OLP_API_KEY}" \
  -H "Thread-Id: ${THREAD_ID}"
```

Polling never binds a conversation, never contacts an upstream and never
dispatches or reserves capacity, so a client can poll on a timer. The answer
is advisory. It reads only the local evidence admission already keeps, which
is the live key, pool and grant checks for the accounts the conversation
tree is pinned to, plus their stored health, allowance and cooldown
observations. It reports no quotas, balances, account names, models or
sibling threads.

A successful reply carries four fields. `status` is `available`,
`unavailable` or `unknown`. `checked_at` is when this check ran.
`last_observed_at` is when the newest stored observation was taken, or
`null` when nothing has been observed yet. `retry_at` is set only when a
current blocker has a known future deadline, and the `Retry-After` header
then carries the same delay in seconds.

| Status | Meaning |
| --- | --- |
| `available` | Current local evidence supports another attempt on every account the tree uses. The newest evidence for each account is a healthy observation taken after its last blocker and after that blocker's deadline. Capacity is still not reserved, so the next generation can still meet a limit. |
| `unavailable` | A current blocker is known, such as an active cooldown, an exhausted allowance, a revoked grant or a retired tree. `retry_at` names when the thread is expected to clear, when every blocker has a known deadline. |
| `unknown` | Current evidence is insufficient. Nothing is known to block the thread, but no healthy observation newer than the last blocker establishes that another attempt will be admitted. |

The two times stay separate because an old observation cannot set a current
retry time. A cooldown observed at 10:00 with a retry deadline of 10:05
answers `unavailable` with `retry_at` 10:05, `last_observed_at` 10:00 and
`checked_at` 10:02 when polled at 10:02. Polled again at 10:06 with no newer
evidence, the same thread answers `unknown` with no `retry_at`, because the
deadline passing alone does not prove the account healthy. A later
successful generation records the healthy observation that moves the thread
to `available`.

An unknown conversation, a thread belonging to another key or another
route, and a conversation whose named parent does not match its stored
lineage all answer the same `404` with code `code_thread_not_found`, so
polling cannot confirm another caller's thread. A retired tree answers
`unavailable` with no retry time. A thread served only by paid API targets
has no subscription evidence to report and answers `unknown` while its tree
is live.

Claude Code can show the status in its status line. Save this script, point
`OLP_CODE_STATUS_URL` at the route's status endpoint, and register the
script as the `statusLine` command in Claude Code settings. The command
receives the session JSON, including `session_id`, on standard input, as
described in the [Claude Code status line documentation](https://code.claude.com/docs/en/statusline),
checked on 2026-11-21.

```sh
#!/bin/sh
input=$(cat)
session=$(printf '%s' "$input" | jq -r '.session_id // empty')
if [ -z "$session" ] || [ -z "${OLP_API_KEY:-}" ] || [ -z "${OLP_CODE_STATUS_URL:-}" ]; then
  printf 'OLP unknown'
  exit 0
fi
curl -fsS --max-time 2 "$OLP_CODE_STATUS_URL" \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H "X-Claude-Code-Session-Id: $session" \
  | jq -er '"OLP \(.status)" + (if .retry_at then " retry \(.retry_at)" else "" end)' \
  || printf 'OLP unknown'
```

```json
{
  "statusLine": {
    "type": "command",
    "command": "~/.claude/olp-statusline.sh"
  }
}
```

The same `curl` call works from any terminal prompt or editor status bar.
Send the identity header of the client that owns the thread, keep the key in
the environment, and treat a failed or slow check as `unknown` rather than
blocking the client on it.

### Drain a subscription account

In **Code mode -> Accounts**, select the project and choose **Start drain** on
the account you want to rotate or remove. New conversation roots, new model
pins and automatic recovery skip that account. A new conversation can use
another eligible account or receive a typed refusal if none can take it.
Draining applies immediately across replicas and does not require a route
republish.

Existing model pins keep the same account, including on reconnect and in child
conversations using the same pin. Normal access, health, quota and concurrency
checks still apply. Disabling the account or revoking its grant still stops
admissions on those pins.

**Remaining eligible pins** counts nonretired model pins with a current grant
and a live key, model, route and pool assignment. The key's owner must still
have project access. Temporary cooldowns and capacity limits do not subtract
pins. **Last activity** shows the latest recorded routing decision. These are
not counts of open requests. Use **Conversation trees** to inspect or retire
pins when their conversations no longer need them.

Choose **Resume routing** to allow new assignments again. API clients can send
`PUT /api/v1/code/accounts/{id}/drain` with the current `If-Match` ETag and
`{"draining": true}` or `{"draining": false}`. Both actions are audited. The
drain is local to the installation and is not exported as configuration.

## Limits and accounting

Request rate, token rate and concurrency reuse shared admission. Those estimates
do not establish hard token bounds. Subscription traffic excludes ordinary
monetary spending limits, and dollar estimates are informational.

A new conversation can use another eligible account if its preferred account
is at capacity. An existing pin stays put. If no allowed account has room,
HTTP returns `429 code_account_busy` with `Retry-After`. On an open WebSocket,
the gateway closes the socket with code `1013` and reason `code_account_busy`.
Retry the same conversation after a short delay. Local saturation does not change account
health, replay the request or send it to paid fallback. An idle WebSocket and
prewarming do not use generation slots. Completion and cancellation release the
slot even if final token usage is unknown. Abandoned slots expire after 11 minutes.

Hard token budgets are optional. An enabled matching budget requires an
adapter-proven conservative bound before dispatch. Project-wide, route-scoped
and key-scoped budgets overlap, and all applicable UTC daily and monthly windows
reserve atomically. An operation without a trustworthy bound returns
`code_token_bound_unavailable` only when a matching hard budget is enabled.
Disabling a budget does not erase previous consumption or uncertainty.

The current qualification has **no proven native Codex generation bound**.
`max_output_tokens`, model context metadata, prompt estimates, a test constant or
an operator-entered number cannot be treated as proof. Positive hard-budget
support remains a release gate rather than an always-reject implementation claim.

After dispatch, final provider usage settles reservations exactly once. Cached
input and reasoning output are subsets of input and output totals. Anthropic
Messages input includes cache reads and writes, and its cached input is the
cache reads. Chat Completions reports usage only in a final chunk when the
client asks for it with `stream_options.include_usage`, as OpenCode does, so a
stream without it leaves consumption uncertain. A disconnect, cancellation or
missing final usage leaves durable uncertainty and does not release a
reservation as unused. A bound overrun is a `bound_violation` and must block
subsequent budgeted admission. Historical unknown usage prevents falsely
enabling a hard guarantee.

Provider-reported allowance is separate metadata with observation and reset
times. Codex inference responses may carry passive allowance windows, while
active Codex and MiniMax reads remain disabled pending live qualification
(`allowance_signal` reports `remains-unqualified` with reason
`live-qualification-pending` for MiniMax profiles, `proven` for Codex passive
observations and `none` for other coding plans). OpenCode Go records upstream
`usage.opencode` and `usage.cost` as `allowance_charge` metadata on attempts
without using it for admission or ordering.

On HTTP 429 rejections, OLP parses bounded JSON error bodies (up to 64 KiB) for
Codex, GLM Coding Plan, Kimi Code, MiniMax M Plan and Qwen Token Plan to record
typed cooldowns across five classes (`quota`, `entitlement`, `rate`, `capacity`
and `concurrency`) and two scopes (`credential` or `model`). A `credential`
cooldown cools the entire account and updates account health (`quota_limited`
for `quota` and `unavailable` for other classes). A `model` cooldown cools only
the affected model on that account without marking other models unavailable.

Codex parses `usage_limit_reached` as a credential-scoped `quota` cooldown and
`turn_rate_limit_exceeded`, `model_at_capacity` and `model_is_at_capacity` as
model-scoped `capacity` cooldowns, reading `error.resets_at`,
`error.resets_in_seconds` and `Retry-After`. GLM Coding Plan classifies quota
codes `1308`, `1310` and `1316` through `1321` as credential-scoped `quota`
cooldowns while parsing RFC3339 reset timestamps embedded in vendor messages,
classifies `1302` as credential-scoped `rate`, classifies `1311` as
model-scoped `entitlement` and `1305` as model-scoped `capacity`, and suppresses
generic cooldown fallback on `1309` and `1314`. Kimi Code classifies quota,
credit and Extra Usage messages or codes (`exceeded_current_quota_error`,
`insufficient_quota`) as credential-scoped `quota`, concurrent-limit messages as
credential-scoped `concurrency`, rate-limit messages or codes
(`rate_limit_exceeded`, `rate_limit_reached_error`, `rate_limit_error`) as
credential-scoped `rate`, and engine overload messages or codes
(`overloaded_error`, `engine_overloaded_error`) as model-scoped `capacity`.
MiniMax M Plan extracts numeric business codes from `base_resp.status_code`,
`error.code` or the parenthesized code in `error.message` and classifies `2056`
and `2067` as credential-scoped `quota`, `2061` as model-scoped `entitlement`,
and `2062`, `1002` and `1039` as credential-scoped `rate`. Qwen Token Plan
classifies `insufficient_quota` as credential-scoped `quota` and
`Throttling.RateQuota` as credential-scoped `rate`, and extends
`insufficient_quota` cooldowns to the next monthly UTC midnight reset day when
`cycle_anchor` is configured on the account.

When no vendor reset timestamp applies, or for unclassified responses that mark
an account unavailable or quota-limited, OLP honors a single valid `Retry-After`
header as unsigned seconds or an HTTP date. Zero seconds and past dates permit
an immediate retry when other eligibility checks pass and no longer active
cooldown exists. Every failure observation preserves a longer active deadline,
including concurrent responses and credential recovery. Missing, invalid or
duplicated `Retry-After` values use a one-minute fallback. Successful
generations already running when another request starts a cooldown do not
shorten it.

Primary and secondary windows are retained independently for each metered limit.
Token and request counts also retain their own observation and reset times.
Credits remain separate from those windows and from local budgets. Partial
observations preserve other dimensions, and stale observations cannot replace
newer ones. Eligibility treats passive exhaustion as unavailable until its supported
reset, including when that reset is unknown. Passing a retry or reset time
permits another attempt once all eligibility checks pass, while reported
allowance values remain tied to upstream observations. Never infer subscription
allowance from a local budget, monetary estimate or a successful fixture request.

For transport failures, only upstream WebSocket close codes `1011` through
`1014` (internal error, service restart, try again later and bad gateway) mark
an account `unavailable` with the one-minute fallback. Other transport errors
record a `transport_error` outcome on the attempt without marking the account
unavailable, because transport noise does not prove account unavailability. The
next client request can test recovery on the same pinned account. OLP schedules
no synthetic health inference. Automatic replay requires the explicit
[initial recovery policy](#portable-conversation-recovery), proven nonexecution
and portable initial state, and transport failures never qualify.

## Fidelity and privacy

The forwarding contract preserves request bodies, response bytes, SSE content,
WebSocket message payloads and order, and end-to-end header values including
repeated values. Observation may parse copies in memory, and it must never
reserialize the forwarded payload. Unknown fields on a qualified operation
remain untouched. Unqualified paths and operation types refuse before upstream
dispatch.

Exceptions are upstream authentication replacing the OLP credential, OLP-only
controls, hop-by-hop headers and protocol-required framing or upgrade handling.
Request authentication fields consumed locally are `Authorization`,
`ChatGPT-Account-ID`, `Cookie`, `X-API-Key` and `X-Goog-API-Key`. A route's
Messages paths also accept the OLP key in `X-API-Key`, as Anthropic SDKs send
it, and refuse in Anthropic's error envelope.

Only the headers of the serving account's adapter replace them upstream. Codex
sets bearer `Authorization` and `ChatGPT-Account-ID`. Claude sets bearer
`Authorization` and `Anthropic-Beta: oauth-2025-04-20`. Antigravity sets bearer
`Authorization`. GLM Coding Plan, MiniMax M Plan and Qwen Token Plan set bearer
`Authorization`. OpenCode Go sets bearer `Authorization` on Chat Completions and
Responses, and `X-Api-Key` on Messages as OpenCode's Anthropic SDK sends it.
Kimi Code sets `X-Api-Key` on Messages, as its Claude Code setup does, and
bearer `Authorization` on Chat Completions.

All `X-OLP-*` fields are consumed. Hop-by-hop exclusions are `Connection`,
its named fields, `Proxy-Connection`, `Keep-Alive`, `Proxy-Authenticate`,
`Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding` and `Upgrade`.
Trailers retain end-to-end values through the downstream framing mechanism.
WebSocket keys, versions, accept values and extensions are negotiated on each
leg, subprotocol requests refuse and compression is disabled. Upstream authority
and HTTP framing are recomputed without reserializing bodies.
The contract does not claim identical TCP segmentation, WebSocket frame
fragmentation, TLS fingerprint, source IP or timing. It cannot guarantee that
an upstream provider accepts an account's use.

Diagnostics retain identifiers, route revisions, native models, operations,
binding and account identity, bounded refusal codes, reservation and usage
metadata, upstream status and outcome origin. Upstream completion and rejection,
transport loss and client cancellation remain distinct from whether usage is
known. WebSocket handshakes and prewarms alone do not establish serving health.
Diagnostics exclude prompts, outputs, reasoning, tool payloads, raw headers,
upstream auth and refresh material. Account names and operator labels are not
places to store secrets.

## Vendor terms

Z.ai restricts the GLM Coding Plan to its officially supported coding tools and
scenarios, OpenCode Go has its own terms, and MiniMax, Kimi and Alibaba each
govern their coding plans by theirs. Kimi's terms forbid altering the client's
identity. Native forwarding preserves the client's identity headers. The separate
Codex identity policy changes only a missing or blank Originator and does
not apply to Kimi or any other adapter. OLP cannot guarantee that a vendor
accepts a plan used through a gateway or shared
by several developers. Review each vendor's terms before pooling an account,
and prefer a personal pool per subscriber where they require it.

### End-user accounting

Keys may identify callers through the ordinary [end-user policy](access.md#end-user-identity).
Code generations and admission refusals retain its project-scoped HMAC digest,
never the raw identifier. `GET /api/v1/code/attempts` and `/api/v1/code/refusals`
accept `end_user_digest=<digest>` or `end_user_digest=unidentified`, and the
console's Attempts and Refusals tabs expose the same filter and identity
metadata. These fields share the existing code-ledger retention and project
isolation boundaries. They do not turn subscription allowances into USD spend.
Changes that would switch identity during admission refuse before dispatch with
`code_end_user_changed`, so reconnect or retry using the updated key policy.

### Attribution policy

Subscription calls enforce the same required and pinned labels as other
inference requests. Attempts retain the resolved attribution object and display
it in diagnostics. The ledger checks current project and key policy before
creating a billable attempt, and changed policy returns
`code_attribution_changed`. See
[attribution requirements](access.md#attribution-requirements-and-pinned-labels)
for configuration, refusal codes and retention.

### Route request sizes

The route editor's optional `max_body_bytes` is published with each subscription
route revision. It lowers the installation and protocol cap for both encoded and
decoded HTTP bodies and individual client WebSocket messages. Oversized requests
are refused before durable generation admission or provider dispatch. See
[per-route request sizes](../develop/request-path.md#per-route-request-sizes).

Workload JWTs use the same explicit pool assignment and current project authority
as static principals. Their verified claim digest survives fresh-policy admission,
issuer changes before dispatch require a retry, and idle subscription sockets
close on the five-second expiry and authority check. See
[workload JWT identity](access.md#workload-jwt-identity) for provisioning and
renewal behavior. No JWT or raw subject enters subscription ledgers.

### Portable conversation recovery

Published route policy `initial_recovery_attempts` defaults to zero. Setting it to
one or two permits that many additional attempts per request for a new or
established text-only conversation on the exact pinned model. Every attempt checks current key,
project, pool, account and grant authority. Native forwarding remains unchanged
when recovery is disabled.

The Codex adapter currently recognizes complete HTTP 401 errors with type
`authentication_error` and code `invalid_api_key`, and complete HTTP 429 errors
with type `usage_limit_reached`. Arbitrary 401 and 429 responses and transient
`rate_limit_exceeded` errors do not qualify. No bytes from a rejected attempt are
sent to the client before the recovery decision. A 401 can reuse its account only
after the existing refresh worker supplies a newer grant for the same credential
and verified principal within ten seconds. A longer cooldown observed from
another request still blocks that account. Without a refreshed grant, recovery
selects another eligible account. Quota recovery respects longer upstream reset
and `Retry-After` values when cooling the rejected account.

An established root can move when the current request supplies complete portable
input, all earlier attempts have settled or aborted, and its tree has one model
pin and no existing children. Existing response references retain their producing
account and cannot authorize continuation on the replacement account. A child
request or a tree with existing children or multiple model pins refuses recovery
without changing authority. New children inherit the recovered root's account.
Concurrent admission and recovery share the durable admission lock, and an
in-flight or uncertain attempt prevents migration.

WebSocket-owned trees cannot move, including idle connections opened while a
rejected HTTP request waits for recovery. Socket ownership is durable and is not
cleared when the socket closes. Bindings created before portable recovery remain
on their original account because their transport ownership is unknown. Start a
new conversation with complete context to opt into recovery for those bindings.

Previous response IDs, turn-state headers, compact operations,
assistant and tool histories and opaque input items cannot move between accounts.
Recovery checks the complete request against recognized fields and types.
Function declarations with client-supplied schemas are portable, while account-owned tools, stored prompt IDs, unknown
fields and unsupported nested options disable recovery. The first native
dispatch still forwards those requests unchanged. A recognized nonexecution
rejection then returns `code_recovery_unsafe` with guidance to use the original
account or start a new conversation with full context and no account-owned state.
Transport loss, server errors, accepted responses, partial output and stream
errors never authorize replay.

Each rejected attempt retains its actual account and route revision. Proven
nonexecution settles at zero before another attempt reserves capacity. The
recovered root and model pin change atomically under the same admission lock,
while later attempts and replicas keep that recovered binding. Unsafe recovery
returns a typed refusal rather than silently choosing a different model.

The rejection envelopes are controlled assumptions observed on 2026-10-30 in
[the pinned Codex executor fixtures](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/runtime/executor/codex_executor_retry_test.go)
and [typed terminal classification](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/runtime/executor/codex_executor_terminal.go).
Those sources establish error shapes. They do not establish live subscription
qualification or permission to replay streams. The complete pre-output HTTP
boundary and durable ownership checks remain required.

## Antigravity enrollment and controlled translation

The `antigravity-subscription` profile uses an operator-supplied Google desktop
OAuth client. Configure its public `client_id` provider option and complete
headless PKCE enrollment with the loopback callback URL and client secret in the
masked grant input. The existing encrypted grant store holds the secret and
refresh token together. Authenticated Google subject and existing Cloud Code
Assist project evidence establish identity, and both must remain unchanged on
refresh. See [Import Antigravity credentials once](#import-antigravity-credentials-once)
and the [Antigravity plugin guide](../../internal/codemode/antigravityauth/antigravity/README.md)
for the completion payload, one-time import envelope and credential setup
requirements.

The independent `chat_to_antigravity` route flag enables an internal Gemini
target after publication (**Translate Chat through Antigravity** in the route
editor). OpenCode Chat Completions ingress supports controlled single-candidate
text and function tool workflows over HTTP unary or SSE, targeting
`v1internal:generateContent` and `v1internal:streamGenerateContent` on
`https://daily-cloudcode-pa.googleapis.com`. Eligible operator-configured model
IDs start with `gemini-`, which is a routing restriction and does not establish
model inventory, entitlement or live compatibility. Native forwarding remains
the default. Public Gemini ingress, Responses, Messages, WebSocket, reasoning
controls, signed state, media and server tools are unsupported. Identity and
prompt compatibility remain disabled.

Google documents that current `generateContent` responses may carry required
thought signatures on any part. This target refuses that state with
`code_upstream_unsupported_state` instead of discarding it, preserves
authoritative usage from dispatched requests and never replays an unsupported
result. No current model or live subscription tool workflow has been qualified,
and signature-free fixtures do not close that gap. A current model and tool
combination that works within the restricted path, or faithful signature
support, remains a prerequisite for live qualification.
