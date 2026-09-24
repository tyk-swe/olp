# Cohere native v2 operation scope

The versioned `cohere-embed-v2` and `cohere-rerank-v2` profiles compose the
existing direct-compatible HTTP and API-key components with separate native
operation contracts. The `cohere-native-v2` preset points to
`https://api.cohere.ai/v2`; an operator may configure an authorized custom v2
base. The existing `cohere` OpenAI-compatible preset and legacy generation,
embedding and rerank adapters remain available. The official compatibility
preset is rejected under a native v2 profile instead of silently appending
`/embed` to `/compatibility/v1`.

The operation-owned codecs keep one immutable OIF request and result source.
They bind only the published model identity and retain all other native request
and result bytes, including unknown authorized extensions, numeric spelling,
member order and provider metadata. Text policy rejects unknown/uninspectable
native content before dispatch. Native Embed v2 supports one of `texts`,
`images`, or ordered multimodal `inputs`, the documented task/typed-storage/
dimension/truncation controls, and the per-dtype `embeddings` map. Integer,
packed-binary, base64, and multi-type outputs require
`raw-vector-storage/1`; no vector is cast to float. Numeric and packed groups
must agree on one logical dimension across dtypes and inputs. Cohere base64
output retains validated original bytes without an assumed float32 width: its
logical dimension stays unknown unless explicitly requested or established by
another native result group. Native Rerank v2 preserves
query/document strings, `top_n`, token cap, priority, original input indices,
provider result order, exact score lexemes/ties, and the native
`meta.billed_units.search_units` category. Both requests use the existing
Attempt, route, slot, authorization, egress, policy, and accounting owners.

The contracts follow [Cohere Embed v2](https://docs.cohere.com/reference/embed)
and [Cohere Rerank v2](https://docs.cohere.com/reference/rerank). They do not
claim sparse or token-multivector Cohere output, a portable cross-provider
mapping, live model quality, or provider-side exactly-once behavior. The
unchanged frozen Cohere embedding row combines sparse/multivector features not
defined by this native API and must **not** be promoted from this evidence.
TEI remains the independently qualified native sparse/multivector path.

## Proposed additive matrix rows after final-source integration

| Row ID | Operation/profile/dialect | Mode/client | Qualified features and evidence |
| --- | --- | --- | --- |
| `cohere-v2-embed-text-typed-raw-http` | embeddings; `cohere-embed-v2/1`; dialect `cohere-embed-v2/1`, upstream API v2 | unary; raw HTTP with `raw-vector-storage/1` for non-float/multi-type | Exact text task, float/int8/ubinary groups and opaque base64 source, common numeric/packed dimensions, input ordering, billed input tokens; `TestStrictCohereNativeEmbedV2PreservesTypedStorageAndBilling`, `TestCohereDocumentedNativeStorageGroupsShareLogicalDimensions`. |
| `cohere-v2-embed-multimodal-raw-http` | embeddings; same profile/dialect | unary; raw HTTP | Ordered text and original image data URI in `inputs`, float result and malformed/unqualified/corrupt refusals; `TestStrictCohereNativeEmbedV2RetainsMultimodalInputAndRejectsCorruption`. |
| `cohere-v2-rerank-raw-http` | rerank; `cohere-rerank-v2/1`; dialect `cohere-rerank-v2/1`, upstream API v2 | unary; raw HTTP | Original query/documents, top-N, token cap/priority, ordered native indices/scores/ties, search-unit accounting and extension refusal under restrictive policy; `TestStrictCohereNativeRerankPreservesResultsAndBillsOnce`, `TestStrictCohereNativeRejectsForeignControlsAndUninspectablePolicyFields`. |

The public Go tests provision and certify a provider with an explicit model,
activate strict routes and keys through management, and dispatch through the
real gateway to an independently scripted local provider. The fixture asserts
final `/v2/embed` or `/v2/rerank` path, authentication, provider-bound source,
client-visible result, one dispatch and usage. Wrong dtype, missing vector,
wrong echoed text, malformed input and foreign controls fail with zero or one
appropriate provider dispatch and no fabricated success. Focused connector
unit tests check profile metadata, official v2 endpoint composition, refusal
of the official compatibility/v1 preset, and preservation of the old
compatible-chat path.

The console's native-operation playground offers both dialects, a Cohere
text/typed-storage example and a Cohere rerank example. It displays each
native embedding dtype group as separate input-indexed storage rows while
keeping the [reviewed result screenshot](console-cohere-native.png) and full
original provider JSON available. The screenshot uses a mock browser response;
public Go tests above establish backend dispatch and semantic behavior. The
focused `cohereNativeOperation.test.ts` and `cohere-native.spec.ts` exercise
browser request routing, storage groups, exact numeric spelling and ranking
ties. Final matrix/fixture inventory assessed revision and exact-head CI are
owned by the integrated release receipt; no frozen row or historical evidence
was edited in this slice.
