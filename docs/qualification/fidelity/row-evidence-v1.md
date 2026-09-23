# Scoped compatibility execution receipt, version 1

This receipt records scripted local-provider behavior through published routes,
the public gateway and pinned clients. It is an execution record for the named
source revisions, not a live-provider or intelligence-quality result. The
47-row frozen inventory and its SHA-256 remain unchanged.

On `efae6135abc5904617f821b4e8b836a4255a6241`, a selected
`go test -mod=readonly -race -tags=integration -count=1 -v
./tests/integration` run passed 23 named row-evidence tests in 105.732 s. The
selection covered strict published cloud profiles, registered native and
qualified unary operations, official OpenAI vector storage, native media source
rejection, Anthropic document bytes, strict image/audio sources/results/events,
strict batch partial success/error files and official OpenAI JavaScript/Python
clients, strict video original assets and official OpenAI JavaScript/Python
clients, Gemini Interactions/Live public and official JavaScript/Python TLS
clients, and both official OpenAI negotiated-continuation clients. The selected
test names and result are retained in the local execution log
`/tmp/olp-spec-context/final-matrix-row-evidence-race.log`; the source files
and commands are part of the repository.

On `789da4210772a9382a6e253888c1a6eed68404cd`, the new public strict
image-variation subtest passed under `-race` in the certified compatible
fixture. It preserved multipart order, original image bytes/filename, model
binding and the native result. On `d58921db6591516f40d698c99d22228ff434615a`,
the same public media suite and the new same-resource Gemini accepted
background-stream/cursor-recovery test passed under `-race` in 9.381 s. The
Gemini test uses fresh gateway instances and proves one provider POST across
reader loss and cursor retrieval; it does not simulate a killed process or
claim that closing the reader stopped provider work.

The distinct Live nonempty session-resumption handle was sent through the
public WebSocket with a valid key at `efae6135`. The gateway closed it before
provider dispatch; the selected race test passed in 4.248 s. This refusal is
specific to a nonempty native handle without an authorized resource mapping.
Ordinary first Live sessions and empty `sessionResumption` settings have
separate positive public and pinned SDK evidence.

The media/video public fixtures use a locally certified
`openai_compatible` profile, whereas the frozen media/video rows name the
direct `openai` profile. The strict batch and unary background fixtures use
versioned Azure OpenAI profiles. These are additive scoped positives, not
permission to promote the direct OpenAI frozen rows. Strict OpenAI audio
translation is unavailable in this revision because no translation endpoint
exists. Gemini Live native session resumption remains refused. Empirical
model quality remains unknown: no paid calls or approved direct/direct versus
direct/OLP trials were run. Frozen performance outcomes and the final G1–G7
gate audit are reported separately.
