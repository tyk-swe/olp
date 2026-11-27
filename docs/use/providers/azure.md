# Azure OpenAI connector

The `azure_openai` provider kind speaks the OpenAI wire contract against an Azure OpenAI or Azure AI Foundry resource. The request path and required configuration fields depend on whether the connection uses Azure's v1 API profiles or its legacy dated API-version profiles.

| Profile | Upstream path | Required fields |
| --- | --- | --- |
| `azure-v1-chat` | `/openai/v1/chat/completions` | Resource origin or base ending in `/openai/v1`, no `api_version` |
| `azure-v1-responses` | `/openai/v1/responses` | Resource origin or base ending in `/openai/v1`, no `api_version` |
| `azure-legacy-chat` or Automatic | `/openai/deployments/{deployment}/{resource}?api-version={api_version}` | Resource origin, `deployment`, and dated `api_version` (`YYYY-MM-DD` or `YYYY-MM-DD-preview`) |
| `azure-legacy-responses` | `/openai/responses?api-version={api_version}` | Resource origin and dated `api_version`, with the deployment named in the request body |

The `azure-v1-chat` and `azure-v1-responses` profiles do not accept a dated `api_version`, `cloud_project`, or `cloud_region` field. They also serve Azure AI Foundry Models such as DeepSeek, Grok, and Llama deployments at `https://{resource}.services.ai.azure.com` with the deployment name as the model. See [provider profiles](../provider-profiles.md) for profile selection and strict-route rules.

## Authentication

| Mode | Credentials |
| --- | --- |
| `api_key` | Azure API key, sent as `Api-Key` |
| `azure_default` | No stored credential, using the ambient Azure identity chain (managed, workload, or local developer identity) |
| `azure_client_secret` | Strict JSON containing only `tenant_id`, `client_id`, and `client_secret` |

Microsoft Entra token scopes match the selected hosting profile. The `azure-v1-chat` and `azure-v1-responses` profiles request the `https://ai.azure.com/.default` scope. Legacy deployment profiles (`azure-legacy-chat`, `azure-legacy-responses`) and Automatic `azure_openai` connections request the `https://cognitiveservices.azure.com/.default` scope.

OLP caches Entra tokens against the connection fingerprint, refreshes them 30 seconds before expiry, and sends them in `Authorization: Bearer`. Token acquisition runs through a bounded authentication transport that permits only the Azure SDK's maintained identity endpoints, including Entra authority hosts and the link-local managed identity endpoint, while inference traffic follows the normal provider egress policy. Tokens and client secrets never appear in logs or diagnostics.

## Operations and retained resources

Like official OpenAI, Azure OpenAI targets can qualify for the Files and Batch APIs, realtime WebSocket sessions, opt-in provider-retained Responses state, and the retained resource operations `fine_tuning`, `vector_store`, `retrieval`, and `container`. File uploads name the route through `X-OLP-Route`, and the gateway pins each retained provider resource to one revision, slot, and credential. See the [compatibility matrix](../compatibility.md) for the qualified operations.

For operator-managed Azure AI Search indexes, use the separate `azure_ai_search` connector kind with the `azure-ai-search` profile and its `https://search.azure.com/.default` Entra scope. See [retrieval](../retrieval.md) for Azure AI Search configuration and query rules.

## Media

An Azure OpenAI deployment serves `image_generation`, `speech`, and `transcription` on transformed routes through OpenAI's media wire at `/openai/deployments/{deployment}/...` with the provider's dated `api_version`. Speech requires a preview API version such as `2025-04-01-preview`. Because Azure's v1 API offers no media in general availability, the `azure-v1-chat` and `azure-v1-responses` profiles do not serve media operations.

Azure offers no costless way to prove that a deployment serves a media operation, so certification sends the smallest real call, just as Vertex AI and Bedrock image certification do. That call generates one low-quality image, synthesizes two characters of speech, or transcribes a tenth of a second of silence, and each certification bills that call. Streaming media, image edits, image variations, audio translation, and video are not certified for Azure OpenAI.

## Testing

Azure OpenAI v1 and legacy endpoint addressing, Entra token scope selection and caching, capability matrices, and profile validation are covered by unit tests in `internal/connectors/*_test.go`, which run in `make test-unit`. Run them focused with the following command.

```sh
go test -count=1 ./internal/connectors
```

To verify behavior against a live resource, run the gateway against a stub or a real Azure deployment.
