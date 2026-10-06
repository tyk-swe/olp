# Azure OpenAI connector

The Azure OpenAI provider speaks the OpenAI wire contract against a resource
deployment. The endpoint is the resource origin; `deployment` and `api_version`
complete the request path
`/openai/deployments/{deployment}/{resource}?api-version={api_version}`.

## Authentication

| Mode | Credentials |
| --- | --- |
| `api_key` | Azure API key, sent as `Api-Key` |
| `azure_default` | No stored credential; the ambient Azure chain (managed, workload, or local developer identity) |
| `azure_client_secret` | JSON `tenant_id`, `client_id`, `client_secret` — strict, no additional fields |

Both Entra modes request the `https://cognitiveservices.azure.com/.default`
scope, cache tokens against the connection fingerprint, refresh thirty seconds
before expiry, and send `Authorization: Bearer`. Token acquisition runs through
a bounded authentication transport that permits only the Azure SDK's maintained
identity endpoints — the Entra authority hosts and the link-local managed
identity endpoint — while provider inference egress is unchanged. Tokens and
client secrets never reach logs or diagnostics.

Like official OpenAI, Azure OpenAI targets can qualify for the Files and Batch
APIs, realtime WebSocket sessions, and opt-in provider-retained Responses state.
File uploads name the route through `X-OLP-Route`, and the gateway pins each
provider resource to one revision, slot, and credential — see the
[compatibility matrix](../compatibility.md).

## Media

An Azure OpenAI deployment serves `image_generation`, `speech` and
`transcription` on transformed routes, through OpenAI's media wire at
`/openai/deployments/{deployment}/...` with the provider's dated API version:
speech needs a preview version, such as `2025-04-01-preview`. Azure's v1 API
offers no media in general availability, so the `azure-v1-*` profiles do not
serve it.

Azure has no costless way to show that a deployment serves a media operation,
so certification makes the smallest real call, as Vertex and Bedrock image
certification does: one low-quality image, two characters of speech, or a
tenth of a second of silence to transcribe. Each certification bills that
call. Streaming, edits, variations, translation and video are not certified
for Azure.

## Testing

```sh
make test-go GO_TEST_PACKAGES=./internal/connectors GO_TEST_ARGS='-run Azure'
```

Stub credential coverage exercises bearer injection, token caching, and failure
classification without Azure credentials. Live qualification follows the shared
`live-providers` workflow with an Entra identity available.
