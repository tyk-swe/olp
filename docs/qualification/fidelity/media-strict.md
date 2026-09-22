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

Selected `-race` media/gateway/routes/runtime tests and the tagged public
service test pass. A full `make check` passed after the strict runner and
inspector were added (Go vet, formatting, Go suites, 70 console files/632
tests, 24 script tests). The subsequent caller semantic-header/query refusal
passed focused tests; final branch checks are recorded in the implementer
handoff. Existing native media tests remain controls for legacy behavior.

This is not the complete lifecycle slice. Strict video create/retrieve/delete,
cloud image wrapper equivalence, file/batch/background recovery and partial
failures, native document assets, independent SDK media journeys, and measured
native media/duplex comparisons remain to be integrated under #216/#218.
Current strict activation refuses unqualified video and wrapper combinations;
media policy coverage intentionally rejects binary or multipart text/content
rules without a complete inspection contract. Quality remains unknown.
