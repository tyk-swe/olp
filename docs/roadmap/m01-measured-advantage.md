# M1: Measured advantage

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | None | [M2](m02-provider-catalog.md) (calibration factors), [M6](m06-cost-management.md) (cost estimates), [M11](m11-operator-ecosystem.md) (client environments) | [M3](m03-routing-resilience.md), [M7](m07-guardrails.md) |

OLP's architecture should make it faster and more correct than LiteLLM, but the
repository has no gateway latency benchmark and qualifies clients only through
SDK smoke suites. This milestone makes both claims measurable, publishes the
results, and turns them into the regression gates that every other milestone
must pass.

## Outcome

- A reproducible benchmark compares OLP with a pinned LiteLLM release on
  identical hardware and workloads, including LiteLLM's own published
  scenarios, and OLP wins every comparative scenario.
- Hot-path performance is regression-gated in CI, and every feature has a
  performance budget.
- Admission token estimates are exact where a public tokenizer exists and
  calibrated elsewhere, so limits and context-window checks rest on accurate
  numbers.
- The coding agents and frameworks LiteLLM documents run against OLP under a
  qualification suite.
- Responses carry standard rate-limit headers and opt-in gateway metadata.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Performance | No gateway latency measurements; [capacity qualification](../deployment.md#capacity-qualification) describes memory bounds, not latency | [8 ms P95 at 1k RPS](https://docs.litellm.ai/docs/benchmarks); a [high-throughput profile](https://docs.litellm.ai/docs/proxy/high_throughput) sustaining 3,000 RPS with 50K–100K-token prompts on 33 pods; an opt-in [Rust core](https://docs.litellm.ai/docs/proxy/rust_gateway) |
| Token estimates | Four characters per token, a flat charge per media part, plus the allowed output ([limits](../gateway.md#limits-and-budgets)) | Rust token counting in the high-throughput profile |
| Client coverage | Official JavaScript and Python SDK smoke suites for OpenAI, Anthropic and Google Gen AI, and an AWS SDK integration test for the Bedrock surface ([SDK tests](../../tests/README.md#sdk-and-live-provider-tests)) | Guides for [Claude Code, Codex and other clients](https://docs.litellm.ai/docs/proxy/client_setup/overview) and [agent SDKs](https://docs.litellm.ai/docs/agent_sdks) |
| Response metadata | `X-Request-Id`, plus `Retry-After` and `X-Should-Retry` on errors; no rate-limit or cost headers, and upstream response headers are not passed through | [Response headers](https://docs.litellm.ai/docs/proxy/response_headers) with rate-limit, cost and deployment details |

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
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens: 16`, key with a cost budget, 3,000 RPS | LiteLLM's high-throughput benchmark, including admission token estimation and the budget check |
| S4 | First target returns 503, second succeeds, on a transformed route | Failover cost |
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
- S6: every stream with a healthy reader completes, stalled readers are closed
  at the 30-second write deadline, resident memory per 1,000 open streams is
  published and stays flat for the run, and the goroutine count returns to its
  baseline after the streams close.
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

The budget binds every milestone from the day M1.1 ships. A workstream that
ships earlier is measured when M1 closes, and a regression found then is fixed
in M1.

### M1.2 Accurate admission token estimates

Admission reserves tokens before dispatch, and the planner excludes targets
whose context cannot fit the estimate. Today's heuristic in
[`internal/gateway/limits.go`](../../internal/gateway/limits.go) over-reserves
for some families and under-reserves for others. Cost budgets do not use the
estimate: they refuse a request only once accrued spend has reached the limit,
so concurrent work can overshoot. [M6.3](m06-cost-management.md#m63-cost-estimation)
adds the first control that prices the estimate.

- A tokenizer registry in a new `internal/tokenizer` package provides byte-pair
  encoders for the public OpenAI encodings (`o200k_base`, `cl100k_base`), with
  rank files embedded in the binary and reviewed under the
  [dependency policy](../../CONTRIBUTING.md#dependency-policy). Message framing
  uses the documented per-message overhead. The existing
  `internal/operations/tokenization` package stays what it is: the native
  token-count operations, whose contract excludes estimates.
- Families without a public tokenizer (Anthropic, Gemini, most hosted
  open-weight models) use the heuristic scaled by a per-family factor from the
  [reference catalog](m02-provider-catalog.md#m24-reference-catalog). Until
  that catalog ships, the factor is 1.
- The embeddings, rerank and classification codecs, which carry their own
  four-characters-per-token estimates, use the same registry.
- Each attempt records the estimate, its provenance (`tokenizer`,
  `calibrated` or `heuristic`) and the reported usage. Usage reports expose the
  estimation error by route and model family, and route simulation shows the
  estimate and its provenance.
- The validated source is parsed once. Estimation is cached by the effective
  request after target-specific translation, defaults and content-policy
  changes, plus tokenizer or model family and calibration revision. Each
  target obtains its own demand before context eligibility and token-limit
  reservation; attempts reuse a count only when those inputs match.

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
  `anthropic-ratelimit-tokens-*` on the Anthropic surface. The Gemini and
  Bedrock protocols define no such headers, so those surfaces send none. The
  limit scripts gain remaining counts and reset times in their reply, which
  carries neither today, so the headers cost no extra Valkey round trip. Keys
  without limits send no rate-limit headers.
- **Upstream headers.** Provider rate-limit headers describe the provider
  credential, not the caller's key, and stay withheld as they are today.
- **Gateway metadata.** A key policy, `response_metadata`, opts into
  `X-OLP-Attempts` (attempts made), `X-OLP-Route-Revision` (the serving route
  revision), `X-OLP-Cost` (the priced cost of a unary response, in the
  installation currency) and `X-OLP-Provider` (the vendor that served the
  request). Provider identity stays hidden unless the key opts in, preserving
  the rule that callers address routes, not upstreams.
- Headers are written before the body. Streaming responses cannot carry cost,
  which remains available through the request history API.

## Non-goals

- Optimizing for synthetic peaks at the expense of the bounded-memory and
  fail-closed guarantees in [deployment](../deployment.md).
- Calling provider token-counting endpoints on the admission path.
- Qualifying clients against live providers. The suite proves wire
  compatibility against fixtures; live behavior stays with the
  `liveproviders` tests.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| Token estimate and its provenance per attempt | PostgreSQL attempt records | Request retention | None; metadata only |
| Benchmark results | `docs/performance.md` | Versioned with the repository | None |
| Tokenizer rank files | Embedded in the binary | Per release | None; public data |

## Change map

| Change | Start here |
| --- | --- |
| Benchmark suite and comparison | new `tests/bench/`, new `scripts/bench-compare.sh`, `Makefile`, `scripts/qualify-image.sh` |
| Token estimates | `internal/gateway/limits.go`, new `internal/tokenizer/`, the estimates in `internal/operations/` |
| Client qualification | new `tests/clients/`, `tests/sdk-smoke/`, `tests/sdk-smoke-python/` |
| Rate-limit headers | `internal/limits/scripts/`, `internal/limits/results.go`, `internal/gateway/` |

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

- [ ] **M1.1** `make bench` and `scripts/bench-compare.sh` reproduce every
      scenario from a clean checkout, and `docs/performance.md` publishes the
      results.
- [ ] **M1.1** OLP meets every [target](#targets) above.
- [ ] **M1.1** CI fails a pull request that regresses a hot-path microbenchmark
      beyond the [budget](#performance-budget).
- [ ] **M1.2** Estimates for OpenAI encodings match the provider-reported
      prompt tokens exactly on the text fixtures of the protocol corpus, and
      usage reports show estimation error for every family.
- [ ] **M1.2** Two translated targets with different tokenizers, framing or
      defaults obtain distinct effective demand where appropriate; context
      eligibility and reservations use that demand, while repeated attempts
      with identical effective inputs reuse the estimate.
- [ ] **M1.3** The client qualification suite passes in CI for every client
      above, and `docs/clients.md` documents each configuration.
- [ ] **M1.4** Rate-limit headers appear on the OpenAI and Anthropic surfaces
      and match the key's Valkey windows in integration tests; metadata headers
      appear only when the key opts in.
- [ ] The [parity matrix](parity.md) performance rows are `Parity` or better.
