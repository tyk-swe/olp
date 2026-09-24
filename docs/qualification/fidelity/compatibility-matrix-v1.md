# Compatibility and evidence matrix, version 1

The [machine-readable matrix](compatibility-matrix-v1.json) assesses
`15350c8906e628daa69d82eff86210dd00caa823`: code
`3a3a3644ab06220478e07080e864f19e9e540c05` plus its committed
[September 24 execution receipt](release-execution-2026-09-24.md).
All named test-source hashes and execution entries are pinned to that code
revision. The subsequent ledger publication changes documentation and ledger
test expectations, not the executed product or protocol tests.

The matrix maps **all 47 original inventory rows exactly once**, retaining the
frozen whole-file and individual-row SHA-256 values, and records 52 additive
operation/profile/mode/client combinations separately. The frozen
`tests/fixtures/fidelity/v1/inventory.json` remains unchanged, including its
historical `unqualified` labels: it is a fixed denominator, not a release
result. The 18-fixture `tests/fixtures/reference-inventory.json` and
`tests/release-behaviors.json` retain their independent purposes.

| Assessed rows | Native | Qualified mapping | Incompatible before dispatch | Unavailable | Unknown |
| --- | ---: | ---: | ---: | ---: | ---: |
| Frozen 47 | 8 | 0 | 0 | 1 | 38 |
| Additive 52 | 28 | 21 | 2 | 0 | 1 |
| All 99 | 36 | 21 | 2 | 1 | 39 |

These are **scoped deterministic contract statuses**. The 57 positive rows
are admitted/completed combinations exercised at their named fixture, profile
revision and client boundary. Two rows record executed pre-dispatch
incompatibility with zero provider work. Forty remain unqualified: one
unavailable and 39 unknown. An implemented operation does not qualify every
provider/model/profile conjunction. All 99 empirical-quality fields remain
`unknown` under the [preregistered quality plan](quality-plan.md); no paid live
trials ran. These limits do not remove any of the specification's 66 features.

`native` means the listed native behavior was exercised within the row's
constraints without translating a retained upstream resource identity into
an OLP-owned one. It does not imply live serving stability or provider-wide
compatibility. `qualified` means a named cross-dialect mapping **or a
same-dialect owned resource-ID mapping** completed its supported subset
through the public strict route. Local file, batch, video, response and Gemini
Interaction IDs require this latter status even when the media bytes and
native event grammar are preserved. `incompatible` requires an executed
pre-dispatch refusal with zero provider work for that exact class.
`unavailable` means the complete row contract is not exposed; `unknown` means
evidence does not justify a stronger classification for the full combination.
The separate `evidence_state` distinguishes integrated execution from partial
assessment; merging source does not automatically promote a status.

The [validator](../../../scripts/compatibility-matrix.mjs) checks frozen IDs
and byte hashes, additive tuple uniqueness, named test symbols, SHA-pinned
source and execution receipts, execution state, and zero-dispatch evidence.
Its [mutation tests](../../../scripts/compatibility-matrix.test.mjs) reject
missing/renamed evidence, frozen-row changes and unsupported status promotion.
A passing validator establishes ledger consistency, not runtime behavior.
The current receipt records **50 public/SDK tests under race detection with
zero skips**, both standalone official SDK suites, repository checks and the
bounded strict performance smoke. Historical [row execution](row-evidence-v1.md)
and [gate status](qualification-status-v1.md) receipts remain dated records;
they do not describe the current release result.

## Generation, continuation and cloud profiles

The eight frozen native rows are official Anthropic JavaScript/Python two-tool
next turns, OpenAI moderation, native OpenAI/Anthropic/Gemini counts, the public
strict Anthropic inline-PDF document block, and direct OpenAI realtime. The
PDF fixture preserves original base64 bytes, text/document ordering, title
and citation settings without OCR. Native Anthropic clients do not establish
translated recovery. The 20 raw generation/profile rows remain `unknown`:
cloud fixtures check profile dispatch and transport, while those frozen rows
also require ordered content, tools, reasoning and native extensions together.

The two negotiated OpenAI Chat-to-Anthropic rows are `qualified` for their
pinned official JavaScript/Python clients and exact streamed reasoning,
two-tool and encrypted-continuation contract. Each sends the exact second
native request; unsupported carriers and controls refuse before dispatch.
The separate plain-text Chat-to-Anthropic `reasoning_effort` combination is
`incompatible`, with an executed zero-dispatch refusal.

## Independent unary operations

Qualified cross-dialect rows cover OpenAI float embeddings to Voyage float
and compatible rerank to Voyage rerank. Frozen OpenAI/Voyage embedding and
native Voyage rerank conjunctions remain `unknown` where individual tested
examples do not establish their entire declared combination.

Three native [Cohere v2](cohere-native-v2.md) additive rows cover text/typed
embeddings, multimodal embeddings and rerank through explicitly versioned v2
profiles. Numeric/packed dtype groups agree on dimensions; original base64
remains opaque without an invented float32 width. Original input/result order,
indices, score spellings/ties and native billing categories are retained.
Restrictive policy refuses nested content it cannot inspect before dispatch.
The frozen Cohere embedding conjunction remains `unavailable` because it
requires sparse/multivector output absent from this native API; TEI supplies
that product feature without pretending to qualify Cohere. The original
Cohere rerank preset/client conjunction remains `unknown` with integrated
partial evidence; the explicit v2 tuple does not silently qualify the legacy
compatibility preset.

## Media and durable resources

Strict image generation, edit, variation, speech, transcription and video
positives use a certified compatible-chat local fixture. Video tests retain
original multipart image bytes, native numeric metadata, encrypted owner-scoped
job source, content/delete identity and expiry. Pinned OpenAI JavaScript 7.4.0
and Python 3.8.0 clients create, retrieve, download exact video/thumbnail bytes
and delete, with one provider create each. Their frozen direct-OpenAI
counterparts remain `unknown` with integrated partial evidence.

[Audio translation](audio-translation.md) is implemented at
`POST /v1/audio/translations`. Two native additive rows execute compatible-chat
raw HTTP and pinned OpenAI JavaScript 7.4.0 against all five formats: JSON,
verbose JSON, text, SRT and VTT. Original multipart, absent-only defaults,
pricing and usage are checked. Exact JSON numeric spelling is a raw-wire
claim; the SDK's parsed numbers retain its ordinary JavaScript precision
limits. The frozen direct-OpenAI translation row is `unknown` with partial
evidence, not an absent operation.

Qualified Azure file and batch rows cover upload/retrieval/content, batch
item identity, separate partial output/error JSONL, cancellation, expiry and
pinned JavaScript/Python upload-to-result journeys. File rows omit deletion;
frozen direct-OpenAI file/batch rows remain `unknown`.

Azure Responses now has one qualified unary and **two qualified background
streaming** rows (raw HTTP and pinned OpenAI JavaScript 7.4.0). They retain an
encrypted local response/parent identity, native SSE, reader-loss recovery on
a fresh gateway, cursor retrieval, cancellation, expiry and one terminal usage
settlement without inference redispatch. Failed native terminal events remain
visible after persistence; generic stream errors leave accepted work
recoverable. The frozen direct-OpenAI streaming row is `unknown` with partial
evidence because the executed profile is Azure legacy Responses. See the
[durable lifecycle contract](durable-lifecycle.md).

## Gemini and realtime

Five qualified Gemini Interactions rows cover encrypted two-turn state,
native SSE steps/cursor retrieval, pinned JavaScript/Python clients over
trusted local TLS, and background recovery. The latter reopens the same
accepted resource on fresh gateways and resumes after reader loss without
another provider POST. This is reader-loss recovery, not a provider-stop or
process-crash claim. Canceled clients cannot prevent the bounded accepted,
status and delete commits; public tests delay those writes with database locks.

Direct Gemini Live raw WebSocket covers native audio/video, activity/VAD,
tool ordering, interruption and completion. Its pinned SDK rows cover the
narrower audio/activity/interruption/completion contract. An unowned nonempty
resumption handle is the second `incompatible` row, with zero provider work.
First sessions and empty resumption settings are separate native positives.

Direct OpenAI and Azure v1 realtime fixtures retain native frames in both
directions, session/VAD controls, tool identity and timing. Unsupported
semantic headers and query controls refuse before provider dial. Early closes
remain cancelled/incomplete until native `response.done`; active network
credential revocation fences both profiles. These are scripted WebSocket
contracts, not WebRTC, sideband, resumption or live speech-quality claims.

## Release evidence

[Release validation](release-validation.md) maps G1–G7 to current local
execution and required full PR CI. The bounded smoke passed all 22 paths with
1,280 successful timed dispatches and 128 intentional zero-dispatch refusals
in 23.725 seconds. Resource limits, overflow, cancellation and recovery are
functional requirements, independent of short timings. Long statistical and
live-quality studies are optional under amended T08/T09/G6. Historical failed
and invalid studies retain their original results in the [performance archive](../../evidence/fidelity-performance/archive.md).
No timing result or connectivity probe promotes a compatibility row.
