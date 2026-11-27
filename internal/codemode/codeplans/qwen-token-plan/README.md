# Qwen Token Plan accounts

This plugin enrolls Alibaba Model Studio Token Plan Team Edition API keys for
code-mode accounts. The operator pastes a key issued by the vendor's key page,
and OLP stores it encrypted as the grant's access token while identifying it by
a profile-scoped fingerprint. Enrollment makes no upstream call and performs no
inference. The ordinary signing hook refuses probes and ordinary routes, so the
key serves only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `qwen-token-plan` | Model Studio, Singapore | `https://token-plan.ap-southeast-1.maas.aliyuncs.com` | `https://modelstudio.console.alibabacloud.com/ap-southeast-1/subscription/overview` |
| `qwen-token-plan-cn` | Bailian, mainland China | `https://token-plan.cn-beijing.maas.aliyuncs.com` | `https://bailian.console.aliyun.com/cn-beijing/subscription/overview` |

Code mode forwards Anthropic Messages to `apps/anthropic/v1/messages` and Chat
Completions to `compatible-mode/v1/chat/completions` beneath the upstream, with
the key as a bearer token.

The subscription overview issues the plan's key, which starts with `sk-sp-`.
Enrollment refuses any key without that prefix because Model Studio
pay-as-you-go keys would bill outside the plan.

## Upstream evidence

The [Token Plan Team quickstart](https://www.alibabacloud.com/help/en/model-studio/token-plan-team-quickstart)
documents the `sk-sp-` plan key, the token-plan upstream hosts and the Claude
Code and OpenAI-compatible endpoints.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o qwen-token-plan.wasm ./internal/codemode/codeplans/qwen-token-plan
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins
(`https://token-plan.ap-southeast-1.maas.aliyuncs.com`,
`https://token-plan.cn-beijing.maas.aliyuncs.com`,
`https://modelstudio.console.alibabacloud.com` and
`https://bailian.console.aliyun.com`). Use the digest as `profile_revision` in
a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "qwen-token-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. Call `POST /api/v1/providers/{id}/grant-enrollments`, which returns the key
   page as `authorization_url` with `input` set to `"secret"`.
2. Create a Token Plan key (`sk-sp-`) on the vendor page, then call
   `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. Completion returns the credential ID and the
   principal `qwen-token-plan:<sha-256 hex>` for `qwen-token-plan` or
   `qwen-token-plan-cn:<sha-256 hex>` for `qwen-token-plan-cn`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key produces a different
principal, so rotate a key by enrolling it, creating a new code account and
revoking the old credential version. Conversations pinned to the old account
must start again.

## Cycle anchor and cooldowns

Qwen Token Plan accounts support the optional **Subscription cycle anchor**
setting (API field `cycle_anchor`, taking a `YYYY-MM-DD` calendar date or
`null`). Setting `cycle_anchor` on a non-Qwen account is rejected. OLP uses the
anchor's day of the month, clamped to the last day of shorter months at 00:00
UTC, to compute the next monthly quota reset.

On HTTP 429 responses, OLP parses bounded JSON error bodies (up to 64 KiB,
reading `error.code` and `error.message` or top-level `code` and `message`).
Code `insufficient_quota` with message
`"Your token-plan quota has been exhausted."` sets a credential-scoped `quota`
cooldown. Code `Throttling.RateQuota` with a message starting with
`"Requests rate limit exceeded"` sets a credential-scoped `rate` cooldown.
Deadlines honor a single valid `Retry-After` header or fall back to one minute.
When `cycle_anchor` is set on the account, an `insufficient_quota` cooldown
extends its deadline to the next monthly UTC reset whenever that date is later
than the header or fallback deadline. These cooldown envelopes do not prove
nonexecution and cannot authorize replay.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for qualification lanes and release gates. Alibaba's terms govern Token Plan
use, and while OLP forwards the client's requests unmodified, it cannot
guarantee that Alibaba accepts their use through a gateway.
