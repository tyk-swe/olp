# Vendor media

OpenAI clients reach the image, speech and transcription APIs of reviewed
vendors through the OpenAI media endpoints, `/v1/images/generations`,
`/v1/audio/speech`, `/v1/audio/transcriptions` and `/v1/audio/translations`, on
transformed routes. Each vendor's [contract](../../internal/vendors/catalog.go)
lists the media operations it serves. A vendor whose API differs from OpenAI's
names the wire its [media codec](../../internal/media/vendor.go) translates;
the others speak OpenAI's media wire as it is.

## Certification

Certifying a media model must not create billable work wherever a vendor
allows it, so a vendor's media operations certify through two proofs: the
reviewed codec proves the wire, and either the model's place in the vendor's
model listing or the vendor's account probe, an authenticated request that
costs nothing, proves the credential reaches it. Without a listing, the model
is the operator's declaration, as it is for any upstream that lists no
models. Vendor media certifies unary operations only.

Vertex AI, Bedrock and Azure OpenAI offer no costless proof, so their media
certifies by the smallest real call, which bills: one low-quality image, two
characters of speech, or a tenth of a second of silence to transcribe.

A codec translates the request, so it serves transformed routes only: strict
routes refuse it with `native_media_contract`, and a profile's native media
defaults do not apply to it.

## Vendors

| Vendor | Operations | Wire | Account probe |
| --- | --- | --- | --- |
| [xAI](https://docs.x.ai/developers/rest-api-reference/inference/images) | `image_generation` | `xai-images`: sizes become aspect ratios | `GET /v1/api-key` |
| [Groq](https://console.groq.com/docs/speech-to-text) | `transcription`, `translation` | `groq-audio`: `json` is served from `verbose_json` | `GET /openai/v1/models` |
| [ElevenLabs](https://elevenlabs.io/docs/api-reference/introduction) | `speech`, `transcription` | `elevenlabs` | `GET /v1/user` |
| [Deepgram](https://developers.deepgram.com/reference/deepgram-api-overview) | `speech`, `transcription` | `deepgram` | `GET /v1/projects` |
| [AssemblyAI](https://www.assemblyai.com/docs/pre-recorded-audio/api-reference/transcripts/submit) | `transcription` | `assemblyai`: polled | `GET /v2/transcript?limit=1` |
| [Runway](https://docs.dev.runwayml.com/api) | `video_create` and the video lifecycle | `runway`: jobs | `GET /v1/organization` |
| [Stability AI](https://platform.stability.ai/docs/api-reference) | `image_generation`, `image_edit` | `stability` | `GET /v1/user/balance` |
| [Recraft](https://www.recraft.ai/docs/api-reference/endpoints) | `image_generation` | `recraft` | `GET /v1/users/me` |
| [Black Forest Labs](https://docs.bfl.ai/api_integration/integration_guidelines) | `image_generation` | `bfl`: polled | `GET /v1/credits` |
| [Gemini API](https://ai.google.dev/gemini-api/docs/image-generation) | `image_generation`, `speech`, `transcription` | `gemini` | model listing |
| [Vertex AI](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/image-generation) | `image_generation`, `speech`, `transcription` | `gemini` | real call |
| [Amazon Polly](https://docs.aws.amazon.com/polly/latest/dg/API_SynthesizeSpeech.html), through Bedrock | `speech` | `polly` | real call |

### xAI

xAI takes an aspect ratio in place of a size: `1024x1024`, `1536x1024`,
`1024x1536`, `1792x1024`, `1024x1792` and `auto` map to `1:1`, `3:2`, `2:3`,
`16:9`, `9:16` and `auto`. It generates 1 to 10 images, returned as `url` or
`b64_json`, and accepts `user`; quality, style, background, moderation,
output format and compression, partial images and streaming are refused.
Image edits are not offered: xAI takes the images to edit by URL, which
OpenAI's multipart edits do not carry.

### Groq

Groq bills audio by the second, with a ten-second minimum, and reports the
duration only in `verbose_json`. A request for `json` is sent as
`verbose_json` and answered with its text alone, so usage carries the
duration. `text` and `verbose_json` pass through; `srt`, `vtt` and diarized
formats, streaming, `include`, chunking strategies and known speakers are
refused, since Groq does not serve them.

### ElevenLabs

API keys travel in `xi-api-key`. Speech names an ElevenLabs voice ID as its
`voice`, returns `mp3`, `opus`, `pcm` or `wav` audio, and takes a `speed` from
0.7 to 1.2; usage is the characters ElevenLabs reports billing in its
`character-cost` header. Transcription returns `json`, its text alone, or
`verbose_json` with the language, the duration ElevenLabs bills by and the
timed words; it takes `language` and refuses a prompt, temperature and the
text and subtitle formats.

### Deepgram

API keys travel as `Authorization: Token`. An Aura model names its voice, so
a request for `aura-2-thalia-en` names `thalia` as its voice; speech returns
`mp3`, `opus`, `aac`, `flac`, `wav` or `pcm`, and usage is the characters
Deepgram reports in `dg-char-count`. Transcription sends the audio itself as
the request body and reads back `json` or `verbose_json` with the duration
Deepgram bills by.

Neither vendor translates speech to English, so `translation` is declined for
both, and for AssemblyAI.

### AssemblyAI

API keys travel in `Authorization` without a scheme. Transcription works
asynchronously within the attempt's deadline: the audio is uploaded, a
transcript submitted for it with the model as its speech model, and polled
each second until complete; the transcript is then deleted, so AssemblyAI
keeps no copy of the audio or its words. It is served as `json`,
`verbose_json` with the language, the duration AssemblyAI bills by and the
timed words, or `text`; it takes `language` and refuses a prompt and a
temperature. The EU region is `https://api.eu.assemblyai.com`.

### Runway

A video creation starts a Runway task, which OLP keeps as a durable video
job pinned to its provider revision, as it does OpenAI's: `GET
/v1/videos/{id}` polls the task, the reconciler settles it, and deleting the
video deletes the task. Every request carries Runway's required
`X-Runway-Version`. The model, such as `gen4.5` or `veo3.1`, names the Runway
model; the size becomes its ratio, `seconds` its duration, four by default as
OpenAI's is, and an image `input_reference` makes it an image-to-video task.
Content is the task's first output, fetched from Runway's CDN, where its
signed URL takes no credential; Runway makes no thumbnails or spritesheets.
Jobs are listed from OLP's own records.

### Stability AI

The model selects the Stability service: `stable-image-ultra`,
`stable-image-core`, or an `sd3.5-*` model, which Stable Diffusion 3.5 names
in its request. Each returns one image as `b64_json`, in `png`, `jpeg` or
`webp`, at the aspect ratio of the requested size. Edits inpaint one image
under its mask, or its transparent areas without one, with the model
`stable-image-inpaint`. A request Stability's content filter refuses fails as
the caller's error, `content_filter`, and a 403 Stability names
`content_moderation` leaves the credential in service.

### Recraft

Recraft generates 1 to 6 raster images at the aspect ratio of the requested
size, as `url` or `b64_json`, in `png` or `webp`. Requests go to its raster
path, which refuses vector models: their SVG is no OpenAI image.

### Black Forest Labs

API keys travel in `x-key`. FLUX works asynchronously: the submission names a
polling URL, which OLP polls each second with the key, within the attempt's
deadline and at most 240 polls, until the work is ready; it then fetches the
signed result, which expires in ten minutes, without the key. Every address
must be on the provider's host or under `bfl.ai`. One image is returned as
`b64_json`, at the requested size in pixels, in `png` or `jpeg`. A moderated
request fails as `content_filter`; any other failure after submission is
ambiguous, since BFL may still bill the work.

### Gemini on the Gemini API and Vertex

Gemini's `generateContent` serves all three operations, on a `gemini` provider
and, through ADC, on a `vertex_ai` one. An image model answers a prompt with
one image, at the aspect ratio of the requested size, as `b64_json`; usage is
the tokens Gemini bills, and an image its safety filters withhold fails as
`content_filter`. Vertex's `imagen-*` models keep their predict API.

A speech model reads the input aloud in one of its prebuilt voices, such as
`Kore`, named as the `voice`; Gemini answers with WAV or raw 16-bit PCM,
which OLP serves as the `wav` or `pcm` the client asked for, and usage is the
tokens billed. A transcription inlines the uploaded audio, which must name
its audio content type, and asks the model for the transcript alone; it is
served as `json` or `text`, with the tokens billed as usage. A prompt,
language hint and timestamps are refused: `generateContent` has no place for
them. Gemini's dedicated transcription model serves only the Interactions
API, which OLP does not translate.

### Amazon Polly

A Bedrock provider speaks through Amazon Polly in its region, at
`polly.{region}`, with its AWS credentials signed for Polly; the identity
needs `polly:SynthesizeSpeech`. The model is the Polly engine, `standard`,
`neural`, `long-form` or `generative`, and the voice a Polly voice ID, such
as `Joanna`. Polly serves `mp3` and `opus`: its PCM is 16 kHz rather than
OpenAI's 24 kHz, and it makes no WAV. Usage is the characters Polly reports
billing in `x-amzn-RequestCharacters`.

### Declined

fal offers no costless authenticated request for ordinary API keys: its
billing endpoint takes an Admin key, and its model listing takes none, so a
fal credential could only be certified by billing an image. It stays with
plugins and custom endpoints.

Imagen on the Gemini API shut down on 2026-08-17, so Gemini API image
generation serves Gemini image models only; Vertex keeps Imagen.

Google Cloud Text-to-Speech is declined: Gemini speech models already serve
Google speech on Vertex and the Gemini API, while Cloud Text-to-Speech sits
on its own host, which a Vertex provider's address does not reach, and its
voices need a language that OpenAI's speech request does not carry.

Video vendors whose models have ended or are ending are declined: Azure
OpenAI's Sora retires on 2026-10-15, Veo on the Gemini API ends on
2026-10-22, and Amazon Nova Reel reached end of life on 2026-09-30. Veo on
Vertex AI is declined too: Google's references disagree on how a finished
operation returns its video, which arrives inside the operation itself unless
it is written to Cloud Storage, and Veo has no deletion. Runway serves Veo 3.1
models.

