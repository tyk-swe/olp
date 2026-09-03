# Performance benchmarks

`make bench` builds `olp` in release mode with `test-util`, starts an isolated
gateway and loopback OpenAI-compatible provider, configures the provider, two
routes, and an API key through the management API, then runs the pinned load
scenarios. PostgreSQL and Valkey must already be running.

Install `oha` 1.12.0 and run:

```console
cargo binstall oha@1.12.0
make bench
```

The default service URLs are
`postgres://olp_test:olp_test@localhost:5433/postgres` and
`redis://localhost:6379/15`. Override them when necessary:

```console
OLP_BENCH_DATABASE_ADMIN_URL=postgres://olp:olp@localhost:5432/postgres \
OLP_BENCH_VALKEY_URL=redis://localhost:6379/15 \
make bench
```

The harness creates and drops a uniquely named PostgreSQL database. It flushes
the selected Valkey logical database before and after the run, so use a
dedicated database. Ambient `OLP_*` gateway settings are cleared before the
server starts so local configuration cannot alter the benchmark. `BENCH_DURATION`
changes the duration of each direct-mock and gateway half, for example
`BENCH_DURATION=20s make bench`. The default is 60 seconds. Clean-tree results
are written to `bench/results/<git-sha>.json`.
Dirty-tree results use `<git-sha>-dirty-<source-fingerprint>.json`, and every
new result records both the commit and source-tree fingerprint. `make
bench-report` resolves the exact filename for the current source tree; set
`BENCH_RESULT` explicitly to report an older capture.

Run the protocol micro-benchmarks and Criterion license check with
`make bench-micro`.

## Reading added latency

Each upstream-backed scenario runs against the mock directly and then through
the gateway. Added latency is the gateway percentile minus the same direct-mock
percentile; it is not a per-request paired measurement. Compare the JSON's
gateway and mock percentiles when investigating a change, and use the gateway
throughput range for the ±10% same-machine reproducibility gate. The local
`/v1/models` route has no upstream request, so its result contains only gateway
latency and throughput.

The mock returns unary chat and embedding responses after 200 ms and emits a
fixed 50-token chat stream. A valid run requires zero admission rejections and
zero scenario errors. The JSON retains error rates, status distributions, raw
`oha` output, and the complete `/metrics` snapshots from before and after the
scenarios.

## Initial baseline

These are the first three post-optimization release runs on an 8-vCPU KVM
guest with an Intel Core Processor (Haswell, no TSX), Linux 7.0.0-30 x86_64,
rustc 1.97.1, and `oha` 1.12.0. Every gateway scenario had zero errors and all
three runs had zero admission rejections. Run 3's direct mock recorded one
connection error in 36,437 requests for `chat_unary_c256` (0.002744%); its
gateway half had no error.

| Run | Scenario | Added p95 | Added p99 | Gateway throughput |
|---:|---|---:|---:|---:|
| 1 | `chat_unary_c16` | 1.530 ms | 2.222 ms | 66.064 req/s |
| 1 | `chat_unary_c64` | 1.619 ms | 2.854 ms | 262.917 req/s |
| 1 | `chat_unary_c256` | 3.098 ms | 7.122 ms | 1,046.987 req/s |
| 1 | `chat_stream_c64` | 32.770 ms | 49.846 ms | 1,051.212 req/s |
| 1 | `models_c256` | — | — | 20,886.165 req/s |
| 1 | `embeddings_c64` | 2.315 ms | 3.244 ms | 262.717 req/s |
| 2 | `chat_unary_c16` | 1.504 ms | 2.021 ms | 66.063 req/s |
| 2 | `chat_unary_c64` | 1.410 ms | 2.280 ms | 263.196 req/s |
| 2 | `chat_unary_c256` | 2.236 ms | 3.667 ms | 1,044.408 req/s |
| 2 | `chat_stream_c64` | 14.469 ms | 18.857 ms | 1,088.652 req/s |
| 2 | `models_c256` | — | — | 20,489.721 req/s |
| 2 | `embeddings_c64` | 2.081 ms | 2.824 ms | 263.101 req/s |
| 3 | `chat_unary_c16` | 2.171 ms | 4.522 ms | 65.864 req/s |
| 3 | `chat_unary_c64` | 1.790 ms | 0.858 ms | 263.237 req/s |
| 3 | `chat_unary_c256` | 3.016 ms | 5.332 ms | 1,046.885 req/s |
| 3 | `chat_stream_c64` | 18.737 ms | 25.638 ms | 1,071.309 req/s |
| 3 | `models_c256` | — | — | 21,149.505 req/s |
| 3 | `embeddings_c64` | 2.226 ms | 3.296 ms | 262.321 req/s |

Gateway throughput's largest max-to-min spread was 3.6%, satisfying the ±10%
same-machine reproducibility criterion. The checked-in raw result is
[`results/aa50f62720231408fa4ba5c5bac6411d83caff2e.json`](results/aa50f62720231408fa4ba5c5bac6411d83caff2e.json).
It predates source fingerprinting and is marked as a dirty-tree capture whose
exact content identity is unavailable.

The checked-in run's final metrics also show 1,221,575 dropped request-metadata
events and 113,527 events of consumer lag. The `/v1/models` result is therefore
an HTTP-path stress signal, not a metadata-complete operating envelope or a
safe production request-rate target.

The measured release objectives are 15 ms p95 / 30 ms p99 added latency for
unary chat and embeddings, and 35 ms p95 / 55 ms p99 for the fixed 50-token
burst stream. The wider stream objective covers the observed tail range rather
than hiding it. The local `/v1/models` route uses its 20,490–21,150 req/s gateway
throughput and gateway latency directly as its baseline.

## Streaming profile

A root-backed `perf` attach during the gateway half of `chat_stream_c64`,
rendered with `cargo-flamegraph`, captured 659 samples. The profile was
dominated by Tokio network and TCP send/syscall paths; it did not show repeated
JSON serialization or circuit/limit lock contention as a hotspot. The
resulting changes preallocate the SSE frame buffer and coalesce only
already-ready response frames after one cooperative scheduler yield. No timer
waits for a future provider token. In the 60-second runs, streaming p95 added
latency improved from 97.418 ms before the change to 14.469–32.770 ms
afterward.
