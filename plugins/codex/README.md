# Codex subscription authorization

This plugin implements the official Codex device authorization and refresh
protocol for code-mode accounts. Its grant is an OLP encrypted credential
version; the refresh worker owns rotation and gateways read only access tokens.
Enrollment, refresh and code-account activation perform no inference. The
ordinary signing hook refuses probes and ordinary routes.

## Upstream evidence

Audited release: **Codex 0.160.0**, tag `rust-v0.160.0`, commit
[`a956835d020762cb2b570053af06f643a11c0ecc`](https://github.com/openai/codex/tree/a956835d020762cb2b570053af06f643a11c0ecc).

- [Device flow](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/device_code_auth.rs#L27-L235):
  JSON `POST /api/accounts/deviceauth/usercode`, string polling interval,
  `user_code`/`usercode`, JSON `POST /api/accounts/deviceauth/token`, 403/404
  pending, fifteen-minute lifetime, and `/codex/device` verification URL.
- [Code exchange](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/server.rs#L709-L810):
  form-encoded OAuth authorization-code exchange with the issued verifier and
  `https://auth.openai.com/deviceauth/callback`. The returned `code_challenge`
  is not an exchange form field; the verifier is.
- [Refresh](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/auth/manager.rs#L1624-L1728):
  JSON refresh-token grant at `https://auth.openai.com/oauth/token`, client ID
  `app_EMoamEEZ73f0CkXaXp7hrann`, optional rotated refresh/ID tokens, permanent
  expired/reused/invalidated/invalid-grant outcomes. OLP does not implement the
  client's endpoint/client-ID environment overrides.
- [Observed identity](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/login/src/token_data.rs#L85-L200):
  `https://api.openai.com/auth.chatgpt_account_id`, `chatgpt_user_id` or `user_id`,
  access-token `exp`, and the FedRAMP marker. OLP hashes the account/user pair
  into its principal. Missing, conflicting, expired or unsupported claims
  refuse; account names, plans, allowance and inference health are not invented.
  Claims are observed only from the fixed TLS issuer's token response and its
  encrypted stored credential, not accepted as authentication from OLP clients.
- [Account header](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider/src/bearer_auth_provider.rs#L22-L45):
  bearer access token and `ChatGPT-Account-ID`.
- [Client configuration and upstream address](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs#L73-L201):
  backend `https://chatgpt.com/backend-api/codex`, custom `base_url`, `env_key`,
  Responses wire protocol, `requires_openai_auth=false`, WebSocket support, and
  request/stream retry controls.
- [Remote compaction selection](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider/src/provider.rs#L461-L473):
  the provider display name `OpenAI` selects the client's remote-compaction
  capability. The generated custom provider deliberately uses that name.
  [Internal metadata](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs#L197-L201)
  is a separate runtime-only field that cannot be enabled by TOML. OLP never
  manufactures first-party `User-Agent`, originator, session or beta headers.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o codex.wasm ./plugins/codex
```

Install through `POST /api/v1/plugins` with `Content-Type: application/wasm` and
approve its declared origins through the existing plugin API. Use the resulting
digest as `profile_revision` in a project-owned provider configuration:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "codex-subscription",
  "profile_revision": "<installed-plugin-digest>"
}
```

Use the existing provider grant-enrollment API, including its `If-Match`,
idempotency and CSRF requirements:

1. `POST /api/v1/providers/{id}/grant-enrollments`.
2. Display the returned upstream device verification URL and one-time user code.
   The human approves it at OpenAI; no raw token is pasted into OLP.
3. `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/poll` at the
   returned interval. Completion returns the credential ID and stable principal.
4. Create the code account with that credential using the existing code-account
   API. Its health starts `unknown`. Do not invoke ordinary provider activation,
   certification or probing as a code-mode prerequisite.

The plugin uses only the host's origin-restricted, redirect-free HTTP capability.
The management server encrypts pending device state. Access tokens and refresh
tokens have separate encryption purposes. Invalid/revoked refresh outcomes and
principal changes flow into the existing grant lapse machinery. A refreshed
access token cannot replace the account/user identity of the original grant.

## Client configuration

`routes.CodexClientConfiguration(publishedRoute, publicGatewayURL, nativeModel)`
returns the shared `codemode.ClientConfiguration`. The gateway URL is HTTPS
(loopback HTTP is permitted for development); its path prefix is preserved and
`/code/{published-slug}` is appended. Native models come from the publication,
never route aliases or guessed model inventory. An empty model selects the first
published model.

Save the returned TOML as `$CODEX_HOME/config.toml`, set `OLP_API_KEY` to the
developer's authorized OLP inference key, and run Codex 0.160.0. The client needs
neither an OpenAI login nor upstream credentials. The generated provider uses
Responses and supports WebSockets, retaining Codex's default client retries.
Regenerate the configuration when changing models so the native model and
`X-OLP-Code-Model` selection hint agree.
OLP transport independently must never replay inference; client retry policy
does not establish that server guarantee.

## Integrated composition

Process startup installs `gateway.NewCodeForwarder()`, `resources.CodeStore`
and `providers.CodeAuthorizer{Pool, Credentials: runtimeManager, Plugins: pluginHost}`.
The gateway passes the immutable `Snapshot.CodeConnection(route, providerID)`
configuration and live admitted account to the authorizer.

The authorizer structurally implements `gateway.CodeAuthorizer` without importing
gateway. It verifies the approved plugin profile, immutable configuration,
provider/project/credential identity, revocation/lapse/expiry and observed
principal. It permits network options but refuses semantic headers,
query/default/model transforms and unknown endpoints. Only upstream
authentication headers and principal leave this boundary.

`GET /api/v1/code/routes/{id}/client-config` is registered and declared in OpenAPI
with code-route read authority, a UUID path parameter, required `gateway_url`
and optional `model`. Contracts and authorization golden are generated from it.
The handler enforces project visibility and reads the published revision rather
than the mutable draft. The console supplies the separately configurable public
gateway address.

### Grant retention during slot rotation

`internal/grants/refresh.go` retains credentials referenced by code accounts.
Its shared selection/execution lookup prefers the latest published immutable
code-route connection with the matching provider, principal, project and plugin
digest. Mutable provider fallback also recognizes code-account references.

Ordinary slot rotation therefore cannot retire a grant still selected by a code
account. The integration suite rotates the ordinary slot, changes the mutable
network configuration and verifies that the retained grant refreshes through
its published connection. Revocation and lapse still refuse; pins never migrate
to another principal.

## Qualification limits

The tests exercise a real compiled WASM plugin through management enrollment,
device polling, PostgreSQL encrypted secrets, worker rotation, live authorization,
and permanent lapse on changed identity. A trusted local TLS fixture terminates
the plugin's fixed auth hostname through configured network options; it never
contacts a live account or issues inference. Unit tests independently assert
the exact OAuth encoding and request fields. The official npm-distributed
`codex-cli 0.160.0` also accepts the generated TOML with `codex features list`.

These auth/configuration checks do not prove live subscription inference.
The integrated transport/client evidence and remaining file/search, platform
and live-account gaps are recorded in
[the qualification matrix](../../docs/qualification/code-mode.md).
ChatGPT cloud tasks, standalone web-search and account
management are outside the generated local-client setup. FedRAMP and tokens
without usable observed identity/expiry refuse. No operation/model hard-token
bound is supplied here; budget qualification must be established separately.
