# Compatibility and evidence matrix, version 1

The [machine-readable matrix](compatibility-matrix-v1.json) assesses source
`e0534816efd4f6f2f45c3a462a78dc1d297a4e91` on 2026-09-22. It maps **every
one of the 47 unchanged frozen inventory rows exactly once** and keeps 27 later
operation/profile/client combinations in a separate additive list. It does not
rewrite `tests/fixtures/fidelity/v1/inventory.json`, whose `unqualified` value
remains the original denominator rather than a release result. The historical
`tests/fixtures/reference-inventory.json` and `tests/release-behaviors.json`
retain their independent purposes.

The assessed source has six frozen `native`, zero frozen `qualified`, fourteen
frozen `unavailable`, and twenty-seven frozen `unknown` rows. The 27 additive
rows have thirteen `native`, two `qualified`, one `incompatible`, ten
`unavailable`, and one `unknown` status. These are **scoped deterministic
contract statuses**, not a
provider-wide badge or a model-quality comparison. The original 47 never leave
the denominator because an alternate TEI or Voyage profile was added.

`native` means the listed direct native behavior was exercised within the
row's stated fixture/profile/client constraints. It does not imply a translated
client carrier, live serving stability or intelligence parity. `qualified`
means a named cross-dialect mapping's supported subset was exercised through
the public strict path; other fields remain incompatible or unknown. An
`incompatible` row requires an executed pre-dispatch refusal with zero provider
work for the exact declared class. `unavailable` means the complete row
contract is not exposed at the assessed revision, even if a legacy operation,
source foundation, or isolated branch implements some of it. `unknown` means
the evidence cannot justify one of the other statuses for the full declared
feature combination. No row silently changes status when another revision is
merged.

The row's `evidence_state` is independent of its contract status:

| State | Meaning at assessed revision |
| --- | --- |
| `integrated-executed` | Named test source and passing execution receipt are integrated; the receipt pins the revision where it ran. |
| `integrated-partial` | An integrated test exercised a narrower behavior, but the complete row was not qualified. |
| `isolated-executed` | A pinned implementer branch reports a passing scripted check; its feature is **not integrated** into this snapshot. |
| `not-executed` | Source inspection or an explicit gap assessment is the evidence; no row-level passing test is claimed. |

The source and receipt SHA-256 values in test evidence are part of the claim.
The [validator](../../../scripts/compatibility-matrix.mjs) rejects a missing,
duplicate, altered, or newly appended frozen row; a changed frozen inventory
digest; duplicate additive tuples; missing/renamed test symbols or files;
changed test source or receipt bytes; and native/qualified/incompatible claims
without matching executed evidence. `node --test
scripts/compatibility-matrix.test.mjs` exercises these failures. A passing
validator confirms ledger consistency, **not** runtime behavior by itself.

The six frozen native rows are the pinned JavaScript/Python Anthropic two-tool
next-turn fixture and OpenAI moderation plus OpenAI, Anthropic and Gemini
native count fixtures. The Anthropic fixture preserves thinking/signature,
parallel call IDs, block order and the SDK-generated second request, but it
uses the historical direct native route and does not prove translated or crash
recovery. The count/moderation fixtures use the management-provisioned strict
route, a scripted provider, exact result and one shared Attempt. Frozen OpenAI
and Voyage embedding rows remain unknown: individual float/base64/packed and
SDK examples passed, but the full storage, dimensions, task and ordered-input
feature conjunction has not been demonstrated through that exact public row.
The frozen Cohere-specific embedding and rerank dialects remain unavailable;
Voyage/TEI/compatible alternatives do not qualify a Cohere row. Media, durable,
document and duplex rows are unavailable as *complete strict contracts* at
this revision; their legacy controls and media source/asset work remain useful
partial evidence.

The two additive qualified rows are only OpenAI **float** embeddings to Voyage
float and compatible rerank to Voyage rerank, as proven by the public fixture in
[unary-operations.md](unary-operations.md). The separately listed TEI,
Gemini/Vertex/Bedrock unary native paths are scoped to their scripted fixtures.
Voyage rerank has a successful mapped target request, but no complete native
client round-trip in this snapshot, so its additive native-client row remains
`unknown`. A separate OpenAI Chat reasoning-effort request against Anthropic
Messages is `incompatible`: the public strict test observes a precise
`reasoning_budget` refusal and zero additional provider dispatch. That narrow
negative does not classify the qualified plain-text subset as incompatible.

The #214 isolated continuation handoff at `d9c3d729` reports real official
SDK negotiated two-turn tests, encrypted
ready/replay and fault coverage; that source is absent from assessed
`e0534816`, so the frozen OpenAI Responses SDK row and two additive translated
client rows remain `unknown` or `unavailable`. The #229 isolated Gemini
Interactions/Live handoff at `6d361ba8` reports public and pinned SDK HTTP/SSE
and WebSocket tests, including native audio/video, tools, cursor and ownership.
Its eight additive rows remain `unavailable` in this snapshot because those
profiles were not yet merged. The isolated branch explicitly refuses a Live
session-resumption handle; even after the branch merges, that handle must not
inherit an ordinary Live-session success claim. These handoffs establish
isolated execution only; the matrix's portable record is this paragraph and
the pinned branch revisions, not an integrated test or a production provider
observation.

## Final #218 refresh procedure

After #214, #229 and #216 are integrated, assess the **merged PR revision**
again. Keep the 47 frozen row IDs and hashes unchanged. For each row, compare
its full feature set with executed public service, fault, official SDK and
packaged/Vite browser results. Update `assessed_revision`, constraints,
status, `evidence_state`, test-source hash, receipt hash and exact run revision
only when the combined-tree result warrants it. Add new versioned rows for any
new operation/profile/mode/client combinations; never remove difficult or
rejected frozen rows. Convert isolated receipts to integrated evidence only
after the source and final tests exist on the merged PR and actually run.
Publish the admitted/completed/refused/incomplete/ambiguous/unknown counts and
the frozen original, barrier, lifecycle and media performance comparisons
separately. A failing frozen budget stays failed until a same-condition rerun
passes without changing the denominator or threshold. The current original
slow-relay p99 inter-event-gap miss and the two failed barrier candidate
captures remain visible in their versioned artifacts. Final G1/G7 also need
the paired no-inference migration shadow, mixed-version restore, generated
contracts, full service/SDK/browser/CI runs and mandatory review; this ledger
alone does not close those gates.

Every row's `empirical_quality` is `unknown`. The preregistered
[quality study](quality-plan.md) has zero approved live trials. Scripted local
providers prove selected protocol behavior, not equal intelligence or live
latency. A future paid study requires its separately approved serving-identity
and cost/privacy scope and must add new versioned evidence without changing
this snapshot's results.
