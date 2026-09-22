# Native SDK reasoning and tool continuation

The #221 slice qualifies the existing stateless Anthropic forwarding path through
the real gateway and both pinned official clients. It reuses the unchanged
[native stream](../../../tests/fixtures/fidelity/v1/anthropic-tool-workflow.sse)
and [next-request reference](../../../tests/fixtures/fidelity/v1/anthropic-tool-next-request.json).
These are independently authored synthetic contracts; the signature and model
are fixture values, not paid-provider captures.

Each client consumes all 19 native events, lets its SDK assemble the final
assistant message, and retains five ordered blocks: thinking/signature, text,
weather call, clock call, and trailing text. It executes both local tools and
passes the **actual SDK-assembled content** plus both identified tool results
back through the SDK's normal request serializer. The provider compares that
entire next request against the frozen reference, including prior history,
thinking budget, token limit, tool descriptions/schemas, ordered blocks and
opaque signature. It then returns a final answer that the client verifies.
Each client run asserts exactly two provider dispatches, one verified initial
request, one verified next request, two tool results and zero fixture rejections.

The initial request takes the frozen controls/tools and first user turn, adding
`stream: true`. Permitted gateway edits are route/model identity rewriting and
provider credential injection. The provider requires `anthropic-version:
2023-06-01`. The JSON oracle ignores only whitespace and object member ordering;
array order, presence, values and opaque strings remain exact. Stream transport
chunks deliberately split the frozen events without modifying their bytes.
Python's SDK emits additional convenience events; its count assertion selects
the native event types while retaining the SDK's normal assembly behavior.

The existing SDK fixture uses a static runtime release and has no resource
store, resolver or OLP continuation helper. Capture counters live only in the
scripted provider fixture and are read through a separate loopback verification
listener. No verification endpoint is installed in the gateway. The previous
OpenAI, Anthropic and Gemini success, stream, model, token-count and error checks
remain in both smoke suites.

Validation executed on 2026-09-22 against runtime base `bc460cbe` plus this test
slice, with dependencies installed from the unchanged frozen lockfiles:

| Client | Anthropic | Existing OpenAI | Existing Google GenAI | Result |
| --- | --- | --- | --- | --- |
| JavaScript | `@anthropic-ai/sdk` 0.116.0 | 7.4.0 | 2.16.0 | Full smoke suite and native next-turn workflow passed |
| Python | `anthropic` 1.4.0 | 3.8.0 | 2.22.0 | Full smoke suite and native next-turn workflow passed |

Commands executed successfully:

```sh
./tests/sdk-smoke/run.sh
./tests/sdk-smoke-python/run.sh
go test -mod=readonly -race -count=1 ./tests/sdkfixture ./tests/fidelity
go vet ./tests/sdkfixture
```

The fixture's corruption checks reject missing signatures/history/tool
descriptions, reordered text, mismatched call identities and changed reasoning
budgets. They establish that the next-request comparison is exercised, alongside
the real SDK executions. The independent frozen references and SDK inventory
are unchanged. No paid inference was used.

This receipt does not complete G2. Explicit strict admission, negotiated
translated carriers, unsupported-client rejection, durable recovery and
acceptance/actionability fault boundaries remain in #214. The native fixture
retains the existing legacy route contract and does not establish empirical
model quality or complete interaction qualification for other clients.
