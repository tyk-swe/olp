# OpenCode Go accounts

This plugin enrolls OpenCode Go API keys for code-mode accounts. The operator
pastes a key from the OpenCode console; OLP stores it encrypted as the grant's
access token and identifies it by a fingerprint. Enrollment makes no upstream
call and performs no inference. The ordinary signing hook refuses probes and
ordinary routes, so the key serves only code-mode routes.

## Profile

| Profile | Upstream | Key page |
| --- | --- | --- |
| `opencode-go` | `https://opencode.ai/zen/go/v1` | `https://opencode.ai/auth` |

Code mode forwards Chat Completions to `chat/completions`, Anthropic Messages to
`messages` and the Responses API to `responses` beneath the upstream. The key
travels as a bearer token, or as `X-Api-Key` on Messages, as OpenCode's own
Anthropic SDK sends it.

## Upstream evidence

- [OpenCode Go](https://opencode.ai/docs/go/): the base URL, the endpoint that
  serves each model, dollar-metered 5-hour, weekly and monthly limits, and the
  session header and user agent clients should send.
- [OpenCode session headers](https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/session/llm/request.ts):
  `x-opencode-session` and `x-opencode-session-id` for the `opencode-go`
  provider, which code mode forwards unchanged.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o opencode-go.wasm ./plugins/opencode-go
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origin `https://opencode.ai`. Use
the digest as `profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "opencode-go",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. `POST /api/v1/providers/{id}/grant-enrollments` returns the OpenCode console
   as `authorization_url`, with `input: "secret"`.
2. Subscribe to Go there and copy the key, then
   `POST .../grant-enrollments/{enrollment_id}/continue` with
   `{"input": "<key>"}`. The completion returns the principal
   `opencode-go:<sha-256 hex>`.
3. Create the code account with that credential. Its health starts `unknown`.

The key never expires or refreshes. A different key is a different principal:
rotate a key by enrolling it, creating a new code account and revoking the old
credential version.

See [code mode](../../docs/features/code-mode.md) and the
[qualification](../../docs/qualification/code-mode.md). OpenCode Go's terms
govern its use; OLP forwards the client's requests unmodified but cannot
guarantee that OpenCode accepts their use through a gateway.
