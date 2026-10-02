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
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -o codex.wasm ./plugins/codex
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
Responses and supports WebSockets, with request/stream retries set to zero.
OLP transport independently must never replay inference; client retry policy
does not establish that server guarantee.

## Integration surfaces

The foundation owns composition and public endpoint declaration. Compose:

```go
gw.CodeAuthorizer = &providers.CodeAuthorizer{
    Pool: pool, Credentials: runtimeManager, Plugins: pluginHost,
}
```

Pass the configuration obtained from `Snapshot.CodeConnection(route, providerID)`
and the live `CodeLedger.Admit` account. The authorizer structurally implements
`gateway.CodeAuthorizer` without importing gateway. It verifies the approved
plugin profile, immutable configuration, provider/project/credential identity,
revocation/lapse/expiry, and observed principal. It permits network options but
refuses semantic headers, query/default/model transforms and unknown endpoints.
It returns only upstream authentication headers and principal.

The ready handler is `(*routes.Server).CodeClientConfiguration`. Register it as:

```go
s.Access.Route(mux, "GET /api/v1/code/routes/{id}/client-config", s.CodeClientConfiguration)
```

Declare that GET in OpenAPI with the existing code-route read authorization,
the UUID `id` path parameter, required string `gateway_url` query parameter,
optional string `model`, and the existing `CodeClientConfiguration` response.
Regenerate contracts and the authorization golden. The handler enforces project
visibility and reads `latest_revision_id`'s document, not a mutable draft.
Add public-boundary tests after registration for scope/project denial, unpublished
or disabled routes, draft changes after publication, and the returned TOML.

### Grant retention during slot rotation

The foundation's `internal/grants/refresh.go` `using` expression only recognizes
ordinary provider slots and revisions. Parent integration must also recognize
code accounts and immutable published connections, or changing the ordinary slot
can retire a grant still selected by a code account/conversation pin.

Add this candidate before the ordinary candidates in its `coalesce`:

```sql
(SELECT v.connections->c.provider_id::text
 FROM olp.code_routes r
 JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
 WHERE EXISTS (
   SELECT 1 FROM olp.code_accounts a
   WHERE a.credential_id=c.id AND a.provider_id=c.provider_id
     AND a.principal=c.principal AND a.project_id=r.project_id)
   AND v.connections->c.provider_id::text->>'profile_revision'=c.plugin_digest
 ORDER BY v.published_at DESC, v.id DESC LIMIT 1)
```

In the mutable-provider fallback, retain the matching-digest condition, but
extend the existing provider-slot `EXISTS` condition with:

```sql
OR EXISTS (SELECT 1 FROM olp.code_accounts a
           WHERE a.credential_id=c.id AND a.provider_id=c.provider_id
             AND a.principal=c.principal AND a.project_id=p.project_id)
```

The same `using` expression feeds due selection and refresh execution, so both
must share this addition. Add parent integration coverage for removing/rotating
ordinary slots while a published code route continues using the enrolled
credential. Revocation/lapse must still refuse; never migrate a pin to another
principal to keep it working.

## Qualification limits

The tests exercise a real compiled WASM plugin through management enrollment,
device polling, PostgreSQL encrypted secrets, worker rotation, live authorization,
and permanent lapse on changed identity. A trusted local TLS fixture terminates
the plugin's fixed auth hostname through configured network options; it never
contacts a live account or issues inference. Unit tests independently assert
the exact OAuth encoding and request fields. The official npm-distributed
`codex-cli 0.160.0` also accepts the generated TOML with `codex features list`.

These are auth/configuration checks, not evidence of live subscription inference,
HTTP/SSE/WebSocket transport, resume/fork/compaction/tools, file operations, model
discovery, or server-mediated search. Those remain the parent transport/client
qualification matrix. ChatGPT cloud tasks, standalone web-search and account
management are outside the generated local-client setup. FedRAMP and tokens
without usable observed identity/expiry refuse. No operation/model hard-token
bound is supplied here; budget qualification must be established separately.
