# Milestone 7 — Performance evidence

| | |
|---|---|
| Dates | Mon 2026-10-12 → Sun 2026-10-18 |
| Goal | The availability and scenario-specific added-latency objectives in `docs/operations.md` are measured, published per release, and regressions are visible |
| Backlog items | PERF-01, PERF-02, PERF-03, PERF-04 |
| Prerequisites | Local PostgreSQL and Valkey; `oha` installed (`cargo binstall oha` locally, `taiki-e/install-action` in CI) |

## PERF-01 — Load harness (M)

- [x] `scripts/bench.sh` and `make bench`: start `olp all` (test-util build) against local PostgreSQL/Valkey with the e2e loopback mock provider configured for a fixed 200 ms unary response and a fixed 50-token stream; create a key and two routes through the management API
- [x] Scenarios with `oha` (pinned): non-streaming chat at 16 / 64 / 256 concurrency for 60 s; streaming chat at 64; `/v1/models` at 256; embeddings at 64
- [x] Output `bench/results/<git-sha>.json`: p50 / p95 / p99 gateway latency, throughput, error rate, and a `/metrics` snapshot before and after; upstream-backed scenarios also record mock and **added latency** (the difference) — admission rejections and scenario errors must be 0 for a valid run
- [x] `bench/README.md`: how to run, machine notes, how to read added latency, and a results table for the first three runs

## PERF-02 — Micro-benchmarks (S)

- [x] `crates/olp-engine/benches/` with Criterion (dev-dependency; confirm `deny` licenses): SSE decoder over the fuzz corpus, OpenAI → canonical → Anthropic and → Gemini request translation, chat and Responses stream encoders, JSON codecs over the largest conformance fixtures
- [x] `make bench-micro`; not in `Required`

## PERF-03 — Non-blocking CI signal (S)

- [x] Full-tier `perf` job running `make bench` with 20 s scenarios, uploading the JSON artifact and posting the latency table as a commit comment (SHA-pinned action)
- [x] Regression signal: compare against the last `main` artifact; warn — never fail — on a p95 regression above 25 %

## PERF-04 — Act on the numbers (M)

- [x] One `cargo flamegraph` session under the streaming scenario; fix what is obvious (allocations in the SSE pump, repeated JSON serialisation, lock contention in circuit or limit state)
- [x] Re-validate the `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` and `OLP_HTTP_MAX_CONNECTIONS` defaults against measured saturation (the 503 + `Retry-After: 1` semantics stay); update the capacity paragraph in `docs/deployment.md` with measured numbers
- [x] Replace the provisional latency objectives in `docs/operations.md` with the measured baseline and the machine it was measured on

## Exit criteria

- [x] `make bench` is reproducible within ±10 % on the same machine
- [x] Added-latency p95 / p99 published in `bench/README.md`; the SLO paragraph cites it
- [ ] `perf` job green on `main` with an artifact attached

## Carry-over

- The `perf` job and artifact require the milestone commit to run on `main`; the
  full-tier job is implemented and remains non-blocking until that external gate runs.
