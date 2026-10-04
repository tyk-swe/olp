# GLM Coding Plan accounts

This plugin enrolls Z.ai GLM Coding Plan API keys for code-mode accounts. The
operator pastes a key the vendor's key page issues; OLP stores it encrypted as
the grant's access token and identifies it by a fingerprint. Enrollment makes
no upstream call and performs no inference. The ordinary signing hook refuses
probes and ordinary routes, so the key serves only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `zai-coding-plan` | Z.ai | `https://api.z.ai/api` | `https://z.ai/manage-apikey/apikey-list` |
| `bigmodel-coding-plan` | BigModel, mainland China | `https://open.bigmodel.cn/api` | `https://open.bigmodel.cn/usercenter/apikeys` |

Code mode forwards Anthropic Messages to `anthropic/v1/messages` and Chat
Completions to `coding/paas/v4/chat/completions` beneath the upstream, with the
key as a bearer token. Coding Plan keys work only on these coding endpoints.

## Upstream evidence

- [GLM Coding Plan overview](https://docs.z.ai/devpack/overview): plans, 5-hour
  and weekly credit windows, models and supported tools.
- [Claude Code](https://docs.z.ai/devpack/tool/claude): `ANTHROPIC_BASE_URL`
  `https://api.z.ai/api/anthropic` with `ANTHROPIC_AUTH_TOKEN`.
- [FAQ](https://docs.z.ai/devpack/faq): the OpenAI-compatible coding endpoint
  `https://api.z.ai/api/coding/paas/v4` for other tools, and the restriction to
  officially supported tools.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o zai-coding.wasm ./plugins/zai-coding
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins: `https://api.z.ai`,
`https://open.bigmodel.cn` and `https://z.ai`. Use the digest as
`profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "zai-coding-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. `POST /api/v1/providers/{id}/grant-enrollments` returns the key page as
   `authorization_url`, with `input: "secret"`.
2. Create a key there, then `POST .../grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. The completion returns the credential ID and the
   principal `zai-coding-plan:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key is a different principal:
rotate a key by enrolling it, creating a new code account and revoking the old
credential version. Conversations pinned to the old account start again.

See [code mode](../../docs/features/code-mode.md) for routes and client
configuration, and the [qualification](../../docs/qualification/code-mode.md)
for what the controlled fixtures prove. Z.ai restricts the plan to its
officially supported tools; OLP forwards the client's requests unmodified but
cannot guarantee that Z.ai accepts their use through a gateway.
