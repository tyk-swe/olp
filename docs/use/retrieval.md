# Cloud retrieval bindings

Cloud retrieval bindings give selected API keys query-only access to operator-managed knowledge bases, RAG corpora, and search indexes through `POST /v1/retrieval/{binding_id}/query`. Callers reference only the binding UUID and submit a bounded query object. They never name provider endpoints, cloud resource identifiers, or raw backend indexes, and they cannot create, modify, or synchronize an index.

Retrieval bindings run on top of explicit versioned provider profiles and transformed routes. See [provider profiles](provider-profiles.md) for profile composition rules, [Amazon Bedrock](providers/bedrock.md), [Google Vertex AI](providers/vertex.md), and [Azure OpenAI](providers/azure.md) for cloud connector details, and [configuration management](../operate/configuration.md) for declarative promotion across environments.

## Supported backends and provider setup

OLP supports three dedicated retrieval backends, each bound to a specific connector kind and versioned profile at revision `1`.

| Backend value | Provider kind | Profile ID (dialect revision) | Authentication modes | Native resource format |
| --- | --- | --- | --- | --- |
| `bedrock_knowledge_base` | `bedrock` | `bedrock-knowledge-bases` (`2023-09-30`) | `default_chain`, `static` | 10-character alphanumeric Knowledge Base ID matching `^[A-Za-z0-9]{10}$` |
| `vertex_rag` | `vertex_ai` | `vertex-rag-engine` (`v1`) | `adc`, `service_account` | Full corpus name `projects/{project}/locations/{location}/ragCorpora/{corpus}` inside the provider's project and location |
| `azure_ai_search` | `azure_ai_search` | `azure-ai-search` (`2025-09-01`) | `api_key`, `azure_default`, `azure_client_secret` | Index name matching `^[a-z0-9][a-z0-9_-]{0,127}$` |

Retrieval profiles refuse `operation_defaults` and model `bindings` on the provider configuration because resource selection and field mappings belong on the retrieval binding. Because cloud retrieval backends do not expose a complete catalog listing, operators declare each Knowledge Base ID, RAG corpus name, or search index name on the provider connection. Certification performs a cost-free `GET` metadata probe and confirms that the returned resource identity matches the declared name (`knowledgeBase.knowledgeBaseId` for Bedrock and `name` for Vertex and Azure).

A route that backs a retrieval binding must include `"retrieval"` in its `operations` list, run in `transformed` fidelity mode, belong to the same project as the binding, and target a provider that does not use `credential_source: "caller"`.

### Amazon Bedrock Knowledge Bases (`bedrock-knowledge-bases`)

Create a `bedrock` provider with `profile_id` set to `"bedrock-knowledge-bases"`, `profile_revision` set to `"1"`, and `cloud_region` set to the AWS region. When `endpoint` is omitted, OLP defaults to the regional Bedrock Agent Runtime origin `https://bedrock-agent-runtime.{region}.amazonaws.com`. Custom endpoints must be a service origin without a path. Metadata certification requires the default regional endpoint so OLP can probe `GET https://bedrock-agent.{region}.amazonaws.com/knowledgebases/{id}`, while queries dispatch to `POST /knowledgebases/{id}/retrieve` signed with SigV4 for the `bedrock` service.

### Vertex RAG Engine (`vertex-rag-engine`)

Create a `vertex_ai` provider with `profile_id` set to `"vertex-rag-engine"`, `profile_revision` set to `"1"`, and the target `cloud_project` and `cloud_region`. OLP derives the regional Vertex AI location endpoint `/v1/projects/{project}/locations/{region}`, certifies each declared corpus with `GET .../ragCorpora/{corpus}`, and sends queries to `POST ...:retrieveContexts` with a Google Cloud bearer token. Every bound corpus must stay within the connection's configured project and location.

### Azure AI Search (`azure_ai_search` connector kind)

Azure AI Search uses its own dedicated connector kind `azure_ai_search` rather than `azure_openai`. Configure `profile_id` as `"azure-ai-search"`, `profile_revision` as `"1"`, `endpoint` as the search service origin without a path (for example `https://my-search.search.windows.net`), and `api_version` as `"2025-09-01"`. Leave `cloud_region`, `cloud_project`, and `deployment` empty, and declare each queryable index name on the provider.

| Auth mode | Credentials and upstream header |
| --- | --- |
| `api_key` | Azure AI Search query API key, sent in the `Api-Key` header. |
| `azure_default` | No stored credential. Uses the ambient Azure identity chain (managed, workload, or local developer identity). |
| `azure_client_secret` | Strict JSON object containing only `tenant_id`, `client_id`, and `client_secret`. |

Both Microsoft Entra modes (`azure_default` and `azure_client_secret`) request tokens for the `https://search.azure.com/.default` scope, cache tokens by connection fingerprint, refresh 30 seconds before expiry, send `Authorization: Bearer`, and run through OLP's bounded identity transport with a 10-second timeout and redirects refused. Certification probes `GET /indexes/{index}?api-version=2025-09-01`, and queries dispatch to `POST /indexes/{index}/docs/search?api-version=2025-09-01`.

## Managing retrieval bindings

Retrieval bindings are project-owned management resources managed through six endpoints under `/api/v1/retrieval-bindings`. Every update creates a new immutable revision while preserving earlier revisions for auditability. All mutations require `Idempotency-Key`, and `PUT` and `DELETE` require `If-Match` carrying the latest observed `ETag`. Every create, update, or retirement advances the gateway authority generation so serving replicas pick up grant changes and retirements promptly.

| Method and path | Required authority | Behavior |
| --- | --- | --- |
| `GET /api/v1/retrieval-bindings` | `read` | Cursor-paginated list of active (non-retired) bindings visible to the caller's authorized projects. |
| `POST /api/v1/retrieval-bindings` | `configure` and project change | Creates a binding and its first immutable revision (max body 64 KiB). Returns HTTP 201 with `ETag` and `Location`. |
| `GET /api/v1/retrieval-bindings/{binding_id}` | `read` and project view | Returns the current binding revision and `ETag`. |
| `PUT /api/v1/retrieval-bindings/{binding_id}` | `configure` and project change | Validates the updated definition and saves the next immutable revision. Requires `If-Match`. |
| `DELETE /api/v1/retrieval-bindings/{binding_id}` | `configure` and project change | Retires the binding, returning HTTP 204. Retired history remains readable and cannot be modified (HTTP 409 `retrieval_binding_retired`). |
| `GET /api/v1/retrieval-bindings/{binding_id}/revisions/{revision_id}` | `read` and project view | Reads a retained immutable revision definition by UUID. |

Each binding specifies `project_id` (immutable after creation), `name` (1 to 100 characters, unique case-insensitively among active bindings in the project, returning HTTP 409 `retrieval_binding_exists` on duplicate names), `route` (route slug up to 128 characters), `target_id` (UUID of a single target on that project's active route or draft whose provider kind and `provider_model` match `backend` and `backend_resource` and whose route allows `retrieval`), `backend`, `backend_resource`, `fields`, `key_grants`, and `enabled`.

`key_grants` accepts up to 2000 canonical UUIDs of active, non-expired API keys in the owning project. OLP sorts and deduplicates the list on save. An empty `key_grants` array is valid and grants query access to no keys.

### Field mappings

The `fields` object controls which backend fields OLP reads, searches, and exposes to structured filters. All backend field paths must match `^[A-Za-z_][A-Za-z0-9_]*(/[A-Za-z_][A-Za-z0-9_]*)*$`.

For `azure_ai_search`, `fields.text` and `fields.id` are required because Azure indexes use operator-defined schemas. Optional `fields.title` and `fields.url` populate citation fields, optional `fields.search` (up to 32 field paths) sets Azure's `searchFields` parameter, and optional `fields.metadata` (up to 64 logical-key to backend-path entries, with logical keys of 1 to 128 characters) maps filterable and returned attributes. OLP sends only the mapped fields in Azure's `select` projection.

For `bedrock_knowledge_base` and `vertex_rag`, setting `text`, `id`, `title`, `url`, or `search` is refused because those backends return native text and citation fields. On Bedrock, use `fields.metadata` to allowlist logical metadata keys for query filters.

The following example creates an enabled Azure AI Search retrieval binding.

```json
{
  "project_id": "019a0000-0000-7000-8000-000000000010",
  "name": "product-docs-search",
  "route": "docs-retrieval",
  "target_id": "019a0000-0000-7000-8000-000000000020",
  "backend": "azure_ai_search",
  "backend_resource": "product-docs-v1",
  "fields": {
    "text": "chunk_text",
    "id": "chunk_id",
    "title": "doc_title",
    "url": "source_url",
    "search": ["doc_title", "chunk_text"],
    "metadata": {
      "category": "category",
      "locale": "meta/locale"
    }
  },
  "key_grants": [
    "019a0000-0000-7000-8000-000000000030"
  ],
  "enabled": true
}
```

## Querying a retrieval binding

Authorized callers query a binding by sending an HTTP `POST` to `/v1/retrieval/{binding_id}/query` with a single JSON object body and no query string parameters.

Authentication uses the gateway's `inference` scope. In addition to valid authentication, the calling API key must carry the `allow_provider_state` policy flag, belong to the binding's owning project, appear in the binding's `key_grants`, and be authorized for `inference` on the binding's route. The binding must be enabled and not retired, the route must allow `retrieval` in `transformed` fidelity mode, and the binding's pinned route target must still be present and certified for the same provider, backend, and resource.

### Request contract

The request body is parsed strictly (unknown fields and duplicate keys are rejected) within the route and installation body limit and a 1 MiB codec bound.

| Field | Type | Constraints and backend support |
| --- | --- | --- |
| `query` | string | Required. 1 to 20,000 non-whitespace bytes. |
| `max_num_results` | integer | Optional. Range 1 to 100, defaults to 10. Maps to Bedrock `numberOfResults`, Vertex `topK`, and Azure `top`. |
| `filters` | object | Optional structured filter tree. Supported on `bedrock_knowledge_base` and `azure_ai_search`. Refused on `vertex_rag`. |
| `vector_distance_threshold` | number | Optional JSON number (not a string). Supported only on `vertex_rag`, mutually exclusive with `vector_similarity_threshold`. |
| `vector_similarity_threshold` | number | Optional JSON number (not a string). Supported only on `vertex_rag`, mutually exclusive with `vector_distance_threshold`. |

Structured `filters` support leaf comparisons (`eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `in`, `nin`) and compound groups (`and`, `or`). Nesting depth is capped at 8 levels, compound `filters` arrays require 2 to 16 child filters, `in` and `nin` require 1 to 100 scalar values (strings, numbers, or booleans), and boolean values are allowed only with `eq` or `ne`. Every leaf `key` must be declared in `fields.metadata`, preventing callers from probing unmapped index fields. OLP compiles filters to native Bedrock `andAll`/`orAll` filter objects or Azure OData filter expressions with escaped string literals.

```json
{
  "query": "How do I rotate a wrapped master key?",
  "max_num_results": 5,
  "filters": {
    "type": "and",
    "filters": [
      { "type": "eq", "key": "category", "value": "operations" },
      { "type": "in", "key": "locale", "value": ["en-US", "en-GB"] }
    ]
  }
}
```

### Response contract

Successful queries return HTTP 200 with a list envelope carrying the binding ID, the exact binding revision ID that served the query, and up to 1000 decoded results (bounded by a 32 MiB upstream decode ceiling and the gateway's retained response limit).

```json
{
  "object": "list",
  "binding_id": "019a0000-0000-7000-8000-000000000040",
  "binding_revision_id": "019a0000-0000-7000-8000-000000000041",
  "data": [
    {
      "text": "Rotate the active master key version before re-sealing stored secrets.",
      "score": 8.42,
      "score_kind": "score",
      "citation": {
        "id": "ops-keys-04",
        "url": "https://docs.example.invalid/operations#keys",
        "title": "Key rotation"
      },
      "attributes": {
        "category": "operations",
        "locale": "en-US"
      }
    }
  ]
}
```

OLP deliberately preserves each backend's native score without normalizing across backends. `score_kind` is `"distance"` when Vertex RAG Engine returns a `distance` value and `"score"` otherwise. Every decoded item must contain non-empty `text` and a native numeric `score`, or OLP fails the response as a protocol error. Citation shapes reflect each backend's native structure. Bedrock populates `citation` with the native `location` object and `attributes` with `metadata`. Vertex populates `citation` with `source_uri`, `source_display_name`, `source_name`, and `chunk`. Azure populates `citation` with `id`, `url`, and `title` and includes only mapped `fields.metadata` keys in `attributes`.

### Error codes

| HTTP status | Error code | Meaning |
| --- | --- | --- |
| 400 | `invalid_parameter` | Query string parameters were sent on `/v1/retrieval/{binding_id}/query`. |
| 400 | `invalid_request` | Request body is not a single unambiguous JSON object. |
| 400 | `unsupported_retrieval_control` | Query length, `max_num_results`, filter syntax, unmapped metadata key, or vector threshold violates the backend contract. |
| 403 | `provider_state_forbidden` | Calling API key does not have the `allow_provider_state` policy enabled. |
| 403 | `route_forbidden` | Calling API key is not authorized for `inference` on the binding's route. |
| 404 | `not_found` | Binding ID does not exist, is retired or disabled, belongs to another project, or does not grant access to this key. |
| 422 | `operation_unavailable` | Binding route does not include `retrieval` in its allowed operations. |
| 422 | `unsupported_transformation` | Binding route uses `strict` fidelity instead of `transformed`. |
| 422 | `retrieval_binding_unavailable` | Pinned route target or backend resource is missing, uncertified, or no longer matches the binding. |
| 502 | `provider_protocol_error` | Upstream returned a non-2xx status or an invalid or incomplete retrieval payload. |
| 503 | `provider_state_unavailable` | Retained resource storage is unavailable. |

## Configuration promotion

Retrieval bindings participate in portable installation configuration under the top-level `retrieval_bindings` array (up to 512 entries per document). Exports record logical identifiers rather than installation-local UUIDs, naming `project`, `name`, `route`, `target_provider`, `backend`, `backend_resource`, `fields`, `key_grants` (exact project-local API-key names, up to 2000), and `enabled`.

During `plan`, OLP verifies that the destination project exists (or is staged in the same document when no key grants are declared), that the destination route contains exactly one target matching `target_provider` and `backend_resource`, and that every name in `key_grants` resolves to exactly one active, non-expired API key in that project. Plans report `create`, `replace`, `reuse`, `stage`, or a blocker if a project, route target, or unique active API key is missing. Applying a plan creates new immutable binding revisions for changed definitions, leaves unchanged bindings intact, and advances gateway authority on commit. Serving still requires the destination route target to be published and certified.

## Console workflow

Operators manage retrieval bindings in the console under **Observe**, then **Provider resources** (`/provider-resources`), inside the **Cloud retrieval bindings** section. Members with `providers.manage` (`configure`) authority can add a binding, edit its route, target ID, backend, backend resource, JSON field mappings, line-delimited API-key UUID grants, and enablement toggle, save a new revision, inspect any historical revision by revision ID, or retire an active binding with confirmation. Concurrent edits are guarded with ETag conflict detection and reload prompts.
