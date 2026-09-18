# M5 evidence: providers, native protocols, and routing

Implemented and qualified on Linux amd64 on **2026-09-16**. This report records
the Go implementation of the retained
[non-media matrix](../../compatibility.md) and
[routing contract](../../provider-routing.md). It does not certify paid provider
accounts. The native arm64 qualification gap inherited from M1–M4 remains a
separate release-platform gate.

## Protocol and connector coverage: M5-01 through M5-06

[`internal/protocols`](../../../internal/protocols/) owns native envelopes,
canonical messages, tools, parameters, usage, and bounded stream translation.
The original OpenAI codecs remain the native OpenAI path. Anthropic Messages
and Gemini generation preserve unmodeled native fields, including nested
extensions; translation refuses semantic request fields that cannot be
represented. Native responses retain extensions. Translated responses retain
modeled text, refusals, tool relationships, candidates, finish reasons, and
usage, and intentionally omit unmodeled provider response extensions.

Generation is unary or streaming. OpenAI Chat and Responses, Anthropic Messages,
and both Gemini API versions share authentication, admission, route selection,
one execution deadline, credential attempt budgets, commitment, cancellation,
and accounting. Input-token counting, embeddings, and moderation use those
same boundaries. Model list/get are gateway-owned, key-filtered route reads
under `models_read`; inference requires `inference`. Retired OpenAI prefixes
and LiteLLM routing headers are not accepted as additional serving controls.

| Connector | Addressing and authentication | Discovery and certification |
| --- | --- | --- |
| OpenAI | Shared pinned HTTP transport; bearer, encrypted headers, or explicit no-auth | Models; both Chat and Responses prove an OpenAI generation capability |
| OpenAI compatible | Same transport with reviewed vendor profiles | Catalog-specific discovery or explicit configured-model proofs; only OpenAI tuples |
| Anthropic | Messages, count_tokens, API key/version headers, custom native endpoints | Bounded paginated model reads and native or translated capability proofs |
| Gemini | generateContent, streamGenerateContent, countTokens; Google API-key header | Bounded paginated models with token-limit facts |
| Azure OpenAI | Resource origin, exact deployment mapping, calendar API version, api-key | Configured deployment proof, generation or embeddings; no generic models assumption |
| Vertex AI | Project/location publisher addressing, stored service account or ADC | Explicit configured-model count proof; shared Gemini codecs |
| Bedrock | Region, model ID/ARN, SigV4, static or ambient credential chain | Foundation-model listing and Converse/ConverseStream/CountTokens proofs |

[`connectors.Supports`](../../../internal/connectors/capabilities.go) is the
reviewed transport matrix; publication still requires successful tuple-specific
certification. Generic endpoints cannot inherit cross-protocol or media
certification. DeepSeek, Fireworks, DeepInfra, Hugging Face, Perplexity, Cohere,
and Voyage retain distinct catalog identities and operation/parameter limits.
Chat-only profiles translate Responses and normalize token-limit aliases.
Cohere preserves seed support and refuses its unsupported controls. Voyage
accepts text embeddings, maps dimensions, preserves truncation, and normalizes
float/base64 output at the gateway.

The request/response matrix, original native-extension fixtures, candidate and
tool relationships, refusal output, completion sequencing, malformed/truncated
streams, usage completeness, embedding encodings, and aggregate stream-state
bounds are exercised by
[`parity_test.go`](../../../internal/protocols/parity_test.go) and
[`stream_parity_test.go`](../../../internal/protocols/stream_parity_test.go).
[`provider_parity_test.go`](../../../tests/integration/provider_parity_test.go)
creates and publishes all seven connector kinds through real management APIs,
then exercises every maintained non-media surface/operation/mode tuple and
explicit refusal through the actual gateway, including native key headers,
ADC, AWS signatures, model rewriting, route-header removal, and scope isolation.

Counting reports known input tokens and zero output tokens. Embeddings report
input-only usage. Moderation does not invent usage. Missing usage or missing
required price components retain M4's explicit completeness/unpriced state.
Every selected attempt pins an immutable pricing revision or explicit unpriced
selection, independently of later pricing publication.

## Cloud dependencies and bounds: M5-03 and M5-04

The cloud implementation uses maintained Go packages for Google OAuth/ADC,
AWS credential discovery (including process and SSO), regional endpoint rules,
SigV4, and AWS event-stream framing. Bedrock control and runtime addresses use
the maintained service endpoint resolvers, including China, GovCloud, and
isolated partitions; no SDK inference client or retry loop is constructed. Inference remains on the existing pinned, no-proxy, no-redirect HTTP
transport. AWS credential clients use one SDK attempt; credential acquisition
has a ten-second ceiling inside the executor's remaining attempt deadline.
Google token refresh uses the context-aware cloud authentication API and
synchronous refresh. One shared ADC detection task prevents canceled requests
from spawning repeated environment-detection probes.

Google OAuth token endpoints use a separate public-only transport: provider
HTTP/private-network exceptions cannot relax them. Google compute metadata is
owned by the maintained metadata client and token retrieval receives the same
caller deadline. AWS's fixed link-local credential endpoints are allowed only
on the authentication transport. HTTP token-exchange bodies on the injected
transports are limited to one MiB; cloud-library errors and all dynamic token,
key, session, and signature values are redacted before returning upstream
errors or recording diagnostics.

[`auth_test.go`](../../../internal/connectors/auth_test.go) proves service-account
JWT exchange and refresh, ADC files, deadline cancellation, token endpoint
egress isolation, static AWS signing, process credentials, cached SSO role
credentials, and bounded token-exchange responses. The seven-connector service
matrix additionally exercises ambient Google metadata and signed Bedrock
inference. Regional resolution is covered by
[`bedrock_endpoint_test.go`](../../../internal/connectors/bedrock_endpoint_test.go);
[`deployment_parity_test.go`](../../../internal/providers/deployment_parity_test.go)
checks that Chat and Responses certification retain the same single deployment
mapping in both request bodies and paths. These are local identity fixtures, not live cloud qualification.

The Bedrock exception reconciliation below includes a scope correction recorded
on 2026-09-18. The frozen media exception concerned image parts inside generation
inputs, not the separate image/audio/video operation tuples.

| Reference exception | Go evidence and disposition |
| --- | --- |
| `NO_BEDROCK_STRUCTURED_OUTPUT` | Non-text structured formats are refused before dispatch; request matrix tests |
| `NO_BEDROCK_CACHED_USAGE` | Input/output/total usage is retained; cached-input breakdown is not invented |
| `NO_BEDROCK_REQUEST_ID` | AWS transport request IDs are not substituted for a protocol completion ID |
| `NO_BEDROCK_RESPONSE_BOUND` | Strengthened: unary HTTP bodies use the executor bound; event advertised lengths are checked before SDK allocation, SDK CRC checks remain, translated frames and aggregate retained tool state are bounded |
| `NO_BEDROCK_MEDIA` | Changed: Go accepts inline base64 PNG/JPEG/GIF/WebP image parts in generation and token-count inputs; remote URLs, image-detail controls, invalid base64, other formats, and system/tool-result images are refused. Separate media operation tuples remain unavailable. |

Bedrock remains translated on every client surface. Inline image support is an
explicit extension of the frozen text/tool-only encoder, qualified by
[`bedrock_test.go`](../../../internal/protocols/bedrock_test.go); shared input
bounds are covered by
[`inline_media_test.go`](../../../internal/protocols/inline_media_test.go).
Tool schemas/results retain the Converse constraints. CountTokens preserves
tools in its converse input. The advertised
one-GiB event fixture is rejected before allocation, corrupt CRCs and truncated
streams fail, and no success marker follows a failed translation.

## Configuration, model facts, and policies: M5-07 and M5-08

Connection options are bounded to one MiB and preserve model canonical identity,
modalities, context/output limits, supported parameters, deployment, region,
quantization, and evidenced privacy facts. Public metadata materializes schema
defaults while preserving unknown, explicit false, explicit empty parameter
lists, and exact integers. Discovery
merges sparse upstream facts without erasing operator facts or enabled/certified
state. Pagination has one deadline, model/cursor limits, and repeated-cursor
rejection. Vendor endpoints without model APIs prove explicitly configured
models. Bulk certification has a one-minute operation bound and cancellation;
per-model outcomes and retry selections remain visible in the console.

Encrypted credentials/headers remain write-only. Reserved transport, tracing,
and routing headers and envelope defaults are refused. Caller values, including
explicit nulls and competing token/dimension aliases, take precedence over
provider defaults. Custom private or HTTP endpoints still require explicit
operator exceptions and use pinned DNS with redirects forbidden. Configured
encrypted Anthropic version/beta headers retain precedence over protocol defaults.

`OLP_CONNECTOR_CONFIG_FILE` accepts the existing shared providers-list format
and restricted credential files. Gateway-only mounted operation may omit the
master key. Mounted transports retain published capabilities, quotas, model
facts, and slot restrictions; only transport addressing, headers, defaults, and
deployment mappings change. The published default-slot ID is required; an older
release must be republished before mounting rather than inferring identity from
a mutable slot name. Enabled named pools require the master key.
Database encrypted revisions remain authoritative when the master key exists.
[`mounted_parity_test.go`](../../../tests/integration/mounted_parity_test.go) proves
real serving without a master key, preserved quota/fact ownership, restricted
secret files, and readiness through a usable named slot after default revocation.
The Helm value, schema, and workload environment are updated together.

[`0007_routing_policies.sql`](../../../internal/database/migrations/0007_routing_policies.sql)
adds persisted installation, route-draft, and API-key policies plus immutable
route-revision policy snapshots. Scope permissions, entity ETags, idempotent
mutation replay, audit, staged draft policy, immediate installation/key
publication, revision comparison, and restoration follow the existing
management lifecycle. The management contract is regenerated from OpenAPI;
`RoutingPreferences` is a closed object with all supported fields.

Hard constraints intersect installation, route, key, defaults, and request.
Unknown model facts fail affirmative requirements. Allowed strategies also
intersect. Preference precedence is request, key, route, installation. A request
cannot add targets, broaden key access or hard constraints, lengthen deadlines,
or increase the published attempt budget. Exactly one bounded JSON
`X-OLP-Routing` is accepted; raw values are neither forwarded nor persisted.
Only a policy digest, chosen strategy/vendor, and pricing pin enter durable
attempt provenance.

## Selection, pools, and console: M5-09 and M5-10

[`PlanRequest`](../../../internal/runtime/plan.go) and
[`SelectSlots`](../../../internal/runtime/slots.go) are shared by draft/published
simulation, playground preparation, and execution. Priority and preferred-order
tiers precede weighted, exact-decimal price, fresh latency, or throughput
ordering. Unknown prices/measurements sort last; ties fall back to the retained
weighted rendezvous function. Fallback limits apply across real credential
attempts, and preview enumerates each eligible slot within the same budget.
Live revocation, cooldown, and quota capacity can change after a preview.

Price selection reuses M4's provider/vendor/kind specificity, effective time,
and immutable revision precedence. Measurements come only from successful
persisted attempts in the last five minutes. Latency needs twenty samples;
throughput independently needs twenty known output counts and excludes
reasoning tokens. Stream latency begins at meaningful output, excluding setup
and usage frames. Inputs refresh every ten seconds and become unknown after
sixty seconds without refresh.

A 401 cooldown follows credential-version identity; a 429 follows logical-slot
identity across rotation. Shared coordination is authoritative when configured;
successful validation clears shared cooldowns. Endpoint circuits permit one
half-open probe, and credential-only failures release that probe without
penalizing sibling credentials. Disabled defaults can coexist with independently
validated enabled named slots.

The console supports native/cloud/vendor editors, model comparison and grouped
route drafts, per-model validation cancellation/retry, route-group cancellation
between mutations, staged policies, strategy/preferences, and exclusion/price/
performance/slot explanations. The retained provider-routing browser journey
runs against both packaged and Vite Go servers and includes Azure deployment
proof, bulk model validation, a standby credential, grouped targets, policy
exclusion, preview credential ordinals, publication, and playground execution.

## Qualification record

Qualified on Linux amd64 on **2026-09-16**, using Go **1.27.1**, Node
**26.8.2**, pnpm **11.24.0**, a native C toolchain, and the Rust-tool guard.
The [machine-readable record](provider-routing-qualification.json) and
[passing logs](provider-routing-qualification.tar.gz) retain the evidence.

| Gate | Result |
| --- | --- |
| `make go-check` | Passed: generated contracts, Go formatting/vet/tests, console formatting/type/lint checks, and 55 Vitest files / 480 tests |
| Complete race-enabled PostgreSQL/Valkey integration package | 129 top-level tests passed in 419.083s, including authenticated/TLS services, all seven connectors, policy publication, metrics, limits, accounting, recovery, and process modes |
| Mounted serving without a master key | Passed against real services; includes quota/fact ownership, file permissions, and named-pool readiness after default revocation |
| Official JavaScript SDKs | OpenAI 7.4.0, Anthropic 0.116.0, Google GenAI 2.16.0: success and typed-error contracts passed |
| Chromium | All 10 journeys passed across packaged and Vite Go servers in 4.0m |
| Final regional addressing/authentication/provider checks | `go test -race ./internal/connectors ./internal/providers` and `go vet ./...` passed; final native build passed |
| Helm | Chart lint passed with the mounted configuration value |

Service, SDK, and browser qualification use deterministic local provider and
identity fixtures. They do not imply paid cloud account certification. Native
Linux arm64 qualification remains the inherited M1–M4 release-platform gate.

Browser evidence: [credential pool](m5-provider-credential-pool.png),
[model comparison](m5-provider-model-comparison.png),
[policy and credential-attempt preview](m5-provider-routing-preview.png), and
[playground routing](m5-provider-playground-routing.png).

## Build and dependency review

[Build measurements](provider-routing-builds.json), [raw five-run records](provider-routing-builds.tar.gz),
[dependency classification](provider-routing-dependencies.json), and
[native build metadata](provider-routing-go-build-info.txt) record the final
cloud dependency graph. Measurements use disposable source snapshots,
prefetched downloads, a private compilation cache emptied for clean builds,
and an explicit warmup plus owned backend edit for incremental builds. The OS
page cache is shared. Each reported median has five successful runs.

| Native backend case | M5 Go median | M5 range | Earlier Go foundation median | Frozen Rust median |
| --- | ---: | ---: | ---: | ---: |
| Clean build | 41.32s | 40.57–47.33s | 28.78s | 228.06s |
| Owned backend edit | 6.09s | 5.80–6.68s | 4.14s | 21.12s |

The recorded host is Linux / Intel Haswell, eight logical CPUs, approximately
24.6 GB RAM; the comparison archive records the same host class and C toolchain.
The Go foundation and frozen Rust reference were measured on September 13;
M5 was measured on September 16. M5 remains about **5.5× faster for clean builds**
and **3.5× faster for the backend edit** than frozen Rust. The foundation
contains fewer features, so its difference is not an isolated estimate of
cloud-library cost.

The Go production graph now has **448 packages and 35 external modules**;
three additional modules are test-only and fifteen are generator-only. The
inventory distinguishes those roles from module-graph-only dependencies.
Maintained Google auth/metadata and AWS auth, process/SSO, signing, endpoint
rules, and event-stream packages account for the cloud implementation; the
bounded shared HTTP executor owns actual inference and retries. The native
unstripped `-trimpath` binary is **50,455,672 bytes**. GLIDE's existing
prebuilt Rust archive and CGO/glibc requirements remain separately inventoried;
normal guarded Go builds do not compile Rust.
