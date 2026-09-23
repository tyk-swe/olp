# Strict native media qualification (partial #216)

The OpenAI-family strict runner now publishes native `image_generation`,
`image_edit`, `image_variation`, `speech`, and `transcription` contracts for
direct OpenAI, direct compatible, and supported Azure hosting profiles. It
binds the selected serving model and absent-only defaults to the caller's
immutable JSON source or ordered multipart source. Staged files keep the
existing spool owner and carry SHA-256, length, media type, filename, and
handle through the attempted call. Unknown JSON members and nested numeric
lexemes remain byte-for-byte; policy-bearing requests reject native options
without complete inspection. The strict response path returns the bounded
native JSON source, a staged binary source, or one bounded SSE event at a time.
It does not transcode assets or replay ambiguous effectful attempts.

Direct OpenAI and compatible profiles also publish `video_create`, `video_get`,
`video_content`, and `video_delete` as a qualified durable local-ID mapping.
Create preserves ordered multipart text, optional image-reference bytes,
filename, media type, duration and size. The existing Attempt and media-job
owners reserve before upload, pin one provider/slot, and reconcile accepted
work without another inference attempt. Native create/get/delete JSON retains
unknown members, order and numeric lexemes; only the provider job ID and model
are overlaid with the API-key-owned local ID and route. Binary video and preview
bytes retain SHA-256, MIME codec parameters, and bounded spool ownership. The
complete create metadata is encrypted under the job UUID in the existing
KeyRing and committed atomically with job activation; it expires at the earlier
of the provider asset expiry or OLP's 30-day media-job retention. Tombstoning
deletes that secret in the same transaction. A fresh owner, current credential,
and pinned compatible target check precede any recovered source read.

`GET /v1/videos` remains an API-key-owned local inventory of OLP jobs. The
[provider list](https://platform.openai.com/docs/api-reference/videos) is
project-wide, so strict publication refuses `video_list` even when the model
certifies the operation. The local inventory is usable for strict-created jobs
but is not advertised as an equivalent native list. Azure video hosting and
unqualified semantic headers, idempotency, or query options fail before strict
dispatch. Video remix, hidden OCR, frame sampling, transcoding, and durable
upload-byte storage have no claimed contract.

Native Anthropic generation passes inline PDF `document` blocks through the
same strict OIF source without extraction or conversion. The public scripted
provider test compares the original base64 bytes, order, title and citation
control. Provider `file_id` references remain owner-scoped resources rather
than arbitrary caller-supplied IDs, consistent with the
[first-party document block](https://platform.claude.com/docs/en/api/http/messages).

The new public `TestStrictMediaPublicNativeSourcesAndAssets` creates providers,
strict routes, and API keys through management, then dispatches to an
independent local scripted provider. It checks exact outbound and returned
JSON, ordered image[1]/mask/model/image[0] bytes, transcription file and
timestamps, speech binary bytes/type, image SSE id/retry/events, zero-dispatch
nested duplicate keys, and safe inspector provenance/redaction. The disposable
fixture seeds only its certified capability and validated slot fingerprint in
PostgreSQL after public provider creation and nonbillable model probe: the
production certifier deliberately requires the official OpenAI host for media
and does not trust a custom endpoint's self-reported model list. No paid
inference was performed.

The public strict video tests publish the provider, certified lifecycle
tuples, strict route and API key through management, then exercise create,
local list, retrieve, MP4/thumbnail content and delete against an independent
scripted provider. They assert exact multipart order/reference bytes, duration,
size, exact native metadata and binary bytes/type, bounded encrypted source,
wrong-owner/expired/revoked/tampered read refusal, atomic failed attachment,
key rotation and deletion cleanup. An unqualified create idempotency header,
duplicate content variant and strict project-wide list claim are rejected
without provider dispatch. A separate public Anthropic test compares the
complete original inline PDF block at the provider boundary. Existing media
job and document tests remain controls for legacy behavior.

Selected `-race` media/gateway/routes/runtime tests and the tagged public
service test pass. A full `make check` passed after the strict runner and
inspector were added (Go vet, formatting, Go suites, 70 console files/632
tests, 24 script tests). The subsequent caller semantic-header/query refusal
passed focused tests; final branch checks are recorded in the implementer
handoff. Existing native media tests remain controls for legacy behavior.

This is not the complete lifecycle slice. Cloud image wrapper equivalence,
independent SDK media journeys, and measured native media/duplex comparisons
remain to be integrated under #216/#218. Current strict activation refuses
unqualified video list and wrapper combinations;
media policy coverage intentionally rejects binary or multipart text/content
rules without a complete inspection contract. Quality remains unknown.
