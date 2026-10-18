# M1: Measured advantage

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | None | [M3](m03-routing-resilience.md), [M7](m07-guardrails.md), and the performance gate of every milestone |

At the 0.1.0 planning baseline, the repository did not measure OLP's performance
or compatibility claims against LiteLLM. This milestone makes
both measurable, publishes the results, and turns them into regression gates
that every later milestone must pass.

The benchmark harness, the LiteLLM comparison, the regression gate, the token
estimates, the client qualification suites and the response metadata are
[delivered](#delivered). The full-rate scenarios have not been run on reference
hardware and no reference results are published. `Implemented` records the
code and fixture evidence; promotion to `Shipped` requires the
[open exit criteria](#exit-criteria) and release qualification below.

## Outcome

- A reproducible benchmark compares OLP with a pinned LiteLLM release on
  identical hardware and workloads, including LiteLLM's own published
  scenarios, and OLP wins every comparative scenario.
- Hot-path performance is regression-gated in CI, and every feature has a
  performance budget.
- Admission token estimates are exact where a public tokenizer exists and
  calibrated elsewhere, so limits, budgets and context-window checks rest on
  accurate numbers. As built, exact covers the first 32 KiB of a request's text,
  the rest is calibrated from that part, and the families without a public
  tokenizer keep the four-character heuristic until measured factors enter
  [M2.4](m02-provider-catalog.md#m24-reference-catalog)
  ([decision 3](#decisions-and-qualification-policy)).
- The coding agents and frameworks LiteLLM documents run against OLP under a
  qualification suite.
- Responses carry standard rate-limit headers and opt-in gateway metadata.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

The first column is the baseline this milestone started from; [Delivered](#delivered)
records what has changed since.

| | OLP at 0.1.0 | LiteLLM reference |
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
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens: 16`, key with a cost budget, 3,000 RPS | LiteLLM's high-throughput benchmark, including admission token estimation and cost reservation |
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
context cannot fit the estimate, and cost budgets reserve an estimate of the
request's cost at admission. At 0.1.0 the heuristic over-reserved for some
families and under-reserved for others, and a cost budget compared accrued spend
alone, so concurrent requests could all be admitted against the same unspent
balance.

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
- **Cost reservation.** A request to a key or budget group with a cost budget
  reserves an estimate of its cost in the same Valkey call that checks the
  balance, and is admitted only if accrued spend, plus what requests in flight
  hold, plus the estimate fits every window. The estimate is the most one request
  could cost across the attempts it may dispatch (the sum over every dispatch a
  failover or credential-slot retry may bill), priced from the gateway's pinned price list: input at the highest
  input-side rate and the reply at the output rate for the tokens the request
  allows. Settlement replaces it with the cost of the attempts that reported
  usage, and accounting removes it when the spend lands. The accrued balance
  stays the authority and fails closed: a missing, malformed or wrong-window
  snapshot answers `503 distributed_limits_unavailable`. The reservation itself
  does not: an attempt with no price, and every request while the gateway's price
  list is more than a minute old, reserve nothing and are judged on accrued spend
  alone, so the reservation is not an invoice cap. A key without a cost budget
  makes no extra Valkey call. See
  [cost reservation](../gateway.md#cost-reservation).

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

## Decisions and qualification policy

Each decision below records what was decided and why. Decisions 1 to 3 are the
ones this specification listed; the rest were made during implementation because
the specification text did not settle them.

1. **Reference hardware is a frozen qualification input.** Use dedicated Linux
   hosts from one general-purpose instance family, with disjoint CPU sets for
   gateway, mock, load generator and state services. S1 assigns exactly two
   vCPUs to the gateway. Size S3 and S6 in a preliminary capacity run, then
   freeze the instance SKU, CPU allocation, memory, kernel/socket settings and
   PostgreSQL/Valkey topology before comparing either gateway. Record those
   values in the [reference-run manifest](../performance.md#record-of-a-reference-run).
   A capacity run is not a qualifying result; changing the frozen topology
   requires rerunning both gateways and their direct baselines. This avoids
   selecting hardware from whichever comparison wins. Until such a manifest
   and run exist, all figures in [performance](../performance.md) remain
   labelled smoke results from a shared 8-vCPU virtual machine.
2. **The pinned LiteLLM release and its configuration: settled.** The comparison
   runs `ghcr.io/berriai/litellm:v1.103.2`, pinned by the digest of its
   multi-architecture index,
   `sha256:f63fb81b831b170ec16851e23c36ac5bf52ef106b271406429524a2ed730bbfd`,
   in [`deploy/compose.bench.yaml`](../../deploy/compose.bench.yaml). It was the
   latest stable release when the comparison was built (published 2026-10-01),
   and `main-stable` pointed at the same digest on 2026-10-02. Moving the pin is
   a deliberate change of tag and digest together, and a published comparison
   names the digest it ran. The scenarios use LiteLLM's documented production
   settings ([`deploy/litellm/production.yaml`](../../deploy/litellm/production.yaml)),
   and S3 uses its high-throughput profile
   ([`deploy/litellm/high-throughput.yaml`](../../deploy/litellm/high-throughput.yaml))
   as far as one host can: the Rust token counting, PgBouncer and the two
   sidecars run, and the 33 pods, the autoscaler and the load balancer are not
   reproduced ([what is pinned](../performance.md#what-is-pinned)).
3. **The tokenizer implementation: in-repository.** The algorithm is small and
   the hot path matters, so `internal/operations/tokenization/estimate` holds
   byte-pair encoders for `o200k_base` and `cl100k_base`, with OpenAI's public
   rank files embedded as data and checked against the SHA-256 that `tiktoken`
   publishes ([dependency policy](../../CONTRIBUTING.md#dependency-policy)). The
   split patterns are hand-written scanners, since Go's `regexp` lacks the
   lookahead they use. The ground truth is `tiktoken` itself, run offline by
   `tests/fixtures/tokens/generate.py` and checked in as fixtures, so a test
   compares token ids and the build needs no Python.
   - **Large prompts.** Exact encoding of a 100K-token prompt is too costly for
     the admission path of S3. Measured on the 8-vCPU smoke machine with `go test
     -bench` (five iterations each, so indicative): `o200k_base` counts 100,000
     tokens exactly in 4.8 ms of JSON, 6.0 ms of prose, 10.1 ms of code and 19.6
     ms of multilingual text, against 0.4 to 1.1 ms through the bounded policy;
     at 3,000 requests per second 6 ms is 18 CPU seconds each second for counting
     alone. So the first `ExactBytes` (32 KiB) of a request's text are counted
     exactly, and the rest is charged at the tokens per byte the exact part
     measured and marked `calibrated`. A ratio needs at least `ExactBytes / 8`
     (4 KiB) of exact text; with less, the tail is charged at four bytes to a
     token and marked `heuristic`. The bound is in bytes and not time, and the
     walk keeps only the first 36 KiB of a prompt's text, so a request of
     megabytes is not held twice. The accuracy of the ratio is stated in
     [how the prompt is estimated](../gateway.md#how-the-prompt-is-estimated).
4. **Requests per vCPU is judged on requests per CPU second.** At a constant
   open-loop rate that both gateways hold, sustained requests per second over the
   allotted vCPUs is the same number for both, so it cannot show which is more
   efficient. The target is judged on requests answered successfully per CPU
   second spent above idle, and both figures are printed
   ([requests per vCPU](../performance.md#requests-per-vcpu)).
5. **The S3 target is judged against LiteLLM's published figures and against the
   same profile measured on the same hardware.** One host cannot run 33 pods, so
   the comparison cites LiteLLM's published constants (33 pods, 132 vCPU, 54.029
   ms at p95, never measured here) and, when a comparable run exists, the profile
   measured on the same hardware. OLP meets the target only if it used fewer
   vCPU than 132, its added p95 is at most 54.029 ms, and it is no worse than the
   measured profile. That is stricter than the specification's one sentence
   ([the S3 profile target](../performance.md#the-s3-profile-target)).
6. **Baseline drift allowance.** Each gateway is compared with a direct-to-mock
   baseline taken in its own session, and an enforcing comparison requires the two
   baselines to agree at p50 and p95 within the larger of 1 ms or 10%. The p99
   difference is noted, since a tail varies between sessions even on pinned CPUs.
   The allowance is a judgment, set in `scripts/bench-compare.mjs`.
7. **What each scenario compares.** S6 is excluded from the comparison: no
   target compares it and its socket buffers at 10,000 streams need more TCP
   memory than most hosts have. S4 measures the circuit and not a per-request
   failover: OLP's per-provider circuit opens after five counted failures within
   30 seconds, so the steady state is about one attempt per request, and
   LiteLLM's router is set to the same policy (five failures, thirty seconds, one
   retry) instead of its defaults of three and five. S5 is not like for like: OLP calls the upstream's
   Chat Completions endpoint and LiteLLM its Responses endpoint, and each result
   records which ([how each scenario differs](../performance.md#how-each-scenario-differs)).
   S3 names an OpenAI model so that its prompts are tokenized; the other
   scenarios' models belong to no family and are charged four characters to a
   token.
8. **Release qualification runs the scenarios only on request.**
   `scripts/qualify-image.sh` runs the scenario suite when
   `OLP_QUALIFY_BENCH=1`. The release workflow runs that script on shared
   GitHub-hosted runners with a 45-minute job limit, where six scenarios at full
   rate cannot finish and the numbers would mean nothing, so an unconditional run
   would break the release. Qualification on reference hardware sets the
   variable, pins the CPUs and sets `OLP_BENCH_ENFORCE=1`. No reference-qualified
   candidate-image run is recorded here.
9. **Cost reservation at admission is in this milestone's scope.** The
   specification said budgets reserve cost from the estimate, but at 0.1.0 they
   compared accrued spend only. Reserving at admission, with the estimate that
   M1.2 produces, closes the gap that concurrent requests are all admitted
   against the same unspent balance, so it was added to M1.2 and the sentence
   above now describes the behavior as built: an estimate-based reservation held
   beside an accrued balance that fails closed, with a request that cannot be
   priced, including any request while the price list is more than a minute old,
   reserving nothing and judged on accrued spend alone
   ([cost reservation](../gateway.md#cost-reservation)).
10. **The shared cooldown is read on every attempt when a limiter is
    configured.** The budget says a feature that is not configured adds no Valkey
    round trip. For admission that holds, and `TestUnconfiguredFeaturesAddNoAllocations`
    in `internal/gateway` pins it. But a gateway with `OLP_VALKEY_URL` reads the
    shared credential and slot cooldown before each attempt, whatever the key and
    the target limit, so an unconfigured key still pays one Valkey round trip an
    attempt. It is the existing cooldown design, so it was documented and left
    ([tests](../../tests/README.md#microbenchmarks)).
11. **Allocations per request come from the Go runtime counters.** The gateway has
    no profiler endpoint and an instrumented build would not be the binary under
    test, so the release binary exports `go_memstats_mallocs_total` and
    `go_memstats_alloc_bytes_total` on its private `/metrics` listener, and a
    scenario reports what was allocated over the run, less what an idle gateway
    allocates, per request answered. The per-operation allocations of individual
    paths are gated by the `testing.B` benchmarks.
12. **The regression gate.** `benchstat` is pinned in `scripts/bench-gate.sh`,
    the threshold is 10% in time or allocations per operation (bytes per
    operation are reported and never gate), and a regression fails only if
    measuring it again, alone and with twice the samples, reproduces it, because
    one comparison in twenty reports a change in a benchmark that did not change.
    Removing a benchmark fails the gate unless the pull request carries the
    `allow-removed-benchmarks` label. CI runs each benchmark for 500 ms
    (`BENCH_TIME`) to keep ten samples a side inside the job's hour
    ([regression gate](../performance.md#regression-gate)).

## Delivered

### M1.1 Gateway benchmark

The [harness](../../tests/bench/README.md) implements deterministic peers,
open-loop HDR measurement and S1–S6. `make bench`, `make bench-compare` and
`make bench-gate` expose scenario/comparison/hot-path gates. Reduced-rate
[smoke results](../performance.md#smoke-run) are not reference results.

### M1.2 Accurate admission token estimates

Target-family counting, embedded OpenAI BPE, supported-field walking and bounded
long-tail estimation feed planning/admission. Attempts retain provenance/family;
reports compare estimated/reported input. See [estimation](../gateway.md#how-the-prompt-is-estimated)
and [cost reservation](../gateway.md#cost-reservation).

### M1.3 Client and agent qualification

[Exact client pins/configuration](../clients.md) and the
[qualification harness](../../tests/clients/README.md) cover real SDKs/frameworks/
CLIs against deterministic fixtures. Later code-mode lanes separately exercise
native subscription continuation/WebSockets; these are not live-provider tests.

### M1.4 Response metadata

Key rate-limit allowances and opt-in attempt/revision/provider/cost headers ship
with contract, console and gateway tests. See [response headers](../gateway.md#response-headers).

## Open items

- Full-rate reference-hardware S1–S6, clean-checkout reproduction and pinned
  LiteLLM comparison remain uncollected. Reduced-rate figures cannot close targets.
- Qualify S3's CPU, single-threaded budget scripts and metadata ingestion at scale;
  a key's shared hash tag is not spread by Valkey clustering. Historical local
  script/consumer measurements are sizing signals, not current capacity guarantees.
- S6 needs sufficient socket-memory capacity or remote peers. Kernel-limited runs
  and streams ended by write deadlines do not establish held-stream capacity.
- OpenAI live-provider prompt counts remain separate from tiktoken oracle evidence.
  Counts beyond the exact text prefix are calibrated, not exact.
- Coding agents are qualified against their own dialects. Translation may refuse
  retained/encrypted/thinking/store semantics that another provider cannot preserve.
- Caller Anthropic beta features can introduce pricing beyond the local estimate;
  error responses still omit opt-in attempt metadata.
- Actual Vertex acceptance of extended JSON-schema fields needs live evidence.
  Fixture/client qualification does not certify every vendor schema rule.

Current engineering/qualification boundaries live in [clients](../clients.md),
[performance](../performance.md) and [code-mode qualification](../qualification/code-mode.md),
not the historical implementation timing diary.

## Remaining delivery order

The benchmark maintainer owns the run manifest and comparisons; the release
maintainer owns candidate-image qualification. Neither depends on completing
M2, M3 or later feature milestones. M2 calibration improves estimates, while M3
adds a shadow variant to the same reference run.

1. **Freeze inputs.** Record the commit and candidate-image digest, the pinned
   LiteLLM tag/digest, topology, workload, all tuning and the S4/S5 comparison
   differences. Retain the existing LiteLLM pin until an explicit rebaseline
   changes tag and digest together. The roadmap's newer documentation baseline
   is not permission to silently change a historical benchmark comparator.
2. **Qualify capacity and correctness.** Run full-rate S1–S6 with isolated
   peers, healthy state services and enforced validity. S3 must accrue budget
   spend and deliver all admitted-request metadata; S6 must hold the requested
   streams concurrently, not merely open then expire them. A missed target
   triggers profiling and a rerun on the frozen setup, not a reduced rate.
3. **Publish reproducible comparisons.** Run S1–S5 for both gateways with their
   own direct baselines and the declared drift allowance. Preserve raw results,
   validity failures, commands, resource use and target verdicts. Repeat from a
   clean checkout. S6 remains an OLP capacity result; S4 is a circuit workload
   and S5 is a documented cross-endpoint workload, so neither supports a broader
   claim about every retry or translation path.
4. **Close external accuracy evidence.** Run the pinned OpenAI prompt-count
   test on the exact-prefix corpus and attach model/version, counts and errors.
   Run M2's Anthropic/Gemini calibration separately; lack of a factor continues
   to mean `heuristic`, never `exact` or a measured calibration.
5. **Qualify the release image.** Run `scripts/qualify-image.sh` with
   `OLP_QUALIFY_BENCH=1` and `OLP_BENCH_ENFORCE=1` on the frozen hardware, and
   link its results plus the clean-checkout reproduction before marking M1
   `Shipped`. A default CI or reduced-rate pass cannot satisfy this gate.

## Exit criteria

Implemented behavior is checked below; uncollected qualification remains open.
Later [integrated repair qualification](../architecture-review.md) does not replace
full-rate or paid-provider evidence.

- [ ] `make bench`/`make bench-compare` reproduce every full-rate reference scenario
  from a clean checkout and publish results in `docs/performance.md`.
- [ ] OLP meets every [target](#targets), including full-scale comparisons.
- [x] CI's unchanged hot-path regression gate and unconfigured-allocation test run.
- [ ] OpenAI text estimates match live reported prompt counts within the exact
  prefix bound. Oracle token IDs/framing and per-family estimate reports already
  pass; `TestLiveOpenAIPromptTokensMatchTheEstimate` still requires paid credentials.
- [x] Pinned client suites and documented configurations pass integrated fixtures.
  Native code continuation and ordinary-route limitations remain separate.
- [x] Both supported rate-header families reflect key windows; metadata is opt-in.
