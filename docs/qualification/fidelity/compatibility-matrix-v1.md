# Compatibility and evidence matrix, version 1

The [machine-readable matrix](compatibility-matrix-v1.json) assesses locked
product source `bc325da50c575803c771531dc4e3aab1ac203a53` on
2026-09-23. Its 38 pinned test-source hashes match the selected public/SDK
run at `66a3ccb3fc737010a345d309ee280a541d4176f5`; the original
[execution receipt](row-evidence-v1.md) remains a historical run, not an
exact-head rerun. The later product change does not promote any row status.
The matrix maps
**all 47 original inventory rows exactly once**, using the frozen whole-file
and individual-row SHA-256 values, and records 45 later operation/profile/mode/
client combinations separately. The frozen
`tests/fixtures/fidelity/v1/inventory.json` remains unchanged, including its
historical `unqualified` labels; those are a fixed denominator, not a release
result. The 18-fixture `tests/fixtures/reference-inventory.json` and
`tests/release-behaviors.json` retain their independent purposes.

| Assessed rows | Native | Qualified mapping | Incompatible before dispatch | Unavailable | Unknown |
| --- | ---: | ---: | ---: | ---: | ---: |
| Frozen 47 | 8 | 0 | 0 | 4 | 35 |
| Additive 45 | 23 | 19 | 2 | 0 | 1 |
| All 92 | 31 | 19 | 2 | 4 | 36 |

These are **scoped deterministic contract statuses**. The 50 positive rows
are the admitted/completed combinations exercised at their named fixture,
profile revision and client boundary in the pinned `66a3ccb3` execution.
Their named test bytes are unchanged at `bc325da5`, but this receipt does not
claim a full exact-head rerun. Two rows record executed pre-dispatch
incompatibility with zero provider work. Forty remain
unqualified: four unavailable and 36 unknown. Neither a successful fixture
nor a connectivity probe establishes provider-model intelligence parity, and
no alternate profile removes a difficult frozen row from the denominator.
All 92 empirical-quality fields remain `unknown` under the
[preregistered quality plan](quality-plan.md); no approved paid live trials
were run.

`native` means the listed direct native behavior was exercised within the
row's explicit constraints without translating a retained upstream resource
identity into an OLP-owned one. It does not imply a translated client carrier,
live serving stability or provider-wide compatibility. `qualified` means a
named cross-dialect mapping **or a same-dialect owned resource-ID mapping**
completed its supported subset through the public strict route. Local file,
batch, video, response and Gemini Interaction IDs require this latter status
even when the media bytes and native event grammar are preserved.
`incompatible` requires an executed pre-dispatch refusal with
zero provider work for that exact class. `unavailable` means the complete row
contract is not exposed; `unknown` means existing evidence does not justify a
stronger classification for the full feature combination. The separately
stored `evidence_state` distinguishes integrated execution from partial or
unexecuted assessment; a status never automatically changes when source is
merged.

The [validator](../../../scripts/compatibility-matrix.mjs) checks all frozen
IDs and byte hashes, additive tuple uniqueness, named test symbols, SHA-pinned
source and execution receipts, execution state, and zero-dispatch evidence for
incompatibilities. Its [mutation tests](../../../scripts/compatibility-matrix.test.mjs)
exercise missing/renamed evidence, frozen-row changes and unsupported status
promotion. A passing validator verifies ledger consistency, not the runtime
behavior itself. The [row execution receipt](row-evidence-v1.md) gives the
exact current-product public and official SDK runs. Its run revisions and
source/receipt hashes are repeated in machine-readable evidence entries so a
later edit cannot silently inherit these claims.

The eight frozen native rows are official Anthropic JavaScript/Python two-tool
next turns, OpenAI moderation, native OpenAI/Anthropic/Gemini counts, and the
public strict Anthropic inline-PDF document block, plus direct OpenAI realtime.
The document fixture keeps
the original base64 bytes, text/document ordering, title and citation setting
without OCR. The two official Anthropic tool clients run the historical
direct-native route; they do not prove translated recovery. The 20 raw
generation/profile rows remain `unknown` because the public cloud fixture
checks profile dispatch and transport while the frozen conjunction also names
ordered content, tools, reasoning and native extensions. The direct OpenAI and
Voyage embedding frozen rows remain `unknown`: individual shape/storage and
pinned SDK examples passed, but the entire feature conjunction in those exact
rows has not been demonstrated. Cohere-specific embeddings/rerank remain
`unavailable`; TEI or Voyage alternatives do not qualify a Cohere dialect.

Strict image generation, edit, variation, speech, transcription and video
positives use a **certified compatible-chat** local fixture. The video public
test preserves original multipart image-reference bytes, native numeric
metadata, encrypted owner-scoped job source, result/content/delete identity
and expiry. The pinned OpenAI JavaScript 7.4.0 and Python 3.8.0 clients each
create, retrieve, download exact video and thumbnail bytes, and delete through
that public strict route. They make one provider create each. The frozen media
and video rows instead name the direct OpenAI profile, so they remain
`unknown` with integrated **partial** evidence; these compatible-hosting
positives appear in additive rows. Strict audio translation has no exposed
endpoint and remains `unavailable`. Strict Responses streaming background
remains `unavailable` at this assessment; Gemini Live and Azure unary
background are distinct contracts.

The direct `openai-responses` revision 1 realtime fixture exercises the frozen
OpenAI duplex row's VAD/session controls, interruption, tool-output identity
and audio timing through the public strict WebSocket. Exact native frames are
checked in both directions and unsupported query or semantic headers refuse
before provider dial. Early normal client/provider closes remain cancelled or
incomplete until a native `response.done`, with explicit Attempt/observation
states. `azure-v1-responses` revision 1 passes the same operation-owned
`openai-realtime` contract as a separate additive row. These are scripted
same-dialect raw WebSocket results, not WebRTC, sideband, session resumption or
live speech-quality claims. The older frozen lifecycle-stress fixture lacks a
versioned video profile, so its strict timed comparison is not inferred from
this functional realtime test.

The qualified Azure OpenAI additive rows cover strict file upload/retrieval/content,
batch item identity, exact separate partial output/error JSONL, cancellation,
expiry, and official pinned OpenAI JavaScript/Python upload-to-result journeys.
The file rows deliberately omit deletion; the direct OpenAI frozen file/batch
rows remain `unknown`. A separate qualified Azure Responses row admits **unary**
background accepted work and one terminal usage record through an encrypted
local `strict_response` ID. Strict streaming
background remains refused, so that frozen streaming row stays unavailable.

The qualified Gemini Interactions additive rows cover public two-turn encrypted
state, SSE native steps and cursor retrieval; official pinned JavaScript and
Python clients passed the streaming next-turn path over trusted local TLS. The
separate background row reopens the **same** encrypted accepted resource on
fresh gateway instances, closes an SSE reader early and resumes its native
cursor without another provider POST. Its local Interaction ID maps to the
encrypted upstream ID. This is reader-loss recovery, not a
process-crash or provider-stop claim. Direct Gemini Live raw WebSocket covers
native audio/video, activity/VAD, tool ordering, interruption and turn
completion. The pinned SDK rows are narrower: audio, activity start,
interruption and turn completion. An unowned nonempty Live resumption handle
is the second explicit `incompatible` row; the public WebSocket test observed
zero provider dispatch. Ordinary first Live sessions and empty resumption
settings are separate native positives.

The two negotiated OpenAI Chat-to-Anthropic rows are `qualified` only
for their pinned official JavaScript/Python clients and the exact streamed
reasoning/two-tool/encrypted-continuation contract. Each sends the exact
second native request, while unsupported carrier/controls fail before
dispatch. Two further qualified cross-dialect rows are OpenAI float embeddings
to Voyage float and compatible rerank to Voyage rerank. The remaining fifteen
qualified rows are native-dialect resource mappings: three video, three Azure
batch, three Azure file, one Azure unary background and five Gemini
Interactions combinations. A plain-text OpenAI Chat to
Anthropic subset does not admit an OpenAI `reasoning_effort` budget: its
separate `incompatible` row passed the public zero-dispatch refusal. Native
Voyage rerank remains `unknown` without a complete direct-native client
round trip.

The [gate status receipt](qualification-status-v1.md) separates ledger counts
from actual runtime violations, accepted-unknown outcomes, incomplete work,
performance and quality evidence. The [final-source performance
record](../../evidence/fidelity-performance/final-bc325-v2/README.md) has
scoped local lifecycle, stress and paired-barrier passes, but the source r6
attempt is invalid and full G6 remains open. Original frozen v1 failures and
budgets remain unchanged. No timed result promotes a compatibility row.
