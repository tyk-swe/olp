# Gemini Interactions and Live v1beta qualification

This record covers the native Gemini lifecycle foundation extracted as #229.
It qualifies the two registered direct Gemini profiles separately from
`gemini-generation`. It does not claim all media/resource, durable resumption,
provider-model quality, or whole-spec G3/G4/G6/G7 qualification; #216 owns the
remaining lifecycle and media combinations.

The source contracts are Google's [Interactions overview](https://ai.google.dev/gemini-api/docs/interactions-overview),
[Interactions streaming guide](https://ai.google.dev/gemini-api/docs/streaming),
[Interactions API reference](https://ai.google.dev/api/interactions-api-v1), and
[Live WebSocket reference](https://ai.google.dev/api/live), read on 2026-09-22.
The exercised first-party clients are pinned in the repository at
`@google/genai` 2.16.0 and `google-genai` 2.22.0. Source fingerprints from the
installed pinned packages are:

| Client source | SHA-256 |
| --- | --- |
| JS `dist/node/index.mjs` | `0b59221ea7789a095d98b95712595a0afb2eb6e9c25f6275ce2e46271a643459` |
| Python `google/genai/live.py` | `dc803d1fe594dba731dd302373adcb232d604e9836346b0ccfbc0c8c26440b57` |
| Python Interactions `generationconfig.py` | `537512c68effae6d8c5bb008e01c242c57747e122857ac5066f0778ab21c78ad` |
| Python Interactions `interactionsseevent.py` | `ec634ea861a8962e669105895b0d6c31ab3bcaa6aa2829d61a093c8f88ba0fd3` |

`gemini-interactions` binds only the direct v1beta Interactions HTTP/SSE
contract. Requests keep source JSON, including ordered `steps`, opaque thought
signatures, tool definitions, native numbers, explicit null, and per-invocation
controls. Omitted `store` retains Google's `true` default. A key without
`allow_provider_state` must explicitly send `store:false`; a retained or
background request fails before provider dispatch. `previous_interaction_id`
must be an owned local ID on the same route. The corresponding provider ID is
encrypted with the existing installation key and committed with the resource
row before the public local ID appears. The owner, active route, historical
provider revision/slot, current credential and network authority are checked
again for continuation, GET, cancel and delete. GET supports a native event
cursor; a cursor may resume within a step without inventing earlier events.
Delete removes the encrypted ID and tombstones the local mapping in one
transaction after upstream success. The local retention limit is 24 hours.

`gemini-live` binds only the direct v1beta
`GenerativeService.BidiGenerateContent` WebSocket contract. OLP authenticates
the SDK's `?key=` or `x-goog-api-key` form before upgrade, then reads and
validates the first setup message before contacting the provider. A bounded
upstream setup acknowledgement precedes all other server events. Later
`clientContent`, `realtimeInput` and `toolResponse` frames retain native JSON,
their client-frame order, and base64 audio/video bytes without resampling or
transcoding. A tool call is delivered before its matching response returns.
Synchronous writes,
read limits, one session deadline, periodic reauthorization and ping bound
memory and slow peers. The public `/gemini` base path supports the JavaScript
SDK's custom-origin WebSocket construction and Python's WSS/header-key shape.
The qualification explicitly observes the pinned JavaScript SDK's origin-only
`//ws/...?...` upgrade failure and its supported `/gemini/ws/...?...` path, plus
Python's `/gemini/ws/...` WSS/header-key path. The TLS CA includes CA signing
key usage and server authentication extensions; neither SDK disables
certificate verification.

The independent local provider in
`tests/integration/gemini_lifecycle_test.go` certifies both profiles through
the public management API and records actual upstream HTTP requests and
WebSocket frames. It checks two-turn affinity and controls, native step/SSE
order, exact audio/video bytes, native tool calls and results, VAD/interruption
and turn markers, a slow reader, background acceptance/retrieval/cancellation,
encrypted metadata, restart, owner separation, revocation, ciphertext tamper,
expiry, malformed setup, duplicate auth, and pre-dispatch policy refusal. The
real pinned
JavaScript and Python clients in `tests/sdk-smoke/gemini-lifecycle.mjs` and
`tests/sdk-smoke-python/gemini_lifecycle.py` run over a disposable trusted TLS
listener through public OLP. They exercise two turns, streaming and cursor
retrieval, GET/cancel/delete, and Live audio/interruption. The provider-side
assertions verify the second request carries the original provider ID and
omits prior-turn tools/generation settings, and that OLP's client key never
reaches the provider. The fixed 47-row reference denominator and historical
performance budgets are untouched.

Current admitted boundary: a nonempty Live `sessionResumption.handle` is
rejected before provider dispatch because a session-bound encrypted mapping is
not yet installed. Provider file/media resource acquisition and full queued or
batch lifecycle combinations remain with #216. Local scripted fixtures prove
wire preservation and safety for the declared combinations; they do not
measure a real Gemini model's latency, quality or billing.
