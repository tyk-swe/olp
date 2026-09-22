# Registered unary operation qualification

This is the scoped deterministic evidence for [#215](https://github.com/tyk-swe/olp/issues/215) and D03–D05/D14–D15. It covers the registered, immediate unary paths below. The fixed historical inventory and the 47-row fidelity denominator remain unchanged. A registered codec or a fixture by itself is not evidence of live model quality.

## Contract and public path

`internal/operations` registers separate immutable request/result views for embeddings, rerank, moderation, classification, scoring, and tokenization/counting. Each codec owns its native schema, validated defaults, model overlay, text-policy locations, result correspondence, usage evidence, probe, and native path. `internal/operationregistry` composes trusted codecs and the two qualified mappings. The gateway selects a registered operation through the published route and existing Attempt, key/slot quota, egress, credential, accounting, and revocation owners. It does not use a Generation request as the operation representation or split candidate/input sets.

The native public entry is `/native/{dialect}/models/{routeSlug}`. Existing OpenAI, Anthropic, and Gemini ingress paths select their corresponding registered operation on strict routes. Both require a published provider profile, certified model capability, strict route, authorized API key, and a compiled target plan. Unknown dialects, mismatched route/model identity, unsupported surfaces, ambiguous JSON, and unqualified mappings fail before provider dispatch. The management profile catalogue publishes operation dialects and default schemas; published-route inspection binds the same contract without inference.

The original generation profile set is **openai-chat, openai-responses, compatible-chat, compatible-responses, anthropic-messages, gemini-generation, azure-legacy-chat, azure-legacy-responses, azure-v1-chat, azure-v1-responses, vertex-gemini, vertex-anthropic, bedrock-converse, and bedrock-anthropic-invoke**. The existing non-strict Bedrock Invoke control remains separate. The cloud-profile integration fixture still executes all 14 generation profiles in strict and non-strict runs, and Bedrock Invoke in the non-strict run. Its selection now excludes only the following 14 new unary-only profiles, each of which has an independently authored public request/result case:

| Added unary-only profile | Public behavior test |
| --- | --- |
| `gemini-batch-embeddings` | `TestStrictNativeHostedEmbeddingShapesPublic` |
| `openai-embeddings` | `TestStrictOperationsPinnedSDKStorage` |
| `openai-input-tokens` | `TestStrictClassificationAndNativeCountPublic` |
| `openai-moderation` | `TestStrictClassificationAndNativeCountPublic` |
| `rerank` | `TestStrictNativeRerankScoresAndIdentityPublic` |
| `tei-classification` | `TestStrictClassificationAndNativeCountPublic` |
| `tei-embeddings` | `TestStrictNativeSparseAndMultivectorPublic` |
| `tei-multivector-embeddings` | `TestStrictNativeSparseAndMultivectorPublic` |
| `tei-rerank` | `TestStrictNativeRerankScoresAndIdentityPublic` |
| `tei-scoring` | `TestStrictClassificationAndNativeCountPublic` |
| `tei-sparse-embeddings` | `TestStrictNativeSparseAndMultivectorPublic` |
| `tei-tokenize` | `TestStrictClassificationAndNativeCountPublic` |
| `voyage-embeddings` | `TestStrictNativeVectorStoragePublic`, `TestStrictOperationsPinnedSDKStorage` |
| `voyage-rerank` | `TestStrictOperationPolicyRefusalPreservesUpstreamOutcome`, `TestStrictQualifiedUnaryOperationsPublic` |

| Operation | Independently exercised public native behavior |
| --- | --- |
| Embeddings | OpenAI float/base64, Voyage signed/unsigned integer and packed binary, TEI dense/sparse/multivector, Gemini single/batch task controls, Vertex ordered predictions and ADC, Bedrock binary and SigV4. Requests and native results retain their source bytes except the declared route-model identity overlay and absent-only defaults. |
| Rerank | Compatible, Voyage, and TEI native ordered document indices, optional original documents, exact score lexemes and ties; no universal score range or result-set splitting. |
| Moderation, classification, scoring | OpenAI category names, values, applied input types and thresholds; TEI pair/batch prediction labels and raw scores; TEI similarity result order and exact scores, including JSON numbers outside `float64` range. |
| Token counting | OpenAI, Anthropic, Gemini, and Bedrock native counts are result data, distinct from billed usage or estimates; TEI tokenizer retains token IDs, text, special flags and native offsets. |

The only registered cross-dialect unary mappings here are OpenAI float embeddings to Voyage float embeddings and compatible rerank to Voyage rerank. Unsupported dtype/task/default collisions and structured document identities fail with a scoped incompatibility. Native unknown fields stay in their original dialect where policy can cover them; a foreign field is not silently mapped to another dialect.

For the later #218 extension demonstration, route validation accepts a trusted registered operation label beyond the historical list, and a registered unary profile can use a trusted relative path under direct-compatible hosting. Model certification, runtime compilation, public admission, and provider dispatch all query that operation contract. New operation/surface names enter weighted rendezvous with length prefixes, so distinct new labels do not share affinity scores; the original generation, rerank, batch, realtime, Bedrock and other established tuples retain their prior score bytes. This seam still requires a public new-operation fixture; the registry unit tests alone do not complete T10.

Integer, packed, sparse and multivector storage requires the explicit `raw-vector-storage/1` client contract. The pinned JavaScript and Python OpenAI SDKs inject base64 on omitted embedding encoding and decode it to float32 arrays; their wire `encoding_format` and User-Agent do not prove caller storage capability. Public SDK tests exercise both actual parser paths, an explicit base64 response, a raw client with the versioned contract, and pre-dispatch refusals without it. Native dtype, packing, logical dimensions, stored bytes, input order and exact numeric source spans remain separate in the operation views.

Input block policy checks the effective request, including declared defaults. Native prompt prefixes and unknown opaque inputs that cannot be inspected under a configured text rule fail before dispatch. Output block or coverage refusal is a local policy outcome after a completed upstream response: the client receives no successful provider result, provider-native usage is retained when present, the upstream state remains terminal, and another Attempt is not used to evade the rule. A malformed or corrupt provider result is a distinct protocol failure, with no partial success.

## Reference provenance and limits

The [Voyage embedding API](https://docs.voyageai.com/reference/embeddings-api.md) and pinned [TEI HTTP types](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs) and [handlers](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/server.rs) were retrieved on 2026-09-22. Their captured source SHA-256 values were `a6fb146a2d4a91194d7e91bc1fac95359cf33b13230685bb2b2c16733d7c200d`, `cea385b5f1bec3e22f7de2394b39318bad6f7bd8a6de4b76a9590527783a624b`, and `ec2f3ca50b6f1b8d51b2b7429a53f1b71e79f5521fad69707a8ed0fd3d0fe077`, respectively. The public tests use hand-authored requests and provider results, capture the actual local upstream body/path/authentication, and compare it with those expectations. No production codec generates the expected side.

These tests exercise scripted local providers, not paid or real model inference. Serving aliases, model quality, provider drift, durable work, files, media, Gemini Interactions/Live, and duplex behavior require their separately scoped evidence. The full G3/G7 and performance gates are evaluated with the complete implementation PR; this slice makes no such claim by itself.

## Validation

The source tests under `internal/operations`, `internal/operationplan`, and `tests/integration/strict_operations_test.go` cover representation, corruption, policy, public configuration/certification/dispatch, SDK behavior, accounting, and no-inference inspection. Service tests use a disposable PostgreSQL database with a scripted local provider. An explicitly selected integration test requires `OLP_TEST_DATABASE_URL` and fails if it is missing.

`node scripts/release-inventory.mjs` passed with 100 management operations, 90 current inference tuples, 133 suite mappings, all 18 frozen fixtures unchanged, and the frozen historical 77-tuple denominator intact. The increase in current tuples is additive; no historical fixture was regenerated. The original cloud-profile public test's strict and non-strict variants passed after scoping its generation invocation to the 14 generation profiles and keeping its Bedrock Invoke control.
