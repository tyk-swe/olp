# OpenCode Go accounts

This plugin enrolls OpenCode Go API keys for code-mode accounts. The operator
pastes a key from the OpenCode console, and OLP stores it encrypted as the
grant's access token while identifying it by a profile-scoped fingerprint.
Enrollment makes no upstream call and performs no inference. The ordinary
signing hook refuses probes and ordinary routes, so the key serves only
code-mode routes.

## Profile and protocol dispatch

| Profile | Upstream | Key page |
| --- | --- | --- |
| `opencode-go` | `https://opencode.ai/zen/go/v1` | `https://opencode.ai/auth` |

OpenCode Go dispatches each model to one upstream endpoint based on its
lowercase model prefix. Models starting with `minimax` or `qwen` use Anthropic
Messages (`messages`), authenticated with `X-Api-Key` as OpenCode's Anthropic
SDK sends it. Models starting with `gpt` or `grok` use the Responses API
(`responses`), authenticated with a bearer `Authorization` header. All other
models use Chat Completions (`chat/completions`), authenticated with a bearer
`Authorization` header.

Generated OpenCode configuration (`opencode-go`) declares each model with its
matching SDK package (`@ai-sdk/anthropic`, `@ai-sdk/openai` or
`@ai-sdk/openai-compatible`). Claude Code speaks only Anthropic Messages, so
on an OpenCode Go route it is offered only the Messages models.

## Upstream evidence

The [OpenCode Go documentation](https://opencode.ai/docs/go/) documents the
base URL, the endpoint serving each model, dollar-metered 5-hour, weekly and
monthly limits, and the session header and user agent clients send. The
[OpenCode session headers implementation](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/session/llm/request.ts)
documents `x-opencode-session` and `x-opencode-session-id` for the
`opencode-go` provider, which code mode forwards unchanged.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o opencode-go.wasm ./internal/codemode/codeplans/opencode-go
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origin `https://opencode.ai`.
Use the digest as `profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "opencode-go",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. Call `POST /api/v1/providers/{id}/grant-enrollments`, which returns the
   OpenCode console as `authorization_url` with `input` set to `"secret"`.
2. Subscribe to Go there and copy the key, then call
   `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. Completion returns the credential ID and the
   principal `opencode-go:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key produces a different
principal, so rotate a key by enrolling it, creating a new code account and
revoking the old credential version. Conversations pinned to the old account
must start again.

## Allowance charge observation

When upstream usage includes a `usage.opencode` object, OLP records
`allowance_charge` on attempt diagnostics, capturing recognized `billing_source`
values (`free`, `balance`, `go`, `go-plus` or `byok`), optional `list_cost`,
`allowance` and `cost` amounts, and reported token counts (`input`, `output`,
`reasoning`, `cache_read`, `cache_write_5m` and `cache_write_1h`). This field
is an observation only and never influences account admission or ordering.
OpenCode Go reports `allowance_signal` as `none` and has no qualified replay
proof.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for qualification lanes and release gates. OpenCode Go's terms govern its use,
and while OLP forwards the client's requests unmodified, it cannot guarantee
that OpenCode accepts their use through a gateway.
