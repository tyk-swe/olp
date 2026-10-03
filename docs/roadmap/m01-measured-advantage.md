# M1: Measured advantage

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | None | [M3](m03-routing-resilience.md), [M7](m07-guardrails.md), and the performance gate of every milestone |

OLP's architecture should make it faster and more correct than LiteLLM, but
nothing in the repository measures either claim today. This milestone makes
both measurable, publishes the results, and turns them into regression gates
that every later milestone must pass.

## Outcome

- A reproducible benchmark compares OLP with a pinned LiteLLM release on
  identical hardware and workloads, including LiteLLM's own published
  scenarios, and OLP wins every comparative scenario.
- Hot-path performance is regression-gated in CI, and every feature has a
  performance budget.
- Admission token estimates are exact where a public tokenizer exists and
  calibrated elsewhere, so limits, budgets and context-window checks rest on
  accurate numbers.
- The coding agents and frameworks LiteLLM documents run against OLP under a
  qualification suite.
- Responses carry standard rate-limit headers and opt-in gateway metadata.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Performance | No published measurements; [capacity qualification](../deployment.md#capacity-qualification) describes memory bounds, not latency | [8 ms P95 at 1k RPS](https://docs.litellm.ai/docs/benchmarks); a [high-throughput profile](https://docs.litellm.ai/docs/proxy/high_throughput) sustaining 3,000 RPS with 50K–100K-token prompts on 33 pods; an opt-in [Rust core](https://docs.litellm.ai/docs/proxy/rust_gateway) |
| Token estimates | Four characters per token, a flat charge per media part, plus the allowed output ([limits](../gateway.md#limits-and-budgets)) | Rust token counting in the high-throughput profile |
| Client coverage | Official JavaScript and Python SDK smoke suites for OpenAI, Anthropic and Google Gen AI ([SDK tests](../../tests/README.md#sdk-and-live-provider-tests)) | Guides for [Claude Code, Codex and other clients](https://docs.litellm.ai/docs/proxy/client_setup/overview) and [agent SDKs](https://docs.litellm.ai/docs/agent_sdks) |
| Response metadata | `X-Request-Id` | [Response headers](https://docs.litellm.ai/docs/proxy/response_headers) with rate-limit, cost and deployment details |

## Scope

### M1.1 Gateway benchmark

A benchmark suite under `tests/bench/`, behind the `bench` build tag and a
`make bench` target, drives an OLP process against a deterministic mock
upstream. `scripts/bench-compare.sh` runs the same scenarios against a pinned
LiteLLM image configured with an equivalent virtual key, budget and model list.

The load generator is open-loop: it schedules requests at a constant arrival
rate and records latency from the scheduled send time, so a stalled gateway
cannot hide queueing delay (coordinated omission). Latencies are recorded in
high-dynamic-range histograms. Gateway overhead is measured as the difference
from a direct-to-mock baseline run in the same session.

The mock upstream serves OpenAI, Anthropic and Gemini dialects with configurable
time to first token, inter-token interval, output length, status codes and
mid-stream failures. It runs on a separate host or CPU set so that it never
competes with the gateway.

#### Scenarios

| ID | Scenario | Why |
| --- | --- | --- |
| S1 | Chat Completions, short prompt, unary, 1,000 RPS | LiteLLM's headline 8 ms P95 scenario |
| S2 | Chat Completions, short prompt, streaming 64 tokens at 20 ms intervals, 1,000 RPS | Time-to-first-token overhead and stream relay cost |
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens: 16`, key with a cost budget, 3,000 RPS | LiteLLM's high-throughput benchmark, including admission token estimation and budget reservation |
| S4 | First target returns 503, second succeeds | Failover cost |
| S5 | Anthropic Messages streaming translated to an OpenAI upstream | Translation cost on transformed routes |
| S6 | 10,000 concurrent streams with slow readers | Memory, goroutines and write-deadline behavior at scale |

Every run records added latency at p50, p95 and p99, time-to-first-token
overhead, sustained RPS per vCPU, resident memory, allocations per request, the
error rate, and request-metadata completeness (events delivered versus
requests admitted).

#### Targets

- S1 to S5: OLP's added latency is lower than LiteLLM's at p50, p95 and p99,
  and OLP sustains more RPS per vCPU, on identical hardware.
- S1 on a 2-vCPU gateway: added latency at most 2 ms at p95 and 5 ms at p99.
- S3: 3,000 RPS at a 100% success rate, using fewer total vCPU than LiteLLM's
  high-throughput profile at equal or better p95.
- S1 to S3 with healthy Valkey: zero lost request-metadata events.

A target the first baseline misses becomes M1 scope (profiling and hot-path
work), not a revised target.

#### Performance budget

- Hot-path microbenchmarks (`testing.B`) cover authentication, admission,
  planning, credential selection, the codecs and stream relay. CI compares them
  with `benchstat` against the merge base and fails on a statistically
  significant regression above 10% in time or allocations per operation.
- A feature that is not configured adds no allocations and no Valkey or
  PostgreSQL round trips to a request.
- A configured feature declares its own budget in its milestone, measured by
  the scenario it affects.
- Full scenario runs are part of release qualification
  ([`scripts/qualify-image.sh`](../../scripts/qualify-image.sh)), and their
  results are published in a new `docs/performance.md` with the exact
  hardware, versions and configuration needed to reproduce them.

### M1.2 Accurate admission token estimates

Admission reserves tokens before dispatch, the planner excludes targets whose
context cannot fit the estimate, and budgets reserve cost from it. Today's
heuristic over-reserves for some families and under-reserves for others.

- A tokenizer registry in `internal/operations/tokenization` provides
  byte-pair encoders for the public OpenAI encodings (`o200k_base`,
  `cl100k_base`), with rank files embedded in the binary and reviewed under the
  [dependency policy](../../CONTRIBUTING.md#dependency-policy). Message framing
  uses the documented per-message overhead.
- Families without a public tokenizer (Anthropic, Gemini, most hosted
  open-weight models) use the heuristic scaled by a per-family factor from the
  [reference catalog](m02-provider-catalog.md#m24-reference-catalog). Until
  that catalog ships, the factor is 1.
- Each attempt records the estimate, its provenance (`tokenizer`,
  `calibrated` or `heuristic`) and the reported usage. Usage reports expose the
  estimation error by route and model family, and route simulation shows the
  estimate and its provenance.
- Estimation runs once per request and is cached for every attempt, including
  translated targets.

### M1.3 Client and agent qualification

A qualification suite under `tests/clients/` replays the wire behavior of the
clients operators actually deploy, against deterministic upstream fixtures, and
asserts that each completes its documented happy paths through OLP.

| Client | Surface exercised |
| --- | --- |
| Claude Code | Anthropic Messages streaming, tool use loops, `count_tokens`, `Anthropic-Beta` features, prompt caching |
| Codex CLI | Responses streaming, reasoning items, tool calls, stored-response continuation |
| Gemini CLI | `streamGenerateContent`, `countTokens`, function calling |
| OpenAI Agents SDK | Responses with tools, handoffs and structured outputs |
| Vercel AI SDK, LangChain, LlamaIndex | Chat, embeddings and tool calling through their OpenAI, Anthropic and Google providers |
| Official Go SDKs | `openai-go`, `anthropic-sdk-go` and `google.golang.org/genai` success and typed-error contracts, alongside the existing JavaScript and Python suites |

A new `docs/clients.md` gives a tested configuration for each client and names
the client versions the suite pins. Pinned versions advance through the
dependency policy, so a client release that breaks compatibility fails CI
before users notice.

### M1.4 Response metadata

- **Rate limits.** Responses carry the caller key's remaining request and token
  allowance in the header family each surface's SDK already reads:
  `x-ratelimit-limit-requests`, `x-ratelimit-remaining-requests`,
  `x-ratelimit-reset-requests` and their `-tokens` counterparts on the OpenAI
  surface, and `anthropic-ratelimit-requests-*` and
  `anthropic-ratelimit-tokens-*` on the Anthropic surface. Values come from the
  admission reservation's reply, so they cost no extra Valkey round trip. Keys
  without limits send no rate-limit headers.
- **Gateway metadata.** A key policy, `response_metadata`, opts into
  `X-OLP-Attempts` (attempts made), `X-OLP-Route-Revision` (the serving route
  revision), `X-OLP-Cost` (the priced cost of a unary response, in the
  installation currency) and `X-OLP-Provider` (the vendor that served the
  request). The headers name the provider only for a key that opts in,
  preserving the rule that callers address routes, not upstreams; an upstream's
  own error message is relayed after credential redaction and is not scrubbed.
- Headers are written before the body. Streaming responses cannot carry cost,
  which remains available through the request history API.

## Non-goals

- Optimizing for synthetic peaks at the expense of the bounded-memory and
  fail-closed guarantees in [deployment](../deployment.md).
- Calling provider token-counting endpoints on the admission path.

## Decisions to settle

1. The reference hardware for published results (recommended: one
   general-purpose cloud instance family, pinned in `docs/performance.md`).
2. The pinned LiteLLM release and its configuration for comparisons
   (recommended: the latest stable release when M1 starts, with its documented
   production settings and the high-throughput profile for S3).
3. The tokenizer implementation: an audited third-party Go package or an
   in-repository encoder (recommended: in-repository, since the algorithm is
   small and the hot path matters).

## Exit criteria

- [ ] `make bench` and `scripts/bench-compare.sh` reproduce every scenario from
      a clean checkout, and `docs/performance.md` publishes the results.
- [ ] OLP meets every [target](#targets) above.
- [ ] CI fails a pull request that regresses a hot-path microbenchmark beyond
      the [budget](#performance-budget).
- [ ] Estimates for OpenAI encodings match the provider-reported prompt tokens
      exactly on the text fixtures of the protocol corpus, and usage reports
      show estimation error for every family.
- [ ] The client qualification suite passes in CI for every client above, and
      `docs/clients.md` documents each configuration.
- [ ] Rate-limit headers appear on both surfaces and match the key's Valkey
      windows in integration tests; metadata headers appear only when the key
      opts in.
