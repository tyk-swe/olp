# MiniMax M Plan accounts

This plugin enrolls MiniMax M Plan API keys for code-mode accounts. The
operator pastes a key issued by the vendor's key page, and OLP stores it
encrypted as the grant's access token while identifying it by a profile-scoped
fingerprint. Enrollment makes no upstream call and performs no inference. The
ordinary signing hook refuses probes and ordinary routes, so the key serves
only code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `minimax-plan` | MiniMax | `https://api.minimax.io` | `https://platform.minimax.io/console/plan` |
| `minimax-plan-cn` | MiniMax, mainland China | `https://api.minimax.cn` | `https://platform.minimax.cn/console/plan` |

Code mode forwards Anthropic Messages to `anthropic/v1/messages` and Chat
Completions to `v1/chat/completions` beneath the upstream, with the key as a
bearer token. Generated OpenCode configuration (`minimax-coding-plan`) targets
the Anthropic Messages protocol (`@ai-sdk/anthropic`) for MiniMax M Plan
models, and Claude Code uses Messages as well.

The key page issues the plan's Subscription Key, which starts with `sk-cp-`.
Enrollment refuses any key without that prefix because MiniMax pay-as-you-go
keys (`sk-api-`) share the host and would bill outside the plan. Legacy Token
Plan subscriptions issue the same Subscription Key and enroll the same way.

## Upstream evidence

The [M Plan in other tools guide](https://platform.minimax.io/docs/m-plan/other-tools)
documents the `sk-cp-` Subscription Key and the OpenAI-compatible and
Anthropic-compatible plan endpoints. The
[Claude Code guide](https://platform.minimax.io/docs/m-plan/claude-code)
documents `ANTHROPIC_BASE_URL` set to `https://api.minimax.io/anthropic` with
`ANTHROPIC_AUTH_TOKEN`.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o minimax-plan.wasm ./internal/codemode/codeplans/minimax-plan
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins
(`https://api.minimax.io`, `https://api.minimax.cn`,
`https://platform.minimax.io` and `https://platform.minimax.cn`). Use the
digest as `profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "minimax-plan",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. Call `POST /api/v1/providers/{id}/grant-enrollments`, which returns the key
   page as `authorization_url` with `input` set to `"secret"`.
2. Create a Subscription Key (`sk-cp-`) on the vendor page, then call
   `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. Completion returns the credential ID and the
   principal `minimax-plan:<sha-256 hex>` for `minimax-plan` or
   `minimax-plan-cn:<sha-256 hex>` for `minimax-plan-cn`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key produces a different
principal, so rotate a key by enrolling it, creating a new code account and
revoking the old credential version. Conversations pinned to the old account
must start again.

## Cooldowns and allowance signal

On HTTP 429 responses, OLP parses bounded JSON error bodies (up to 64 KiB) to
extract MiniMax numeric business codes from `base_resp.status_code`,
`error.code` or the parenthesized code at the end of `error.message`. Business
codes `2056` and `2067` set a credential-scoped `quota` cooldown. Code `2061`
sets a model-scoped `entitlement` cooldown affecting only that model. Codes
`2062`, `1002` and `1039` set a credential-scoped `rate` cooldown. Deadlines
honor a single valid `Retry-After` header or fall back to one minute. These
cooldown envelopes do not establish nonexecution and cannot authorize replay.

Both MiniMax profiles report `allowance_signal` as `remains-unqualified` with
`allowance_signal_reason` set to `live-qualification-pending`. Active allowance
reads against `/v1/token_plan/remains` remain disabled pending live
qualification, and existing active observations do not influence admission or
ordering.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for qualification lanes and release gates. MiniMax's terms govern M Plan use,
and while OLP forwards the client's requests unmodified, it cannot guarantee
that MiniMax accepts their use through a gateway.
