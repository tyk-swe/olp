# M1: Measured advantage

| Status | Depends on | Unlocks |
| --- | --- | --- |
| In progress | None | [M3](m03-routing-resilience.md), [M7](m07-guardrails.md), and the performance gate of every milestone |

OLP's architecture should make it faster and more correct than LiteLLM, but at
0.1.0 nothing in the repository measured either claim. This milestone makes
both measurable, publishes the results, and turns them into regression gates
that every later milestone must pass.

The benchmark harness, the LiteLLM comparison, the regression gate, the token
estimates, the client qualification suites and the response metadata are
[delivered](#delivered). The full-rate scenarios have not been run on reference
hardware and no reference results are published, so the milestone stays in
progress until the [open exit criteria](#exit-criteria) close.

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
  tokenizer keep the four-character heuristic until
  [M2.4](m02-provider-catalog.md#m24-reference-catalog) supplies their factors
  ([decision 3](#decisions-to-settle)).
- The coding agents and frameworks LiteLLM documents run against OLP under a
  qualification suite.
- Responses carry standard rate-limit headers and opt-in gateway metadata.

## Baseline

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
  could cost across the attempts it may dispatch (the dearest attempt, not their
  sum), priced from the gateway's pinned price list: input at the highest
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

## Decisions to settle

Each decision below records what was decided and why. Decisions 1 to 3 are the
ones this specification listed; the rest were made during implementation because
the specification text did not settle them.

1. **The reference hardware for published results: not yet named.** It is named
   in the [record of a reference run](../performance.md#record-of-a-reference-run)
   when a reference run is published, because the choice has to fit what S3
   needs and no full-rate run has measured that. The recommendation stands: one
   general-purpose cloud instance family, with the gateway, the mock upstream and
   the load generator pinned to CPUs of their own. Until then every figure in
   [performance](../performance.md) is a labelled smoke figure from a shared
   8-vCPU virtual machine.
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
   variable, pins the CPUs and sets `OLP_BENCH_ENFORCE=1`. That line has never
   been executed, because it needs a candidate image.
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

- The [`tests/bench/`](../../tests/bench/README.md) harness: a mock upstream that
  serves the OpenAI (Chat Completions and Responses), Anthropic and Gemini
  dialects with configurable timing and failures, an open-loop load generator
  that measures from the scheduled send time into HDR histograms, and scenarios
  S1 to S6 that run a real `olp` process against a direct-to-mock baseline.
  `make bench` runs OLP alone, `make bench-compare` also runs the pinned LiteLLM
  release and judges the targets, and `OLP_BENCH_ENFORCE=1` fails a run that is
  not a reference run or that misses or does not check a target. See
  [method](../performance.md#method), [scenarios and targets](../performance.md#scenarios-and-targets),
  [reproduce](../performance.md#reproduce) and
  [the LiteLLM comparison](../performance.md#the-litellm-comparison).
- Hot-path `testing.B` benchmarks for authentication, planning, credential
  selection, the codecs, stream relay, admission, the whole request including the
  S3 request, estimation, cost bounds and plugin signing, compared with the merge
  base by `make bench-gate` and the `bench` job of the CI workflow. The
  [performance budget](../performance.md#performance-budget) is held by
  `TestUnconfiguredFeaturesAddNoAllocations` as an ordinary unit test. The
  benchmarks found work done for nothing, now skipped: quota lookup names built on
  every attempt with no quota configured, and discarded decodes of model metadata
  on every plan.
- A reduced-rate [smoke run](../performance.md#smoke-run) of S1 to S6 and of one
  comparison, labelled as non-reference.

### M1.2 Accurate admission token estimates

- The `internal/operations/tokenization/estimate` package: encoders, a registry
  that names the family of an upstream model (`openai-o200k`, `openai-cl100k`,
  `anthropic`, `gemini`, `other`), a walker that reads a request in any dialect
  once, OpenAI's documented message framing, and the bounded policy for long
  prompts. The gateway counts each family once per request however many attempts,
  slots or translated targets use it, and planning, the context-window check and
  admission use the same counts.
- Every attempt records its estimate, provenance and model family
  (migration `0013_estimate_accuracy.sql`). Usage reports total estimated and
  reported input tokens over the attempts that had both, and group by
  `model_family` and `estimate_provenance`; the console usage page shows both.
  Route simulation shows the estimate, provenance and family of each target.
  See [how the prompt is estimated](../gateway.md#how-the-prompt-is-estimated),
  [accounting delivery](../operations.md#accounting-delivery-and-shutdown) and
  [explain and observe](../provider-routing.md#explain-and-observe).
- Cost reservation for key and shared budgets, with settlement by the cost
  attempts incurred and removal when accounting lands the spend
  ([cost reservation](../gateway.md#cost-reservation),
  [spend budget reconciliation](../operations.md#spend-budget-reconciliation)).

### M1.3 Client and agent qualification

- Nine suites under [`tests/clients/`](../../tests/clients/README.md), run by
  `tests/clients/run.sh` from `scripts/integration.sh`: the harness's own tests,
  the Vercel AI SDK, the official Go SDKs (their own module, so their
  dependencies stay out of the gateway's), Claude Code, Codex CLI, Gemini CLI, the
  OpenAI Agents SDK, LangChain and LlamaIndex.TS, each a pinned release run
  headless against a gateway whose upstream is a deterministic scripted fixture.
  [Client compatibility](../clients.md) names the pinned releases
  ([pinned releases](../clients.md#pinned-releases)), what the suites assert, a
  tested configuration for each client and the
  [open items](../clients.md#open-items).
- The clients exposed gateway incompatibilities that were fixed: Messages
  requests may carry `system` turns and `?beta=true`, `Anthropic-Beta` reaches an
  Anthropic upstream on transformed routes and repeated header lines are read as
  one list, the Anthropic surface sends `request-id`, a translated request
  ignores fields that only spell out a default, Gemini JSON Schema fields are read
  and capital type names rewritten, Gemini embedding routes answer the right
  error and report usage, and the Bedrock encoder refuses a late system message
  instead of hoisting it
  ([gateway behavior that shapes client configuration](../clients.md#gateway-behavior-that-shapes-client-configuration)).

### M1.4 Response metadata

- Rate-limit headers on the OpenAI and Anthropic surfaces, for the dimensions the
  key limits, from the admission reservation's reply, on success and on a limit
  rejection; and the opt-in `response_metadata` key policy for `X-OLP-Attempts`,
  `X-OLP-Route-Revision`, `X-OLP-Provider` and `X-OLP-Cost`, in the management
  contract, the console key form and the gateway. See
  [response headers](../gateway.md#response-headers),
  [rate-limit headers](../gateway.md#rate-limit-headers),
  [gateway metadata](../gateway.md#gateway-metadata),
  [which responses carry them](../gateway.md#which-responses-carry-them) and
  [key response metadata](../access.md#key-response-metadata).

## Open items

What the implementation found that is not done. The first group is evidence
this milestone still owes; the rest are product decisions and engineering work
for later.

**Evidence still to produce**

- Full-rate scenarios on reference hardware, the reference results in
  [performance](../performance.md#reference-results), and a reproduction from a
  clean checkout. The comparison script has run to the end only for S1, at 5% of
  its rate. `OLP_QUALIFY_BENCH` has never been executed.
- The `bench` CI job has not run on a hosted runner; this change is its first
  run, and it compares only `BenchmarkSign`, because every other benchmark is
  absent at the base. A repository maintainer must create the
  `allow-removed-benchmarks` label, which the workflow reads when it starts.
- Sizing S3. The smoke run spent 10.06 CPU ms of gateway per request at 90
  requests per second, about 30 vCPU at 3,000 requests per second by arithmetic,
  before the CPUs the mock and the load generator need; CPU per request fell as
  the rate rose in S1, so this is a signal for choosing the machine and not a
  result. Cost reservation was measured, when it was written, to add about 150 µs
  of Valkey script time to a priced request (about twice the old path); that
  figure was not repeated. A priced request runs three scripts, reserve, settle
  and accrue, which took 242 to 279 µs of script time together in eight runs of
  `BenchmarkLimitsPricedRequest` against a disposable Valkey 9.1 with
  `appendonly yes` on the shared smoke machine, so the increment is not the
  whole cost. At 3,000 requests per second on one budgeted
  key the three scripts are about 0.7 to 0.85 s of Valkey main-thread time each
  second, before anything else Valkey does for those requests: S3's one key may
  saturate the thread that runs them, which a full-rate run has not tried. The
  key's budget keys share a hash tag, so clustering does not spread it.
- S6 has not run above 150 streams. At the default warmup and period the
  streams open over 70 seconds but the gateway ends a stream thirty seconds after
  its writes block, so the window must be shortened; the 273 CPU ms spent for
  each stream held needs checking at scale.
- The request-metadata consumer persists one event per PostgreSQL transaction,
  116 to 164 events a second in the smoke run, so at the rates of S1 to S3 events
  arrive minutes after the load ends. The harness waits and counts them, and more
  workers share the stream, but batching `processEntries` is the follow-up.

**Product decisions for later**

- The walker does not count the arguments of an Anthropic `tool_use`, a Gemini
  `functionCall`, a function response or encrypted reasoning, so heuristic
  families under-count them; counting them changes the charge that a parity test
  pins, and belongs with the per-family factors of
  [M2.4](m02-provider-catalog.md#m24-reference-catalog), which stay at 1 until the
  catalog ships.
- A cost-budget key refuses requests with `503 distributed_limits_unavailable`
  until the next reconciliation pass, at most a minute, after it is created, after
  a budget is added, and after each UTC day or month rollover. Initializing
  eagerly would invent a zero spend, which the design forbids.
- While the price list is more than a minute old, a request reserves no cost and
  is judged on accrued spend alone, and there is no metric for it.
- A provider that sets any `options.network` field or has a profile switches
  from the shared transport, which caps no connections, to a pool for each
  credential slot capped at 64 connections to the host
  ([provider connection capacity](../deployment.md#provider-connection-capacity)).
  Whether the fallback should match is open.
- None of the three coding agents can run through a route that translates to
  another vendor, because the gateway refuses what the target cannot preserve, and
  a translated Responses request refuses `store`. An operator-chosen way to drop
  such fields is a roadmap decision ([open items](../clients.md#open-items)).
- Transformed routes forward a caller's `Anthropic-Beta` header to an Anthropic
  upstream, so a beta such as `context-1m` can change provider pricing beyond
  what the estimate models; strict routes already forwarded it.
- Error responses carry no `X-OLP-*` headers, so a key that opted in sees no
  attempt count on a 502 after several failed attempts.

**Unverified or deferred engineering**

- Gemini schemas outside the OpenAPI subset are sent in `parametersJsonSchema`
  and `responseJsonSchema`. The pinned Google SDK passes both for both backends,
  but acceptance by the Vertex `/v1` API is unverified, and the scripted fixture
  does not model Gemini refusing a non-string `enum` in the subset fields.
- The output bound of a Gemini estimate reads `generationConfig` whole from the
  caller, while the encoder merges provider defaults in member by member, so a
  caller that sets one member over a default for another can be over or
  under-reserved.
- The hot path still has costs found and not changed: a native OpenAI request
  to an OpenAI-wire provider is re-marshaled for the upstream (about 1.6 to 1.9 ms
  of the roughly 5.4 ms of CPU for a 100K-token prompt), and splicing it instead
  changes the bytes sent upstream, so it needs a fidelity review; translated
  routes decode and encode the canonical form in several passes; and a stream
  relay costs about 4,500 allocations per 64-frame stream.
- The client suites do not cover a key's own `429`, which needs a Valkey-backed
  lane, stored-response continuation, which needs a database-backed lane, or a
  stream that fails after its first event. Every `pnpm install` now downloads the
  client packages, including a Claude Code binary of about 230 MB, and the first
  hosted CI run settles whether the runner allows Codex's sandbox and binding
  `127.0.0.2`.

## Exit criteria

- [ ] `make bench` and `scripts/bench-compare.sh` reproduce every scenario from
      a clean checkout, and `docs/performance.md` publishes the results.
      *Remaining:* the harness, the comparison and the release-qualification line
      exist and every scenario has run at reduced rates
      ([smoke run](../performance.md#smoke-run)), but no full-rate run has been
      made on reference hardware, the comparison has run to the end only for S1 at
      5% of its rate, and [reference results](../performance.md#reference-results)
      reads "None yet".
- [ ] OLP meets every [target](#targets) above.
      *Remaining:* no target that needs full scale or a comparison has been
      judged. Each is `not_checked` below full scale and `needs_comparison` in a
      run of OLP alone, so OLP's performance against LiteLLM is unmeasured at full
      rate on reference hardware. The one target judged at any scale, zero lost
      request-metadata events, was `met` in every smoke run of S1 to S3, which
      says nothing about full rate.
- [x] CI fails a pull request that regresses a hot-path microbenchmark beyond
      the [budget](#performance-budget).
      *Evidence:* the `bench` job runs `scripts/bench-gate.sh`, and
      `scripts/check-benchstat.mjs` fails on a reproduced regression above 10%;
      the script's tests pass, and a deliberate slowdown of `BenchmarkSign` made
      the gate exit 1 locally. The job has not yet run on a hosted runner (see
      [open items](#open-items)).
- [ ] Estimates for OpenAI encodings match the provider-reported prompt tokens
      exactly on the text fixtures of the protocol corpus, and usage reports
      show estimation error for every family.
      *Done:* `TestOracleFixtures` compares every token id of `o200k_base` and
      `cl100k_base` with `tiktoken` on `tests/fixtures/tokens`, and a framing test
      compares the OpenAI Cookbook's message counts. The usage reports set the
      estimate against the reported input tokens by route, `model_family` and
      `estimate_provenance`, and integration tests cover them end to end, but they
      seed three families (`openai-o200k`, `anthropic` and `unknown`) and not
      every one. Go's newer Unicode tables can split a newly assigned character
      differently from `tiktoken`.
      *Remaining:* the comparison with provider-reported prompt tokens. None has
      been made: the `tiktoken` fixtures are that tokenizer's output, not usage a
      provider reported, the protocol corpus carries only synthetic usage
      (`prompt_tokens: 3`), and no live provider comparison has been run. It needs
      recorded provider responses with their prompts, or a live run. For a prompt
      past the first 32 KiB of text the estimate is calibrated and not exact
      ([decision 3](#decisions-to-settle)), so the criterion can hold only for
      prompts within that bound.
- [ ] The client qualification suite passes in CI for every client above, and
      `docs/clients.md` documents each configuration.
      *Done:* `docs/clients.md` documents each configuration, and all nine suites
      passed locally on the final tree and in the full `make integration` run at
      `4a3508d5`; `make integration`, which CI runs, includes them. Two surfaces
      are qualified differently from the table above: Codex's stored-response
      continuation is its history replay with reasoning items, because it sends
      `previous_response_id` only over a WebSocket transport the gateway does not
      serve, and the typed `429` of a key's own limit waits for a Valkey-backed
      lane ([open items](../clients.md#open-items)). The coding agents run only
      through routes to their own vendor ([open items](#open-items)).
      *Remaining:* a passing run on a hosted runner. The suites have not run
      there, and a runner that cannot bind `127.0.0.2` or does not allow Codex's
      sandbox fails the egress and Codex tests
      ([requirements](../clients.md#running-the-suites)).
- [x] Rate-limit headers appear on both surfaces and match the key's Valkey
      windows in integration tests; metadata headers appear only when the key
      opts in.
      *Evidence:* `TestResponseHeadersMatchTheKeysValkeyWindow` runs on the
      OpenAI and Anthropic surfaces, and `TestResponseMetadataHeadersFollowTheKeysPolicy`
      holds the opt-in.
