# Google Vertex AI connector

The `vertex_ai` connector kind serves Google Cloud publisher models, Anthropic and OpenAI-compatible models hosted on Vertex AI, Vertex RAG Engine retrieval, Vertex AI Live WebSocket sessions, Vertex batch prediction, Vertex supervised fine-tuning, and Google Cloud Document AI OCR. All Vertex connections authenticate with Google Cloud OAuth 2.0 bearer tokens scoped to `https://www.googleapis.com/auth/cloud-platform`. Because Vertex AI has no model-list endpoint that proves catalog completeness, operators declare models on the connection and OLP certifies each declared model directly.

See [provider profiles](../provider-profiles.md) for how explicit profiles bind hosting and dialects, [provider routing](../provider-routing.md) for route configuration, and the [compatibility matrix](../compatibility.md) for supported operations across surfaces.

## Configuration and addressing

A `vertex_ai` connection requires `cloud_project` and `cloud_region`, both lowercase identifiers matching `^[a-z0-9][a-z0-9-]{0,127}$`. Do not set `deployment` or `api_version`, because Vertex encodes deployment and API version details inside the native endpoint path.

When `endpoint` is omitted, OLP derives the default URL from the selected profile, `cloud_project`, and `cloud_region`. Host selection follows Google Cloud's location rules. A `global` location uses `aiplatform.googleapis.com` for Vertex AI services or `documentai.googleapis.com` for Document AI. The multi-region locations `us` and `eu` on Vertex AI use `aiplatform.us.rep.googleapis.com` and `aiplatform.eu.rep.googleapis.com`. All other locations use `{region}-aiplatform.googleapis.com` for Vertex AI or `{region}-documentai.googleapis.com` for Document AI.

Vertex credentials are bound strictly to Google's endpoint for the configured location. Every Vertex connection must use HTTPS or WSS on the default port 443 against the derived Google hostname for its `cloud_region`, and must use the system trust roots without a custom `network.trust_roots_pem` override. Every explicit Vertex profile also requires the endpoint path to match `/v1/projects/{project}/locations/{region}` followed by the profile's required path suffix.

| Field | Requirement | Meaning |
| --- | --- | --- |
| `cloud_project` | Required | Google Cloud project ID that owns and bills calls. |
| `cloud_region` | Required | Google Cloud location such as `us-central1`, `europe-west4`, `us`, `eu`, or `global`. |
| `endpoint` | Optional | Derived automatically from the profile, project, and location. Must match the derived Google hostname and profile path. |
| `batch_storage` | Required for `vertex-batch-prediction` | Object containing `gs://` `input_uri_prefix` and `output_uri_prefix`, each at most 1024 bytes. Must omit `role_arn`. |
| `options.tuning_dataset_uri_prefixes` | Optional | Up to 32 `gs://` URI prefixes allowed for Vertex supervised tuning datasets, normalized and sorted on save. |
| `options.document_uri_prefixes` | Optional | Up to 32 `gs://` URI prefixes allowed for Document AI `gcsDocument.gcsUri` inputs, normalized and sorted on save. |
| `options.semantic_headers` | Optional | Profile-allowed static headers (for example `Anthropic-Beta` on `vertex-anthropic`), at most 16 entries and 2048 bytes per value. |
| `options.query_settings` | Optional | Profile-allowed query settings (`$xgafv` set to `"1"` or `"2"` on profiles that declare it). |

Model identifiers follow the hosting profile's grammar. Publisher models on Automatic, `vertex-gemini`, `vertex-anthropic`, `vertex-batch-prediction`, and `vertex-realtime` match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` after stripping any leading `models/` prefix and cannot end with a dot. The `vertex-openai` profile requires `publisher/model` identifiers matching `^[a-z0-9][a-z0-9-]{0,62}/[A-Za-z0-9][A-Za-z0-9._@:-]{0,127}$`, such as `google/gemini-3.8-flash` or `meta/llama-4-scout-17b-16e-instruct-maas`. The `vertex-rag-engine` profile requires full RAG corpus resource names inside the connection's project and location.

## Profiles

An unprofiled (Automatic) `vertex_ai` connection targets `/v1/projects/{project}/locations/{region}/publishers/google` and serves transformed routes for Gemini generation, token counting, embeddings, media, and supervised fine-tuning. Explicit versioned profiles (revision `1`) unlock strict native routes and dedicated cloud APIs.

| Profile ID | Dialect and revision | Path suffix under location | Certified operations | Semantic headers and query settings |
| --- | --- | --- | --- | --- |
| Automatic (omitted) | Gemini `v1` default | `/publishers/google` | `generation`, `token_count`, `embeddings`, `fine_tuning`, `image_generation`, `speech`, `transcription` | None |
| `vertex-gemini` | `gemini-generate-content` (`v1`) | `/publishers/google` | `generation`, `token_count`, `embeddings`, `fine_tuning`, `image_generation` | Query `$xgafv` |
| `vertex-anthropic` | `anthropic-messages` (`vertex-2023-10-16`) | `/publishers/anthropic` | `generation` | Header `Anthropic-Beta`, query `$xgafv` |
| `vertex-openai` | `openai-chat` (`v1`) | `/endpoints/openapi` | `generation` | None |
| `vertex-rag-engine` | `vertex-rag-engine` (`v1`) | None (location root) | `retrieval` | None |
| `vertex-realtime` | `vertex-realtime` (`v1`) | None (WebSocket host) | `realtime` | None |
| `vertex-document-processing-ocr` | `vertex-document-processing-ocr` (`v1`) | None (Document AI location root) | `ocr` | None |
| `vertex-batch-prediction` | `vertex-batch-prediction` (`v1`) | `/publishers/google` | `batch` | Query `$xgafv` |

### Google publisher (`vertex-gemini` and Automatic)

The Google publisher hosting calls `/models/{model}:generateContent` for unary generation, `/models/{model}:streamGenerateContent?alt=sse` for streaming, `/models/{model}:countTokens` for token counting, and `/models/{model}:predict` for Vertex text embeddings (`vertex-embeddings`). Because Vertex AI offers a costless `countTokens` endpoint on Google publisher models, the default connection test probes `token_count` rather than billable generation whenever the selected profile supports it.

### Anthropic publisher (`vertex-anthropic`)

The `vertex-anthropic` profile serves Claude models on Vertex AI (for example `claude-sonnet-5-5`) at `/publishers/anthropic/models/{model}:rawPredict` for unary calls and `:streamRawPredict` for streaming calls. Because Vertex expects the model in the URL and `anthropic_version` inside the JSON body, OLP removes `model` from the outgoing body and injects `"anthropic_version": "vertex-2023-10-16"`, refusing requests that carry a conflicting `anthropic_version`.

On strict Anthropic ingress, OLP maps an inbound `Anthropic-Version: 2023-06-01` header to the `vertex-2023-10-16` hosting revision and refuses unknown revisions. The profile declares `Anthropic-Beta` as an allowed semantic header. Callers on native Anthropic routes can forward `Anthropic-Beta` per request, or operators can pin it in `options.semantic_headers`. If a caller sends an `Anthropic-Beta` value that disagrees with the profile's configured value, OLP refuses the request rather than silently overriding either side.

### OpenAI-compatible and MaaS models (`vertex-openai`)

The `vertex-openai` profile targets `/v1/projects/{project}/locations/{region}/endpoints/openapi/chat/completions` using the `openai-chat` dialect. Use this profile for OpenAI-compatible Google publisher models such as `google/gemini-3.8-flash` and Vertex Model-as-a-Service (MaaS) partner models such as `meta/llama-4-scout-17b-16e-instruct-maas`. Every model name must use the `publisher/model` format.

### Vertex RAG Engine (`vertex-rag-engine`)

The `vertex-rag-engine` profile enables query-only retrieval against operator-managed RAG corpora through explicit retrieval bindings. It certifies cost-free by issuing a `GET` against `/ragCorpora/{corpus_id}` and verifying the returned `name`, and serves queries via `POST /v1/projects/{project}/locations/{region}:retrieveContexts`. Retrieval profiles do not accept `operation_defaults` or model `bindings` on the provider because all resource and filter rules belong on the retrieval binding. See [cloud retrieval bindings](../retrieval.md) for full setup and query semantics.

### Vertex AI Live (`vertex-realtime`)

The `vertex-realtime` profile serves strict bidirectional WebSocket sessions at `GET /native/vertex-realtime/sessions/{route}`, dialing `wss://{host}/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent`. It admits no `operation_defaults` or model binding defaults.

Every session begins with one JSON `setup` frame whose `setup.model` names the route slug. OLP rewrites `/setup/model` to `projects/{project}/locations/{region}/publishers/google/models/{model}` before forwarding to Vertex and waits for the upstream setup acknowledgement before marking the session delivered. Sessions are strictly connection-scoped, so `sessionResumption.handle` is refused. Input or output audio transcription alongside generated output is also refused unless `generationConfig.responseModalities` is explicitly empty for transcription-only mode, because combined transcription has no complete usage meter on this surface. If an earlier session for the calling key ended with billing-uncertain exposure, new sessions refuse with HTTP 409 `session_exposure_unresolved` until that exposure resolves.

### Google Cloud Document AI (`vertex-document-processing-ocr`)

The `vertex-document-processing-ocr` profile targets Google Cloud Document AI at `https://{host}/v1/projects/{project}/locations/{region}/processors/{model}:process`, where `{model}` is the processor ID, served through the native operation endpoint `POST /native/vertex-document-processing-ocr/models/{model}`.

Requests must contain either `rawDocument` (with base64 `content` and `mimeType`) or `gcsDocument` (with `gcsUri` and `mimeType`), never both. Optional `processOptions.individualPageSelector.pages` accepts an array of 1-based positive page integers. Permitted MIME types are `application/pdf`, `image/png`, `image/jpeg`, `image/tiff`, `image/gif`, `image/bmp`, and `image/webp`. Every `gcsDocument.gcsUri` must fall inside the connection's configured `options.document_uri_prefixes`. Responses that embed raw binary `document.content` or page `image.content` payloads are rejected, and returned pages are metered as media units.

### Vertex batch prediction (`vertex-batch-prediction`)

The `vertex-batch-prediction` profile is a strict batch-only profile on the `native` surface and does not serve live generation. Callers submit, inspect, list, and cancel jobs through `POST /native/vertex-batch-prediction/batch-jobs`, `GET /native/vertex-batch-prediction/batch-jobs`, `GET /native/vertex-batch-prediction/batch-jobs/{id}`, and `POST /native/vertex-batch-prediction/batch-jobs/{id}/cancel`, naming a batch-enabled transformed route in `X-OLP-Route`.

Configure nonsecret `batch_storage` with `gs://` `input_uri_prefix` and `output_uri_prefix` values. Every submission body accepts `input_uri`, `output_uri_prefix`, and optional `display_name` (1 to 128 letters, digits, underscores, or hyphens), and refuses `timeout_hours` (which applies only to Bedrock). OLP verifies that `input_uri` stays under `batch_storage.input_uri_prefix` and `output_uri_prefix` stays under `batch_storage.output_uri_prefix`, refusing violations with HTTP 400 `storage_scope`.

Certification probes `GET .../batchPredictionJobs?pageSize=1` without creating billable work. On submission, OLP creates a JSONL batch prediction job targeting `publishers/google/models/{model}` and attaches `labels.olp_submission` so reconciliation can adopt jobs safely after transport timeouts or restarts. Once a job reaches a terminal state, OLP verifies that `outputInfo.gcsOutputDirectory` stays inside `batch_storage.output_uri_prefix`, lists `prediction*.jsonl` objects via `https://storage.googleapis.com/storage/v1/b/{bucket}/o`, streams each object pinned by its Cloud Storage generation within the batch byte cap, decodes each prediction response for token usage, and commits durable page cursors as it settles the job.

## Authentication

| Mode | Credentials |
| --- | --- |
| `adc` | No stored credential. Resolves Application Default Credentials from the host environment. |
| `service_account` | Stored Google Cloud service-account JSON key. |

In `adc` mode, OLP checks `GOOGLE_APPLICATION_CREDENTIALS` first, then `~/.config/gcloud/application_default_credentials.json`, and finally the Compute Engine metadata server (`instance/service-accounts/default/token`). Ambient detection runs once per process so a cancelled caller request does not trigger duplicate environment probes.

In `service_account` mode, OLP parses the sealed JSON key strictly as a Google service account credential. Both modes request the `https://www.googleapis.com/auth/cloud-platform` scope over a bounded authentication transport with a 10-second timeout, a 1 MiB response limit, and redirects refused. Tokens are cached per connection fingerprint (up to 256 entries) and refreshed 30 seconds before expiry. Token exchange failures with HTTP 4xx status codes other than 408 and 429 reject the credential, while transient failures leave it in service. Service account JSON keys and access tokens are redacted from logs and upstream diagnostics.

## Fine-tuning (Vertex tuning jobs)

Vertex Google publisher connections (`vertex-gemini` or Automatic) serve supervised fine-tuning jobs through the retained resource endpoints under `/native/vertex-tuning/jobs` (`POST /native/vertex-tuning/jobs`, `GET /native/vertex-tuning/jobs`, `GET /native/vertex-tuning/jobs/{id}`, and `POST /native/vertex-tuning/jobs/{id}/cancel`), which map upstream to `/v1/projects/{project}/locations/{region}/tuningJobs` and `:cancel`.

Certification probes `GET .../tuningJobs?pageSize=1` without creating a job. Creating a tuning job is a metered retained operation, so callers must hold `allow_provider_state` policy permission and satisfy the accounting owner's acceptance rules.

Before forwarding a create request, OLP validates that `supervisedTuningSpec.trainingDatasetUri` and any optional `supervisedTuningSpec.validationDatasetUri` fall within the connection's `options.tuning_dataset_uri_prefixes`. Requests that set `customBaseModel`, `distillationSpec`, `preferenceOptimizationSpec`, `evaluationConfig`, or reserved gateway correlation keys inside `labels` are refused before any upstream call.

## Media

Transformed routes backed by a `vertex_ai` connection serve OpenAI image generation (`/v1/images/generations`), speech synthesis (`/v1/audio/speech`), and audio transcription (`/v1/audio/transcriptions`) through Gemini `generateContent` and the legacy Imagen `:predict` API. Strict routes refuse media codecs with `native_media_contract`.

Gemini image models such as `gemini-3.1-flash-image` return one image per request as `b64_json` at the aspect ratio of the requested size, bill the tokens reported by Gemini `usageMetadata`, fail with HTTP 400 `content_filter` when safety or prompt filters block the output, and fail with HTTP 400 `no_image` if the model responds without an image part. Models prefixed with `imagen-` use `/models/{model}:predict`, accept `n` from 1 to 4, return `b64_json`, and map `1024x1024`, `1536x1024`, and `1024x1536` to aspect ratios (`1:1`, `3:2`, `2:3`). Note that Google discontinued Vertex Imagen endpoints on 2026-06-30 in favor of Gemini image models.

Gemini speech models read `input` aloud using a prebuilt voice name (such as `Kore`) in `wav` or `pcm` format at natural pace (`speed` is refused) and bill tokens. Gemini transcription inlines the uploaded audio bytes (whose part content type must start with `audio/`), returns `json` or `text`, bills tokens, and refuses `prompt`, `language`, timestamps, chunking strategies, known speakers, and streaming. Because Vertex AI has no costless probe for media operations, certifying each media capability sends the smallest real billable call (one image, two characters of speech in `wav` using voice `Kore`, or a tenth of a second of silence to transcribe). See [vendor media](media.md) for the full media contract.

## Testing

Gemini protocol translation and Vertex connector validation run in the unit tier. Run them focused with the following commands.

```sh
go test -count=1 ./internal/domain/protocols -run Gemini
go test -count=1 ./internal/connectors -run Vertex
```

Paid live cloud qualification is not part of the automated suite. Validate live credential, batch storage, Document AI, or Vertex Live changes against a real Google Cloud project or a local stub and record the exact command and output.
