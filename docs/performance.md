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
a reduced-rate smoke run on a shared 8 vCPU development virtual machine
([Smoke run](#smoke-run)): OLP's scenarios S1 to S6 at 30% of the roadmap's
rates or less, and one scenario of the LiteLLM comparison at 5%, to show that
the script runs. They show that the harness works and what its output looks
like. They are not reference numbers, they are not comparable with LiteLLM's
published figures, they say nothing about whether a target is met, and they must
not be quoted as the performance of either gateway. LiteLLM's published figures
appear as LiteLLM publishes them, with links; the smoke run is the only place
this repository measures LiteLLM, and it is not a reference run.

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
more TCP memory than most single hosts have. Its streams must also all open
within the thirty seconds after which the gateway ends a stream whose writes are
blocked, which the default 70 seconds of warmup and measured period do not allow
([Smoke run](#s6-held-streams)). The
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
command with a smaller `OLP_BENCH_SCALE`; [Smoke run](#smoke-run) records the
settings of one, and the shortened warmup and period that S6 needs.

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
| Whole request | `internal/gateway` `BenchmarkGateway`, `BenchmarkGatewayLargePrompt` | Serves a request from key lookup to the last byte written, with the upstream and the client held in memory: unary, streamed, and an OpenAI stream translated for an Anthropic client; and the request of S3, a prompt of 100,000 tokens from a key with a cost budget, unary and streamed, and the same prompt sent as an Anthropic client sends it, with the process's CPU time per iteration, garbage collection included, reported beside the wall time |
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
  for none. The one exception is the shared cooldown: a gateway that has a
  limiter reads it before every attempt, whatever the key and the target limit,
  which costs an unconfigured key one Valkey round trip an attempt. The test
  does not cover that read ([tests](../tests/README.md#microbenchmarks)).
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

**These are reduced-rate, non-reference numbers from a shared 8 vCPU virtual
machine.** They are not reference results: the machine is a shared development
VM without pinned reference hardware, the rates are 1% to 30% of the roadmap's,
and the services are the development compose file's. They are not comparable
with LiteLLM's published figures, which come from a different test on a
different deployment. **No roadmap target has been judged**: every target that
needs full scale or a comparison is `not_checked` or `needs_comparison` in the
results, and this guide does not judge them. The figures below are copied
from the result files that `scripts/bench.sh` wrote under `.local/` (rounded to
two decimals, and to one for requests per CPU second and memory), to show what
the harness produces and that it runs end to end on this tree.

| | |
| --- | --- |
| Run | 2026-10-03, 22:16 to 23:04 UTC, by a coding agent session, one scenario to a `scripts/bench.sh` invocation |
| Commit | `9ef71679`, on a clean tree. The commit that records these numbers also changes the wording of S6's validity message and the README's S6 notes, which alters no measurement |
| Host | KVM guest with 8 vCPUs (`nproc` 8), Intel Core Processor (Haswell, no TSX), 22.9 GiB and no swap, kernel 7.0.0-34-generic, Go 1.27.1, Node.js 26.8.2, Docker 29.8.2 with Compose 5.5.1. The guest exposes no CPU frequency governor. `net.ipv4.tcp_mem` is 279588 372786 559176 pages and the descriptor limit 524,288 |
| Machine state | Before every run: no benchmark, `olp`, mock, load generator, LiteLLM or `go test` process, no container and no Docker volume. The machine was not idle: a host PostgreSQL 18, a Valkey server, the Docker daemon and some developer tooling sessions (editors, coding agents) ran beside the runs. A 3-second `vmstat` before S3 and every run after it showed 97 to 99% idle across the 8 CPUs; it was not taken before S1, S2, S4 and S5. The one-minute load average at the check was 0.73 before S1 and 1.39, 2.85 and 1.62 before S2, S4 and S5, which ran back to back and so include the scenario before. From S3 on, the script waited for it to fall below 0.8 first, and it read 0.49 to 0.79. The load the harness read when each test started, after the services and the build, is in the first table |
| CPU pinning | Gateway CPUs 0-1, mock upstream 2-3, load generator 4-5, so the gateway had 2 vCPUs (read back from `/proc`) and `pins_disjoint` is true in every result. CPUs 6-7 were left free. The PostgreSQL and Valkey containers were not pinned, nor were the harness's other processes |
| OLP | `olp 0.1.0` from `make build-go`, SHA-256 `ccebd834a24195f6c6f11823db8f5d8ad4c09f453672b428022657d14f4f9430`, for every run |
| Mock upstream | Two builds of the same source. `go build -mod=readonly -trimpath -tags bench -o .local/bin/mockupstream ./tests/bench/cmd/mockupstream` (dynamically linked, SHA-256 `4b40fa6b12ef755d24f23dee602c80f40087454486b439b38b509c0943308db5`) served S1 to S5, the valid S6 run and the first default-settings S6 run. `scripts/bench-compare.sh` then replaced that file with its own static build (`CGO_ENABLED=0`, SHA-256 `fb9fed965c3bfd99d8c3b0e2732e5352657795f1cd32bfe7b0af048c2f654f5c`), which served the comparison, the two higher-rate points and the second default-settings S6 run. The results record the `olp` hash and not the mock's |
| Services | `scripts/bench.sh`'s default (`BENCH_SERVICES=compose`): `postgres:18` (image ID `sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722`) and `valkey/valkey:9` (`sha256:418652cfb58ef879d4978c33553735d7147016032d5aefaa14c828e611eb9dfd`, `appendonly yes`), with the development compose file's settings |
| Gateway limits | Derived by the scenarios and recorded in each result under `gateway.derived_limits`; for example 1,032 requests in flight and a pool of 512 connections for S1, and 1,624 and 664 for S6 |

Each scenario was run once with these settings, S6 apart (below), from the
repository root, after `make build-go` and the mock's build above had made the
binaries:

```sh
export OLP_TEST_BINARY=$PWD/.local/bin/olp OLP_BENCH_MOCK_BINARY=$PWD/.local/bin/mockupstream
export OLP_BENCH_GATEWAY_CPUS=0-1 OLP_BENCH_MOCK_CPUS=2-3 OLP_BENCH_LOADGEN_CPUS=4-5

# S1, S2, S4 and S5, one invocation each: 100 requests per second,
# the default 10 s of warmup and 60 s measured
OLP_BENCH_SCALE=0.1 BENCH_SCENARIOS=S1 scripts/bench.sh

# S3: 30 requests per second
OLP_BENCH_SCALE=0.01 BENCH_SCENARIOS=S3 scripts/bench.sh

# S6: 150 streams, opened over 18 s
OLP_BENCH_SCALE=0.015 OLP_BENCH_WARMUP=3s OLP_BENCH_DURATION=15s BENCH_SCENARIOS=S6 scripts/bench.sh

# Two further points at higher rates, with OLP_BENCH_OUT=.local/bench-extra
OLP_BENCH_SCALE=0.3 BENCH_SCENARIOS=S1 scripts/bench.sh    # 300 requests per second
OLP_BENCH_SCALE=0.03 BENCH_SCENARIOS=S3 scripts/bench.sh   # 90 requests per second
```

Partway through, `scripts/bench-compare.sh` rebuilt the mock upstream in place
(see the table above), so the last three invocations ran against the static
build and the earlier ones against the dynamic one.

The scale was chosen at the start (0.1, and the smaller S3 and S6 below) and not
searched for, so these are not the highest rates the gateway holds: in S1 to S5
it used between 0.36 and 1.23 of its 2 CPUs, on average over each run. Every
run below was valid at the first attempt, so no rate was lowered, apart from
S6, whose default settings did not give a valid run at 150 streams
([S6](#s6-held-streams)).

### S1 to S5

Added latency is the difference of percentiles between the same load straight at
the mock and through the gateway, for all requests (`added_latency_ms.all`); the
time to first token is for the streaming scenarios. The sustained rate is what
finished in the measured period, 60 seconds after 10 of warmup. `load` is the
one-minute load average the harness read at the start of the test.

| Scenario | Scale | Target rps | Sustained rps | Valid | Added p50 / p95 / p99 ms | TTFT overhead p50 / p95 / p99 ms | Load |
| --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 0.1 | 100 | 100 | yes | 1.65 / 2.83 / 5.89 | - | 1.77 |
| S2 | 0.1 | 100 | 100 | yes | 3.07 / 9.22 / 20.48 | 2.88 / 8.85 / 20.29 | 2.85 |
| S3 | 0.01 | 30 | 30 | yes | 5.95 / 11.04 / 13.63 | 5.02 / 9.76 / 13.04 | 1.73 |
| S4 | 0.1 | 100 | 100 | yes | 1.52 / 2.48 / 3.36 | - | 1.62 |
| S5 | 0.1 | 100 | 100.05 | yes | 5.12 / 26.62 / 40.96 | 4.53 / 25.46 / 38.38 | 2.49 |
| S1 | 0.3 | 300 | 300 | yes | 1.38 / 2.03 / 4.59 | - | 2.32 |
| S3 | 0.03 | 90 | 90.03 | yes | 8.32 / 20.58 / 27.65 | 6.62 / 17.46 / 24.30 | 2.28 |

The offered rate was 99.998% or more of the target in every run, and none of
these seven runs had a failed request, a dropped one or a negative added
latency. The last two rows are the higher-rate points.

| Scenario | Target rps | Gateway CPUs used (of 2) | Requests per CPU second | CPU ms per request | RSS before / peak / after MiB | Errors % | Metadata delivered | Wait for metadata after the load | Allocations per request: objects / KiB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | 100 | 0.43 | 232.7 | 4.30 | 185.7 / 188.7 / 69.7 | 0 | 7000 of 7000 | 0.0 s | 1,063 / 86.0 |
| S2 | 100 | 0.98 | 100.2 | 9.98 | 182.2 / 182.4 / 93.9 | 0 | 7000 of 7000 | 5.0 s | 5,518 / 479.6 |
| S3 | 30 | 0.36 | 83.9 | 11.91 | 185.7 / 185.9 / 72.8 | 0 | 2100 of 2100 | 0.0 s | 1,883 / 3,394.1 |
| S4 | 100 | 0.42 | 242.0 | 4.13 | 186.0 / 188.5 / 69.3 | 0 | 7000 of 7000 | 0.0 s | 1,151 / 93.5 |
| S5 | 100 | 1.23 | 80.1 | 12.49 | 185.5 / 189.1 / 88.0 | 0 | 7000 of 7000 | 16.6 s | 10,283 / 1,019.9 |
| S1 | 300 | 0.64 | 470.1 | 2.13 | 185.0 / 187.6 / 71.6 | 0 | 21000 of 21000 | 68.7 s | 844 / 69.3 |
| S3 | 90 | 0.91 | 99.4 | 10.06 | 186.2 / 186.2 / 80.5 | 0 | 6300 of 6300 | 4.6 s | 1,797 / 3,384.2 |

CPU per request, requests per CPU second and allocations are over the whole run,
warmup included, and the requests the gateway answered successfully; the
gateway's CPU is net of what it spends idle. Metadata delivered is events in
PostgreSQL against requests admitted, after the harness waited for them to stop
arriving; in every run the pipeline had delivered all of them, none were dropped
or abandoned, Valkey was healthy and `request-metadata-lost` was `met`. The wait
shows how far behind the load the one consumer was: in the runs that had more
than a few events to wait for, it persisted 116 to 164 events a second, and at
300 requests per second, a rate above that, 11,288 events were still to arrive
when the load ended.

What else the results record: S3's key accrued a cost with no unpriced attempt,
so the run did the budget work it exists for; S4's route made 7,000 attempts on
the second target and 7 on the first, the five failures that open its circuit
and the probes after it, with 1.001 attempts per request; S5's upstream calls were all OpenAI Chat
Completions (7,000 of them), as the guide describes.

### S6, held streams

S6 holds streams open against readers that take 8 KiB a second, so its figures
are not those of the other scenarios: no stream finishes, and the generator
cancels them at the end.

| Settings | Streams the generator held | Streams the gateway held at once | Valid |
| --- | --- | --- | --- |
| Defaults: 10 s warmup and 60 s measured, streams opened over 70 s | 150 | 66 | NO, twice |
| `OLP_BENCH_WARMUP=3s OLP_BENCH_DURATION=15s`, streams opened over 18 s | 150 | 150 | yes |

At the default settings the gateway ended every stream at 30.2 to 30.5 seconds
(150 of 150 `client_cancelled` records with that duration in the gateway's log
of the second run), which is the 30-second write deadline of
[`responseWriteTimeout`](../internal/gateway/server.go), so only the streams of
the last 30 seconds were open at once: 66 of 150 in both runs, where the
harness's message said to raise the length of the completions. Streams opened
over 70 seconds and ended after 30 are never all open at once, whatever their
number, as long as their writes block soon after they open, as these did. The
message now names both causes. At 18 seconds all 150 were open together from
the opening of the last until the gateway ended the first, 12 seconds later,
and the generator canceled the rest at 33 seconds. 150 is the most streams run
here: a larger count was not tried.

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

At 273 CPU milliseconds each, the 150 streams cost about 41 CPU seconds, which
would be 2.3 CPUs if all of it fell in the 18 seconds the streams were opened
over: more than the gateway's two, and the time to first token grew with it. That
was not checked beyond the arithmetic. The default settings' first run, with
streams opened over 70 seconds, spent 304.71 ms for each stream and added 2.22,
2.90 and 11.23 ms of time to first token at p50, p95 and p99.

### The comparison script

`scripts/bench-compare.sh` was run for S1 on this tree, to confirm that it still
works, in a result directory of its own, with the same pins (LiteLLM's two
workers on CPUs 0-1) and these settings:

```sh
OLP_BENCH_SCALE=0.05 OLP_BENCH_DURATION=10s OLP_BENCH_WARMUP=3s BENCH_SCENARIOS=S1 \
  OLP_BENCH_GATEWAY_CPUS=0-1 OLP_BENCH_MOCK_CPUS=2-3 OLP_BENCH_LOADGEN_CPUS=4-5 \
  OLP_BENCH_OUT=.local/bench-compare scripts/bench-compare.sh
```

It ran to the end and wrote `compare.json` and `compare.md`. The table is copied
only as proof of that: each load was 500 requests over 10 seconds, so its p99 is
five samples, and the script judged no target (four were `not_checked` at scale
0.05, and `request-metadata-lost` was `met`). The baselines of the two sessions
differed by 0, -0.06 and -0.16 ms at p50, p95 and p99.

| Scenario | Gateway | p50 ms | p95 ms | p99 ms | rps/vCPU | req/cpu-s | cpu ms/req | peak MiB | errors % | Valid |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S1 | OLP | 1.49 | 4.66 | 5.79 | 25.0 | 214.1 | 4.67 | 188 | 0.000 | yes |
| S1 | LiteLLM | 12.53 | 25.14 | 29.66 | 25.1 | 56.1 | 17.84 | 1566 | 0.000 | yes |

LiteLLM ran as `ghcr.io/berriai/litellm:v1.103.2@sha256:f63fb81b831b170ec16851e23c36ac5bf52ef106b271406429524a2ed730bbfd`
in the `production` profile. Its memory is the cgroup's, which is not the same
measurement as OLP's resident set ([reading a comparison](#reading-a-comparison)).
The earlier smoke run of S2 to S5 against LiteLLM, made on a busier machine with
an earlier revision of the harness, has been removed with this one: it is in the
history of this file, and was not made on this tree.

### Reading these figures

- **Added latency is a difference of percentiles**, and at 100 requests per
  second the p99 of a 60-second period rests on 60 requests. The tails of S2 and
  S5 (p95 of 9.22 and 26.62 ms) are larger than those of the unary scenarios; S5
  used 1.23 of the gateway's 2 CPUs on average. They were not investigated, and
  nothing here says whether they come from the gateway, the machine or the
  harness.
- **CPU per request depends on the rate.** S1 cost 4.30 ms at 100 requests per
  second and 2.13 ms at 300, and S3 11.91 ms at 30 and 10.06 at 90, so a figure
  measured at a smoke rate is not the figure at full rate, and requests per CPU
  second here are not what a CPU carries at the roadmap's rates.
- **Allocations** are read from counters that trail by the objects of the
  processors' cached spans, so at these rates they are indicative.
- **The machine is shared**, and another tenant of the host or of the VM can
  move any one of these figures; each was measured once.
