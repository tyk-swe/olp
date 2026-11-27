# Claude subscription enrollment

This confined plugin implements headless browser enrollment for the
`claude-subscription` profile and native Anthropic Messages authorization for
code-mode routes. Live subscription inference, entitlement and native client
startup remain unqualified. Enrollment and generated setup do not establish
subscription support in production.

## Authentication evidence

The official [Claude Code authentication guide](https://code.claude.com/docs/en/authentication)
documents subscription sign-in, refresh and account access. On 2026-10-30, npm
resolved the official Claude Code package to version 2.1.314. Its
[Linux artifact](https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-2.1.314.tgz)
was read without installing or executing it, and its SHA-512 integrity matched
[the registry metadata](https://registry.npmjs.org/@anthropic-ai%2fclaude-code-linux-x64/2.1.314).
The artifact contains the OAuth beta `oauth-2025-04-20`, the fixed OAuth profile
path, the public OAuth client ID and the HTTPS code callback used here.

The [pinned authentication implementation](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/auth/claude/anthropic_auth.go)
supplies navigation for the authorization-code JSON exchange, refresh and profile
response fields. It is secondary evidence. OLP requires successful profile
verification and stable identity even where that implementation permits a failed
profile lookup. Its client impersonation and identity or prompt modifications
are not part of this plugin.

Authorization uses `https://claude.ai/oauth/authorize`, token exchange uses
`https://platform.claude.com/v1/oauth/token`, and profile verification uses
`https://api.anthropic.com/api/oauth/profile`. Only the authenticated profile's
nonempty account and organization UUIDs determine the stable principal. Names,
email addresses, plan labels, local claims and model calls do not establish it.

## Installation and enrollment

Build from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o claude.wasm ./internal/codemode/claudeauth/claude
```

Install the WASM module through `POST /api/v1/plugins` with
`Content-Type: application/wasm`, or the console's Plugins page, and approve
its three declared origins (`https://claude.ai`, `https://platform.claude.com`
and `https://api.anthropic.com`). Configure a project-owned provider with the
installed digest:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "claude-subscription",
  "profile_revision": "<installed-plugin-digest>"
}
```

Start the provider's grant-enrollment flow with `If-Match`. Open the returned
authorization URL in a browser, sign in and paste the returned `code#state` or
complete HTTPS callback URL into the enrollment's continue action. The server
starts no callback listener. A missing or mismatched state refuses before token
exchange. The grant lifecycle encrypts pending PKCE state, access material and
separately scoped refresh material.

Create a code account with the resulting credential and explicit native models.
Its health remains unknown. Ordinary provider probing, activation and synthetic
inference are not prerequisites. Refresh must verify the same account and
organization. Changed identity and revoked refresh grants lapse authorization.
The authorizer refuses expired, lapsed or revoked credentials before forwarding,
including for already published routes.

Use the route client-config operation with `client=claude-code`, then set the
developer's `OLP_API_KEY`. Generated setup uses the gateway address and
configured native models, never upstream tokens. Upstream requests target
`https://api.anthropic.com/v1/messages`, and authorization adds only bearer
`Authorization` and `Anthropic-Beta: oauth-2025-04-20`. Existing client beta
values, identity metadata, prompts, tool history and native Messages bodies are
retained. Personal pools keep their owner and key isolation rules.

## One-time credential import

Operators migrating an existing Claude Code login can import the local
`.credentials.json` document once. In the console, open **Code mode ->
Accounts**, select the Claude provider connection in the account editor, paste
the document into **Claude .credentials.json**, and choose **Import Claude
credentials**, then save the account with the returned credential. Through the
management API, call `POST /api/v1/providers/{provider_id}/claude-import` with
the current `If-Match` header and a JSON body containing `credentials` as a
JSON string, plus optional `slot_id` to target an existing slot.

The supported document contains only a `claudeAiOauth` object with string
fields `accessToken` and `refreshToken`, a positive `expiresAt` Unix timestamp
in milliseconds, and a `scopes` array including `user:profile` and
`user:inference`. Optional fields are `refreshTokenExpiresAt` and
`subscriptionTypeVerifiedAt` as positive Unix timestamps in milliseconds,
`subscriptionType` and `rateLimitTier` as strings or `null`,
`subscriptionTypeSource` set to `sign_in` or `profile`, and `clientId` matching
the supported Claude Code OAuth client ID. Unknown fields, duplicate keys, API
keys and unsupported client identifiers are rejected.

OLP authenticates the pasted access token against
`https://api.anthropic.com/api/oauth/profile`, then refreshes through the
pinned plugin and requires the same account and organization UUIDs. Expired or
revoked access tokens require a fresh local sign-in before import. An occupied
slot requires the same verified principal, and same-identity replacement
preserves conversation pins once the account is saved with the new credential.
Stop using the original local credentials after a successful import because OLP
owns refresh afterward and does not synchronize local files.

## Qualification limits

Claude subscription currently has no automated fixture or live qualification
lane. Authentication profile verification and native forwarding do not prove
live subscription acceptance, entitlement or allowance. Identity and prompt
compatibility remain independently disabled. This adapter provides no proven
operation or model token bound for hard token budgets and retains no request or
response content.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for current qualification lanes and release gates.
