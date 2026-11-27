# Vendor media

Reviewed media vendors can serve OpenAI clients through the gateway's image, speech, transcription, and video endpoints on transformed routes. Those endpoints are `/v1/images/generations`, `/v1/images/edits`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations`, and `/v1/videos*` (`POST /v1/videos`, `GET /v1/videos`, `GET /v1/videos/{video_id}`, `GET /v1/videos/{video_id}/content`, and `DELETE /v1/videos/{video_id}`).

Each vendor [contract](../../../internal/domain/vendors/catalog.go) lists the media operations it serves. When a vendor API differs from OpenAI's media wire, its [media codec](../../../internal/media/vendor.go) performs the translation. Other reviewed vendors serve the OpenAI media wire directly.

## Certification

Certifying a media model avoids creating billable work wherever a vendor allows it. A vendor's media operations certify through two proofs. The reviewed codec proves the wire contract, and either the model's presence in the vendor's model listing or the vendor's account probe, which is an authenticated request that costs nothing, proves the credential reaches the vendor. Without a listing, the model identifier is the operator's declaration, just as it is for any upstream that publishes no model list. Vendor media operations certify in `unary` mode except Runway's `video_create`, which certifies in `async` mode (`video_create`/`openai`/`async`).

Vertex AI, Bedrock, and Azure OpenAI offer no costless proof for media operations, so their media certifies by sending the smallest real call. That call generates one low-quality image, synthesizes two characters of speech, or transcribes a tenth of a second of silence, and each certification bills that call.

Because a vendor media codec translates requests and responses, it serves transformed routes only. Strict routes refuse it with `native_media_contract`, and a profile's native media defaults do not apply to it.

## Vendors

| Vendor | Operations | Wire | Account probe |
| --- | --- | --- | --- |
| [xAI](https://docs.x.ai/developers/rest-api-reference/inference/images) | `image_generation` | `xai-images`, mapping sizes to aspect ratios | `GET /v1/api-key` |
| [Groq](https://console.groq.com/docs/speech-to-text) | `transcription`, `translation` | `groq-audio`, serving `json` from `verbose_json` | `GET /openai/v1/models` |
| [ElevenLabs](https://elevenlabs.io/docs/api-reference/introduction) | `speech`, `transcription` | `elevenlabs` | `GET /v1/user` |
| [Deepgram](https://developers.deepgram.com/reference/deepgram-api-overview) | `speech`, `transcription` | `deepgram` | `GET /v1/projects` |
| [AssemblyAI](https://www.assemblyai.com/docs/pre-recorded-audio/api-reference/transcripts/submit) | `transcription` | `assemblyai`, polled | `GET /v2/transcript?limit=1` |
| [Runway](https://docs.dev.runwayml.com/api) | `video_create`, `video_list`, `video_get`, `video_content`, `video_delete` | `runway`, durable video jobs | `GET /v1/organization` |
| [Stability AI](https://platform.stability.ai/docs/api-reference) | `image_generation`, `image_edit` | `stability` | `GET /v1/user/balance` |
| [Recraft](https://www.recraft.ai/docs/api-reference/endpoints) | `image_generation` | `recraft` | `GET /v1/users/me` |
| [Black Forest Labs](https://docs.bfl.ai/api_integration/integration_guidelines) | `image_generation` | `bfl`, polled | `GET /v1/credits` |
| [Gemini API](https://ai.google.dev/gemini-api/docs/image-generation) | `image_generation`, `speech`, `transcription` | `gemini` | Model listing |
| [Vertex AI](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/image-generation) | `image_generation`, `speech`, `transcription` | `gemini` | Real call |
| [Amazon Polly](https://docs.aws.amazon.com/polly/latest/dg/API_SynthesizeSpeech.html), through Bedrock | `speech` | `polly` | Real call |

### xAI

xAI takes an aspect ratio instead of pixel dimensions. The sizes `1024x1024`, `1536x1024`, `1024x1536`, `1792x1024`, `1024x1792`, and `auto` map to `1:1`, `3:2`, `2:3`, `16:9`, `9:16`, and `auto`. xAI generates 1 to 10 images returned as `url` or `b64_json` and accepts `user`. Quality, style, background, moderation, output format, output compression, partial images, and streaming are refused. Image edits are not offered because xAI takes source images by URL, which OpenAI's multipart edit format does not carry.

### Groq

Groq bills audio by the second with a ten-second minimum and reports duration only in `verbose_json`. When a client requests `json`, OLP sends `verbose_json` upstream and answers with the text field alone so usage records carry the billed duration. Both `text` and `verbose_json` pass through. Subtitle formats (`srt` and `vtt`), diarized formats, streaming, `include`, chunking strategies, and known speakers are refused because Groq does not serve them.

### ElevenLabs

API keys travel in `Xi-Api-Key`. Speech takes an ElevenLabs voice ID as `voice`, returns `mp3`, `opus`, `pcm`, or `wav` audio, and accepts a `speed` from 0.7 to 1.2. Usage records the characters ElevenLabs reports in its `Character-Cost` response header. Transcription returns `json` with the text alone or `verbose_json` with the language, the billed duration, and timed words. It accepts `language` and refuses prompts, temperature, text format, and subtitle formats.

### Deepgram

API keys travel as `Authorization: Token <key>`. An Aura model names its voice inside the model identifier, so a request for `aura-2-thalia-en` names `thalia` as its `voice`. Speech returns `mp3`, `opus`, `aac`, `flac`, `wav`, or `pcm`, and usage records the characters Deepgram reports in `Dg-Char-Count`. Transcription uploads the audio as the request body and returns `json` or `verbose_json` with the billed duration.

Neither ElevenLabs nor Deepgram translates speech to English, so `translation` is declined for both, and for AssemblyAI as well.

### AssemblyAI

API keys travel in `Authorization` without a scheme prefix. Transcription runs asynchronously within the attempt's deadline and at most 240 polling steps. OLP uploads the audio, submits a transcript using the configured model as `speech_models`, polls once per second until the transcript completes, and deletes the upstream transcript after reading it so AssemblyAI retains no copy of the audio or text. AssemblyAI transcripts return as `json`, `verbose_json` with language, billed duration, and timed words, or `text`. Requests accept `language` and refuse prompts and temperature. The EU endpoint is `https://api.eu.assemblyai.com`.

### Runway

A video creation request starts a Runway task that OLP tracks as a durable media job pinned to its provider revision, matching OpenAI's video lifecycle. `GET /v1/videos/{video_id}` polls the task, the background reconciler settles it, and `DELETE /v1/videos/{video_id}` deletes the upstream task. Every request sends Runway's required `X-Runway-Version` header (`2024-11-06`). The upstream model, such as `gen4.5` or `veo3.1`, selects the Runway model. The requested `size` becomes Runway's ratio, `seconds` becomes its duration (4 seconds by default, matching OpenAI), and an image `input_reference` selects the `image_to_video` path. Content returns the task's first output fetched from Runway's CDN without sending credentials. Runway provides no thumbnails or spritesheets, and `GET /v1/videos` lists jobs from OLP's own records.

### Stability AI

The configured model selects the Stability service, accepting `stable-image-ultra`, `stable-image-core`, or an `sd3.5-*` model for Stable Diffusion 3.5. Each call returns one image as `b64_json` in `png`, `jpeg`, or `webp` at the aspect ratio of the requested size. Edits inpaint one image under its mask, or across transparent areas when no mask is supplied, using the model `stable-image-inpaint`. A request refused by Stability's content filter fails as the caller error `content_filter`, and an HTTP 403 typed as `content_moderation` leaves the provider credential in service.

### Recraft

Recraft generates 1 to 6 raster images at the aspect ratio of the requested size, returned as `url` or `b64_json` in `png` or `webp`. Requests target Recraft's raster endpoint and refuse vector models because SVG output does not match the OpenAI image contract.

### Black Forest Labs

API keys travel in `X-Key`. FLUX runs asynchronously within the attempt's deadline and at most 240 polling steps. Submission returns a `polling_url` that OLP polls once per second with the API key until status reaches `Ready`, after which OLP downloads the signed `result.sample` image without sending credentials (signed URLs expire after ten minutes). Credentialed polling requests must stay on the provider's own host or an HTTPS host under `bfl.ai` and pass egress policy. Uncredentialed signed result downloads allow any HTTPS host (or the provider's own host) that passes the gateway egress policy. One image returns as `b64_json` at the requested pixel dimensions in `png` or `jpeg`. A moderated request fails as `content_filter`, while any other failure after submission is treated as ambiguous because Black Forest Labs may still bill the job.

### Gemini on the Gemini API and Vertex

Gemini's `generateContent` serves `image_generation`, `speech`, and `transcription` on both `gemini` providers and `vertex_ai` providers authenticated through `adc` or `service_account`. An image model such as `gemini-3.1-flash-image` answers a prompt with one image at the aspect ratio of the requested size as `b64_json`. Usage records the tokens Gemini bills, and an image withheld by safety filters fails as `content_filter`. In code, any model name prefixed with `imagen-` routes through the `:predict` image encoder on both connectors, although Google shut down Imagen on the Gemini API on 2026-08-17 and discontinued Vertex Imagen endpoints on 2026-06-30.

A speech model reads the input aloud in one of Gemini's prebuilt voices, such as `Kore`, named in `voice`. Gemini answers with WAV or raw 16-bit PCM, which OLP serves as `wav` or `pcm` according to the client request, and usage records the billed tokens. Transcription inlines the uploaded audio, which must declare an `audio/*` content type, and requests the transcript text alone. It returns `json` or `text` with billed tokens as usage. Prompts, language hints, and timestamp granularities are refused because `generateContent` has no fields for them. Gemini's dedicated transcription model serves only the Interactions API, which OLP does not translate onto OpenAI transcription.

### Amazon Polly

A Bedrock provider serves speech synthesis through Amazon Polly in its configured AWS region at `polly.{region}`, signing requests with its AWS credentials for the `polly` service. The AWS identity needs `polly:SynthesizeSpeech`. The model names a Polly engine (`standard`, `neural`, `long-form`, or `generative`) and `voice` names a Polly voice ID such as `Joanna`. Polly serves `mp3` and `opus` because its PCM runs at 16 kHz rather than OpenAI's 24 kHz and it does not output WAV. Usage records the character count Polly returns in `x-amzn-RequestCharacters`.

### Declined

fal offers no costless authenticated endpoint for ordinary API keys. Its billing endpoint requires an Admin key and its model listing requires no key, so certifying a fal credential would require billing an image. fal remains available through provider plugins and custom endpoints.

Google shut down Imagen on the Gemini API on 2026-08-17, so Gemini API image generation serves Gemini image models such as `gemini-3.1-flash-image`. Google also discontinued Imagen's Vertex endpoints on 2026-06-30 in favour of Gemini image models (originally migrating callers to `gemini-2.5-flash-image` and now `gemini-3.1-flash-image`). The signed reference catalog marks retired Imagen models accordingly so routes that still target them warn.

Google Cloud Text-to-Speech is declined because Gemini speech models already serve Google speech on Vertex AI and the Gemini API, while Cloud Text-to-Speech runs on a separate host and requires a voice language that OpenAI's speech request does not carry.

Video vendors whose models have reached end of life are also declined. Azure OpenAI's Sora retired on 2026-10-15, Veo on the Gemini API ended on 2026-10-22, and Amazon Nova Reel reached end of life on 2026-09-30. Veo on Vertex AI is declined because Google's references disagree on how finished operations return video bytes when Cloud Storage output is not configured, and Veo offers no deletion endpoint. Runway serves Veo 3.1 models through its task API.

## Durable media jobs

Asynchronous video creation on `POST /v1/videos` records a durable media job with an OLP UUID before dispatching upstream. Each job pins the runtime generation, provider revision, credential slot, credential version, and route fidelity (`strict` or `transformed`) used at creation. Subsequent client calls on `GET /v1/videos`, `GET /v1/videos/{video_id}`, `GET /v1/videos/{video_id}/content`, and `DELETE /v1/videos/{video_id}` enforce ownership by API key, obey creation fidelity, and route directly to the pinned upstream job without failing over to another target.

Each media job tracks both its execution state (`queued`, `running`, `succeeded`, `failed`, or `cancelled`) and its reconciliation lifecycle (`creating`, `active`, `create_ambiguous`, `create_cleanup_pending`, `delete_pending`, or `deleted`). A background reconciliation worker polls runnable jobs every 5 seconds in bounded batches (up to 16 jobs per pass, 4 concurrent workers), backs off exponentially up to 300 seconds on transient errors, and retains active records until the provider's `expires_at` deadline or 30 days after creation.

Operators inspect and manage durable jobs through `/api/v1/media-jobs`, scoped to their accessible projects.

| Endpoint | Behavior |
| --- | --- |
| `GET /api/v1/media-jobs` | Lists jobs in descending creation order with cursor pagination (`limit` 1 to 200, default 50) and optional filters `route`, `state`, `lifecycle`, `api_key_id`, `provider_id`, `created_after`, and `created_before`. |
| `GET /api/v1/media-jobs/{job_id}` | Returns one job's metadata, reconciliation status, and observed `ETag`. |
| `POST /api/v1/media-jobs/{job_id}/refresh` | Runs an immediate reconciliation poll against the pinned provider (5-minute deadline). Returns `409 media_job_busy` if another worker holds the claim, or `409 media_job_caller_credential` when the job ran on a caller-supplied credential. |
| `GET /api/v1/media-jobs/{job_id}/content` | Streams downloadable output for an active succeeded job, accepting an optional `variant` query parameter of `video`, `thumbnail`, or `spritesheet`. Audits `media_job.content_download` and returns `409 media_content_unavailable` when no output is available or `503 media_content_unavailable` if the provider cannot deliver it. |
| `DELETE /api/v1/media-jobs/{job_id}` | Deletes the provider's stored output, requiring the observed `If-Match` ETag (`412 etag_mismatch` if stale). Returns `204 No Content` once confirmed, or `409 media_job_delete_pending` if deletion was initiated and background reconciliation is still waiting for provider confirmation. |

Revoking a provider credential takes effect immediately without checking existing `media_jobs`. Any subsequent background poll, operator refresh, content download, or delete checks live eligibility on the pinned credential version and fails with `media_job_credential_revoked`.

In the console, **Media jobs** at `/media-jobs` lists asynchronous jobs with filters for route, state, lifecycle, API key ID, provider ID, and creation time window alongside cursor pagination. Selecting a job opens `/media-jobs/[jobId]` with its metadata, lifecycle timeline, and identifiers. Operators with the `media.manage` capability can run **Refresh now**, choose a `video`, `thumbnail`, or `spritesheet` variant to **Download content**, or run **Delete content**. The console shows lifecycle and reconciliation metadata only and never displays uploaded or generated media inline.
