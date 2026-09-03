# Baseline — measured on 2026-08-29

Reference point for the whole plan, taken at commit `c6f9c95` on `main`.
Nothing in this file is edited later; progress is measured against it.

## Repository at a glance

| Measure | Value |
|---|---|
| Age, authorship | First commit 2026-07-19; 318 commits, 306 by one maintainer, 10 by Dependabot; 115 pull requests |
| Rust | 142 k lines in 465 files across `apps/olp`, `crates/olp-engine`, `crates/olp-db`, `tests/conformance`, `tests/e2e`, `fuzz` |
| Console | 26.9 k lines in 202 files (SvelteKit, `adapter-static`) |
| Persistence | 47 forward-only migrations (`0001`–`0047`), 346 checked SQLx statements in `.sqlx/` |
| Dependencies | 434 crates in `Cargo.lock` (25 `aws-*`), Rust 1.97.1, Node 24, pnpm 11, fuzz nightly `2026-05-15` |
| Tests | 1 001 Rust test functions (122 `#[ignore]`d, DB/Valkey-backed), 37 vitest files / 331 tests, 28 Playwright specs, 29 e2e contracts, 28 conformance cases, 4 fuzz targets |
| Production code quality | Zero `unwrap()` outside tests, one `#[allow]`, zero clippy warnings, `unsafe_code = "forbid"`, distroless non-root image |
| Releases | None. No tags, no GitHub Releases, no `ghcr.io/tyk-swe/olp` image, no `oci://ghcr.io/tyk-swe/charts/openllmproxy` chart — all referenced by README, CHANGELOG, `deploy/helm/values.yaml`, and `docs/deployment.md` |
| Governance | `main` unprotected, no rulesets; 62 remote branches (25 already merged); GitHub description contradicts the README |

## Verified locally the same day

| Gate | Result |
|---|---|
| `make check-static` | pass, 9.5 s |
| `make check-heavy` (cold) | pass, 11 min: clippy `-D warnings` clean, 885 Rust tests passed / 118 skipped, 331 vitest tests, svelte-check, eslint, build |
| DB suites (`--profile db --run-ignored ignored-only`, `olp-db` + `olp`) | 83 passed |
| Coverage, unit suite only (what `make coverage` measures) | **61.78 %** |
| Coverage, unit suite plus DB suites | **86.00 %** — `olp-db` 88.7 %, `apps/olp` 82.6 %, `olp-engine` 87.6 % |

Per-crate, unit suite only: `olp-db` 25.2 %, `apps/olp` 59.9 %, `olp-engine`
86.4 %. The DB pass alone moves `olp-db` from 25.2 % to 88.7 %.

## CI forensics

Every push to `main` from 2026-08-26 onward failed the `Required` job, which
also blocked pull requests (Dependabot #115). Earlier in August the streak was
similar: only four of the last twenty push runs were green.

### `Rust / coverage` — floor failure caused by measurement, not by tests

- CI artifact `rust-coverage-33230867088-1`: 35 809 / 57 972 lines = **61.77 %** against `--fail-under-lines 62`.
- The 83 `#[ignore]`d PostgreSQL/Valkey tests run only under `make db-test`,
  which `make coverage` never invokes; the Makefile comment even says so.
- `cargo llvm-cov` prints nothing on a floor failure; the log ends with
  `make: *** [Makefile:90: coverage] Error 1`, which is why the cause was not
  obvious for days.
- Fix: milestone 1, `CI-01`.

### `Quality` — `cargo deny` yanked-crate error

- `error[yanked]: chacha20 0.10.1`, reached through `rand 0.10.2` from both
  `google-cloud-gax` and `sqlx-postgres`. `cargo update -p chacha20` resolves
  to 0.10.2 (dry run confirmed).
- `deny.toml` also carries `ignore = ["RUSTSEC-2026-0235"]`, which cargo-deny
  reports as matching nothing.
- Fix: milestone 1, `CI-02`.

### `Console / Firefox` — screenshot drift (full tier)

- `console/tests/e2e/operations.spec.ts:163`: `request-explorer.png` differs by
  83 pixels; tolerance is `maxDiffPixels: 50` (`console/playwright.config.ts`).
- The Firefox baseline was refreshed on 2026-08-25 (`fc902fa`); the UI changed
  on 2026-08-26 (`38776a3`) and only some baselines followed.
- Fix: milestone 1, `CI-03`.

### `Replicated worker qualification` — minute-boundary race (full tier)

- `worker_ha_shared_valkey_installations_are_isolated` in
  `tests/e2e/tests/ha.rs` lists every `prefix_b:*` key, shuts installation A
  down, then asserts each key still exists.
- `crates/olp-db/scripts/reserve_limits.lua` sets `PEXPIRE` on the rate hash to
  the end of the current UTC minute and on the concurrency zset to the lease
  expiry. The 2026-08-29 failure timestamp is 03:20:00.39Z.
- This is a test race; installation isolation is intact. Fix: milestone 1,
  `CI-04`.

### Dependabot — cargo group never opens its pull request

- Update run 1541862126 created the PR list, then failed on `futures` with
  `unknown_error`; the grouped cargo PR does not exist.
- Fix: milestone 1, `CI-05`.

## Gaps that motivate weeks 3–7

| Gap | Evidence |
|---|---|
| Live-provider tests never run | `OLP_LIVE_OPENAI_API_KEY`, `OLP_LIVE_ANTHROPIC_API_KEY`, `OLP_LIVE_GEMINI_API_KEY`, `OLP_AZURE_OPENAI_LIVE_*`, `OLP_VERTEX_LIVE_*`, `OLP_BEDROCK_LIVE_*` gate tests; no workflow sets them |
| SDK smoke is JavaScript only | `tests/sdk-smoke/package.json` pins `openai`, `@anthropic-ai/sdk`, `@google/genai`; no Python equivalent |
| Slow pull-request tier | ~16 min per PR; `Rust / fuzz replay` alone is 15 min because the composite action sets `cache-targets: false` |
| No first-request documentation | No `curl`, SDK snippet, or statement that `model` is the route slug anywhere in README or `docs/` |
| No distributed tracing | No OpenTelemetry crates in `Cargo.lock`; only Prometheus metrics and `x-request-id` |
| No spend controls | Keys carry `requests_per_minute`, `tokens_per_minute`, concurrency, and expiry (`crates/olp-engine/src/domain/auth.rs`) but no cost limit, although every attempt is priced |
| No performance evidence | `docs/operations.md` promises 99.9 % availability and ≤ 15 ms p95 / ≤ 30 ms p99 added latency; nothing measures it, no benches, no load tool |
| Chart gap | `docs/deployment.md` tells operators to add a NetworkPolicy; the chart has no template for one |
| Grandfathered size violations | `scripts/source-size-baseline.txt`: 9 files over 30 KB (all tests) and 65 functions over 100 lines |
