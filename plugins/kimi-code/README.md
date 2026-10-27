# Kimi Code accounts

This plugin enrolls Kimi Code API keys for code-mode accounts. The operator
pastes a key the vendor's key page issues; OLP stores it encrypted as the
grant's access token and identifies it by a fingerprint. Enrollment makes no
upstream call and performs no inference. The ordinary signing hook refuses
probes and ordinary routes, so the key serves only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `kimi-code` | Kimi | `https://api.kimi.ai/coding` | `https://www.kimi.ai/code/console` |
| `kimi-code-cn` | Kimi, mainland China | `https://api.kimi.com/coding` | `https://www.kimi.com/code/console` |

Code mode forwards Anthropic Messages to `v1/messages` with the key in
`X-Api-Key`, as Kimi's Claude Code setup places it through `ANTHROPIC_API_KEY`,
and Chat Completions to `v1/chat/completions` with the key as a bearer token.

## Upstream evidence

- [Kimi Code documentation](https://www.kimi.com/code/docs/en/): the plan's
  coding endpoints and account console.
- [Claude Code](https://www.kimi.com/code/docs/en/third-party-tools/claude-code):
  `ANTHROPIC_BASE_URL` `https://api.kimi.ai/coding` with `ANTHROPIC_API_KEY`.

Kimi's terms forbid altering the client's identity; code mode forwards the
client's own `User-Agent` unchanged.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o kimi-code.wasm ./plugins/kimi-code
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins: `https://api.kimi.ai`,
`https://api.kimi.com`, `https://www.kimi.ai` and `https://www.kimi.com`. Use the digest as
`profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "kimi-code",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. `POST /api/v1/providers/{id}/grant-enrollments` returns the key page as
   `authorization_url`, with `input: "secret"`.
2. Create a key there, then `POST .../grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. The completion returns the credential ID and the
   principal `kimi-code:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key is a different principal:
rotate a key by enrolling it, creating a new code account and revoking the old
credential version. Conversations pinned to the old account start again.

See [code mode](../../docs/use/code-mode.md) for routes and client
configuration, and the [qualification](../../docs/develop/qualification.md)
for what the controlled fixtures prove. Kimi's terms govern its use; OLP
forwards the client's own `User-Agent` and requests unmodified but cannot
guarantee that Kimi accepts their use through a gateway.
