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

Build from the repository root.

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o claude.wasm ./plugins/claude
```

Install the WASM through the existing plugin API and approve its declared origins.
Configure a project-owned provider with the installed digest.

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "claude-subscription",
  "profile_revision": "<installed-plugin-digest>"
}
```

Start the provider's existing grant-enrollment flow with `If-Match`. Open the
returned authorization URL in a browser, sign in and paste the returned
`code#state` or complete HTTPS callback URL into the enrollment's continue action.
The server starts no callback listener. A missing or mismatched state refuses
before token exchange. The existing lifecycle encrypts pending PKCE state,
access material and separately scoped refresh material.

Create a code account with the resulting credential and explicit native models.
Its health remains unknown. Ordinary provider probing, activation and synthetic
inference are not prerequisites. Refresh must verify the same account and
organization. Changed identity and revoked refresh grants lapse authorization.
The existing authorizer refuses expired, lapsed or revoked credentials before
forwarding, including for already published routes.

Use the existing route client-config operation with `client=claude-code`, then
set the developer's `OLP_API_KEY`. Generated setup uses the gateway address and
configured native models, never upstream tokens. Upstream authorization adds
only bearer authentication and its OAuth beta. Existing client beta values,
identity metadata, prompts, tool history and native Messages bodies are retained.
Personal pools keep their existing owner and key isolation rules.

## Qualification limits

Direct fixtures cover PKCE exchange, required verified identity, stable refresh,
refusal sanitization, native authorization and a two-turn tool exchange. The
service fixture is authored and compile-checked for management enrollment with
the actual WASM plugin, encrypted refresh storage, expiry, revocation and changed
identity. It has not been executed as part of this implementation run. No live
subscription calls or administrative inference were made. Authentication profile
success does not prove inference acceptance or allowance. Identity and prompt
compatibility remain independently disabled. This adapter provides no proven
operation/model token bound for hard token budgets and retains no request or
response content.
