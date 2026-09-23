# Compatibility and evidence matrix, version 1

The [machine-readable matrix](compatibility-matrix-v1.json) assesses source
`44c88077888cc1274dd3d3687a7dca48f084ac91` on 2026-09-23. It maps
**all 47 original inventory rows exactly once**, using the frozen whole-file
and individual-row SHA-256 values, and records 44 later operation/profile/mode/
client combinations separately. The frozen
`tests/fixtures/fidelity/v1/inventory.json` remains unchanged, including its
historical `unqualified` labels; those are a fixed denominator, not a release
result. The 18-fixture `tests/fixtures/reference-inventory.json` and
`tests/release-behaviors.json` retain their independent purposes.

| Assessed rows | Native | Qualified translation | Incompatible before dispatch | Unavailable | Unknown |
| --- | ---: | ---: | ---: | ---: | ---: |
| Frozen 47 | 7 | 0 | 0 | 5 | 35 |
| Additive 44 | 37 | 4 | 2 | 0 | 1 |
| All 91 | 44 | 4 | 2 | 5 | 36 |

These are **scoped deterministic contract statuses**. The 48 positive rows
are the admitted/completed combinations actually exercised at their named
fixture, profile revision and client boundary. Two rows record executed
pre-dispatch incompatibility with zero provider work. Forty-one remain
unqualified: five unavailable and 36 unknown. Neither a successful fixture
nor a connectivity probe establishes provider-model intelligence parity, and
no alternate profile removes a difficult frozen row from the denominator.
All 91 empirical-quality fields remain `unknown` under the
[preregistered quality plan](quality-plan.md); no approved paid live trials
were run.

`native` means the listed direct native behavior was exercised within the
row's explicit constraints. It does not imply a translated client carrier,
live serving stability or provider-wide compatibility. `qualified` means the
named cross-dialect mapping's supported subset completed through the public
strict route. `incompatible` requires an executed pre-dispatch refusal with
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
exact combined-tree public and official SDK runs. Its run revisions and
source/receipt hashes are repeated in machine-readable evidence entries so a
later edit cannot silently inherit these claims.

The seven frozen native rows are official Anthropic JavaScript/Python two-tool
next turns, OpenAI moderation, native OpenAI/Anthropic/Gemini counts, and the
public strict Anthropic inline-PDF document block. The document fixture keeps
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
endpoint and remains `unavailable`. Original OpenAI realtime and streaming
background combinations remain `unavailable` at this assessment; Gemini Live
and Azure unary background are distinct contracts.

The Azure OpenAI additive rows cover strict file upload/retrieval/content,
batch item identity, exact separate partial output/error JSONL, cancellation,
expiry, and official pinned OpenAI JavaScript/Python upload-to-result journeys.
The file rows deliberately omit deletion; the direct OpenAI frozen file/batch
rows remain `unknown`. A separate Azure Responses row admits **unary**
background accepted work and one terminal usage record. Strict streaming
background remains refused, so that frozen streaming row stays unavailable.

The direct Gemini Interactions additive rows cover public two-turn encrypted
state, SSE native steps and cursor retrieval; official pinned JavaScript and
Python clients passed the streaming next-turn path over trusted local TLS. The
separate background row reopens the **same** encrypted accepted resource on
fresh gateway instances, closes an SSE reader early and resumes its native
cursor without another provider POST. This is reader-loss recovery, not a
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
dispatch. The other two qualified rows are OpenAI float embeddings to Voyage
float and compatible rerank to Voyage rerank. A plain-text OpenAI Chat to
Anthropic subset does not admit an OpenAI `reasoning_effort` budget: its
separate `incompatible` row passed the public zero-dispatch refusal. Native
Voyage rerank remains `unknown` without a complete direct-native client
round trip.

The [gate status receipt](qualification-status-v1.md) separates ledger counts
from actual runtime violations, accepted-unknown outcomes, incomplete work,
performance and quality evidence. In particular, the original frozen
slow-stream relay budget, encrypted continuation barrier, and native
lifecycle/media stress comparisons are independent of this matrix. No
historical failed capture or frozen budget was rewritten to make these rows
positive.
