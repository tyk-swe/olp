# Configuration reference

This reference covers configuration for the OLP server processes. Remote administration clients use `OLP_MANAGEMENT_URL` and `OLP_MANAGEMENT_TOKEN_FILE` instead, as described in [management automation](automation.md).

Server settings come from environment variables or command-line flags, with flags taking precedence. Run `olp <subcommand> --help` to inspect the flags accepted by the installed binary. [`cmd/olp/config_flags.go`](../../cmd/olp/config_flags.go) registers the command-line flags, generated environment variable names, and defaults, while [`cmd/olp/config.go`](../../cmd/olp/config.go) resolves file-backed values and validates the full configuration.

Credential secrets use mounted files. Database and Valkey URLs can be supplied inline or through their corresponding `_FILE` variables. OLP validates the complete configuration before binding any listener.

## Runtime variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `OLP_DATABASE_URL` | required | Primary PostgreSQL connection URL. |
| `OLP_DATABASE_URL_FILE` | unset | Mounted file holding the primary PostgreSQL URL, mutually exclusive with `OLP_DATABASE_URL`. |
| `OLP_DATABASE_PASSWORD_FILE` | unset | Mounted PostgreSQL password injected when `OLP_DATABASE_URL` omits a password and passfile and `PGPASSWORD` is unset. Compose sets this for its bundled database. |
| `OLP_DATABASE_PASSWORD_FILE_URL` | unset | Restricts `OLP_DATABASE_PASSWORD_FILE` injection to connections whose resolved host, port, database, and user match this URL. Connection options such as `sslmode` may differ, and all fallback hosts must match. |
| `OLP_DATABASE_READ_URL` | unset | Optional regional PostgreSQL read replica for runtime release and key or credential authority reads. Writes and historical secret reads stay on the primary, and replica delay consumes the 60-second authority budget. See [regional deployment](deployment.md#regional-read-replicas). |
| `OLP_DATABASE_READ_URL_FILE` | unset | Mounted file holding the read-replica URL, mutually exclusive with `OLP_DATABASE_READ_URL`. |
| `OLP_RUNTIME_ROLE` | unset | Existing PostgreSQL runtime login granted schema, table, and sequence access by `olp migrate` (same as `--runtime-role`). |
| `OLP_REGION` | unset | Deployment region name for regional rate and concurrency overrides, regional cost-reconciliation leadership, and provider locality. Use the same name across gateways and workers sharing a Valkey. See [regional fleets](deployment.md#regional-fleets). |
| `OLP_DATABASE_MAX_CONNECTIONS` | `20` | Pool size from 1 to 10000, excluding detached worker sessions. See [connection budget](deployment.md#production-example-and-connection-budget). |
| `OLP_VALKEY_URL` | optional for `all`, `gateway`, `control`, required for `worker` | Coordination Valkey `redis://` or `rediss://` URL for installation-scoped limits, hints, and streams. |
| `OLP_VALKEY_URL_FILE` | unset | Mounted file holding the coordination Valkey URL, mutually exclusive with `OLP_VALKEY_URL`. |
| `OLP_VALKEY_TLS_CA_FILE` | unset | PEM trust roots for coordination Valkey TLS. Requires `OLP_VALKEY_URL` with a `rediss://` URL. |
| `OLP_CACHE_VALKEY_URL` | unset | Dedicated evicting Valkey `redis://` or `rediss://` URL for opt-in response caching. Must point to a separate host or port from `OLP_VALKEY_URL`. |
| `OLP_CACHE_VALKEY_URL_FILE` | unset | Mounted file holding the response-cache Valkey URL, mutually exclusive with `OLP_CACHE_VALKEY_URL`. |
| `OLP_CACHE_VALKEY_TLS_CA_FILE` | unset | PEM trust roots for response-cache Valkey TLS. Requires `OLP_CACHE_VALKEY_URL` with a `rediss://` URL. |
| `OLP_LISTEN_ADDR` | `127.0.0.1:8080` | Public listener address and port. Containers override this to `0.0.0.0:8080`. |
| `OLP_OBSERVABILITY_LISTEN_ADDR` | `127.0.0.1:9090` | Private health and metrics listener address and port. |
| `OLP_OTLP_TRACES_ENDPOINT` | unset | Complete HTTP or HTTPS OTLP traces endpoint. Leaving it unset disables tracing. |
| `OLP_OTLP_HEADERS_FILE` | unset | Mounted JSON object of additional OTLP exporter headers, read only when tracing is enabled. |
| `OLP_TRACE_SAMPLE_RATIO` | `1.0` | Sampling ratio from `0.0` through `1.0` for locally rooted traces. |
| `OLP_TRACE_PROPAGATE_UPSTREAM` | `true` | Injects the current W3C trace context into provider attempts. |
| `OLP_TRACE_ACCEPT_INBOUND` | `true` | Accepts a valid inbound W3C trace context as the request parent. |
| `OLP_HTTP_MAX_CONNECTIONS` | `1024` | Admitted TCP connections per listener from 1 to 100000. |
| `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` | `256` | Inference work admission limit from 1 to 100000. Excess work queues when enabled or returns HTTP 503. |
| `OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS` | `32` | Management and console work admission limit from 1 to 100000. |
| `OLP_HTTP_ADMISSION_QUEUE_DEPTH` | `0` | Inference requests allowed to wait for a full pool from 0 to 100000, dequeued weighted fair by priority. Setting `0` returns HTTP 503 immediately. |
| `OLP_HTTP_ADMISSION_QUEUE_TIMEOUT` | `2s` | Longest wait in the admission queue from `1ms` to `1m`, never exceeding the route deadline. |
| `OLP_HTTP_MAX_IN_FLIGHT_SHADOW_REQUESTS` | `16` | Mirrored shadow attempts in flight per gateway from 1 to 100000. Excess mirrors are dropped and never queued. |
| `OLP_HTTP_CONNECTION_MAX_AGE_SECONDS` | `300` | Age in seconds from 1 to 86400 at which HTTP/2 connections receive GOAWAY. |
| `OLP_HTTP_CONNECTION_DRAIN_TIMEOUT_SECONDS` | `30` | Grace period in seconds from 1 to 600 for draining connections. |
| `OLP_PUBLIC_ORIGIN` | `http://127.0.0.1:8080` | Trusted browser origin for OIDC redirects, SAML, WebAuthn, and generated links. |
| `OLP_LOCAL_LOGIN_ENABLED` | `true` | Keeps local password sign-in available after first-owner setup. |
| `OLP_TRUSTED_PROXY_CIDRS` | empty | Comma-separated proxy CIDRs permitted to supply `X-Forwarded-For`. |
| `OLP_MANAGEMENT_ALLOWED_CIDRS` | empty | Up to 64 client CIDRs allowed to reach management endpoints and the console, resolved through trusted proxies. Empty allows all clients. See [Management network restrictions](../use/access.md#management-network-restrictions). |
| `OLP_GATEWAY_CORS_ALLOWED_ORIGINS` | empty | Exact browser origins allowed to call the inference gateway cross-origin. Wildcards are refused and the management API stays same-origin. |
| `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` | empty | Comma-separated CIDRs exempt from the non-public provider egress denylist. See [Provider egress policy](#provider-egress-policy). |
| `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` | empty | Comma-separated lowercase hostnames or IP literals whose provider endpoints may use plain HTTP. |
| `OLP_HTTP_MAX_JSON_BODY_BYTES` | `2097152` | Largest JSON request body before and after gzip inflation, from 64 KiB to 64 MiB. |
| `OLP_HTTP_MAX_MEDIA_BODY_BYTES` | `67108864` | Largest raw or multipart media request body, from 1 MiB to 1 GiB. See [Body size caps](#body-size-caps). |
| `OLP_HTTP_MAX_INLINE_MEDIA_ITEMS` | `4` | Inline base64 media items accepted per JSON request, from 1 to 64. |
| `OLP_HTTP_MAX_INLINE_MEDIA_ITEM_BYTES` | `1048576` | Decoded size cap for one inline media item, from 1 KiB to 64 MiB. |
| `OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES` | `2097152` | Decoded size cap for all inline media in one request, from 1 KiB to 64 MiB. |
| `OLP_PROVIDER_MAX_RESPONSE_BYTES` | `16777216` | Largest provider response body buffered for non-streaming operations, including a stream aggregated for a non-streaming caller, from 1 MiB to 256 MiB. |
| `OLP_PROVIDER_MAX_EVENT_BYTES` | `1048576` | Largest single streamed provider event, from 64 KiB up to `OLP_PROVIDER_MAX_RESPONSE_BYTES`. |
| `OLP_CONSOLE_DIR` | `console/build` | Directory containing built static console assets. |
| `OLP_MEDIA_SPOOL_DIR` | unset | On-disk media spool directory. Defaults to `olp-media-spool` inside the system temporary directory. |
| `OLP_MEDIA_SPOOL_CAPACITY_BYTES` | `1073741824` | On-disk spool capacity (default 1 GiB, minimum 256 MiB). The spool also caps uploads and pending cleanups at 4,096 files in total. |
| `OLP_CONNECTOR_CONFIG_FILE` | unset | Optional file-backed connector configuration path. |
| `OLP_UNCONFINED_PLUGIN_DIR` | unset | Experimental. Absolute directory inside the container image holding unconfined plugin executables. Setting this variable is the only way to enable the [unconfined plugin tier](../use/plugins.md#unconfined-plugins-experimental). |
| `OLP_LOG_LEVEL` | `info` | JSON log severity (`debug`, `info`, `warn`, or `error`). |
| `OLP_SHUTDOWN_TIMEOUT` | `30s` | Shared HTTP, metadata, delivery, and worker shutdown budget from `1ms` to `10m`. |
| `OLP_DEPENDENCY_REQUEST_TIMEOUT` | `2s` | Per-request dependency deadline from `1ms` to `1m`. |
| `OLP_STARTUP_TIMEOUT` | `10s` | Startup and ordinary maintenance deadline from `1ms` to `1m`. |
| `OLP_METRICS_TENANT_LABELS` | empty | Optional comma-separated business-metric tenant labels chosen from `project`, `key`, and `end_user`. Empty keeps only route, provider kind, direction, currency, phase, action, outcome, or result labels. |
| `OLP_METRICS_SERIES_CAP` | `5000` | Per-process cap on active business-metric series from 64 to 1000000. Overflow increments `olp_metrics_series_overflow_total`. |

### External secret and ambient variables

When resolving external secrets or workload-identity wrapped master keys from HashiCorp Vault (`internal/platform/secretstore/vault.go`), OLP reads four dedicated environment variables rather than storing Vault access tokens in database references.

| Variable | Default | Purpose |
| --- | --- | --- |
| `OLP_VAULT_ADDR` | unset | Trusted HashiCorp Vault `http://` or `https://` origin without userinfo, path, query, or fragment. Must match the scheme and host of any Vault secret reference before OLP opens the JWT file. |
| `OLP_VAULT_ROLE` | unset | Vault JWT or Kubernetes authentication role name (up to 128 characters). |
| `OLP_VAULT_JWT_FILE` | unset | Regular file with mode `0400`, `0440`, `0600`, or `0640` holding the projected workload identity JWT (up to 32 KiB). |
| `OLP_VAULT_AUTH_MOUNT` | `jwt` | Vault authentication mount segment used at `/v1/auth/<mount>/login`. |

OLP and its integrated cloud SDKs also observe a small set of standard ambient environment variables.

| Variable | Purpose |
| --- | --- |
| `PGPASSWORD` | When non-empty, suppresses `OLP_DATABASE_PASSWORD_FILE` injection and passes the password directly to PostgreSQL connections. |
| `HOSTNAME` | Identifies the gateway process instance and builds the Valkey stream consumer identity on workers. |
| `AWS_REGION`, `AWS_DEFAULT_REGION` | Fallback AWS region for S3 object-store export sinks when neither the sink credential nor the endpoint sets a region, alongside standard AWS SDK default credential chain behavior. |
| `GOOGLE_APPLICATION_CREDENTIALS` | Service-account JSON file path read by Google Application Default Credentials for Vertex AI connectors, Google billing imports, and Google Cloud Secret Manager. |
| `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID` | Selects Azure workload identity when `AZURE_FEDERATED_TOKEN_FILE` is set, or specifies a user-assigned managed identity client ID through `AZURE_CLIENT_ID` for Azure Key Vault secret resolution. |

At maximum connection age, the server stops admitting new requests on that connection and sends HTTP/2 GOAWAY. Existing streams have the configured drain interval to finish before the connection closes. SIGTERM stops admission and gives HTTP, metadata delivery, and workers one shared shutdown budget. Keep `OLP_SHUTDOWN_TIMEOUT` below the deployment termination grace period minus its pre-stop delay because forced termination can leave accounting completeness gaps.

The CLI loopback listener default is intentional, and Compose and Helm set their container listeners explicitly. Keep the observability listener private and list only trusted peers in `OLP_TRUSTED_PROXY_CIDRS`.

Tracing activates only when `OLP_OTLP_TRACES_ENDPOINT` is set. Supply the complete URL, such as `https://collector.example.com/v1/traces`, because OLP does not rewrite its path. Endpoint userinfo and fragments are rejected. Tracing exports spans only, while Prometheus metrics and JSON logs keep their own destinations.

Exporter credentials belong in `OLP_OTLP_HEADERS_FILE`, formatted as a UTF-8 JSON object of valid HTTP header names and values such as `{"x-scope-orgid":"tenant-a"}`. The file uses the same secret permissions described below. Inline `OTEL_EXPORTER_OTLP_TRACES_HEADERS` and `OTEL_EXPORTER_OTLP_HEADERS` are rejected when tracing is enabled. Invalid endpoints, ratios, or header files fail startup.

Inbound `traceparent` headers are accepted only when both tracing and inbound acceptance are enabled, and an invalid header starts a fresh local trace. Caller `tracestate` is discarded. Upstream propagation sends fresh span headers and never exporter credentials. Spans carry only allowlisted identifiers, classification, timing, usage, and pricing, with no prompts, outputs, tool payloads, raw headers, or provider error bodies. Only canonical lowercase hyphenated UUID `x-request-id` values enter the trace attribute. See [tracing operations](observability.md#distributed-tracing) for sampling, monitoring, and local Jaeger exploration.

Request and attempt spans also record OpenTelemetry GenAI semantic convention keys pinned at schema `1.41.0` (`gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens`, and `gen_ai.usage.output_tokens`), a convention set still in development upstream. `gen_ai.request.model` records the route slug named by the caller, while `gen_ai.response.model`, finish reasons, and token counts appear only when reported by the provider. Provider and operation names use the conventions' well-known values where a provider kind or surface maps to one (`openai`, `anthropic`, `gcp.gemini`, `gcp.vertex_ai`, `aws.bedrock`, `azure.ai.openai`, `ibm.watsonx.ai`, and operations `chat`, `generate_content`, `text_completion`, `embeddings`), falling back to OLP's own names otherwise. Prompt, output, and tool payload capture stays out of spans.

All HTTP modes (`all`, `gateway`, and `control`) require PostgreSQL and the authentication HMAC key. Configure Valkey for production because without it runtime hints fall back to polling and hard-limited keys fail closed. `worker` requires Valkey and exposes only the private observability listener, while `migrate` and `doctor` bind no listener. `olp doctor` verifies Valkey connectivity whenever `OLP_VALKEY_URL` is configured. See [process modes](../develop/runtime.md#process-modes) for worker and connector requirements.

## File-based secrets

Secret files must be regular files with mode `0400`, `0440`, `0600`, or `0640`. Group-write and world permissions are rejected. Workers require both the HMAC and master keys even when using mounted connectors. Every command refuses to start if `OLP_AUTH_HMAC_KEY`, `OLP_MASTER_KEY`, or `OLP_BOOTSTRAP_TOKEN` is supplied inline.

| Variable | Required by | File contents |
| --- | --- | --- |
| `OLP_MASTER_KEY_FILE` | `all`, `control`, `worker`, any `gateway` reading database-encrypted credentials, `doctor`, `master-key` | JSON master-key ring shown below. Also supports [workload-identity wrapped keys](external-secrets.md). |
| `OLP_AUTH_HMAC_KEY_FILE` | `all`, `gateway`, `control`, `worker`, `doctor`, `master-key` | 32 random bytes encoded as hexadecimal or standard base64. |
| `OLP_BOOTSTRAP_TOKEN_FILE` | First-owner setup on `all` or `control` | Random 32 to 256 byte token for one-time owner setup. Read on every setup attempt so it can be mounted after startup or removed after setup. A missing, unreadable, or wrongly sized file makes setup report that no token is configured. The final path component must be a regular file, so projected Kubernetes Secrets should point under `..data`. |
| `OLP_OTLP_HEADERS_FILE` | Traced `all`, `gateway`, `control`, or `worker` | Optional JSON object of OTLP exporter headers. |

```json
{
  "active_version": 1,
  "keys": [
    { "version": 1, "key": "<32 random bytes encoded as hex or base64>" }
  ]
}
```

A master-key ring holds from 1 to 32 distinct positive versions. Keep the authentication HMAC key stable because its installation fingerprint prevents accidental replacement and replacing it invalidates stored API-key and bootstrap-token digests. Preserve both key files for recovery and follow the [key rotation procedure](operations.md#master-key-rotation-and-recovery) before retiring any master-key version.

Compose generates these files on first start as described in [Getting Started](../getting-started.md#install-olp). For local scripts, `source scripts/secrets.sh <private-directory>` creates missing secret files without printing their contents, and `olp init-secrets --dir DIR --database-dir DIR` creates the same set as Compose without overwriting existing files. Production secret distribution remains the operator's responsibility. Database and Valkey URLs also accept `_FILE` variants that are mutually exclusive with their inline settings.

## Compose-only variables

Compose reads `OLP_IMAGE`, defaulting to the versioned release image used by the quick start, and `OLP_HOST_PORT`, which also determines the default `OLP_PUBLIC_ORIGIN`. These settings configure the Compose wrapper rather than the OLP binary. Compose supplies the generated password for its bundled database URL, while a custom `OLP_DATABASE_URL` provides its own credentials such as a `passfile` query parameter referencing a mounted PostgreSQL password file. The tracing overlay `deploy/compose.tracing.yaml` reads `OLP_TRACE_SAMPLE_RATIO`, `OLP_TRACE_PROPAGATE_UPSTREAM`, and `OLP_TRACE_ACCEPT_INBOUND` from `.env`, and leaving them empty preserves the binary defaults.

## OpenAI-compatible provider presets

The release-owned wizard catalog resolves a reviewed HTTPS endpoint and `api_key` authentication into ordinary `openai_compatible` fields. Self-hosted runtimes (`tgi`, `vllm`, `ollama`, `lmstudio`, `llamacpp`, `infinity`, and `docker-model-runner`) are the exception and start unauthenticated at a placeholder endpoint that the operator replaces with their server address. Account-scoped platforms (`azure-document-intelligence`, `databricks`, `snowflake-cortex`, and `cloudflare-workers-ai`) also start with a placeholder endpoint whose host or path identifies the operator's account. The saved provider record keeps the vendor ID separately from its editable connection fields.

Each preset encodes a reviewed vendor contract in [`internal/domain/vendors`](../../internal/domain/vendors/catalog.go), covering supported operations, generation dialects, request fields the vendor refuses or renames, and the upstream documentation reviewed. Presets whose vendors implement Chat Completions faithfully select the `compatible-chat` profile so they can serve strict routes, while specialized search, OCR, decision, and Cohere v2 presets pin their dedicated profiles. Choose **Automatic** to fall back to generic parameter defaults instead.

| ID | Provider | Endpoint | Profile |
| --- | --- | --- | --- |
| `typesafe-jev` | TypeSafe Jev | `https://api.typesafe.ai/v1` | `systemone` |
| `groq` | Groq | `https://api.groq.com/openai/v1` | `compatible-chat` |
| `mistral` | Mistral | `https://api.mistral.ai/v1` | |
| `openrouter` | OpenRouter | `https://openrouter.ai/api/v1` | `compatible-chat` |
| `together` | Together AI | `https://api.together.ai/v1` | |
| `tgi` | Text Generation Inference | `https://tgi.example.internal/v1` (placeholder) | |
| `vllm` | vLLM | `https://vllm.example.internal/v1` (placeholder) | |
| `deepseek` | DeepSeek | `https://api.deepseek.com` | |
| `fireworks` | Fireworks | `https://api.fireworks.ai/inference/v1` | `compatible-chat` |
| `deepinfra` | DeepInfra | `https://api.deepinfra.com/v1/openai` | |
| `huggingface` | Hugging Face | `https://router.huggingface.co/v1` | |
| `cohere` | Cohere | `https://api.cohere.ai/compatibility/v1` | |
| `cohere-native-v2` | Cohere native v2 | `https://api.cohere.ai/v2` | `cohere-v2` |
| `jina` | Jina AI | `https://api.jina.ai/v1` | |
| `elevenlabs` | ElevenLabs | `https://api.elevenlabs.io/v1` | |
| `deepgram` | Deepgram | `https://api.deepgram.com/v1` | |
| `assemblyai` | AssemblyAI | `https://api.assemblyai.com` | |
| `runway` | Runway | `https://api.dev.runwayml.com/v1` | |
| `stability` | Stability AI | `https://api.stability.ai` | |
| `recraft` | Recraft | `https://external.api.recraft.ai/v1` | |
| `bfl` | Black Forest Labs | `https://api.bfl.ai/v1` | |
| `voyage` | Voyage AI | `https://api.voyageai.com/v1` | |
| `perplexity` | Perplexity | `https://api.perplexity.ai` | `perplexity-search` |
| `tavily` | Tavily | `https://api.tavily.com` | `tavily-search` |
| `exa` | Exa | `https://api.exa.ai` | `exa-search` |
| `parallel` | Parallel | `https://api.parallel.ai/v1` | `parallel-search` |
| `brave` | Brave | `https://api.search.brave.com` | `brave-search` |
| `azure-document-intelligence` | Azure Document Intelligence | `https://your-resource.cognitiveservices.azure.com` (placeholder) | `azure-document-intelligence-ocr` |
| `xai` | xAI | `https://api.x.ai/v1` | `compatible-chat` |
| `cerebras` | Cerebras | `https://api.cerebras.ai/v1` | `compatible-chat` |
| `sambanova` | SambaNova | `https://api.sambanova.ai/v1` | `compatible-chat` |
| `nebius` | Nebius Token Factory | `https://api.tokenfactory.nebius.com/v1` | `compatible-chat` |
| `novita` | Novita AI | `https://api.novita.ai/openai/v1` | |
| `nvidia-nim` | NVIDIA NIM | `https://integrate.api.nvidia.com/v1` | |
| `featherless` | Featherless | `https://api.featherless.ai/v1` | |
| `baseten` | Baseten | `https://inference.baseten.co/v1` | |
| `moonshot` | Moonshot AI | `https://api.moonshot.ai/v1` | `compatible-chat` |
| `moonshot-cn` | Moonshot AI (China) | `https://api.moonshot.cn/v1` | `compatible-chat` |
| `dashscope` | Alibaba Cloud Model Studio | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | `compatible-chat` |
| `dashscope-cn` | Alibaba Cloud Model Studio (China) | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `compatible-chat` |
| `zai` | Z.ai | `https://api.z.ai/api/paas/v4` | |
| `zhipu` | Zhipu BigModel | `https://open.bigmodel.cn/api/paas/v4` | |
| `minimax` | MiniMax | `https://api.minimax.io/v1` | `compatible-chat` |
| `minimax-cn` | MiniMax (China) | `https://api.minimax.cn/v1` | `compatible-chat` |
| `volcengine-ark` | Volcengine Ark | `https://ark.cn-beijing.volces.com/api/v3` | `compatible-chat` |
| `byteplus-modelark` | BytePlus ModelArk | `https://ark.ap-southeast.bytepluses.com/api/v3` | `compatible-chat` |
| `scaleway` | Scaleway Generative APIs | `https://api.scaleway.ai/v1` | `compatible-chat` |
| `ovhcloud` | OVHcloud AI Endpoints | `https://oai.endpoints.kepler.ai.cloud.ovh.net/v1` | `compatible-chat` |
| `nscale` | Nscale | `https://inference.api.nscale.com/v1` | `compatible-chat` |
| `databricks` | Databricks | `https://your-workspace.cloud.databricks.com/ai-gateway/mlflow/v1` (placeholder) | |
| `snowflake-cortex` | Snowflake Cortex | `https://your-account.snowflakecomputing.com/api/v2/cortex/v1` (placeholder) | |
| `cloudflare-workers-ai` | Cloudflare Workers AI | `https://api.cloudflare.com/client/v4/accounts/your-account-id/ai/v1` (placeholder) | |
| `vercel-ai-gateway` | Vercel AI Gateway | `https://ai-gateway.vercel.sh/v1` | |
| `ollama` | Ollama | `https://ollama.example.internal/v1` (placeholder) | |
| `lmstudio` | LM Studio | `https://lmstudio.example.internal/v1` (placeholder) | |
| `llamacpp` | llama.cpp server | `https://llamacpp.example.internal/v1` (placeholder) | |
| `infinity` | Infinity | `https://infinity.example.internal` (placeholder) | |
| `docker-model-runner` | Docker Model Runner | `https://model-runner.example.internal/engines/v1` (placeholder) | |

Selecting a preset does not certify a provider or model. Creation and edits still enforce HTTPS, public-egress, SSRF, and reachability checks unless the destination is exempted by the egress allowlists below, and only live exact-tuple certification makes a capability eligible for activation. Use **Custom endpoint** for other compatible services, including explicitly configured private HTTP and unauthenticated endpoints. See [provider routing](../use/provider-routing.md) for encrypted custom headers, defaults, model facts, credential pools, quotas, and routing policies.

## Body size caps

OLP validates the JSON, media, and inline-media caps together at startup. An inline media item must fit inside the inline total, both stay within 64 MiB, and `OLP_HTTP_MAX_MEDIA_BODY_BYTES` must not exceed half of `OLP_MEDIA_SPOOL_CAPACITY_BYTES`. Multipart admission reserves half of the spool for untrusted parsers, so a larger media cap would make every multipart request return HTTP 503. Raise `OLP_MEDIA_SPOOL_CAPACITY_BYTES` and its backing volume before raising `OLP_HTTP_MAX_MEDIA_BODY_BYTES`. Header count and size caps remain fixed.

Multipart upload admission reserves `min(OLP_HTTP_MAX_MEDIA_BODY_BYTES, maxFileBytes * maxFiles)` from the spool budget before parsing (`internal/gateway/kernel/file_multipart.go`), bounded by fixed per-file upload limits on each endpoint.

| Endpoint | Per-file limit and max files | Reservation formula |
| --- | --- | --- |
| `POST /v1/images/edits` | 50 MiB, up to 33 files | `min(OLP_HTTP_MAX_MEDIA_BODY_BYTES, 50 MiB * 33)` |
| `POST /v1/images/variations` | 50 MiB, 1 file | `min(OLP_HTTP_MAX_MEDIA_BODY_BYTES, 50 MiB)` |
| `POST /v1/audio/transcriptions`, `POST /v1/audio/translations` | 25 MiB, 1 file | `min(OLP_HTTP_MAX_MEDIA_BODY_BYTES, 25 MiB)` |
| `POST /v1/videos` | 20 MiB reference, 1 file | `min(OLP_HTTP_MAX_MEDIA_BODY_BYTES, 20 MiB)` |
| `POST /v1/files`, `POST /anthropic/v1/files` | Full media body cap, 1 file | `OLP_HTTP_MAX_MEDIA_BODY_BYTES` |

`OLP_PROVIDER_MAX_RESPONSE_BYTES` and `OLP_PROVIDER_MAX_EVENT_BYTES` apply across every gateway connector kind (OpenAI, OpenAI-compatible, Anthropic, Gemini, Azure OpenAI, Azure AI Search, Vertex AI, Amazon Bedrock, Amazon SageMaker AI, IBM watsonx.ai, and plugin connectors). The response cap also bounds buffered media responses, staged file and batch payloads, retained response and interaction bodies, and streamed events aggregated for a non-streaming caller.

## Provider egress policy

Provider endpoints must be absolute HTTPS URLs without credentials, query strings, or fragments, and they must resolve exclusively to public addresses. Literal hosts are verified before DNS, and every address in each DNS answer is verified again before a pinned client is built and on every revalidation. Two allowlists widen that policy for private or on-premises upstreams such as a VPC-hosted vLLM server or an Azure private endpoint. Both default to empty, preserving public-only HTTPS enforcement.

`OLP_PROVIDER_EGRESS_ALLOW_CIDRS` lists comma-separated CIDR ranges (for example `10.0.0.0/8,fd00::/8`) exempt from the non-public address denylist. The exemption covers literal IP hosts and every resolved DNS address. An answer set mixing allowlisted and denied addresses still fails closed, and a later DNS rebind outside the allowlist fails closed as well.

`OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` lists comma-separated exact lowercase hostnames or IP literals (for example `vllm.internal,10.1.2.3`) allowed to use `http://`. The scheme check is host-keyed because it runs synchronously before DNS during every management write.

A plain-HTTP endpoint on a private literal IP needs both settings, with the host listed in `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` and the IP covered by `OLP_PROVIDER_EGRESS_ALLOW_CIDRS`. The `all`, `gateway`, `control`, `worker`, `doctor`, and `master-key` commands accept both allowlists, and `all`, `gateway`, `control`, and `worker` log a startup warning naming both lists whenever either is non-empty. Transports refuse redirects, require TLS 1.2 or newer, bound dial and handshake timeouts, and cap response headers at 32 KiB. Health probes and inference traffic share the same normalized endpoint and egress policy. These allowlists never relax OIDC issuer or Vertex token endpoint checks. Because egress exceptions are installation-wide administrative trust decisions rather than tenant isolation boundaries, review the [egress](../../internal/platform/egress) package rules carefully before widening them.

## Operations script variables

No environment variable relaxes OIDC transport checks. OIDC endpoints must use HTTPS and loopback issuers are never accepted.

For local manual checks, point providers at loopback mock upstreams through the standard egress allowlists (`OLP_PROVIDER_EGRESS_ALLOW_CIDRS=127.0.0.0/8,::1/128` and `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS=127.0.0.1,localhost`) rather than relying on any compiled-in escape hatch.

Operations script variables are intentionally kept separate from server runtime settings. The `OLP_BACKUP_*`, `OLP_RESTORE_*`, and `OLP_PSQL` families support backup and restore scripts. See [`CONTRIBUTING.md`](../../CONTRIBUTING.md) and [Operations](operations.md) for command-specific requirements.

## Mounted connectors

`OLP_CONNECTOR_CONFIG_FILE` loads the `providers` list illustrated in [`deploy/connectors.example.json`](../../deploy/connectors.example.json). Each entry identifies a provider, uses the same nested `configuration` object as the management API, and points to an optional `credential_file`. Credential files require restricted permissions and multiline credentials must be valid JSON, while ADC and the AWS default chain reject stored credentials. A provider with a profile, including a [plugin provider](../use/plugins.md#providers-from-plugin-profiles) and its static credential, mounts only secret material so its `configuration` must match the published revision. A provider authenticating through a [grant](../use/plugins.md#grant-enrollment) cannot be mounted because its grant lives under a database credential version that requires the master key.

Without `OLP_MASTER_KEY_FILE`, each mounted connector serves the published default credential slot and enforces its slot and connection limits. Releases containing enabled named slots require the master key so each attempt can load its published credential version. Database-encrypted revisions remain authoritative whenever the master key is configured.

## Installation settings

Runtime installation policies stored in PostgreSQL are managed through `GET /api/v1/settings`, `GET /api/v1/settings/{key}`, and `PUT /api/v1/settings/{key}` (`internal/settings/settings.go`). Updates require `If-Match` with the current `ETag`, an `Idempotency-Key`, and `settings` authority (plus extra authority for authentication controls noted below), and record a `setting.update` audit event.

| Key | Default | Allowed values and behavior |
| --- | --- | --- |
| `retention.requests_days` | `30` | Integer from `1` to `3650`. Controls how many days OLP retains request history, ingested trace spans, agent runs, and CloudEvents metering receipts. |
| `retention.usage_days` | `90` | Integer from `1` to `3650`. Controls how many days OLP retains hourly usage rollups, batch settlement evidence, and imported provider billing totals. |
| `retention.audit_days` | `365` | Integer from `1` to `3650`. Controls how many days OLP retains management audit log entries. |
| `guardrails.stream_window_bytes` | `65536` | Integer from `256` to `1048576` (1 MiB). Sets the streaming guardrail inspection window and immediately publishes a new runtime release. Rejected if incompatible with published guardrails. |
| `budgets.time_zone` | `UTC` | Valid IANA time zone name. Schedules day, week, and month calendar boundary transitions without resetting in-progress windows. Reads include a computed `calendar` status object. |
| `limits.valkey_unavailable` | `fail_closed` | `fail_closed` or `fail_open`. `fail_closed` rejects requests when configured Valkey limits cannot be enforced, while `fail_open` lets rate-limited and concurrency-limited keys proceed without a Valkey lease during an outage. Key and group cost budgets always fail closed. See [limit enforcement](../develop/accounting.md#limits-and-budgets). |
| `auth.local_login_enabled` | `true` | `true` or `false`. Additionally requires `local_login` authority and verifies that at least one usable installation owner remains able to sign in. |
| `auth.mfa_required` | `false` | `true` or `false`. Additionally requires `access` authority. Requires enrolled local MFA factors for password sign-in. |

## Configuration promotion artifacts

Vertex `vertex-batch-prediction` and Bedrock `bedrock-batch-inference` providers require the nonsecret `batch_storage` object carrying `input_uri_prefix`, `output_uri_prefix`, and for Bedrock `role_arn`. Prefixes are validated, normalized with a trailing slash, and exported or promoted alongside provider configuration without carrying credentials or object contents.

Export, plan, and apply operate on nonsecret desired state (`openllmproxy.dev/config/v2`) rather than database backups. Resources match by natural names, with project and provider names matched case-insensitively and routes, models, and credential references matched exact-case. The canonical SHA-256 digest blanks `exported_at` and sorts collections. Export writes active providers (or drafts when inactive), latest published route revisions including retired ones, and effective prices.

| Section | Contents promoted |
| --- | --- |
| `budget_time_zone`, `installation_budget`, `require_local_mfa` | Installation budget calendar time zone, installation spend cap, and local MFA requirement. Omission or `null` preserves the destination setting. |
| `organizations` | Portable organization names and organization budgets. User memberships stay local to each installation. |
| `projects` | Project definitions, project budgets, budget groups, project catalog policies, and key limit templates. |
| `saml`, `scim_group_mappings`, `workload_issuers` | SAML IdP and SP settings, SCIM group-to-role and project grants, and workload identity issuers. Containing any of these sections requires installation `access` authority even on replay or unchanged plans. |
| `providers` | Provider connections, profile pins, options and commitments, per-connection network and TLS intent, credential slot references, and `billing_import` entries. |
| `routes`, `templates` | Latest published route revisions (including retired routes, route groups, `cache`, and `prompt_cache` policies) and route templates. |
| `pricing`, `discounts`, `rate_cards` | Effective pricing entries, in-force discount multipliers, and in-force consumer rate card revisions. |
| `sinks` | Managed HTTPS export sinks (`/api/v1/sinks`) carrying scope, name, destination, streams, enabled flag, and opaque `credential_ref`. |
| `guardrails`, `guardrail_policies`, `guardrail_attachments` | Typed guardrail definitions and credential references, ordered guardrail policies, and additive guardrail and toolset attachments. |
| `mcp_servers` | MCP server definitions, upstream endpoints, and bound credentials. |
| `retrieval_bindings` | Project-owned cloud retrieval bindings for Bedrock Knowledge Bases, Vertex RAG corpora, and Azure AI Search indexes. |
| `agent_publications` | Published agent definitions and bindings. |
| `code` | Coding subscription accounts, shared and personal pools, coding route drafts and latest publications, and local token budget policies. |

Promotion artifacts never export API keys, users, tokens, usage, audit rows, media records, certification state, runtime IDs, or secret values. Plugin digests and options are portable, whereas plugin modules, grant tokens, observed principals, and runtime facts remain local. Credential authority uses stable `provider-name/slot-name-or-default` references, or `null` for credentialless modes. Slot key restrictions export empty and imports reject non-empty lists while preserving existing destination restrictions so operators re-establish local key bindings on the destination.

| Operation | Behavior |
| --- | --- |
| `GET /api/v1/configuration/export` | Returns the `openllmproxy.dev/config/v2` artifact and its canonical SHA-256 digest. |
| `POST /api/v1/configuration/plan` | Validates the artifact and returns the digest, planned actions, conflicts, and blockers without modifying state. |
| `POST /api/v1/configuration/apply` | Requires `Idempotency-Key` and stages changes inside one installation-serialized transaction, rolling back on any conflict or blocker. |

All three endpoints require `configure` and all-project authority. Global `read` and `configure` machine tokens can automate promotion, whereas project-scoped tokens cannot. Changing or including feature-specific sections additionally requires the feature's declared operation permissions (such as `access`, `settings`, `keys`, or `manage_organization`) even when definitions are unchanged.

Validation rejects unknown fields, oversized collections, duplicate identities, cross-project targets, undeclared bindings, and secret values larger than 64 KiB. Omitted, `null`, or empty fidelity defaults to `strict` and is never inherited, and redacting under strict mode reports `fidelity_policy_conflict`. Artifacts never bypass individual feature validation checks.

Plugin builds referenced by an artifact must be installed and approved (or unconfined plugins enabled and permitted), otherwise planning blocks. Static secrets bind through write-only `secret_bindings`, and grant slots require same-build destination enrollment reported as an `enroll` action. Binding values are neither echoed nor replayed in plaintext, and the idempotency fingerprint stores only digest, names, and value hashes.

Apply delegates provider validation, credential boundaries, and certification rules to each feature owner, preserves unchanged grant lifetimes, and advances collection and parent ETags. Missing projects assign the applying actor as manager. Imported providers stage as dirty drafts, kind changes conflict, and imported capabilities stay declared until certified. Imported routes stage as non-activated drafts, cross-project slug conflicts and lifecycle differences require explicit activation or retirement, and the active runtime generation stays pinned.

Route template selectors using `provider:<uuid>` must match a provider in the template's destination project, or an installation-wide provider for an installation-wide template. When promoting across installations, replace source provider UUIDs with destination UUIDs before planning because a missing provider or project mismatch blocks apply. Vendor selectors promote unchanged.

Unchanged price entries are no-ops, while differing prices create a new revision and republish. Future effective times survive promotion, and past or current effective times rebase to the apply instant.

Top-level `discounts` carries each scope's in-force multiplier, including multiplier `1` scope endings (`provider_kind`, optional `vendor_id`, and `provider` name). Apply publishes a now-effective discount whenever a listed scope's multiplier differs, and listing multiplier `"1"` ends that scope's discount while omitted scopes stay untouched. Top-level `rate_cards` carries each card's in-force revision including neutral revisions (`owner` with `kind` set to `installation`, `organization`, or `project`, plus `markup` and `prices` without provider scope). Apply creates missing cards and publishes a new revision when a listed card differs. Publishing `markup` `"1"` with no prices neutralizes a listed card, and omitted cards remain untouched. Connection commitments travel inside provider `options`, and each budget's `cost_basis` travels inside its `budget_policy`. A provider's `billing_import` entry (`enabled`, `scope`, and `credential_ref` as `<provider>/billing`) promotes alongside the provider, binding its source-specific credential through `secret_bindings` and blocking apply if unbound. Apply decrypts stored billing credentials internally to detect changes without returning secret values to clients, and omitting an existing `billing_import` deletes the import and its sealed credential. See [FinOps](finops.md#billing-imports) for credential formats. Feature-specific budget, group, and calendar omission semantics live in [access](../use/access.md) rather than a single global merge rule. Passing `expected_digest` fences destination drift, remaining blockers return HTTP 409 with the plan body, and successful staging writes one digest-only audit event.

The **Import & export** tab under Settings clears entered secrets whenever the artifact changes. CLI [reviewed promotion](automation.md#configuration-promotion), [GitHub Actions](automation.md#github-actions), and [Terraform](automation.md#terraform-and-opentofu) preserve review and atomicity boundaries.

### Versioned provider profiles and per-connection networking

Provider drafts pin profile IDs and revisions, parameter defaults, model bindings, semantic headers, and proxy or TLS intent. Network secrets use separately sealed provider references. See [profiles](../use/provider-profiles.md) for public shapes and credential destination rules. Omitting a profile selects Automatic kind-derived endpoints and transformed routes.

Guardrail configuration promotes through typed `guardrails` definitions, ordered `guardrail_policies`, and additive `guardrail_attachments`. See [Guardrails](../use/guardrails.md) for lifecycles, portable references, and examples.

### Response-cache intent

Route `cache` and `prompt_cache` policies round-trip through export, plan, and apply and publish inside the immutable runtime snapshot. Cache Valkey URLs and credentials remain environment bindings outside promotion artifacts, so restoring configuration into an empty installation restores cache policy without cached responses. See [response caching](../use/caching.md).

### Coding configuration

The optional `code` section carries subscription accounts, shared and personal pools, coding route drafts and latest publications, and local token budget policies. It preserves account weights, model aliases and model pools, compatibility choices, recovery policy, research tools, request limits, and paid participation. A publication retains its frozen provider configuration and paid model and slot policy independently from the current provider revision. Historical revisions, conversations, allowance observations, credentials, and budget usage stay on their installation.

Enroll or configure destination providers before promoting coding configuration. Supply `code_bindings` on plan and apply, or pass `--code-bindings-file` to the CLI. Its `credentials`, `users`, and `api_keys` maps bind artifact references to explicitly authorized destination UUIDs. Subscription principals come from enrolled destination credentials, paid credentials must match the destination provider slot's active published credential, and frozen paid slot key restrictions require explicit destination key bindings. Frozen paid provider and slot limits must match the destination's live limits or planning and apply return HTTP 422. Paid fallback preserves its separate model targets and explicit recovery limit, runtime attempt links stay local, and personal owners must be active project members whose assigned keys belong to them.

Code UUIDs identify references inside the artifact. Destination accounts match project, provider, and credential principal. Pools and budgets first reuse same-project artifact references or a UUIDv5 destination identity derived from the destination project, resource kind, and artifact reference, preserving duplicate pool names and duplicate budget policies without an external registry. A unique natural match can reuse a pool by project, name, kind, and owner, or a budget by project, route, key, and the presence of daily and monthly windows. Routes match project and slug. Unrelated ambiguous identities and unresolved or cross-project references are refused, existing budget windows keep their usage, and route slugs must be renamed when copying into another project on the same installation because slugs are globally unique.

Plan and apply reuse the standard configuration endpoints, transactions, and live management authorization. Reapplying an equivalent artifact yields `noop` actions. Changing a CLI binding file after planning requires a fresh plan, and the console clears its reviewed plan whenever code bindings change. Coding publication validates both the frozen policy and the destination's current provider eligibility without requiring certification or live inference.
