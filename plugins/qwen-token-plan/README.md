# Qwen Token Plan accounts

This plugin enrolls Alibaba Model Studio Token Plan Team Edition API keys for
code-mode accounts. The operator pastes a key the vendor's key page issues;
OLP stores it encrypted as the grant's access token and identifies it by a
fingerprint. Enrollment makes no upstream call and performs no inference. The
ordinary signing hook refuses probes and ordinary routes, so the key serves
only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `qwen-token-plan` | Model Studio, Singapore | `https://token-plan.ap-southeast-1.maas.aliyuncs.com` | `https://modelstudio.console.alibabacloud.com/ap-southeast-1/subscription/overview` |
| `qwen-token-plan-cn` | Bailian, mainland China | `https://token-plan.cn-beijing.maas.aliyuncs.com` | `https://bailian.console.aliyun.com/cn-beijing/subscription/overview` |

Code mode forwards Anthropic Messages to `apps/anthropic/v1/messages` and Chat
Completions to `compatible-mode/v1/chat/completions` beneath the upstream, with
the key as a bearer token.

The subscription overview issues the plan's key, which starts with `sk-sp-`;
enrollment refuses a key without that prefix because Model Studio
pay-as-you-go keys would bill outside the plan.

## Upstream evidence

- [Token Plan Team quickstart](https://www.alibabacloud.com/help/en/model-studio/token-plan-team-quickstart):
  the `sk-sp-` plan key, the token-plan upstream hosts and the Claude Code and
  OpenAI-compatible endpoints.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o qwen-token-plan.wasm ./plugins/qwen-token-plan
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins:
`https://token-plan.ap-southeast-1.maas.aliyuncs.com`,
`https://token-plan.cn-beijing.maas.aliyuncs.com`,
`https://modelstudio.console.alibabacloud.com` and
`https://bailian.console.aliyun.com`. Use the digest as `profile_revision` in a
project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "qwen-token-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. `POST /api/v1/providers/{id}/grant-enrollments` returns the key page as
   `authorization_url`, with `input: "secret"`.
2. Create a key there, then `POST .../grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. The completion returns the credential ID and the
   principal `qwen-token-plan:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key is a different principal:
rotate a key by enrolling it, creating a new code account and revoking the old
credential version. Conversations pinned to the old account start again.

See [code mode](../../docs/use/code-mode.md) for routes and client
configuration, and the [qualification](../../docs/develop/qualification.md)
for what the controlled fixtures prove. Alibaba's terms govern Token Plan use;
OLP forwards the client's requests unmodified but cannot guarantee that
Alibaba accepts their use through a gateway.
