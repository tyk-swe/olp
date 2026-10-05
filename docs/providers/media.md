# Vendor media

OpenAI clients reach the image, speech and transcription APIs of reviewed
vendors through the OpenAI media endpoints, `/v1/images/generations`,
`/v1/audio/speech`, `/v1/audio/transcriptions` and `/v1/audio/translations`, on
transformed routes. Each vendor's [contract](../../internal/vendors/catalog.go)
lists the media operations it serves. A vendor whose API differs from OpenAI's
names the wire its [media codec](../../internal/media/vendor.go) translates;
the others speak OpenAI's media wire as it is.

## Certification

Certifying a media model must not create billable work, so a vendor's media
operations certify through two proofs: the reviewed codec proves the wire,
and the vendor's account probe, an authenticated request that costs nothing,
proves the credential reaches the vendor. The model is the operator's
declaration, as it is for any upstream that lists no models. Vendor media
certifies unary operations only.

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

