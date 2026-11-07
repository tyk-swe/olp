# Grok Build subscription authorization

This plugin enrolls Grok Build subscriptions with device authorization. Authenticated userinfo at the fixed `https://auth.x.ai` issuer supplies the stable subject. OLP hashes the issuer and subject into the account principal. Display names, email and decoded JWT claims never establish identity. The profile refuses ordinary signing and paid API inference.

Enrollment requires an explicit positive token expiry and a refresh token. Refresh calls authenticated userinfo again and refuses missing, ambiguous or changed subjects. The existing encrypted grant writer preserves an omitted refresh token and atomically stores rotated tokens. Access tokens remain encrypted at rest and are never returned in client configuration.

The subscription endpoint is `https://cli-chat-proxy.grok.com/v1/responses`. OLP never substitutes the separately billed public API endpoint. Existing code accounts, pools, keys, budgets and bounded recovery apply.

## Installation

Build from the repository root.

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -buildmode=c-shared -o grok-build.wasm ./internal/grokauth/grok
```

Install the module through `POST /api/v1/plugins` with `Content-Type: application/wasm`, approve its declared origins, and permit it through the existing plugin controls. Use its returned digest as the provider's `profile_revision`, with `kind` set to `plugin`, `auth_mode` set to `grant` and `profile_id` set to `grok-build-subscription`. Start device enrollment through the normal provider credential flow, approve the user code on the displayed provider page, and poll until authorization finishes. Assign the resulting credential to an existing Grok Build code account and pool.

Publish `responses_to_grok` to enable the supported text and function-tool compatibility lane. Separately publish `grok_cli_identity` to send the source-backed Grok client compatibility headers. Both policies default off and editing a draft does not change the published route. Generated Codex configuration uses HTTP and SSE with WebSockets disabled. Opaque reasoning state, server-side response references, images and non-function tools are unsupported.

## Evidence and limits

Device endpoints, public client and refresh forms follow [OpenCode's pinned implementation](https://github.com/anomalyco/opencode/blob/145e948159e8dbaa3229d354a98dcfbcff7cfe92/packages/opencode/src/plugin/xai.ts). The authenticated userinfo identity seam is qualified with controlled fixtures. Anonymous discovery established endpoint availability, not successful live enrollment or independent-gateway authorization.

The separate identity policy follows [Grok Build proxy headers](https://github.com/xai-org/grok-build/blob/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8/crates/codegen/xai-grok-shell/src/agent/proxy_headers.rs) and [client defaults](https://github.com/xai-org/grok-build/blob/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8/crates/codegen/xai-grok-sampler/src/client.rs). Compatibility version `1.0.45` comes from that pinned source's package metadata. It is not a claim about the current released client. OLP does not invent account, deployment or conversation identities.

Applicable provider terms may restrict independent gateways, client identity changes or subscription sharing. Accounts may be suspended. Controlled fixture success does not prove live provider acceptance, and live independent-gateway acceptance remains unverified. No acknowledgement gate is added.
