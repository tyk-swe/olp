# Console interaction and operation evidence

Qualification date: 2026-09-22. This record covers the console behavior added
for OLP-implementation-spec.md D21–D22 and ticket #217. It does not claim live
provider intelligence parity or a performance gate.

The saved-draft and published-route inspectors call the existing management
simulation endpoint. A blank native request is labeled tuple-only. An actual
request uses the same prepared planner as strict execution and reports each
target's eligibility, ingress/OIF/egress/return dialects, serving binding,
allowlisted exact scalar values, default provenance, semantic context,
dispositions and execution/client obligations. The new bounded structural view
reads the **prepared** native document: ordered instruction scope, roles and
block kinds, plus numeric correspondence between explicit native tool-call IDs
and later results. Source text, schema property names, tool IDs/arguments,
opaque reasoning and unknown native field names are absent from the response.
The backend test mutates private markers, parallel tool order, exact large
numbers and the structure bounds. The browser checks the inspected native
request and then verifies the scripted provider saw zero dispatches.

Provider capabilities are displayed per operation, surface and mode as
discovered identity, operator declaration, server contract test, stale or
unknown evidence. Certification timestamps are shown only where the API
reports them. Inspector evidence remains scoped to the selected plan; neither
connectivity nor the plan asserts empirical model quality. Provider and route
revision comparisons now expose semantic defaults, serving bindings, network
configuration and fidelity changes. The schema-driven native editors and
their ETag/dirty-draft behavior remain the authority from the earlier console
configuration qualification.

The management playground projection remains unavailable for strict routes.
For the qualified OpenAI Chat → Anthropic tool contract, the console uses a
user-entered inference API key held only in page memory and the public
`chat-anthropic-tools-v1` carrier. Tool calls become available only after the
ready continuation marker. Ordered observations and ordinary assistant/tool
messages are retained for the next request; the user supplies one result per
call. A failed delivery can recover the same submission, and ready earlier
turns can be selected for a new result branch. The UI does not execute tools or
show encrypted native state. A ready carrier's `olp.native_usage` is retained
with exact numeric spelling and shown separately from ordinary Chat usage; a
focused client test covers large cache counts and `-0`. The real Chromium test confirmed that a bare
client was rejected with `state_carrier` and zero provider calls, while the
negotiated two-turn browser workflow made exactly two provider calls and sent
the frozen direct-native second request, including thinking signature,
before/after text and both ordered results. Neither the API key nor the native
signature appeared in the rendered interaction or provider credential capture.

Strict unary operations use the bounded registered public
`/native/{dialect}/models/{route}` path with a page-memory inference key.
Embedding requests explicitly declare `raw-vector-storage/1`; the browser
retains the exact native response bytes and never converts packed/integer/
sparse/multivector storage to float values. Vector layout, declared dtype,
logical dimensions and observed storage are separate columns. Rerank rows
retain result order, input index/ID and exact score spelling, including ties.
Native counts are labeled separately from estimates and billed usage. The
public browser fixture proved that a packed vector without the explicit client
contract failed before dispatch, while the supported client returned exact
base64 storage, `9007199254740993`, `-0`, and native rerank scores through one
provider call per operation. Raw result JSON is downloadable without a display
conversion. The Vite proxy and Helm ingress now route `/native` to the gateway.

Retained media jobs show only recorded lifecycle milestones and the retention
deadline, without presenting a poll as a provider completion timestamp or
downloading assets. The realtime event viewer is a local import of a native
event array: it preserves VAD, turn and interruption order plus exact timing,
while hiding audio/text payloads, IDs and unknown event names. It makes no
network call. A browser cannot supply the bearer WebSocket header required by
the public realtime endpoint; live sessions remain an SDK workflow.

Validation on an isolated combination of PR branch `c11ce0fe` (including
registered operation #215), continuation branch `65fa4fc2`, and console
branch through `2b827e59` (temporary qualification HEAD `402726d1`):

- `go test ./internal/gateway ./internal/routes ./internal/oif ./internal/interaction ./internal/operationplan ./internal/resources` passed after the two feature branches were combined. The combined OpenAPI source regenerated cleanly; Svelte typecheck/ESLint and focused component/client tests passed.
- `make test-console` passed all 644 console unit/component tests on the console branch.
- Chromium packaged and Vite projects each passed all four `strict-interaction.spec.ts` journeys: plan/tool continuation, exact operation-result presentation, registered public vector/rerank execution, and local realtime event order. Real public provider requests and pre-dispatch refusals were asserted in the browser journey, not inferred from component mocks.
- The existing `accounting.spec.ts` serial Chromium file passed both tests, including the retained media detail, at 320 px and 1440 px with zero Axe violations. Its disposable database prefix is now honored by the test seeding path.
- `helm lint deploy/helm` and a rendered Ingress with a trusted-proxy CIDR passed; `/native` maps to the gateway service.

Reviewed screenshots: [effective plan](console-effective-inspection.png),
[strict tool continuation](console-strict-tool-continuation.png),
[native vector](console-native-vector.png),
[native rerank](console-native-rerank.png),
[realtime event order](console-realtime-events.png), and
[retained media lifecycle](console-media-lifecycle.png).

The Chromium run used the committed continuation checkpoint `65fa4fc2`.
`olp.native_usage` was added by #214 after that checkpoint, so the new usage
display has focused client coverage and still needs the final integrated
browser rerun. The browser operation-result presentation test with a mocked management
response verifies exact numeric/source rendering without claiming upstream
behavior. The real public vector/rerank journey above supplies that separate
provider-wire proof. The frozen native tool request/event corpus was not
rewritten for the console.
