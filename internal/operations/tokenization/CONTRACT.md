# Native token-count and tokenizer contracts

The operation-owned views preserve the native source for OpenAI input-token
counting, Anthropic message counting, Gemini token counting, Bedrock token
counting and TEI tokenization. They have no generation or SDK dependency.
Native counts are results of the counting operation, never estimated counts or
provider billing usage. `Estimate` remains a separate bounded request-size
estimate; no definition installs a Usage hook for native count results.

Counts retain their original field names, scope and exact numeric tokens.
Integer validation uses bounded exact arithmetic instead of floating point.
Gemini cached and per-modality counts retain their categories without summing
or merging them. TEI tokens retain IDs, order, text, special flags and nullable
offsets; the codec does not infer offset units or normalize Unicode.

The native request branches and fields were checked on 2026-09-22 against:

- [OpenAI input token API](https://developers.openai.com/api/reference/typescript/resources/responses/subresources/input_tokens/methods/count).
- [Anthropic message token API](https://platform.claude.com/docs/en/api/typescript/messages/count_tokens).
- [Gemini token API](https://ai.google.dev/api/tokens).
- [Bedrock CountTokens API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_CountTokens.html).
- [Pinned TEI native types](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs)
  and [handlers](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/server.rs).

The pinned TEI archive hashes were verified. These sources establish native
wire structure; no live or paid model qualification is implied. Evidence IDs
refer to the local fixture scope.

Gemini contents and nested generateContentRequest stay distinct. The nested
model is validated against the selected route before a serving-model overlay;
absent/null nested model members stay absent/null. Bedrock converse and opaque
invokeModel bodies are separate union variants, with base64 source retained.
Known provider-owned continuation/file/cache references fail with
`resource_affinity` until an authorized resolver exists. This does not classify
arbitrary JSON-schema properties or tool arguments as provider resources.

Native unknown data remains available when no text policy needs to interpret
it. Policy hooks inspect effective text and tool/schema strings, refuse unknown
input node kinds, opaque assets, encrypted history, provider-owned tool prompts,
and model-configured TEI prompt prefixes. Function argument JSON is inspected
without changing its original representation. Output hooks include unknown text
and property names. A refusal never fabricates a native count.

`tokenization_test.go` independently asserts exact integers beyond float64
precision, integer spelling, zero/null/empty/omitted distinctions where valid,
union shape, offsets, corruption rejection, serving/resource affinity, policy
coverage and immutable public views. Management, runtime, SDK and provider-wire
integration are tested outside this package.
