# MiniMax M Plan accounts

This plugin enrolls MiniMax M Plan API keys for code-mode accounts. The
operator pastes a key the vendor's key page issues; OLP stores it encrypted as
the grant's access token and identifies it by a fingerprint. Enrollment makes
no upstream call and performs no inference. The ordinary signing hook refuses
probes and ordinary routes, so the key serves only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `minimax-plan` | MiniMax | `https://api.minimax.io` | `https://platform.minimax.io/console/plan` |
| `minimax-plan-cn` | MiniMax, mainland China | `https://api.minimax.cn` | `https://platform.minimax.cn/console/plan` |

Code mode forwards Anthropic Messages to `anthropic/v1/messages` and Chat
Completions to `v1/chat/completions` beneath the upstream, with the key as a
bearer token.

The key page issues the plan's Subscription Key, which starts with `sk-cp-`;
enrollment refuses a key without that prefix because MiniMax pay-as-you-go
keys (`sk-api-`) share the host and would bill outside the plan. Legacy Token
Plan subscriptions issue the same Subscription Key and enroll the same way.

## Upstream evidence

- [M Plan in other tools](https://platform.minimax.io/docs/m-plan/other-tools):
  the `sk-cp-` Subscription Key and the OpenAI-compatible and
  Anthropic-compatible plan endpoints.
- [Claude Code](https://platform.minimax.io/docs/m-plan/claude-code):
  `ANTHROPIC_BASE_URL` `https://api.minimax.io/anthropic` with
  `ANTHROPIC_AUTH_TOKEN`.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o minimax-plan.wasm ./plugins/minimax-plan
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins: `https://api.minimax.io`,
`https://api.minimax.cn`, `https://platform.minimax.io` and
`https://platform.minimax.cn`. Use the digest as `profile_revision` in a
project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "minimax-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. `POST /api/v1/providers/{id}/grant-enrollments` returns the key page as
   `authorization_url`, with `input: "secret"`.
2. Create a key there, then `POST .../grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. The completion returns the credential ID and the
   principal `minimax-plan:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key is a different principal:
rotate a key by enrolling it, creating a new code account and revoking the old
credential version. Conversations pinned to the old account start again.

See [code mode](../../docs/use/code-mode.md) for routes and client
configuration, and the [qualification](../../docs/develop/qualification.md)
for what the controlled fixtures prove. MiniMax's terms govern M Plan use; OLP
forwards the client's requests unmodified but cannot guarantee that MiniMax
accepts their use through a gateway.
