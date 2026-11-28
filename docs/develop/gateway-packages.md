# Gateway packages

`internal/gateway/kernel` is the execution kernel. It owns the `Call`, the route
leg, the attempt loop, settlement, the generation routes and the retained steps
that read call or pin state, which surfaces call without reaching into internal
fields. A package under `internal/gateway/surface/` owns the routes of one
surface and calls the kernel through its exported API. `internal/gateway` itself
is the assembly, whose `Register` mounts the kernel and each surface. The other
packages hold what the kernel uses and never import it back.

| Package | Owns | May import |
| --- | --- | --- |
| `gateway/fault` | client-visible error values, per-surface error bodies, rate-limit headers and committed stream writing | none |
| `gateway/events` | the terminal request envelope, attempt facts and the log and accounting sinks | none |
| `gateway/quota` | limiter admission, typed key, route, target and aggregate reservations, cost reservations and supply | `events`, `fault` |
| `gateway/ingress` | concurrency slots, API key authentication, attribution, end-user identity and bounded body reading | `fault` |
| `gateway/health` | circuit state, credential cooldowns and fleet health | `events` |
| `gateway/codeforward/coderesearch` | bounding, method filtering and event-ID sealing for the subscription research MCP surface | none |
| `gateway/codeforward/codexwire` | Codex wire request classification, response observation and passive allowance header parsing without rewriting payloads | none |
| `gateway/codeforward` | `/code/{slug}` forwarding and the research proxy | `events`, `fault`, `ingress`, `quota`, `codeforward/coderesearch`, `codeforward/codexwire` |
| `gateway/traceingest` | OTLP trace ingest on `POST /v1/traces` | none |
| `gateway/delegation` | delegation grant creation on `POST /v1/delegations` | `fault`, `ingress` |
| `gateway/models` | the model catalog on the OpenAI, Anthropic and Gemini surfaces | `fault`, `ingress` |
| `gateway/guardstream` | streamed output guardrails, with the tool call and text framing of each wire family in its own file | none |
| `gateway/realtimeresponses` | which responses a realtime session still owes usage for, and the usage each terminal frame reports | `ingress` |
| `gateway/publishedprompt` | replacing a published prompt reference with the rendered messages and defaults in each wire family's form | `fault`, `ingress` |
| `gateway/generationrefs` | where a generation request names provider resources, and rewriting the ones a result or a stored input item list returns to the caller's identifiers | none |
| `gateway/anthropicbeta` | validating and forwarding `Anthropic-Beta` headers across compatible route targets | `fault` |
| `gateway/duplexrelay` | shared bidirectional WebSocket relay loop, bounded upstream writer, delegation reauth and abnormal teardown | `fault`, `ingress` |
| `gateway/managedbatch` | managed batch JSONL line translation, manifest metadata and Gemini batch record and result codecs | `fault` |
| `gateway/kernel` | the `Call`, admission, the attempt loop, pinned exchanges, settlement, native and generation dispatch, and retained steps such as retained dispatch, page admission and result projection | every top-level gateway package above except `duplexrelay` |
| `gateway/healthprobes` | the health probe worker, which picks opted-in targets, claims each once per interval across the fleet, sends one small request through `kernel.ProbeGeneration`, and shares the result and the fleet circuit | `health`, `kernel`, `quota` |
| `gateway/surface/anthropicstate` | Anthropic files and message batches | `fault`, `kernel` |
| `gateway/surface/bedrock` | Bedrock Converse and Invoke under `/bedrock/model/{model}` | `fault`, `kernel` |
| `gateway/surface/geminilive` | Gemini Live sessions under `/gemini/ws/` and `/ws/` | `duplexrelay`, `fault`, `ingress`, `kernel`, `quota` |
| `gateway/surface/media` | OpenAI images, audio and video jobs under `/v1/images`, `/v1/audio` and `/v1/videos`, including media execution, delivery and video job lifecycles | `events`, `fault`, `kernel`, `quota` |
| `gateway/surface/nativeduplex` | Native duplex sessions under `/native/{dialect}/sessions` | `duplexrelay`, `fault`, `ingress`, `kernel` |
| `gateway/surface/openaistate` | OpenAI files, batches, stored responses and continuations | `fault`, `generationrefs`, `ingress`, `kernel` |
| `gateway/surface/realtime` | OpenAI realtime WebSocket sessions and WebRTC calls under `/v1/realtime` | `duplexrelay`, `fault`, `ingress`, `kernel`, `quota`, `realtimeresponses` |
| `gateway/surface/retrieval` | Cloud retrieval queries under `/v1/retrieval/{binding_id}/query` | `fault`, `kernel` |
| `gateway/surface/cloudbatches` | Vertex and Bedrock batch jobs under `/native/{dialect}/batch-jobs` | `fault`, `ingress`, `kernel` |
| `gateway/surface/geminiinteractions` | Gemini Interactions under `/gemini/v1beta/interactions`, whole and streamed | `fault`, `kernel` |
| `gateway/surface/geministate` | Native Gemini files, resumable uploads and batches, including `:batchGenerateContent` | `fault`, `kernel` |
| `gateway/surface/guardrails` | Standalone guardrail evaluation under `/v1/guardrails/apply` | `fault`, `kernel` |
| `gateway/surface/passthrough` | Pass-through routes under `/native/{dialect}/routes/{route}/` | `fault`, `ingress`, `kernel` |
| `gateway/surface/retained` | Retained provider resources from the `retainedapi` catalog, such as fine-tuning jobs, vector stores, conversations, containers, evals, Anthropic skills, Gemini cached contents and agents, and Vertex tuning jobs, plus `/v1/vector_stores/{id}/search` and `/v1/responses/compact` | `fault`, `ingress`, `kernel`, `quota` |
| `gateway/surface/playground` | The console playground, which runs a route for a signed-in member through `kernel.MemberGeneration` and exposes `Handle` and `Stream` to the management API | `fault`, `kernel`, `publishedprompt` |

Each gateway package below `gateway` lists the gateway packages it may import in
an `imports.txt` beside its code, one per line, and an empty file allows none.
`scripts/check-lint.sh` fails on an import the file does not list and on any
such package without the file, so a new gateway package declares its imports
when it is added. The last column above describes those files. Most surface
packages follow `surface/anthropicstate`, exporting one
`Register(k *kernel.Server, mux *http.ServeMux)`, keeping handlers on an
unexported `surface` type that holds the kernel, and registering in
`internal/gateway/gateway.go`. Three surfaces take extra collaborators or wire
into management instead. `retained.Register(k *kernel.Server, mux *http.ServeMux, retrievalQuery http.HandlerFunc)`
takes the retrieval query handler for vector store search.
`media.Register(k *kernel.Server, mux *http.ServeMux, deps *Deps)` takes its
`*media.Deps` bundle through `gateway.WithMedia`.
`playground.NewAPI(k *kernel.Server) *API` builds the playground API whose
`Handle` and `Stream` handlers `cmd/olp` mounts on the management API mux.

The kernel never names a surface or a vendor flag for one. A surface that needs
something from a shared step sets a general field on the `Call`, as
`PinnedHeaders` carries the Anthropic file header. A kernel step that differs by
provider asks the provider's `connectors.Config` or its `domain/vendors`
contract, as `ResponsesPath` and `BatchWindowFor` do, and
`scripts/check-lint.sh` refuses a kernel comparison of a provider kind or vendor
with a literal. The refusal matrix in `internal/gateway/surfaces_test.go`
registers the same assembly, so it covers a surface's routes after it moves.

The kernel owns the `/gemini/{version}/models/{model}:{action}` route because
generation lives there. A surface that serves one of those actions returns it
from an `Actions` function, as `surface/geministate` does for
`batchGenerateContent`, and the assembly passes the map to `RegisterKernel`.

A handler in a surface reads as a list of steps. `surface/geminiinteractions` is
the example for a request that admits, pins, sends and answers. Its handler
calls one named kernel step per stage, such as `ReserveGeneration`,
`GatePinnedGeneration` and `SettleGeneration` in `kernel/pinned_generation.go`,
and a surface calls kernel steps without reading or writing the `Call`'s
admission or settlement fields. When a surface needs a stage the kernel does not
name yet, add the step to the kernel and call it.

A step takes what it needs as arguments and returns what it found, so its
dependencies appear in its signature. The request value is built once and never
written again, as `surface/retained/request.go` shows. The only state a handler
writes is what its deferred settlement must still read when a step panics, such
as the pin and the status, and that state lives in one small type that says so,
like `retainedEnd`.

Put new code in the kernel only when every surface needs it. Code that reads a
few values from the `Call` takes them as a config struct and moves beneath the
kernel, as `guardstream.Config` does. A handler that can be built from its own
collaborators belongs in its own package, as `codeforward.Forwarder`,
`traceingest.Ingester`, `delegation.Handler` and `models.Handler` are. The
delegation handler takes the ingress gate, the key authority and the kernel
header preamble through its constructor, so it tests without a `Server` while
its responses keep the request identity and CORS headers every inference
response carries. Session reauthentication for delegated calls stays in the
kernel because it reads the `Call` it reauthenticates.
