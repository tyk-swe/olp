# Performance

How OLP measures what it adds to a request, how to reproduce the measurements
against LiteLLM, and the gates that keep hot-path performance from slipping. The
targets come from the
[M1 milestone](roadmap/m01-measured-advantage.md#m11-gateway-benchmark); the
harness lives in [`tests/bench/`](../tests/bench/README.md).

## Status of the numbers

**No reference results are published yet.** The harness is built and has been
smoke-tested, but the full-rate runs on reference hardware have not been made,
so no target below has been judged. The only measurements in this guide are from
a reduced-scale smoke run on a shared, busy 8 vCPU development machine
([Smoke run](#smoke-run)), of OLP and of LiteLLM. They show that the harness
works and what its output looks like. They are not reference numbers, they say
nothing about whether a target is met, and they must not be quoted as the
performance of either. LiteLLM's published figures appear as LiteLLM publishes
them, with links; the smoke run is the only place this repository measures
LiteLLM, and it is not a reference run.

Reference results will be published under
[Reference results](#reference-results) when they exist, with the
[record](#record-of-a-reference-run) filled in so that anyone can reproduce
them.

## Method

**Open loop.** The load generator schedules request *k* at `start + k / rate`
and never waits for the system under test. A closed-loop generator that sends
the next request when the previous one returns slows down exactly when the
gateway does, and so hides the queueing it should show (coordinated omission).
Here a stalled gateway shows up in the percentiles, because every request that
should have been sent during the stall records the wait it would have suffered.
Sends are bounded by an in-flight cap and a backlog; a request that finds
neither is dropped and counted, never silently delayed.

**Latency from the scheduled time.** Latency is measured from the scheduled send
time to the end of the response; for a stream, to its closing event (`[DONE]`,
`message_stop` or the chunk with a finish reason), which is when a client has
its answer, so whatever a gateway does after sending it and before it ends the
response, such as settling a budget, is not charged to the stream. The
generator still reads the response to its end, and a stream that closes without
its event, or with an error, is a failure timed to the end of the response. Time
to first token is the arrival of the first data frame, also from the scheduled
time. A warmup period at the same rate precedes the measured one and is
excluded from every statistic.

**High-dynamic-range histograms.** Latencies are recorded in HDR histograms
(`github.com/HdrHistogram/hdrhistogram-go`, three significant figures, from one
microsecond to an hour), so p99 and p99.9 are not smoothed by sampling or
bucketing, and no raw sample list grows with the run.

**Baseline subtraction.** The gateway's overhead is the difference of
percentiles between two runs of the same load in the same session: one straight
at the mock upstream, then one through the gateway. The mock's own time to first
token and token intervals cancel out. Each gateway gets a baseline of its own,
run immediately before it, and a comparison prints how far the two baselines lie
from each other, since they should not differ: the same load against the same
mock on the same CPUs. A difference means the sessions were not alike.
Differences of percentiles can be negative on a noisy machine, which is a sign
the run is not good enough to judge, and the harness takes it for one: a gateway
cannot answer faster than the upstream it calls, so an added p50 or p95 below
zero by more than a baseline's jitter, a millisecond or a tenth of the
baseline's figure, makes the run invalid, and no target is met by a negative
figure at any percentile.

**A deterministic upstream on its own CPUs.** The mock upstream serves OpenAI
(Chat Completions and Responses), Anthropic and Gemini dialects with
configurable time to first token, token interval, output length, status codes
and mid-stream failures. It runs as a separate process, and a reference run pins
it to CPUs the gateway and the load generator do not use.

**A valid run.** A run is `valid` only if the generator delivered its schedule:
nothing dropped or abandoned, at most 1% of sends later than 5 ms after their
scheduled time, and at least 99% of the target rate offered. It must also have
delivered answers: the latency of a run is that of its successful requests, so a
gateway that fails requests quickly would look faster for it, and a run in which
more than 0.1% of the requests of either load failed is invalid (S6 aside, whose
streams are canceled when it ends). A gateway that shed requests, an added
latency below zero by more than jitter, or metadata still arriving after the
drain also makes the run invalid. A target that rests on latency is judged only
on valid runs.

**What a run records.** Added latency at p50, p95 and p99, and
time-to-first-token overhead; sustained requests per second (those that finished
during the measured period, so a gateway that falls behind does not report the
offered rate), per vCPU and per CPU second; the gateway's CPU time per request
it answered successfully, net of what it spends idle;
resident memory before, at its peak and after; the error rate; and, for OLP,
request-metadata completeness (events delivered against requests admitted).
Allocations per request of the whole gateway are read from the Go runtime's
heap allocation counters, which the release binary serves on its private
`/metrics` listener as `go_memstats_mallocs_total` and
`go_memstats_alloc_bytes_total`: what it allocated over the run, less what it
allocates idle over the same time, over the requests it answered successfully.
The `testing.B` benchmarks of the [regression gate](#regression-gate) report and
gate the allocations per operation of individual paths.

**A reference run** is one at full scale, with the gateway, the mock and the
load generator each pinned to CPUs of their own, no two sharing one, and valid
load runs. Everything else is a smoke run.

## Scenarios and targets

| ID | Scenario | At full scale |
| --- | --- | --- |
| S1 | Chat Completions, short prompt, unary | 1,000 RPS |
| S2 | Chat Completions, short prompt, streaming 64 tokens at 20 ms intervals | 1,000 RPS |
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens: 16`, key with a cost budget | 3,000 RPS |
| S4 | First target returns 503, second succeeds | 1,000 RPS |
| S5 | Anthropic Messages streaming, translated to an OpenAI upstream | 1,000 RPS |
| S6 | Slow-reader streams held open | 10,000 streams |

S6 is OLP only: no target compares it, and its socket buffers at full scale need
more TCP memory than most single hosts have. The
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

A target the first baseline misses becomes M1 scope (profiling and hot-path
work), not a revised target.

A target is `met`, `missed` or `not_checked`. It is checked only under the
conditions it is stated for, and one that was not says why: a smoke run does not
judge any of them. With `OLP_BENCH_ENFORCE=1` a missed or unchecked target, a
run that is not a reference run, and baselines that disagree at p50 or p95 all
fail the run; the baselines' p99 is noted, since a tail varies between sessions
even on pinned CPUs.

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
command with `OLP_BENCH_SCALE=0.02`, `OLP_BENCH_DURATION=8s` and
`OLP_BENCH_WARMUP=3s`.

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
- **Request metadata outlasts the load.** One metadata consumer persists an event
  at a time, in a PostgreSQL transaction of its own, so at the rates of S1 to S3
  the events of a run arrive minutes after its load ends: a half-rate S1 run on a
  development machine (8 vCPUs, otherwise idle) delivered 12,500 events in about
  86 seconds from the start of the load, about 150 events a second, and a run at
  full rate produces 70,000 to 210,000. The harness waits for as long as events
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
- **S4's failure policy is matched.** OLP's circuit opens after five consecutive
  failures for thirty seconds. LiteLLM's router is set to cool a deployment down
  after five failures for thirty seconds, with one retry, since OLP's route
  allows two attempts; LiteLLM's own defaults are three failures and five
  seconds. A gateway that keeps a failing target out of rotation measures the
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

Hot-path microbenchmarks (`testing.B`) sit beside the code they measure, in the
test files of these packages. Each iteration does the work of one request, or
one stream, on a fixed fixture with no database or network:

| Path | Benchmark | What an iteration does |
| --- | --- | --- |
| Authentication | `internal/runtime` `BenchmarkAuthenticate` | Resolves an API key among a thousand: a valid one, an unknown one and one with the wrong secret |
| Planning | `internal/runtime` `BenchmarkPlanRequest`, `BenchmarkEligibility` | Weighs the targets of a route against the request and the policies in force, ranks their credential slots and orders the attempts, with and without a price catalogue; checks that a credential version may still serve |
| Credential selection | `internal/runtime` `BenchmarkSelectSlots` | Ranks the slots of one provider |
| Codecs | `internal/protocols` `BenchmarkTranslateRequest`, `BenchmarkTranslateResponse` | Reads a client's request in OpenAI Chat, Anthropic or Gemini form and writes it for a target, each dialect to itself and to and from OpenAI Chat; reads an upstream's response and writes it for the caller |
| Stream relay | `internal/protocols` `BenchmarkTranslateStream`, `internal/gateway` `BenchmarkStreamWriter` | Relays a stream of 64 deltas to the client, natively and translated, through the server-sent-event decoder; writes one frame to the client and renews its write deadline |
| Admission | `internal/gateway` `BenchmarkAdmission` | Admits a request and settles it for a key with no limits, with request and token limits, with a concurrency limit, with a cost budget and in a budget group, and an attempt for a target with a quota, against a limiter client that keeps no state |
| Whole request | `internal/gateway` `BenchmarkGateway` | Serves a request from key lookup to the last byte written, with the upstream and the client held in memory: unary, streamed, and an OpenAI stream translated for an Anthropic client |
| Estimation | `internal/operations/tokenization/estimate` `BenchmarkEstimate`, `BenchmarkMeter`, `BenchmarkHeuristic`, `BenchmarkWalkAgainstLegacy` | Counts a short prompt for each family; counts the bounded part of a long prompt exactly and charges the rest at the measured ratio; walks a request of 30, 4,000 and 100,000 tokens beside the walker it replaced |
| Cost | `internal/usage` `BenchmarkCostBound`, `BenchmarkPriceCost` | Bounds the cost a request could incur, which admission reserves for a key with a cost budget, and prices the usage of an attempt |
| Plugin signing | `internal/plugins` `BenchmarkSign` | Runs a plugin's signing hook, interpreted and compiled |

The gate skips four benchmarks of the encoder itself, `BenchmarkLoad`,
`BenchmarkCount`, `BenchmarkUnbrokenPieces` and `BenchmarkMeterWorstCase`,
whose input is chosen to be slow rather than to be what a request pays
([tests](../tests/README.md#microbenchmarks) says why); `BENCH_SKIP` selects
them.

What they do not cover is the rest of the request: the HTTP server, the
executor's dispatch to the upstream, usage accounting and the metadata pipeline,
and the admission checks that reach Valkey, which need services and are measured
by the scenarios instead. The allocations per request of the whole gateway are
measured by the scenarios, which see everything a request touches; the gate
gates the allocations per operation of the paths above. A feature that is not
configured allocates nothing for a request, and `TestUnconfiguredFeaturesAddNoAllocations`
in `internal/gateway` holds that with `testing.AllocsPerRun` as an ordinary unit
test, so `make test` does too.

`make bench-gate` (`scripts/bench-gate.sh`) runs every `internal` package that
declares a benchmark at the pull request's merge base and at the working tree,
one sample of each in turn so that drift on the machine hits both alike, then
compares them with a pinned `benchstat`. `scripts/check-benchstat.mjs` fails on
a statistically significant regression above 10% in time or allocations per
operation. Bytes per operation are reported and never gate. The `bench` job of
the CI workflow runs the same script on every pull request and keeps its samples
and report as the `bench-gate` artifact. A sample of every benchmark takes about
two minutes at the default benchtime and one at 500 ms on an 8-vCPU machine
(the packages' benchmarks add up to about 60 seconds at 500 ms, of which the
estimate package is a half), so CI runs each for 500 ms (`BENCH_TIME`) to keep ten samples a
side. That is about twenty minutes for the two sides before the trees are
compiled, and a regression is measured again, alone, with twice the samples. The
job's timeout is an hour, which leaves room for a slower runner or several
benchmarks to confirm; if the job nears it, lower `BENCH_COUNT` (at least 6) or
`BENCH_TIME` rather than raising the timeout.

A benchmark is compared only if the base has it too, so a pull request that adds
one is not gated by it, and its first comparison is the next pull request's.

The gate is built so that it cannot pass without having compared anything:

- **A regression must reproduce.** One comparison in twenty reports a change in
  a benchmark that did not change, and the gate compares many. The benchmarks
  that show a regression in the first comparison are measured again, alone and
  with twice the samples, and fail the gate only if they show it again. The
  summary lists the ones that did not as noise.
- **Nothing compared is a failure.** A pull request that deletes or renames
  every benchmark, or one in which the base or the head produced no benchmark
  result, fails rather than passes, and says so in the job summary. The one
  exception is a base that declares no benchmark at all, which has nothing to
  regress from: the gate says that and passes.
- **Removing a benchmark is a decision.** A benchmark of the base that the head
  lacks is one whose regressions the gate can no longer see, so it fails the
  gate unless the removal is allowed: set `BENCH_ALLOW_REMOVED=1`, which CI does
  for a pull request that carries the `allow-removed-benchmarks` label (the label
  is read when the workflow starts, so add it before pushing, or push again).

| Variable | Meaning |
| --- | --- |
| `BENCH_BASE` | Base ref (default `origin/main`, then `main`) |
| `BENCH_COUNT` | Samples per side, at least 6 (10); a regression is measured again with twice as many |
| `BENCH_THRESHOLD` | Regression percentage that fails (10) |
| `BENCH_ALLOW_REMOVED` | `1` lets benchmarks of the base be absent from the head (0) |
| `BENCH_TIME`, `BENCH_RUN`, `BENCH_FILTER` | `-benchtime`, the `-bench` pattern and a package filter |
| `BENCH_OUT` | Output directory (`.local/bench/gate`) |

The gate compares a pull request with its base. It does not replace the
scenarios, which measure the whole gateway.

## Performance budget

- A feature that is not configured adds no allocations and no Valkey or
  PostgreSQL round trips to a request. Where a path is benchmarked, a change that
  makes it allocate shows as an allocations-per-operation regression in the gate,
  so a change to a hot path adds or extends the benchmark for it. Admission is
  held to this directly: a test asserts that admitting a key with no limits and a
  target with no quotas allocates nothing and, with no limiter at all, reaches
  for none.
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
| OLP | Version, git commit, binary SHA-256 (in each result's `environment`) |
| LiteLLM | Image digest, profile of each scenario, workers |
| Services | PostgreSQL and Valkey versions, image digests and settings, and whether `BENCH_SERVICES=external` |
| Toolchain | Go, Node.js, Docker and Compose versions |
| Run | Exact commands and environment, scale, duration and warmup |
| Results | `compare.md` for each invocation, and whether `OLP_BENCH_ENFORCE=1` passed |
| Files | Where the result JSON and the load generator's reports are kept |

## Reference results

None yet.

## Smoke run

**These are reduced-scale, non-reference numbers from a shared 8 vCPU machine.**
They are what `make bench-compare` printed, copied unedited, to show the shape
of its output and that the harness runs end to end. No target was judged, and
nothing here says how OLP or LiteLLM perform.

| | |
| --- | --- |
| Run | 2026-10-02, `make bench-compare` with `OLP_BENCH_SCALE=0.02`, `OLP_BENCH_DURATION=8s` and `OLP_BENCH_WARMUP=3s`: 20 requests per second (60 for S3), measured for 8 seconds after 3 of warmup |
| Pins | Gateway CPUs 2-3 (so LiteLLM ran two workers), mock upstream CPU 6, load generator CPU 7 |
| Host | Intel Core Processor (Haswell, no TSX), 8 vCPU virtual machine with 22.9 GiB, kernel 7.0.0-34-generic, shared with other processes: the load average at the start of the runs was between 4.1 and 14.8 |
| OLP | `olp 0.1.0`, binary SHA-256 `c67e84576c1d42642852f24465966ebfef42da0872c7cbb60a27903f3f7afe8f` |
| LiteLLM | `ghcr.io/berriai/litellm:v1.103.2@sha256:f63fb81b831b170ec16851e23c36ac5bf52ef106b271406429524a2ed730bbfd`, `production` profile for S1, S2, S4 and S5 and `high-throughput` for S3 |
| Services | The development compose file's PostgreSQL 18 and Valkey 9 |
| Harness | An earlier revision than this guide describes: it counted the throughput from every request that eventually succeeded and the CPU cost from every request sent, and did not yet make a failing run or a negative added latency invalid. The raw reports of every run below record no failed request, in the warmup or the measured period, and no negative added latency beyond jitter, so only `rps/vCPU` was counted differently |

| scenario | gateway | p50 ms | p95 ms | p99 ms | ttft p95 ms | rps/vCPU | req/cpu-s | cpu ms/req | peak MiB | errors % | valid |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | OLP | 2.91 | 6.21 | 11.20 | - | 10.0 | 193.5 | 5.17 | 177 | 0.000 | yes |
| S1 | LiteLLM | 15.12 | 24.83 | 35.22 | - | 10.0 | 44.3 | 22.55 | 1500 | 0.000 | yes |
| S2 | OLP | 3.07 | 7.17 | 8.19 | 5.44 | 10.0 | 75.9 | 13.18 | 178 | 0.000 | yes |
| S2 | LiteLLM | 223.23 | 610.30 | 671.74 | 536.50 | 10.0 | 10.9 | 92.15 | 1538 | 0.000 | NO |
| S3 | OLP | 537.98 | 968.74 | 1130.56 | 1011.54 | 30.0 | 42.6 | 23.49 | 179 | 0.000 | NO |
| S3 | LiteLLM | 5801.31 | 8818.34 | 9669.02 | 9036.88 | 30.0 | 19.7 | 50.87 | 2577 | 0.000 | NO |
| S4 | OLP | 2.85 | 4.78 | 3.20 | - | 10.0 | 168.8 | 5.92 | 176 | 0.000 | yes |
| S4 | LiteLLM | 14.11 | 24.77 | 37.14 | - | 10.0 | 44.1 | 22.67 | 1501 | 0.000 | NO |
| S5 | OLP | 2.05 | 3.07 | 1.02 | 0.69 | 10.0 | 63.6 | 15.72 | 177 | 0.000 | NO |
| S5 | LiteLLM | 37.89 | 164.86 | 260.10 | 115.81 | 10.0 | 19.2 | 51.98 | 1481 | 0.000 | yes |

`valid` is `NO` where the load generator sent more than 1% of a run's requests
more than 5 ms late, which on a busy machine it did: S2 (LiteLLM, 4 of 160
requests), S3 (OLP's baseline, 11 of 480, and LiteLLM, 24 of 480), S4 (LiteLLM,
2 of 160) and S5 (OLP's baseline, 2 of 160). The baselines of the two sessions
differed at p99 in S1 and S3. Every target was `not_checked`, because the runs
were at scale 0.02, with the reasons the targets table of `compare.md` gives;
three targets that OLP's own scenario judges alone, request-metadata loss for S1
to S3, were `met`.

Three things about reading it. Added latency is a difference of percentiles, so
it can be erratic: in S4 and S5 OLP's p99 is below its p95. CPU per request is
net of idle but, at 20 to 60 requests per second, may still contain work that
does not grow with the request rate, which a full-rate run spreads over many
more requests. And LiteLLM's container logs, kept under
`.local/bench/litellm/raw/`, held only start-up lines in these runs and, for S3,
PgBouncer's connection messages, with no per-request errors that would have cost
it CPU. S4's attempts per request were 1.0227 for OLP, from its attempt records,
and 1.0545 for LiteLLM, from the mock's counters.
