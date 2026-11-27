# Codex subscription authorization

This plugin implements the official Codex device authorization and refresh
protocol for code-mode accounts. Its grant is an OLP encrypted credential
version. The refresh worker owns rotation and gateways read only access tokens.
Enrollment, refresh and code-account activation perform no inference. The
ordinary signing hook refuses probes and ordinary routes.

## Upstream evidence

The original authentication protocol audit inspected **Codex 0.160.0**, tag
`rust-v0.160.0`, commit
[`a956835d020762cb2b570053af06f643a11c0ecc`](https://github.com/openai/codex/tree/a956835d020762cb2b570053af06f643a11c0ecc).

In the [device flow](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/device_code_auth.rs#L27-L235),
Codex posts JSON to `/api/accounts/deviceauth/usercode`, reads the string
polling interval and `user_code` or `usercode`, polls JSON
`POST /api/accounts/deviceauth/token` with 403 or 404 pending responses over a
fifteen-minute lifetime, and sends the user to `/codex/device`. For
[code exchange](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/server.rs#L709-L810),
it sends a form-encoded OAuth authorization-code exchange with the issued
verifier and `https://auth.openai.com/deviceauth/callback`, passing the
verifier rather than the returned `code_challenge`.

For [refresh](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/auth/manager.rs#L1624-L1728),
Codex uses a JSON refresh-token grant at `https://auth.openai.com/oauth/token`
with client ID `app_EMoamEEZ73f0CkXaXp7hrann`, optional rotated refresh and ID
tokens, and permanent expired, reused, invalidated or invalid-grant outcomes.
OLP does not implement the client's endpoint or client-ID environment overrides.

For [observed identity](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/token_data.rs#L85-L200),
OLP reads `https://api.openai.com/auth.chatgpt_account_id`, `chatgpt_user_id`
or `user_id`, access-token `exp` and the FedRAMP marker, hashing the account
and user pair into its principal. Missing, conflicting, expired or unsupported
claims refuse, and account names, plans, allowance and inference health are not
invented. Claims are observed only from the fixed TLS issuer's token response
and its encrypted stored credential, never accepted as authentication from OLP
clients.

For upstream dispatch, the [account header](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider/src/bearer_auth_provider.rs#L22-L45)
sends the bearer access token and `ChatGPT-Account-ID`. The
[client configuration and upstream address](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs#L73-L201)
use backend `https://chatgpt.com/backend-api/codex` with custom `base_url`,
`env_key`, Responses wire protocol, `requires_openai_auth=false`, WebSocket
support and default client retry controls. For
[remote compaction selection](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider/src/provider.rs#L461-L473),
the provider display name `OpenAI` selects the client's remote-compaction
capability, so the generated custom provider uses that name.
[Internal metadata](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs#L197-L201)
is a separate runtime-only field that cannot be enabled by TOML.

By default OLP never manufactures first-party `User-Agent`, originator, session
or beta headers. When a route administrator explicitly enables and publishes
`codex_cli_identity: true` (**Enable Codex CLI identity compatibility** in the
route editor), OLP supplies `Originator: codex_cli_rs` on Codex subscription
HTTP and WebSocket requests only when all supplied `Originator` values are
absent or blank. Explicit nonblank `Originator` values, `User-Agent`, other
headers and all body content stay unchanged.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o codex.wasm ./internal/codemode/codexauth/codex
```

Install through `POST /api/v1/plugins` with `Content-Type: application/wasm`
and approve its declared origins (`https://auth.openai.com` and
`https://chatgpt.com`) through the plugin API or the console's Plugins page.
Use the resulting digest as `profile_revision` in a project-owned provider
configuration:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "codex-subscription",
  "profile_revision": "<installed-plugin-digest>"
}
```

Use the provider grant-enrollment API, including its `If-Match`, idempotency
and CSRF requirements:

1. Call `POST /api/v1/providers/{id}/grant-enrollments`.
2. Display the returned upstream device verification URL and one-time user code
   so the user approves it at OpenAI without pasting a raw token into OLP.
3. Call `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/poll` at
   the returned interval. Completion returns the credential ID and stable
   principal.
4. Create the code account with that credential using the code-account API. Its
   health starts `unknown`. Do not invoke ordinary provider activation,
   certification or probing as a code-mode prerequisite.

The plugin uses only the host's origin-restricted, redirect-free HTTP
capability. The management server encrypts pending device state, and access
tokens and refresh tokens have separate encryption purposes. Invalid or revoked
refresh outcomes and principal changes flow into the grant lapse machinery. A
refreshed access token cannot replace the account and user identity of the
original grant.

### One-time credential import

To migrate an existing Codex CLI login once instead of running device sign-in,
open **Code mode -> Accounts**, select the Codex provider in the account
editor, paste `auth.json` into **Codex auth.json**, and choose **Import Codex
credentials**, then save the account with the returned credential. Through the
management API, call `POST /api/v1/providers/{provider_id}/codex-import` with
`If-Match` and a JSON body containing `credentials` (the `auth.json` document
encoded as a string) plus optional `slot_id`.

The document must contain a `tokens` object with matching `access_token`,
`id_token`, `refresh_token` and `account_id`, plus optional `auth_mode` set to
`chatgpt`, `OPENAI_API_KEY` set to `null` and RFC3339 `last_refresh`. OLP
verifies the refresh token through the pinned Codex plugin before storing it,
requires the same verified principal on an occupied slot, and preserves
conversation pins when the account is saved with the replacement credential.
Stop using the original local credentials after import.

## Client configuration and translation policies

`codeclient.Generate` in `internal/codemode/codeclient` returns the shared
`codemode.ClientConfiguration`. The gateway URL must be HTTPS (loopback HTTP is
permitted for development), and OLP preserves its path prefix and appends
`/code/{published-slug}`. Native models come from the publication, never
guessed model inventory, and an empty model selects the first published model.

Use the OLP CLI to manage `$CODEX_HOME/config.toml` (defaulting to
`$HOME/.codex/config.toml`) without hand-merging TOML:

```sh
olp code-setup preview --route "$ROUTE_ID" --gateway-url "$GATEWAY_URL"
olp code-setup apply --route "$ROUTE_ID" --gateway-url "$GATEWAY_URL" --revision "$REVISION_ID"
olp code-setup status
olp code-setup detach
```

Alternatively, merge the returned TOML fragment into `$CODEX_HOME/config.toml`,
set `OLP_API_KEY` to the developer's authorized OLP inference key, and run
Codex. The client needs neither an OpenAI login nor upstream credentials. The
generated provider uses Responses and supports WebSockets for subscription
models, retaining Codex's default client retries. Regenerate the configuration
when changing models so the native model and `X-OLP-Code-Model` selection hint
agree.

By default OLP forwards each inference once. When a published route sets
`initial_recovery_attempts` to 1 or 2, OLP permits that many bounded recovery
attempts for portable HTTP requests after a proven HTTP 401
(`authentication_error` with code `invalid_api_key`) or HTTP 429
(`usage_limit_reached`) nonexecution rejection before any bytes reach the
client. Accepted work, partial output, transport failures, WebSocket-owned
trees and non-portable state are never replayed.

Two independent route opt-ins expose Codex accounts to other coding clients
through the Codex Responses transport after publication. Setting
`messages_to_codex: true` enables HTTP `v1/messages` text and tool translation
for Claude Code. Setting `chat_to_codex: true` enables HTTP
`v1/chat/completions` text and function-tool translation for OpenCode under the
`olp-codex` provider. Both policies default to disabled, preserve prompts and
client identity by default, and refuse unsupported semantics such as signed or
opaque reasoning, media and server-owned state.

## Integrated composition

Process startup wires `codeforward.New` (`internal/gateway/codeforward`),
`codesessions.New` (`internal/codesessions`) and
`codeaccounts.Authorizer{Pool, Credentials, Plugins}`
(`internal/codesessions/codeaccounts`). The gateway passes the immutable
`Snapshot.CodeConnection(route, providerID)` configuration and live admitted
account to the authorizer.

`codeaccounts.Authorizer` structurally implements `codeforward.Authorizer`
without importing `internal/gateway`. It verifies the approved plugin profile,
immutable configuration, provider, project and credential identity, revocation,
lapse, expiry and observed principal. It permits network options while refusing
semantic headers, query, default or model transforms and unknown endpoints.
Only upstream authentication headers and principal leave this boundary.

`GET /api/v1/code/routes/{id}/client-config` is registered and declared in
OpenAPI with code-route read authority, a UUID path parameter, required
`gateway_url`, and optional `model`, `client`, `platform` (`linux`, `macos` or
`windows`), `plan_model` and `small_model` (Codex rejects `plan_model` and
`small_model`). Contracts and the authorization golden are generated from it.
The handler enforces project visibility and reads the published revision rather
than the mutable draft. The console supplies the separately configurable public
gateway address.

### Grant retention during slot rotation

`internal/grants/refresh.go` retains credentials referenced by code accounts.
Its selection query prefers the latest published immutable code-route
connection with the matching provider, principal, project and plugin digest,
before falling back to active provider revisions, mutable provider drafts with
code-account references and retained resources.

Ordinary slot rotation therefore cannot retire a grant still selected by a code
account. Revocation and lapse still refuse, and conversation pins never migrate
to another principal.

## Qualification limits

The fixture qualification gate (`make qualify-code`) installs the
lockfile-pinned Codex CLI (`@openai/codex@0.171.0` from
`tests/qualification/clients/package.json`) through
`scripts/qualification-clients.sh` and runs one HTTP `responses` journey
against a scripted vendor proxy. The `client-config` API response still reports
`"client_version": "latest"`, while qualification run records capture the exact
executed binary version. Live Codex qualification uses `make qualify-code-live`
with `OLP_LIVE_CODEX_AUTH_JSON` and `OLP_LIVE_CODEX_MODEL`.

Fixture checks do not prove live subscription inference. The current
qualification lanes, live-lane requirements and remaining gaps are recorded in
[the qualification guide](../../../../docs/develop/qualification.md), and the
full operator and client setup is in
[code mode](../../../../docs/use/code-mode.md). ChatGPT cloud tasks,
standalone web search and account management are outside the generated
local-client setup. FedRAMP accounts and tokens without usable observed
identity or expiry refuse. No operation or model hard-token bound is supplied
here, so budget qualification must be established separately.
