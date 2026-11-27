# Kimi Code accounts

This plugin enrolls Kimi Code API keys for code-mode accounts. The operator
pastes a key issued by the vendor's key page, and OLP stores it encrypted as
the grant's access token while identifying it by a profile-scoped fingerprint.
Enrollment makes no upstream call and performs no inference. The ordinary
signing hook refuses probes and ordinary routes, so the key serves only
code-mode routes.

## Profiles

| Profile | Account system | Upstream | Key page |
| --- | --- | --- | --- |
| `kimi-code` | Kimi | `https://api.kimi.ai/coding` | `https://www.kimi.ai/code/console` |
| `kimi-code-cn` | Kimi, mainland China | `https://api.kimi.com/coding` | `https://www.kimi.com/code/console` |

Code mode forwards Anthropic Messages to `v1/messages` with the key in
`X-Api-Key`, matching Kimi's Claude Code setup through `ANTHROPIC_API_KEY`,
and Chat Completions to `v1/chat/completions` with the key as a bearer token.

## Upstream evidence

The [Kimi Code documentation](https://www.kimi.com/code/docs/en/) documents
the plan's coding endpoints and account console. The
[Claude Code guide](https://www.kimi.com/code/docs/en/third-party-tools/claude-code)
documents `ANTHROPIC_BASE_URL` set to `https://api.kimi.ai/coding` with
`ANTHROPIC_API_KEY`.

Kimi's terms forbid altering the client's identity, and code mode forwards the
client's own `User-Agent` unchanged.

## Installation and enrollment

Build the confined plugin from the repository root:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildmode=c-shared -o kimi-code.wasm ./internal/codemode/codeplans/kimi-code
```

Install it through `POST /api/v1/plugins` with `Content-Type: application/wasm`,
or the console's Plugins page, and approve its origins (`https://api.kimi.ai`,
`https://api.kimi.com`, `https://www.kimi.ai` and `https://www.kimi.com`). Use
the digest as `profile_revision` in a project-owned provider:

```json
{
  "kind": "plugin",
  "auth_mode": "grant",
  "profile_id": "kimi-code",
  "profile_revision": "<installed-plugin-digest>"
}
```

1. Call `POST /api/v1/providers/{id}/grant-enrollments`, which returns the key
   page as `authorization_url` with `input` set to `"secret"`.
2. Create a key on the vendor page, then call
   `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue`
   with `{"input": "<key>"}`. Completion returns the credential ID and the
   principal `kimi-code:<sha-256 hex>` for `kimi-code` or
   `kimi-code-cn:<sha-256 hex>` for `kimi-code-cn`.
3. Create the code account with that credential. Its health starts `unknown`.
   Do not invoke ordinary provider activation, certification or probing.

The key never expires or refreshes. A different key produces a different
principal, so rotate a key by enrolling it, creating a new code account and
revoking the old credential version. Conversations pinned to the old account
must start again.

## Account settings and cooldowns

Kimi Code accounts support the optional **Extra Usage** setting (API field
`extra_usage_enabled`, accepting `true`, `false` or `null`). It records
whether the member enabled Kimi's pay-as-you-go Extra Usage beyond the included
plan quota so paid overflow use is never read as spare included allowance.
Setting `extra_usage_enabled` on a non-Kimi account is rejected.

On HTTP 429 responses, OLP parses bounded JSON error bodies (up to 64 KiB) to
record typed cooldowns. Quota, credit and Extra Usage messages or error codes
(`exceeded_current_quota_error` and `insufficient_quota`) set a
credential-scoped `quota` cooldown. Concurrent request limit messages set a
credential-scoped `concurrency` cooldown. Rate-limit messages or error codes
(`rate_limit_exceeded`, `rate_limit_reached_error` and `rate_limit_error`) set
a credential-scoped `rate` cooldown. Engine overload messages or error codes
(`overloaded_error` and `engine_overloaded_error`) set a model-scoped
`capacity` cooldown affecting only that model. Deadlines honor a single valid
`Retry-After` header or fall back to one minute. These cooldown envelopes do
not prove nonexecution and cannot authorize replay.

See [code mode](../../../../docs/use/code-mode.md) for routes and client
configuration, and [qualification](../../../../docs/develop/qualification.md)
for qualification lanes and release gates. Kimi's terms govern its use, and
while OLP forwards the client's own `User-Agent` and requests unmodified, it
cannot guarantee that Kimi accepts their use through a gateway.
