# Native classification contracts

These codecs interpret immutable OIF spans for three distinct operations:
OpenAI moderation, TEI prediction/classification and TEI similarity/scoring.
The original request and result remain authoritative. Result views retain
category identity, duplicate prediction labels, positions, numeric spelling,
threshold metadata and applied input scope. No score normalization, ordering,
candidate splitting, threshold inference or cross-provider equivalence is
performed. Evidence IDs identify the scoped fixture contracts, not live model
quality or every possible provider interaction.

TEI shape rules come from the pinned revision
[`29ccc53ba56c9b4f4de8f19a14858d527fab680d`](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/types.rs)
and its [native handlers](https://github.com/huggingface/text-embeddings-inference/blob/29ccc53ba56c9b4f4de8f19a14858d527fab680d/router/src/http/server.rs).
The archived source hashes were verified against the implementation manifest.
A flat one/two-string prediction input is a single sequence; nested arrays are a
batch. Similarity retains the source sentence and whole comparison set.

OpenAI moderation structure and joint multimodal result scope were checked
against the [primary API reference](https://developers.openai.com/api/reference/typescript/resources/moderations/methods/create)
on 2026-09-22. Category names are native data rather than a fixed proxy enum.
Older results may omit applied-input-type metadata; reported metadata is
retained. Thresholds are never manufactured from scores.

Input/output text hooks are used only when a policy applies. Image inputs,
unknown input extensions and model-owned TEI prompt prefixes cannot silently
claim full text-policy coverage. Unknown output text and category names are
inspected, while opaque result scopes refuse inspection. These checks block;
they never redact or repair the native source.

`classification_test.go` uses independently authored native JSON and corrupt
counterexamples. It tests source conservation, array/result cardinality,
precision, nullable/omitted controls, category correspondence, duplicate labels,
policy coverage, immutable public accessors and model-only overlays. Parent
operation registration, runtime and public-provider qualification remain owned
by the #215 integration slice.
