# GLM Coding Plan accounts

This plugin enrolls Z.ai GLM Coding Plan API keys for code-mode accounts. The
operator pastes a key issued by the vendor's key page, and OLP stores it
encrypted as the grant's access token while identifying it by a profile-scoped
fingerprint. Enrollment makes no upstream call and performs no inference. The
ordinary signing hook refuses probes and ordinary routes, so the key serves
only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `zai-coding-plan` | Z.ai | `https://api.z.ai/api` | `https://z.ai/manage-apikey/apikey-list` |
| `bigmodel-coding-plan` | BigModel, mainland China | `https://open.bigmodel.cn/api` | `https://open.bigmodel.cn/usercenter/apikeys` |

Code mode forwards Anthropic Messages to `anthropic/v1/messages` and Chat
Completions to `coding/paas/v4/chat/completions` beneath the upstream, with the
key as a bearer token. The global `zai-coding-plan` profile also supports
optional [research tools](../../../../docs/use/code-mode.md#research-tools) at
the fixed Z.ai search, web-reader and repository MCP endpoints. The route binds
a pool account explicitly, and research works independently of its inference
models. Vision and local-file forwarding are excluded.

## Upstream evidence

The [GLM Coding Plan overview](https://docs.z.ai/devpack/overview) documents
plans, 5-hour and weekly credit windows, models and supported tools. The
[Claude Code guide](https://docs.z.ai/devpack/tool/claude) documents
`ANTHROPIC_BASE_URL` set to `https://api.z.ai/api/anthropic` with
`ANTHROPIC_AUTH_TOKEN`. The [FAQ](https://docs.z.ai/devpack/faq) documents the
OpenAI-compatible coding endpoint `https://api.z.ai/api/coding/paas/v4` for
other tools and the restriction to officially supported tools.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o zai-coding.wasm ./internal/codemode/codeplans/zai-coding
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins (`https://api.z.ai`,
`https://open.bigmodel.cn` and `https://z.ai`). Use the digest as
`profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "zai-coding-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. Call `POST /api/v1/providers/{id}/grant-enrollments`, which returns the key
   page as `authorization_url` with `input` set to `"secret"`.
2. Create a key on the vendor page, then call
   `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. Completion returns the credential ID and the
   principal `zai-coding-plan:<sha-256 hex>` for `zai-coding-plan` or
   `bigmodel-coding-plan:<sha-256 hex>` for `bigmodel-coding-plan`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key produces a different
principal, so rotate a key by enrolling it, creating a new code account and
revoking the old credential version. Conversations pinned to the old account
must start again.

## Cooldown classification

On HTTP 429 responses, OLP parses bounded two-field JSON error objects
(`error.code` and `error.message`, up to 64 KiB) to record typed cooldowns.
Quota error codes `1308`, `1310` and `1316` through `1321` set a
credential-scoped `quota` cooldown and parse RFC3339 reset timestamps embedded
in the vendor's error message templates, falling back to a single valid
`Retry-After` header or one minute when no future timestamp parses. Code `1302`
sets a credential-scoped `rate` cooldown. Code `1311` sets a model-scoped
`entitlement` cooldown, and code `1305` sets a model-scoped `capacity`
cooldown, cooling only that model on the account without marking other models
unavailable. Codes `1309` and `1314` suppress the generic 429 account cooldown
fallback. None of these envelopes prove nonexecution or authorize replay.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for qualification lanes and release gates. Z.ai restricts the plan to its
officially supported tools, and while OLP forwards the client's requests
unmodified, it cannot guarantee that Z.ai accepts their use through a gateway.
