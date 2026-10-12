# Performance

How OLP measures what it adds to a request, how to reproduce the measurements
against LiteLLM, and the gates that keep hot-path performance from slipping. The
targets come from the
[M1 milestone](roadmap/m01-measured-advantage.md#m11-gateway-benchmark); the
harness lives in [`tests/bench/`](../tests/bench/README.md).

## Status of the numbers

**No full-rate reference results are published.** Historical reduced-rate runs
on a shared 8-vCPU development VM establish harness operation, not either
product's production performance. Do not compare them to vendor-published figures.
Full-scale/comparative targets remain unchecked; zero metadata loss passed only
at measured smoke rates. New reference results need the [complete record](#record-of-a-reference-run).

## Method

- **Open loop:** schedule request `k` at `start + k/rate`, without waiting for
  completion. In-flight/backlog bounds count drops rather than hiding delay.
- **Scheduled latency:** measure from scheduled send to response end or validated
  stream terminal. TTFT uses first data frame; missing terminal/error is failure.
  Warmup is excluded from latency/error histograms, but sustained completion rate,
  process CPU/allocations and metadata cover their declared whole-run windows.
- **HDR:** three significant figures, one microsecond–one hour; bounded storage.
- **Baseline subtraction:** each gateway follows its own direct-to-mock baseline
  in the same session. Added percentiles are differences, not paired request
  samples. Compare baseline drift. Negative p50/p95 beyond jitter (one millisecond
  or 10% of baseline) invalidates; no negative percentile can satisfy a target.
- **Separate peers:** deterministic OpenAI/Anthropic/Gemini mocks have controlled
  TTFT/interval/output/errors and dedicated CPUs on reference runs.
- **Validity:** no dropped/abandoned sends, ≤1% later than five milliseconds,
  ≥99% offered rate and ≤0.1% failed requests for either load (S6's end-cancelled
  held streams are separate). Shedding, excessive negative latency or unfinished
  metadata drain invalidates; latency targets require valid loads.
- **Evidence:** added p50/p95/p99, TTFT, completed throughput, net idle-adjusted CPU/
  request, requests/CPU-second, RSS before/peak/after, errors and metadata delivered/
  admitted. Runtime allocation counters estimate whole-process/request cost;
  `testing.B` isolates individual paths.
- **Reference:** full scale, valid loads, disjoint gateway/mock/generator CPU pins.
  Every other run is smoke; neither a gate nor a small sample proves fleet capacity.

## Scenarios and targets

| ID | Scenario | At full scale |
| --- | --- | --- |
| S1 | Chat Completions, short prompt, unary | 1,000 RPS |
| S2 | Chat Completions, short prompt, streaming 64 tokens at 20 ms intervals | 1,000 RPS |
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens: 16`, key with a cost budget | 3,000 RPS |
| S4 | First target returns 503, second succeeds | 1,000 RPS |
| S5 | Anthropic Messages streaming, translated to an OpenAI upstream | 1,000 RPS |
| S6 | Slow-reader streams held open | 10,000 streams |
| S1-shadow | S1 with every request mirrored to a shadow target | 1,000 RPS |

S6 is OLP only: no target compares it, and its socket buffers at full scale need
more TCP memory than most single hosts have. Its streams must also all open
within the thirty seconds after which the gateway ends a stream whose writes are
blocked, so S6 opens them over at most 3 seconds of warmup and 15 measured
whatever `OLP_BENCH_WARMUP` and `OLP_BENCH_DURATION` say: over the default 70
seconds the first were gone before the last opened ([Smoke run](#s6-held-streams)). The
[scenario guide](../tests/bench/README.md#scenarios) has each scenario's mock
behavior, how the gateway is sized for it and what to know when reading it.

| Target | Judged by |
| --- | --- |
| S1 to S5: OLP's added latency is lower than LiteLLM's at p50, p95 and p99 | `scripts/bench-compare.sh`, strictly lower at each, for the mode the scenario exercises (all requests for S3) |
| S1 to S5: OLP sustains more RPS per vCPU | `scripts/bench-compare.sh`, as requests handled per CPU second ([below](#requests-per-vcpu)) |
| S1 on a 2-vCPU gateway: added latency at most 2 ms at p95 and 5 ms at p99 | the scenario suite, only for a gateway pinned to exactly two CPUs, with the mock upstream and the load generator pinned to CPUs of their own, at full scale, in valid runs |
| S3: 3,000 RPS at a 100% success rate | the scenario suite, at full scale, in valid runs |
| S3: fewer total vCPU than LiteLLM's high-throughput profile at equal or better p95 | `scripts/bench-compare.sh` ([below](#the-s3-profile-target)) |
| S1 to S3 with healthy Valkey: zero lost request-metadata events | the scenario suite: every admitted request has its metadata, and Valkey was healthy throughout |
| S1-shadow: the S1 latency and metadata targets hold with every request mirrored, and at least 99% of caller requests reach the shadow target | the scenario suite, under the same conditions as S1 ([shadow targets](provider-routing.md#shadow-targets)) |

A target the first baseline misses becomes M1 scope (profiling and hot-path
work), not a revised target.

A target is `met`, `missed`, `needs_comparison` or `not_checked`. It is checked
only under the conditions it is stated for, and one that was not says why. A run
of OLP alone reports the targets that set it against LiteLLM as
`needs_comparison`, which `scripts/bench-compare.sh` settles. A smoke run judges
none of the targets that need full scale or a comparison. The metadata target is
the one judged at any scale, and it is `not_checked` only when Valkey was not
healthy throughout or the gateway's metrics did not refresh after the run. With
`OLP_BENCH_ENFORCE=1` a missed or unchecked target, a run that is not a
reference run, and baselines that disagree at p50 or p95 all fail the run; the
baselines' p99 is noted, since a tail varies between sessions even on pinned
CPUs.

## Reproduce

Requirements are those of the [contributor guide](../CONTRIBUTING.md): Go,
Node.js, Docker Compose, `psql` and, to pin processes, `taskset`. The harness
reads `/proc` and cgroup v2 files, so it runs on Linux only.

```sh
make bench            # OLP alone: S1 to S6 at full rates
make bench-compare    # OLP and LiteLLM: S1 to S5
```

`make bench` (`scripts/bench.sh`) starts a disposable PostgreSQL and Valkey,
builds the release binary and the mock upstream, runs the scenarios against a
real `olp all` process and writes `.local/bench/<scenario>.json`.
`make bench-compare` (`scripts/bench-compare.sh`) does that for each scenario,
then runs the same load, from the result OLP just wrote, against the LiteLLM
release pinned in [`deploy/compose.bench.yaml`](../deploy/compose.bench.yaml),
then compares the two. Only one gateway runs at a time. Both gateways get the
same CPUs, the same mock upstream binary, the same load generator code, the same
PostgreSQL and Valkey, and the same load. The code is shared and the process is
not: OLP's scenarios run the load generator inside the test process, which also
samples `/proc` and the gateway's metrics, and LiteLLM's run the separately
built, static `cmd/loadgen` under `taskset`, so the comparison prints how far
the two sessions' direct-to-mock baselines lie from each other, which is what
shows a difference between the two.

A reference comparison is two invocations, because S1's target is stated for two
vCPUs and the others are made on whatever the hardware record names:

```sh
# S1 on two vCPUs.
OLP_BENCH_GATEWAY_CPUS=0-1 OLP_BENCH_MOCK_CPUS=2-3 OLP_BENCH_LOADGEN_CPUS=4-7 \
  OLP_BENCH_ENFORCE=1 BENCH_SCENARIOS=S1 scripts/bench-compare.sh

# S2 to S5 on the CPUs of the record.
OLP_BENCH_GATEWAY_CPUS=<cpus> OLP_BENCH_MOCK_CPUS=<cpus> \
  OLP_BENCH_LOADGEN_CPUS=<cpus> OLP_BENCH_ENFORCE=1 \
  BENCH_SCENARIOS=S2,S3,S4,S5 scripts/bench-compare.sh
```

The CPU lists above are an example of the shape, not the reference layout. The
three processes must not share a CPU (an enforcing run refuses to start when two
do, and any other run is not a reference run), and S3 needs a couple of CPUs for each of
the mock and the generator
([scenario guide](../tests/bench/README.md#mock-upstream)). Pinning applies to
OLP with `taskset`, and to LiteLLM as the `cpuset` of every container of its
deployment, with a worker for each CPU. Reference runs should also point
`BENCH_SERVICES=external` at PostgreSQL and Valkey set up as a deployment would
run them, rather than the development compose file's.

| Variable | Meaning |
| --- | --- |
| `BENCH_SCENARIOS` | Scenarios to run, from S1 to S5 (`make bench` takes S6 too) |
| `BENCH_SERVICES` | `compose` (default) starts disposable PostgreSQL and Valkey; `external` uses `OLP_TEST_DATABASE_URL` and `OLP_TEST_VALKEY_URL`, whose user may create databases. LiteLLM gets a database of its own, `litellm_bench`, dropped and made anew |
| `OLP_BENCH_SCALE` | Multiplies every rate and concurrency; 1 is the roadmap's full rates |
| `OLP_BENCH_DURATION`, `OLP_BENCH_WARMUP` | The measured period (60s) and the warmup before it (10s) |
| `OLP_BENCH_GATEWAY_CPUS`, `OLP_BENCH_MOCK_CPUS`, `OLP_BENCH_LOADGEN_CPUS` | CPU lists as `taskset` writes them; the gateway's is also LiteLLM's cpuset |
| `OLP_BENCH_ENFORCE` | `1` fails on a missed or unchecked target; needs full scale and all three pins, on CPUs that do not overlap |
| `OLP_BENCH_OUT` | Result directory (`.local/bench`) |
| `OLP_BENCH_LATE_AFTER` | When a send counts as late (5ms) |
| `OLP_TEST_BINARY`, `OLP_BENCH_MOCK_BINARY`, `OLP_BENCH_LOADGEN_BINARY` | Use these binaries instead of building them |

The other variables, such as the drain period and S6's, are in the
[scenario guide](../tests/bench/README.md#scenarios). A smoke run is the same
command with a smaller `OLP_BENCH_SCALE`; [Smoke run](#smoke-run) records the
settings of one.

### Where results land

| File | Holds |
| --- | --- |
| `.local/bench/<scenario>.json` | OLP's result for a scenario: both load reports and everything measured around them |
| `.local/bench/litellm/<scenario>.json` | LiteLLM's result, in the same shape |
| `.local/bench/raw/`, `.local/bench/litellm/raw/` | The load generator's reports, baseline and gateway, per scenario, and the tail of LiteLLM's container logs |
| `.local/bench/compare.json`, `compare.md` | The comparison and the judged targets, as data and as a table |
| `.local/bench/gate/` | The microbenchmark gate's samples and its report |

`.local` is not committed. Reference results are published in this guide, and
the result files of the run are kept with the record.

## The LiteLLM comparison

### What is pinned

LiteLLM runs from `ghcr.io/berriai/litellm:v1.103.2` pinned by the digest of its
multi-architecture index,
`sha256:f63fb81b831b170ec16851e23c36ac5bf52ef106b271406429524a2ed730bbfd`. It
was the latest stable release when the comparison was built, published
2026-10-01
([release notes](https://github.com/BerriAI/litellm/releases/tag/v1.103.2)), and
the `main-stable` tag pointed at the same digest on 2026-10-02. To move to a
newer release, change the tag and the digest in the compose file together; a
published comparison names the digest it ran.

Every container runs on the host's network, as OLP and the mock do, so the
comparison measures the gateways and not Docker's NAT or its userland proxy. The
proxy listens on loopback only. Response caching is off: the load generator
sends identical prompts, and a cache would answer them without calling the
upstream.

### Settings

The settings are the ones LiteLLM documents for production, in
[`deploy/litellm/production.yaml`](../deploy/litellm/production.yaml) and
[`deploy/compose.bench.yaml`](../deploy/compose.bench.yaml), from its
[production guide](https://docs.litellm.ai/docs/proxy/prod):

| Setting | Value |
| --- | --- |
| Workers | One for each CPU of the gateway's cpuset, the guide's rule for a machine with nothing scaling it |
| Memory | 4 GiB for each worker, the guide's sizing, as a container limit |
| Open files | 524,288 for every container, as the scripts raise OLP's, since a container starts with 1,024 |
| `LITELLM_MODE`, `LITELLM_LOG`, `json_logs`, `set_verbose` | `PRODUCTION`, `ERROR`, on, off |
| `LITELLM_SALT_KEY`, `LITELLM_MASTER_KEY` | Made for each run |
| `proxy_batch_write_at` | 60 seconds |
| `use_redis_transaction_buffer` | On: the guide asks for it above about 1,000 requests per second, which every scenario reaches |
| `disable_error_logs`, `request_timeout` | On, 600 seconds |
| Routing | `simple-shuffle`, with Valkey as its Redis |
| Telemetry | Off |
| Database | A database of its own in the PostgreSQL that OLP uses |

S3 runs on LiteLLM's
[high-throughput profile](https://docs.litellm.ai/docs/proxy/high_throughput),
which its documentation described as a development preview in nightly builds
when this was written; the 1.103.2 chart and image carry its pieces. One host
cannot reproduce a Kubernetes deployment of 33 pods, so what is reproduced is
each setting of the profile that exists on one host:

| In the profile | Here |
| --- | --- |
| `LITELLM_RUST=1`, Rust token counting | Set; the image's native module loads |
| Four workers per pod | One for each CPU of the cpuset |
| A PgBouncer pool in each pod (8 database connections, 1,000 clients) | The same pool, started by the gateway container |
| A collector sidecar for spend processing, and a metrics server sidecar | Both run, as containers on the same cpuset, and are counted in CPU and memory |
| `callbacks: [prometheus]`, `allow_requests_on_db_unavailable`, `KEEPALIVE_TIMEOUT=75` | Set |
| 33 pods, the autoscaler and its metrics adapter, the load balancer, rollout settings and the chart's separate gateway, backend and UI images | Not reproduced. The single image runs the same code |

LiteLLM publishes what the profile did on its 33 pods
([benchmarks](https://docs.litellm.ai/docs/benchmarks)): they requested 132 vCPU
in total and sustained 3,000 requests per second at a request latency of 30.581
ms at p50, 54.029 ms at p95 and 91.645 ms at p99, taken from the gateway's own
metrics. Those are LiteLLM's figures from LiteLLM's test, with a Locust
generator behind a load balancer. They are not reproduced here, and the
comparison's constants for them are marked as published.

### How each scenario differs

- **Every provider carries a sized pool.** A provider's connection pool caps the
  requests it can hold in flight, and a provider that sets any `options.network`
  field, or has a profile, is capped at 64 connections to its host by default,
  where one that sets none shares the gateway's transport and is capped at none
  (see [provider connection capacity](deployment.md#provider-connection-capacity)).
  The benchmark sets the pool of every provider it provisions, to a few times the
  concurrency the scenario expects, and records it in the result under
  `gateway.derived_limits`, so S2 and S6 are never queued in a default pool. It
  also means the scenarios measure the pool a tuned or profiled provider uses,
  not the shared transport.
- **Request metadata outlasts the load.** One metadata consumer persists a page
  of up to 100 events in a PostgreSQL transaction, but each event is still about
  ten statements, so at the rates of S1 to S3 the events of a run arrive minutes
  after its load ends: S1 at 300 requests per second produces 21,000 events, and
  on the [smoke run](#smoke-run)'s machine 7,654 of them were still to arrive
  when its load ended and the harness waited 31.0 seconds for them, at 247 events
  a second (68.7 seconds at 164 events a second when each event had a transaction
  of its own). A run at full rate produces
  70,000 to 210,000. The harness waits for as long as events
  keep landing and reports how long that took and at what pace (`drain` in the
  result), so a late event is counted as delivered and a lost one is not. Read
  `request_metadata_completeness` with the wait beside it: a run whose events
  arrive late has lost nothing, and one whose events stop arriving is invalid.
  A deployment sizes the pipeline for its own rate by running more workers, which
  share the stream.
- **S3's model is named as an OpenAI model.** Admission counts a prompt with the
  exact tokenizer only for a model whose family it can name, and the other
  scenarios' upstream models (`bench-chat` and the failover pair) are of none, so
  they are charged four characters to a token. S3 calls `gpt-4o-bench` on OLP and
  on LiteLLM alike, which puts the estimate of a 50K to 100K-token prompt, the
  first 32 KiB counted exactly and the rest at the measured ratio, in the figures
  it reports.
- **S3's key is not servable for the first minute.** A cost budget is enforced
  against spend PostgreSQL has confirmed, so a key created with one answers
  `503 distributed_limits_unavailable` until the worker plane's next
  reconciliation pass, which runs every minute (see
  [limits and budgets](gateway.md#limits-and-budgets)). The scenario waits for
  that pass, which the worker logs as `reconciled cost budgets`, before the
  warmup and does not count the wait; a run against a key that is not yet
  installed would measure rejections.
- **S3's budget is real on both sides.** OLP's key has a daily and monthly cost
  limit and its model is priced. LiteLLM's key has a `max_budget` of 1,000,000
  and its model carries the same prices (USD 2 and 4 per million tokens). The
  runner waits for the key's spend to reach LiteLLM's database and fails the run
  if it did not, since a LiteLLM run that did no budget work is not comparable.
  The OLP run is held to the same: it reads the key's accrued cost from the
  management API once the request metadata has arrived (`budget` in the result),
  and a key that accrued nothing makes the run invalid and leaves the success
  target unchecked.
- **S4's failure policy is matched.** OLP's circuit opens for thirty seconds
  after five counted failures within thirty seconds
  ([gateway](gateway.md#request-path)). LiteLLM's router is set to cool a
  deployment down after five failures for thirty seconds, with one retry, since
  OLP's route allows two attempts; LiteLLM's own defaults are three failures and
  five seconds. A gateway that keeps a failing target out of rotation measures the
  skip and not a failed attempt for each request, but the two do it differently,
  so each result records its attempts per request, OLP's from its attempt
  records and LiteLLM's from the mock's counters, and how they divided between
  the two models. Both are held to the premise that the first target failed: a
  run in which OLP made no attempt on it, the mock injected no error or no
  request failed over, and a LiteLLM run in which the mock injected no error, is
  invalid, as it measured a healthy route.
- **S5 translates differently.** The client speaks Anthropic Messages to both.
  OLP calls the upstream's Chat Completions endpoint, and LiteLLM calls its
  Responses endpoint; each result records the mock's request counts by endpoint
  (`run_dialects` for OLP, `attempts.by_dialect` for LiteLLM). The mock serves
  both with the same timing and the client receives the same Anthropic stream,
  but the two gateways do different work.

### Reading a comparison

`compare.md` and the terminal show two rows per scenario. Added latency is for
the mode the scenario exercises. `rps/vCPU` is sustained requests per second
over the vCPUs the gateway may use; `req/cpu-s` is requests answered successfully
per CPU second spent above idle; `peak MiB` is resident memory. For OLP that is the
process's resident set. For LiteLLM it is the cgroup's anonymous memory plus
mapped file pages, summed over its containers and sampled every 250 ms, which
leaves out the page cache a container's files leave behind but is not the same
measurement as one process's resident set.

#### Requests per vCPU

At a constant open-loop rate that two gateways both hold, sustained requests per
second over allotted vCPUs is the same number for both, so it cannot show which
is more efficient. The target is therefore judged on the other figure: requests
answered successfully per CPU second spent above idle, which is what one fully
used vCPU would carry (a request a gateway failed cost it little and is not
counted, or failing fast would look efficient). Both figures are printed.

#### The S3 profile target

The target compares OLP with a deployment of 33 pods that this host cannot run.
It is judged against LiteLLM's published figures and, when a comparable run
exists, against the same profile measured on the same hardware. It is met only
if OLP used fewer vCPU than the 132 the profile requested, its added p95 is at
most the profile's published 54.029 ms, and it is no worse than the profile
measured here. When the same-hardware run was not valid, the published figures
stand alone and the note says the cross-check was not made.

#### When a target is not checked

A comparison is judged only when both runs are at full scale and valid, on the
same vCPUs, under the same load, and when the figures it needs exist. If LiteLLM
cannot hold the offered load on the hardware, its run is invalid and the targets
that compare the two are not checked; give the pair more CPUs, or read the
success rate beside it.

## Regression gate

`make bench-gate` alternates merge-base/working-tree samples, compares pinned
benchstat time/allocations and fails reproducible statistically significant >10%
regressions. Bytes/op report without gating. Flagged names remeasure with twice
samples before failure. CI retains samples/report as `bench-gate`.

| Path | Benchmark | What an iteration does |
| --- | --- | --- |
| Authentication | `internal/runtime` `BenchmarkAuthenticate` | Resolves an API key among a thousand: a valid one, an unknown one and one with the wrong secret |
| Planning | `internal/runtime` `BenchmarkPlanRequest`, `BenchmarkEligibility` | Weighs the targets of a route against the request and the policies in force, ranks their credential slots and orders the attempts, with and without a price catalogue; checks that a credential version may still serve |
| Credential selection | `internal/runtime` `BenchmarkSelectSlots` | Ranks the slots of one provider |
| Codecs | `internal/protocols` `BenchmarkTranslateRequest`, `BenchmarkTranslateResponse` | Reads a client's request in OpenAI Chat, Anthropic or Gemini form and writes it for a target, each dialect to itself and to and from OpenAI Chat; reads an upstream's response and writes it for the caller |
| Stream relay | `internal/protocols` `BenchmarkTranslateStream`, `internal/gateway` `BenchmarkStreamWriter` | Relays a stream of 64 deltas to the client, natively and translated, through the server-sent-event decoder; writes one frame to the client and renews its write deadline |
| Admission | `internal/gateway` `BenchmarkAdmission` | Admits a request and settles it for a key with no limits, with request and token limits, with a concurrency limit, with a cost budget and in a budget group, and an attempt for a target with a quota, against a limiter client that keeps no state |
| Whole request | `internal/gateway` `BenchmarkGateway`, `BenchmarkGatewayLargePrompt` | Serves a request from key lookup to the last byte written, with the upstream and the client held in memory: unary, streamed, and an OpenAI stream translated for an Anthropic client; and the request of S3, a prompt of 100,000 tokens from a key with a cost budget, unary and streamed, and the same prompt sent as an Anthropic client sends it, with the process's CPU time per iteration, garbage collection included, reported beside the wall time |
| Estimation | `internal/operations/tokenization/estimate` `BenchmarkEstimate`, `BenchmarkMeter`, `BenchmarkHeuristic`, `BenchmarkWalkAgainstLegacy` | Counts a short prompt for each family; counts the bounded part of a long prompt exactly and charges the rest at the measured ratio; walks a request of 30, 4,000 and 100,000 tokens beside the walker it replaced |
| Cost | `internal/usage` `BenchmarkCostBound`, `BenchmarkPriceCost` | Bounds the cost a request could incur, which admission reserves for a key with a cost budget, and prices the usage of an attempt |
| Plugin signing | `internal/plugins` `BenchmarkSign` | Runs a plugin's signing hook, interpreted and compiled |
| Request parsing | `internal/oif` `BenchmarkPlainEnd` | Finds the end of the plain run of a 405,000-byte JSON string, as the parser reads a prompt, eight bytes at a time and, beside it, one byte at a time |

| Variable | Meaning |
| --- | --- |
| `BENCH_BASE` | Base ref (default `origin/main`, then `main`) |
| `BENCH_COUNT` | Samples per side, at least 6 (10); a regression is measured again with twice as many |
| `BENCH_THRESHOLD` | Regression percentage that fails (10) |
| `BENCH_ALLOW_REMOVED` | `1` lets benchmarks of the base be absent from the head (0) |
| `BENCH_TIME`, `BENCH_RUN`, `BENCH_FILTER` | `-benchtime`, the `-bench` pattern and a package filter |
| `BENCH_OUT` | Output directory (`.local/bench/gate`) |

Encoder stress benchmarks `BenchmarkLoad`, `BenchmarkCount`,
`BenchmarkUnbrokenPieces` and `BenchmarkMeterWorstCase` are skipped through
`BENCH_SKIP`: artificial worst cases do not represent request cost. Whole network/
Valkey/accounting pipeline cost belongs to scenarios, not these in-memory paths.
`TestUnconfiguredFeaturesAddNoAllocations` separately holds zero-cost disabled
admission behavior.

New names are reported, not comparable until a later base includes them. Missing
head names fail unless reviewed `BENCH_ALLOW_REMOVED=1` (CI's
`allow-removed-benchmarks` label). Empty output/no shared names fails; only a base
with no benchmarks is explicitly exempt. Do not raise thresholds to clear noise.
Use at least six samples and confirm flags. CI uses 500 ms samples and a one-hour
job; lower duration/count within these bounds rather than extending deadlines.

## Performance budget

- A feature that is not configured adds no allocations and no Valkey or
  PostgreSQL round trips to a request. Where a path is benchmarked, a change that
  makes it allocate shows as an allocations-per-operation regression in the gate,
  so a change to a hot path adds or extends the benchmark for it. Admission is
  held to this directly: a test asserts that admitting a key with no limits and a
  target with no quotas allocates nothing and, with no limiter at all, reaches
  for none. The one exception is the shared cooldown: a gateway that has a
  limiter reads it before every attempt, whatever the key and the target limit,
  which costs an unconfigured key one Valkey round trip an attempt. The test
  does not cover that read ([tests](../tests/README.md#microbenchmarks)).
- Adaptive routing keeps to that budget: a route without fallbacks, selectors,
  retries, affinity, spend caps or shadow targets, and a connection without
  priority shares, caps or probes, plans and executes exactly as before. The
  `capacity` strategy and spend caps read every candidate slot's headroom and
  cap state in one pipelined Valkey round trip per plan, which
  `TestCapacityStrategyAddsOneValkeyRoundTrip` holds. Shadow attempts run after
  the caller's response, in their own pool: S1-shadow holds the S1 targets.
- A configured feature declares its own budget in its milestone: the added
  latency, CPU per request or memory it may cost, in the scenario it affects.
  The pull request that ships it records the scenario's result before and after.
- Full scenario runs are part of release qualification:
  `OLP_QUALIFY_BENCH=1 scripts/qualify-image.sh` runs the scenarios against the
  binary the candidate image ships. They mean something only on reference
  hardware, with the processes pinned and `OLP_BENCH_ENFORCE=1`. The LiteLLM
  comparison is run by hand with `scripts/bench-compare.sh`.

## Record of a reference run

Fill this in for each published run, with the result files kept beside it.

| | |
| --- | --- |
| Date, and who ran it | |
| Hardware | Provider, instance type, region, vCPU, memory, CPU model, tenancy |
| Host | Operating system, kernel, CPU frequency governor, `net.ipv4.tcp_mem`, descriptor limit |
| CPU layout | Gateway, mock upstream and load generator CPU lists; the vCPU count the comparison was made on |
| OLP | Version and binary SHA-256, which each result's `environment` records (`olp_version`, `olp_binary_sha256`), and the git commit and whether the tree was clean, which the results do not record and the version does not tell, since it is the same for every commit: write them down by hand |
| LiteLLM | Image digest, profile of each scenario, workers |
| Services | PostgreSQL and Valkey versions, image digests and settings, and whether `BENCH_SERVICES=external` |
| Toolchain | Go, Node.js, Docker and Compose versions |
| Run | Exact commands and environment, scale, duration and warmup |
| Results | `compare.md` for each invocation, and whether `OLP_BENCH_ENFORCE=1` passed |
| Files | Where the result JSON and the load generator's reports are kept |

## Reference results

None yet.

## Smoke run

**Historical, reduced-rate, non-reference measurements.** These establish that
harness/comparison paths ran, not that full-scale targets passed. Values are from
single shared-machine runs, rounded; no fresh capacity claim follows from them.

| Record | Value |
| --- | --- |
| Initial run | 2026-10-03, clean `9ef71679`; OLP 0.1.0 binary SHA-256 `ccebd834a24195f6c6f11823db8f5d8ad4c09f453672b428022657d14f4f9430` |
| Host | Shared 8-vCPU Haswell KVM guest, 22.9 GiB/no swap, kernel 7.0.0-34, Go 1.27.1, Node 26.8.2, Docker 29.8.2/Compose 5.5.1 |
| Pins | Gateway 0–1, mock 2–3, generator 4–5; PostgreSQL 18/Valkey 9 AOF everysec and developer processes unpinned |
| Timing | 10-second warmup/60-second measured unless S6 explicitly overrides |
| Limitations | VM load varied; mock rebuilt dynamic→static for later points; results retain OLP hash, not mock hash; exact original service/build digests remain in file history |

Representative historical commands:

```sh
export OLP_TEST_BINARY=$PWD/.local/bin/olp OLP_BENCH_MOCK_BINARY=$PWD/.local/bin/mockupstream
export OLP_BENCH_GATEWAY_CPUS=0-1 OLP_BENCH_MOCK_CPUS=2-3 OLP_BENCH_LOADGEN_CPUS=4-5
OLP_BENCH_SCALE=0.1 BENCH_SCENARIOS=S1 scripts/bench.sh
OLP_BENCH_SCALE=0.01 BENCH_SCENARIOS=S3 scripts/bench.sh
OLP_BENCH_SCALE=0.015 OLP_BENCH_WARMUP=3s OLP_BENCH_DURATION=15s BENCH_SCENARIOS=S6 scripts/bench.sh
OLP_BENCH_SCALE=0.3 BENCH_SCENARIOS=S1 scripts/bench.sh
OLP_BENCH_SCALE=0.03 BENCH_SCENARIOS=S3 scripts/bench.sh
```

### S1 to S5

| Scenario | Scale | Target rps | Sustained rps | Valid | Added p50 / p95 / p99 ms | TTFT overhead p50 / p95 / p99 ms | Load |
| --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 0.1 | 100 | 100 | yes | 1.65 / 2.83 / 5.89 | - | 1.77 |
| S2 | 0.1 | 100 | 100 | yes | 3.07 / 9.22 / 20.48 | 2.88 / 8.85 / 20.29 | 2.85 |
| S3 | 0.01 | 30 | 30 | yes | 5.95 / 11.04 / 13.63 | 5.02 / 9.76 / 13.04 | 1.73 |
| S4 | 0.1 | 100 | 100 | yes | 1.52 / 2.48 / 3.36 | - | 1.62 |
| S5 | 0.1 | 100 | 100.05 | yes | 5.12 / 26.62 / 40.96 | 4.53 / 25.46 / 38.38 | 2.49 |
| S1 | 0.3 | 300 | 300 | yes | 1.38 / 2.03 / 4.59 | - | 2.32 |
| S3 | 0.03 | 90 | 90.03 | yes | 8.32 / 20.58 / 27.65 | 6.62 / 17.46 / 24.30 | 2.28 |

| Scenario | Target rps | Gateway CPUs used (of 2) | Requests per CPU second | CPU ms per request | RSS before / peak / after MiB | Errors % | Metadata delivered | Wait for metadata after the load | Allocations per request: objects / KiB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 100 | 0.43 | 232.7 | 4.30 | 185.7 / 188.7 / 69.7 | 0 | 7000 of 7000 | 0.0 s | 1,063 / 86.0 |
| S2 | 100 | 0.98 | 100.2 | 9.98 | 182.2 / 182.4 / 93.9 | 0 | 7000 of 7000 | 5.0 s | 5,518 / 479.6 |
| S3 | 30 | 0.36 | 83.9 | 11.91 | 185.7 / 185.9 / 72.8 | 0 | 2100 of 2100 | 0.0 s | 1,883 / 3,394.1 |
| S4 | 100 | 0.42 | 242.0 | 4.13 | 186.0 / 188.5 / 69.3 | 0 | 7000 of 7000 | 0.0 s | 1,151 / 93.5 |
| S5 | 100 | 1.23 | 80.1 | 12.49 | 185.5 / 189.1 / 88.0 | 0 | 7000 of 7000 | 16.6 s | 10,283 / 1,019.9 |
| S1 | 300 | 0.64 | 470.1 | 2.13 | 185.0 / 187.6 / 71.6 | 0 | 21000 of 21000 | 68.7 s | 844 / 69.3 |
| S3 | 90 | 0.91 | 99.4 | 10.06 | 186.2 / 186.2 / 80.5 | 0 | 6300 of 6300 | 4.6 s | 1,797 / 3,384.2 |

Every point was valid with no drops/failures, ≥99.997% offered schedule and eventual
complete metadata. Drain wait reveals lag, not loss: the then-current consumer
persisted 116–164 events/s and had 11,288 pending after 300 RPS. Do not treat that
historical implementation as current worker capacity. S3 was priced; S4 had
7000 second-target/seven first-target attempts including circuits/probes; S5 was
all translated OpenAI Chat upstream calls.

### S6, held streams

Slow readers consume 8 KiB/s; end cancellation is not successful completion.

| Settings | Streams the generator held | Streams the gateway held at once | Valid |
| --- | --- | --- | --- |
| Defaults: 10 s warmup and 60 s measured, streams opened over 70 s | 150 | 66 | NO, twice |
| `OLP_BENCH_WARMUP=3s OLP_BENCH_DURATION=15s`, streams opened over 18 s | 150 | 150 | yes |
| `OLP_BENCH_SCALE=0.1` at the defaults, which S6 now shortens to 18 s | 1,000 | 997 | yes, bounded by the kernel |

| Measure | Value (valid run) |
| --- | --- |
| Streams | 150 target, 150 held by the generator, 150 by the gateway at its peak (sampled each second) |
| Response size | 7,091,188 bytes (40,000 tokens) at 8,192 bytes a second |
| Gateway CPU | 273.27 ms for each stream held, net of idle; 1.25 CPUs on average over the 32.9-second run |
| TTFT overhead p50 / p95 / p99 | 632.86 / 1,290.88 / 1,574.11 ms |
| Gateway RSS before / peak / after | 184.6 / 188.3 / 108.6 MiB; growth 25.7 KiB for each stream held |
| Kernel TCP memory (`/proc/net/sockstat`, every 2 s) | Peak 174,729 pages, about 683 MiB at 4 KiB a page, in the gateway's run, and 92,184 pages in the baseline against the mock; `net.ipv4.tcp_mem`'s pressure threshold is 372,786 pages |
| Errors | 0 failed streams; the 125 streams of the measured period were still open when the generator canceled them at the end, which the error rate leaves out (`error_rate.gateway_canceled_at_end`), so the success rate does not apply |
| Allocations per stream held | 665,136 objects and 56.7 MiB, over the whole run and net of idle |
| Metadata | 150 of 150 delivered |

Opening 150 over seventy seconds failed to hold them together because the
30-second write deadline ended earlier streams. Eighteen-second opening held
150/150; its CPU/TTFT/allocation costs above need independent scale qualification.

The later 1000-stream run held 997 but was kernel-limited: about 6.8 GiB needed
socket memory versus 1.4 GiB TCP-pressure threshold. Valkey/PostgreSQL commands
also timed out. Eight/833 measured streams returned 502; TTFT p50/p95/p99 was
7.68/15.61/19.20 seconds, CPU 62.64 ms/held stream, peak RSS 311.9 MiB and all
1001 metadata events delivered. Its harness validity does not make it a capacity
result. Larger runs need raised qualified kernel limits or remote peers.

### The comparison script

Historical 2026-10-06 run at `ed2586cb` plus consumer-batching change, same CPU pins;
S1–S5 ran at 3% with two LiteLLM workers. Shared load ranged 0.9–12.3; other tests
ran during S2. S1 was repeated after a script edit. No full-scale comparative
target was judged; smoke metadata target passed S1–S3.

```sh
OLP_BENCH_SCALE=0.03 OLP_BENCH_GATEWAY_CPUS=0-1 OLP_BENCH_MOCK_CPUS=2-3 \
  OLP_BENCH_LOADGEN_CPUS=4-5 OLP_BENCH_OUT=.local/bench-compare scripts/bench-compare.sh
```

| Scenario | Gateway | p50 ms | p95 ms | p99 ms | TTFT p95 ms | rps/vCPU | req/cpu-s | cpu ms/req | peak MiB | errors % | Valid |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | OLP | 1.38 | 1.92 | 2.05 | - | 15.0 | 218.8 | 4.57 | 193 | 0.000 | yes |
| S1 | LiteLLM | 15.39 | 24.40 | 37.49 | - | 15.0 | 50.8 | 19.68 | 1543 | 0.000 | yes |
| S2 | OLP | 2.05 | 10.24 | 29.70 | 10.14 | 15.0 | 83.3 | 12.01 | 193 | 0.000 | yes |
| S2 | LiteLLM | 8,400.90 | 16,460.80 | 20,292.61 | 16,566.05 | 12.5 | 13.1 | 76.34 | 1762 | 0.000 | yes |
| S3 | OLP | 6.34 | 14.82 | 22.27 | 13.23 | 45.0 | 112.9 | 8.86 | 191 | 0.000 | yes |
| S3 | LiteLLM | 23,246.27 | 35,336.70 | 39,792.45 | 35,104.42 | 16.9 | 15.9 | 62.85 | 3478 | 47.039 | NO |
| S4 | OLP | 1.55 | 2.67 | 2.88 | - | 15.0 | 206.3 | 4.85 | 189 | 0.000 | yes |
| S4 | LiteLLM | 21.94 | 53.92 | 109.38 | - | 15.0 | 44.5 | 22.46 | 1544 | 0.000 | yes |
| S5 | OLP | 2.05 | 2.05 | 2.05 | 2.64 | 15.0 | 76.2 | 13.13 | 189 | 0.000 | yes |
| S5 | LiteLLM | 31.74 | 65.54 | 101.38 | 63.55 | 15.0 | 22.1 | 45.34 | 1566 | 0.000 | yes |

LiteLLM S2 queued despite success; S3 offered only 60.6%, dropped 1144 and failed
47.04%, so its comparative targets are invalid. This is two workers on this host,
not correctly sized production LiteLLM. S5's identical OLP percentiles are HDR
steps, not exact equal samples. Comparison uses the pinned image/profile above;
cgroup memory versus OLP RSS is not like-for-like.

### Reading these figures

Percentile differences and p99 at small sample counts are noisy. S2/S5 tails were
not investigated. CPU/request changes with offered rate; approximate allocation
counters trail cached spans. Shared tenancy and single runs preclude production
throughput or full-rate efficiency claims.
