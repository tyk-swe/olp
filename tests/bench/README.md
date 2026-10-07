# Gateway benchmark harness

Everything here carries the `bench` build tag, so `go build ./...`, `go vet ./...`
and `make test-go` ignore it. The harness's own tests, which need no services,
are what `make test-bench` runs, and `make test` and `make check` include it:

```sh
make test-bench   # go test -tags=bench -skip '^TestScenario' ./tests/bench/...
```

They are the only guard on the instruments (open-loop scheduling, latency from
the scheduled time, the percentiles, the mock's pacing, and what makes a run
valid and a target judged), so a change to anything here runs them, and they
run under `-race` in CI with the rest.

The scenarios, `TestScenarioS1` to `TestScenarioS6` and `TestScenarioS1Shadow`, are the one thing `make
test-bench` skips. They need services and fail without them, by design (a
benchmark that silently measured nothing would be worse than one that did not
run), so `go test -tags=bench ./tests/bench/...` with nothing skipped fails; run
them with `make bench`.

| Package | Purpose |
| --- | --- |
| `mockupstream`, `cmd/mockupstream` | Deterministic OpenAI (Chat Completions and Responses), Anthropic and Gemini upstream with controllable timing and failures |
| `loadgen`, `cmd/loadgen` | Open-loop constant-arrival-rate load generator that can drive any base URL |
| `benchtest` | In-process `http.RoundTripper` for tests that run on the fake clock of `testing/synctest` |
| the package itself (`scenarios_test.go` and its helpers) | The roadmap's scenarios S1 to S6, run end to end by `make bench` |

## Mock upstream

```sh
go run -tags=bench ./tests/bench/cmd/mockupstream -addr 127.0.0.1:0 -ttft-ms 20 -interval-ms 5 \
  -model 'primary:status=503' -model 'secondary:output_tokens=8'
# {"address":"127.0.0.1:41233","listener":"mock"}
```

It serves by path suffix: `.../chat/completions` (OpenAI), `.../responses`
(OpenAI Responses, which OpenAI providers are certified against too),
`.../messages` (Anthropic) and `.../models/{model}:generateContent` or `:streamGenerateContent`
(Gemini), each unary or as server-sent events, over HTTP/1.1 and cleartext
HTTP/2. `GET .../models` lists models in each dialect's shape so a provider can
be probed and discovered. Usage is reported in every dialect, with the prompt
token count derived from the request body (one token per four bytes). Bodies
are drained through a pooled buffer, never parsed or retained.

A request's behavior is the default, overlaid by the rule for the model it
names, overlaid by its headers. Rules and defaults use the same keys:

| Key (`-model`, JSON) | Header | Effect |
| --- | --- | --- |
| `ttft_ms` | `x-mock-ttft-ms` | Delay before the first frame; unary responses wait `ttft + (tokens-1) * interval` |
| `interval_ms` | `x-mock-interval-ms` | Gap between token frames |
| `output_tokens` | `x-mock-output-tokens` | Completion length, 1 to 100,000 |
| `status` | `x-mock-status` | 200, or 400 to 599 to fail every request with the dialect's error body |
| `error_delay_ms` | `x-mock-error-delay-ms` | Hold an error response (errors are immediate by default) |
| `fail_after_tokens` | `x-mock-fail-after-tokens` | Fail a stream after N tokens |
| `fail_mode` | `x-mock-fail-mode` | `abort` (connection closed or stream reset) or `error` (in-band error event) |
| `fail_first_n` | `x-mock-fail-first-n` | The first N requests of each model return 503 |

**OLP does not forward client headers upstream**, so a gateway run is shaped
by model rules alone; the headers serve direct runs and unit tests. To make a
route's first target fail and its second succeed, give the two targets
different upstream models and a `status=503` rule for the first. Provider
probing, discovery and certification call the mock too, so arm failing rules
after the provider is certified, with `PUT /_mock/config` (a JSON
`{"default": {...}, "models": {"name": {...}}}`) or `Server.SetConfig`.
`GET /_mock/stats` reports requests, streams, injected failures, in-flight
counts and per-model counts; `POST /_mock/reset` zeroes them, including the
fail-first-n counters.

## Load generator

```sh
go run -tags=bench ./tests/bench/cmd/loadgen -url http://127.0.0.1:8080 -api-key "$KEY" -model team-chat \
  -rate 3000 -duration 60s -warmup 10s -stream-share 0.5 -prompt-tokens 50k,75k,100k \
  -max-in-flight 8000 -json .local/bench/s3.json
```

Request k is due at `start + k/rate`; latency is measured from that scheduled
time, so a stalled gateway shows its queueing in the percentiles instead of
being hidden by a generator that waits (coordinated omission). Sends are
bounded by `-max-in-flight` and `-max-backlog`; a request that finds neither a
slot nor room to wait is dropped and counted. The report is `valid` only if
nothing was dropped or abandoned, at most 1% of sends were later than
`-late-after`, and the offered rate reached 99% of the target; use `-strict` to
turn an invalid run into exit status 2. Valid means the schedule was kept, not
that the answers were good: the failures of a gateway are the harness's to
judge (see below). Reports hold HDR-histogram summaries
(p50, p90, p95, p99, p99.9, max) for successful latency overall, unary and
streaming, time to first data frame, latency from the actual send, failures and
send lag, plus counts, error kinds, status codes and achieved rates. The
throughput is the successful requests that finished during the measured period,
warmup requests included, per second of it: what the system sustained, which a
system that falls behind and answers late does not report as the offered rate.
It needs a warmup at least as long as a request, or the period's first answers
are still missing. `loadgen.Compare(baseline, run)` computes the gateway's added
latency.

- `-prompt-tokens`: sizes used in equal shares; 0 is a short prompt. Streaming
  and unary requests each rotate through the sizes on their own, so the sizes
  and the modes stay independent: with two sizes and half the requests
  streaming, a rotation over the request number would give every stream one size
  and every unary request the other. Prompts are built from a fixed vocabulary of
  common words, one word to a token, so 100K tokens weigh about 520 KB.
- `-header 'x-mock-ttft-ms: 50'`: for direct runs against the mock.
- `-slow-read-bps N` reads each stream at N bytes per second on connections with
  a 4 KiB receive buffer (and, over HTTP/2, a 4 KiB per-stream window, which
  would otherwise be 4 MiB and swallow any stream whole). The kernel's socket buffers can absorb a short
  response whole: in a smoke run against the mock, 64-token responses (12 KB)
  finished writing while their readers were still crawling, whereas
  20,000-token responses (3 MB) kept the mock's writes blocked. S6 therefore
  always holds the connections and their per-stream state open, but exercises
  write deadlines only with responses larger than the buffers.
- A stream succeeds only when its dialect's closing event arrives: `[DONE]`,
  `message_stop`, or a Gemini chunk with a `finishReason`.
- On a busy shared 8-vCPU machine, a 100K-token request cost the generator
  about 0.7 ms of CPU and the mock about 0.5 ms on loopback, mostly in the
  kernel, so S3's 3,000 requests per second need a couple of dedicated cores
  for each; give them separate CPU sets. These are smoke figures, not reference
  results.

## Scenarios

```sh
make bench                                     # all six, at the roadmap's full rates
BENCH_SCENARIOS=S1,S4 OLP_BENCH_SCALE=0.05 \
  OLP_BENCH_DURATION=10s OLP_BENCH_WARMUP=3s make bench    # a smoke run
```

`scripts/bench.sh` starts a disposable PostgreSQL and Valkey (a unique compose
project, random ports), builds the release binary and the mock, runs the
selected scenarios and tears everything down; it prints a table of the results
afterwards (`scripts/bench-summary.mjs`). With `BENCH_SERVICES=external` it uses
the services `OLP_TEST_DATABASE_URL` and `OLP_TEST_VALKEY_URL` name instead,
which is how a reference run points it at tuned dedicated ones, and with
`OLP_TEST_BINARY` set it benchmarks that binary rather than building one.

| ID | Scenario | At scale 1 | Mock upstream |
| --- | --- | --- | --- |
| S1 | Chat Completions, short prompt, unary | 1,000 RPS | 20 ms to answer, 16 tokens |
| S2 | Chat Completions, short prompt, streaming | 1,000 RPS, 64 tokens at 20 ms intervals, about 1,300 streams open | 20 ms to first token |
| S3 | 50K, 75K and 100K-token prompts in equal shares, 50% streaming, `max_tokens` 16, key with a cost budget | 3,000 RPS | 20 ms to first token, 2 ms between tokens |
| S4 | First target returns 503, second succeeds | 1,000 RPS | the first target's model always answers 503 |
| S5 | Anthropic Messages streaming, translated to an OpenAI upstream | 1,000 RPS | as S2 |
| S6 | Slow-reader streams | 10,000 held open at once | 40,000-token completions, read at 8 KiB/s |
| S1-shadow | S1 with a shadow target sampling every request | 1,000 RPS | as S1, for both the serving and the shadow model |

A scenario runs this sequence in one session, and fails if any step does:

1. Start the mock upstream and a real `olp all` process (through
   `internal/testutil.StartProcess`) with a database of its own, so the
   scenarios do not touch each other's metadata, and provision a provider,
   route, key and budget through the management API as the console would.
2. Wait until the gateway serves the new configuration.
3. Run the load straight at the mock: the baseline.
4. Run the same load, at the same rate and with the same prompts, through the
   gateway, watching its CPU and memory.
5. Wait for request metadata to land in PostgreSQL, for as long as it keeps
   arriving, count it, and shut the gateway down cleanly (a process that cannot
   is a failure).
6. Write `.local/bench/<scenario>.json` and the two raw load reports under
   `.local/bench/raw/`.

The settings are environment variables, all optional.

| Variable | Meaning |
| --- | --- |
| `OLP_BENCH_SCALE` | Multiplies every rate and concurrency; 1 is the roadmap's full rates. Prompt sizes do not scale. |
| `OLP_BENCH_DURATION`, `OLP_BENCH_WARMUP` | The measured period (default 60s) and the warmup before it (10s). S6 opens its streams over at most 3 seconds of warmup and 15 measured, whatever these say, and then holds them for as long as its measured period: the gateway ends a stream thirty seconds after its writes block, so S6 must open them all within less than that (see [below](#what-to-know-when-reading-a-run)). |
| `OLP_BENCH_GATEWAY_CPUS`, `OLP_BENCH_MOCK_CPUS`, `OLP_BENCH_LOADGEN_CPUS` | Pin the process to CPUs, as `taskset` lists them (`0-1`, `2,4-5`). The gateway's vCPUs are the CPUs it may run on, read back from `/proc` as proof the pin took, and are what RPS per vCPU divides by. |
| `OLP_BENCH_ENFORCE` | `1` fails the test on a missed target. See below. |
| `OLP_BENCH_DRAIN` | How long the wait for request metadata may go without any arriving before it gives up (90s). The wait lasts as long as metadata keeps landing. |
| `OLP_BENCH_LATE_AFTER` | When a send counts as late for the generator's validity check (5ms). |
| `OLP_BENCH_S6_OUTPUT_TOKENS`, `OLP_BENCH_S6_READ_BPS` | The length of S6's completions and the pace of its readers. |
| `OLP_BENCH_OUT` | Where results go (`.local/bench`); a relative path is relative to the repository root. |
| `OLP_BENCH_KEEP_GATEWAY_LOG` | `1` copies the gateway's log, one line per request, to `raw/<scenario>-gateway.log`. |

### What a result holds

Every figure is measured; one that has no honest source is `null` beside a
reason. The result of S1 at full scale on reference hardware is the one to read
first; the rest of this section says what each field means.

- `added_latency_ms`: gateway minus baseline at p50, p95 and p99, for all
  successful requests and for unary and streaming ones apart. For a stream it
  is latency to its closing event, so what a gateway does between that and
  ending the response is not counted. `ttft_overhead_ms` is the same for the
  first token. Both are differences of percentiles, so they can be negative on a
  noisy machine, which is a sign the run is not good enough to judge, and the
  harness treats it as one: a gateway cannot answer faster than the upstream it
  calls, so an added p50 or p95 below zero by more than a baseline's jitter (a
  millisecond or a tenth of the baseline's figure) makes the run invalid, and a
  target is never met by a negative figure at any percentile.
- `throughput`: the sustained successful RPS (the requests that finished during
  the measured period), `rps_per_vcpu` over the gateway's vCPUs, and
  `gateway_cpu_ms_per_request`, which is the gateway's CPU time per request it
  answered successfully, net of what it spends idle: a request it failed or shed
  cost it little, and counting those would make a gateway that fails fast look
  the more efficient. At a
  low rate the idle share dominates the gross figure (kept beside it); at a
  rate where requests dominate, `requests_per_cpu_second` is what one fully
  used core carries.
- `resources`: resident memory of the gateway before the run, its peak (`VmHWM`,
  reset at the start of the run; where the kernel refuses the reset it is the
  largest sample instead, with a warning, since `VmHWM` would then include
  start-up) and after, thread count, the most inference
  requests it held at once, requests its admission pool shed, and the CPU time
  of the gateway, the mock and the generator, so a run that was limited by the
  mock or the generator can be seen to be.
- `allocations_per_request`: heap objects (`value`) and bytes
  (`bytes_per_request`) the whole gateway process allocated per request it
  answered successfully, warmup included, as its CPU time per request is. They
  come from the Go runtime's counters, which the release binary serves on its
  private `/metrics` listener (`go_memstats_mallocs_total` and
  `go_memstats_alloc_bytes_total`), read at the start and end of the run, less
  the rate it allocates at idle (`idle_objects_per_second`), read around the
  pause between the two runs. The runtime counts an allocation when its span
  leaves the processor's cache, so each reading trails by the objects of the
  cached spans: negligible over a full-rate run, and the reason a smoke run's
  figure is indicative. The figure is `null` beside a reason when a counter
  could not be read, which is the case for a binary built before the gateway
  served them. The allocations per operation of the hot paths are reported and
  gated by the `testing.B` benchmarks that sit beside the code (`make
  bench-gate`; which paths they cover is in the
  [performance guide](../../docs/performance.md#regression-gate)). Goroutines are
  not observable from outside: the operating-system thread count is the nearest
  figure.
- `error_rate` and the HTTP statuses and error kinds behind it. After a run with
  failures one request of the largest prompt is sent and the gateway's answer is
  recorded as `error_sample`, since the generator keeps only statuses.
- `request_metadata_completeness`: of the requests the gateway admitted (those
  the generator sent, warmup included, less any its admission pool shed), how
  many have a request row in PostgreSQL once the pipeline has drained, which
  is how many *events delivered*. The usage report's own count, the gateway's
  epoch counters and the count after shutdown are beside it as cross-checks.
  The wait for the events lasts as long as they keep arriving, and `drain`
  records how long it took, how many events were still to arrive when the load
  ended and the pace they arrived at. One pipeline consumer persists an event at a
  time, so at the rates of S1 to S3 the events outlast the load by minutes: in the
  [smoke run](../../docs/performance.md#smoke-run), S1 at 300 requests per second
  produced 21,000 events, 11,288 of them still to arrive when its load ended, and
  the wait for them lasted 68.7 seconds at 164 events a second. A full-rate run
  produces 70,000 to 210,000 events, which at that pace would take many minutes
  to arrive; none has been run. A run whose events arrive late has them all
  delivered and says how late; one whose events stop arriving is
  `settled: false`. Valkey's health and
  the gateway's own dropped and abandoned counters come from
  a metrics snapshot that the gateway refreshes every fifteen seconds, so they
  are read from one taken after the run ended (`metrics_stale` says when none
  appeared in time, which leaves the target unchecked), and the totals the
  gateway wrote for its epoch at shutdown, which are exact, count as well.
- `attempts`: attempts per request, by upstream model and by provider.
- `budget`: for S3, what the key's cost budget accrued over the run (daily and
  monthly cost, and the attempts no price covered), read from the key through the
  management API once the request metadata has arrived. It is the OLP side of
  what LiteLLM's runner checks of the key's spend: a key that accrued nothing
  means the run did none of the budget work S3 is for, so the run is invalid and
  the success target is `not_checked`.
- `failover`: for S4, the attempts on each target and how many requests failed
  over. A run in which the first target was never tried, the mock injected no
  error, or no request failed over measured a healthy route, and is invalid.
- `validity` and `reference_conditions`: a run is `valid` when the generator
  delivered its schedule in both runs, no more than 0.1% of the requests of
  either run failed (outside S6, whose streams end by being canceled), the
  added latency is not below zero by more than jitter, the gateway shed nothing,
  metadata finished arriving and the scenario's own premise held (S3's key was
  charged, S4's first target failed, S6's streams were held open). The latency
  of a gateway that failed requests is that of the ones that succeeded, so a
  gateway that fails fast would look faster, and an invalid run leaves the
  latency targets `not_checked`. It is a *reference* run only at full scale,
  with all three processes pinned to CPUs of their own (`pins_disjoint`: no two
  share one, which with `OLP_BENCH_ENFORCE=1` stops the run before it starts)
  and valid. Smoke numbers are not reference numbers.
- `targets`: the roadmap's targets, each judged against the run.

### Targets

Each target is `met`, `missed`, `needs_comparison` or `not_checked`, with the
measured figure and `meets_target` (null unless it could be judged).

| Target | Judged here |
| --- | --- |
| S1 on a 2-vCPU gateway: added latency at most 2 ms at p95 and 5 ms at p99 | Only for a gateway pinned to exactly two CPUs, with the mock upstream and the load generator pinned to CPUs of their own, at full scale, in valid runs |
| S3: 3,000 RPS at a 100% success rate | At full scale, in valid runs |
| S1 to S3 with healthy Valkey: zero lost request-metadata events | At any scale; unhealthy Valkey, or metrics that did not refresh after the run, make it `not_checked` |
| S1 to S5: added latency below LiteLLM's, more RPS per vCPU; S3 with fewer vCPUs than LiteLLM's profile | `needs_comparison`: it takes the same scenario run against LiteLLM, which `scripts/bench-compare.sh` does |

A smoke run reports them and fails nothing. With `OLP_BENCH_ENFORCE=1`, which
requires full scale, a target that is missed or `not_checked` fails the test, and
so does a run that is not a reference run (unpinned processes, an invalid
generator run, shed requests, metadata still arriving): otherwise a
misconfigured reference run would pass without having measured anything.

### Comparison with LiteLLM

```sh
make bench-compare                             # S1 to S5, at the roadmap's full rates
BENCH_SCENARIOS=S1 OLP_BENCH_SCALE=0.02 OLP_BENCH_DURATION=8s OLP_BENCH_WARMUP=3s \
  OLP_BENCH_GATEWAY_CPUS=2-3 OLP_BENCH_MOCK_CPUS=6 OLP_BENCH_LOADGEN_CPUS=7 scripts/bench-compare.sh
```

For each scenario the script runs OLP through `scripts/bench.sh`, then runs the
load that OLP's result records `workload` against the LiteLLM release pinned in
`deploy/compose.bench.yaml` (`scripts/bench-litellm.mjs`), with this directory's
`cmd/loadgen` and a direct-to-mock baseline of its own, on the same CPUs and the
same mock upstream binary, and then compares the two
(`scripts/bench-compare.mjs`), which settles the targets that
`needs_comparison` above leaves open. The results are
`.local/bench/litellm/<scenario>.json` in OLP's shape, and `compare.json` and
`compare.md` beside OLP's. S6 is not compared. The method, LiteLLM's settings,
how to read the table and the record to fill in for a published run are in
[docs/performance.md](../../docs/performance.md).

### What to know when reading a run

- **S4 measures the circuit.** A provider's circuit opens after five counted
  failures within thirty seconds and stays open for thirty seconds, then lets one
  probe through ([gateway](../../docs/gateway.md#request-path)). With
  the first target always failing, nearly every request is served by the second
  without trying the first: `attempts.per_request` is about 1.0, not 2.0, and
  `failover.requests_failed_over` is the five that opened the circuit plus a
  probe every thirty seconds. So the added latency is the cost of skipping a
  target, and the failed attempt itself is paid only by those requests. The two
  targets are separate providers because the circuit is per provider: on one
  provider both targets would be skipped.
- **The gateway's limits are raised for the load.** Its defaults (1,024
  connections and 256 requests in flight) protect a small deployment and would
  shed S2's 1,300 streams. A provider's pool is a limit too, and it has two
  defaults. A provider with no `options.network` and no profile shares the
  gateway's transport, which caps the connections to a host at none and keeps 16
  idle; any `options.network` field, even a timeout alone, or a profile gives it
  a pool of its own for each credential slot, capped at 64 connections to the
  host, 16 of them idle, and a request past the cap waits in the pool without an
  error. The benchmark always sets `options.network`, so it measures that pool,
  sized to each scenario's expected concurrency and recorded in the result
  (`gateway.derived_limits`), and never the shared transport. A provider takes at
  most 4,096 connections to a host, so S6 spreads its streams over as many
  providers as that needs, as an operator would.
- **S3's key has a cost budget, so it is not servable for up to a minute.** The
  gateway refuses a budget key whose spend Valkey does not know yet, and the
  worker plane installs it once a minute. The scenario waits for that pass.
- **S6 holds streams; it does not finish them.** The completions are longer than
  the sockets' buffers and the readers slower than the run is long, so the
  generator cancels the streams at the end, which is not an error. The result
  reports how many streams the gateway held at once (`slow_readers`), which
  must be nearly all of them or the scenario flags itself invalid, and the
  gateway's memory growth per stream. On loopback each connection's send buffer
  is about 2.6 MB, because the loopback MTU is 64 KiB, so a stream must be
  millions of bytes before the gateway's writes meet the reader; and the
  gateway spends CPU filling those buffers before it can hold the streams.
  Across a real network the buffers are far smaller. The kernel's own limit
  on TCP memory (`net.ipv4.tcp_mem`) bounds the run too: a held stream keeps
  about 7 MB in socket buffers on loopback, and the result carries a warning
  when the limit is below what the streams need. Pace the readers below about
  6 bytes a second (`OLP_BENCH_S6_READ_BPS=1`) to stall them instead, which
  tests the gateway's 30-second write deadline: it ends each stream thirty
  seconds after the stream's writes block (the gateway's log calls it
  `client_cancelled`), so the streams must all open within thirty seconds, or
  the first will be gone before the last is open and the run is flagged
  invalid. That holds at the default pace too, since the readers' 8 KiB a
  second does not drain the send buffers within thirty seconds: in smoke runs
  at 150 streams on a 2-vCPU gateway, the default warmup and period, which
  opened them over 70 seconds, left the gateway holding 66 at once, twice, and
  in the gateway's log of the second run every one of the 150 was ended at 30.2
  to 30.5 seconds; a 3-second warmup and a 15-second period held all 150 and the
  run was valid ([smoke run](../../docs/performance.md#smoke-run)). S6 therefore
  opens its streams over at most those 18 seconds, whatever `OLP_BENCH_WARMUP`
  and `OLP_BENCH_DURATION` say, and keeps a shorter schedule. Filling a
  stream's buffers also costs the gateway CPU: 273 CPU milliseconds for each
  stream held, net of idle, in that valid run (`gateway_cpu_ms_per_request`),
  for the default 40,000 tokens.
- **The gateway logs a line for every request** at its default level, so a
  full S3 run writes a few hundred megabytes to its process log, which is in
  the test's temporary directory. A failing test prints only the log's last
  megabyte.
- **PostgreSQL and Valkey run with the development compose file's settings**
  (Valkey with `appendonly yes`), as under `make integration`, which no tuned
  deployment does. A reference run points `BENCH_SERVICES=external` at services
  set up as the deployment would have them. Valkey is on the path of every
  request that has a budget or a limit, and a slow one shows as
  `distributed_limits_unavailable` (503) from the gateway, which fails closed.
- **The mock listens on loopback.** A full-rate run puts the generator, the
  gateway and the mock on one host, which the CPU pins keep apart; the roadmap
  also allows a separate host for the mock.
- **A 100K-token prompt is about 520 KB**, which the four-characters rule
  estimates at about 130K tokens. S3's upstream model is named as an OpenAI model
  (`gpt-4o-bench`), so the gateway counts the first 32 KiB of a prompt's text with
  its exact `o200k_base` encoder and charges the rest at the tokens per byte it
  measured there, which is the work of admission that S3 exists to include. The
  other scenarios' models, which the gateway cannot name a family for, are charged
  by the rule, and their prompts are short.
