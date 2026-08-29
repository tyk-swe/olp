# Milestone 1 — Green and protected `main`

| | |
|---|---|
| Dates | Mon 2026-08-31 → Sun 2026-09-06 |
| Goal | Every push to `main` is green in both CI tiers, and `main` can no longer go red silently |
| Backlog items | CI-01, CI-02, CI-03, CI-04, CI-05, GOV-01, GOV-02, GOV-03, GOV-04 |
| Prerequisites | None. Local PostgreSQL 18 and Valkey for CI-01 and CI-04 (`CONTRIBUTING.md` → Database-backed tests) |
| Evidence | [`../baseline.md`](../baseline.md#ci-forensics) |

Order matters: CI-01 first, because it is what turns `Required` green and lets
every other pull request merge. CI-02 is a one-line change and can ride in the
same PR. Everything else is independent.

## CI-01 — Coverage measures the DB suites (M)

The floor fails at 61.77 % because the 83 `#[ignore]`d PostgreSQL/Valkey tests
never run under `make coverage`. Measured with them: 86.0 %.

- [ ] Split the `coverage` recipe in `Makefile` into lockstep-friendly targets:
  - [ ] `coverage-unit` — `SQLX_OFFLINE=true NEXTEST_PROFILE=ci cargo llvm-cov nextest --no-report --locked --workspace --all-features`
  - [ ] `coverage-db` — the same `--no-report` invocation routed through `scripts/run-postgres-tests.sh`; add an `OLP_DB_TEST_RUNNER` variable (default `cargo nextest run`) so the run-token sweep, `--profile db` parallelism bounds, and the Valkey-less skip list are reused rather than copied
  - [ ] `coverage-report` — `cargo llvm-cov report --summary-only` first, then `cargo llvm-cov report --lcov --output-path lcov.info --ignore-filename-regex 'src/test_support\.rs' --fail-under-lines 80`, so a floor failure prints the percentage instead of a bare `make: *** Error 1`
  - [ ] `coverage` stays as the umbrella target: `cargo llvm-cov clean --workspace` → unit → db → report
- [ ] Give the `Rust / coverage` job services: copy the pinned `postgres:18@sha256:…` and `valkey/valkey:9.1@sha256:…` `services:` block and the `OLP_TEST_DATABASE_ADMIN_URL` / `OLP_TEST_DATABASE_URL_PREFIX` / `OLP_VALKEY_URL` env from `postgres-integration`; add the `scripts/ci/install-postgres-client.sh 18` step
- [ ] Floor: 80. Put the measured per-crate numbers (`olp-db` 88.7, `apps/olp` 82.6, `olp-engine` 87.6) in the commit message so the next raise is deliberate
- [ ] `make ci-lockstep`, `make script-selftest`, `make shellcheck` pass; `scripts/test-postgres-test-databases.sh` gains a case for the runner override
- [ ] `CONTRIBUTING.md` "Validation" and the Makefile header comment say coverage now includes the DB suites and needs the `make db-test` environment
- [ ] On the PR: coverage job green, `rust-coverage-*` artifact present, the summary line visible in the job log

## CI-02 — Yanked `chacha20` (S)

- [ ] `cargo update -p chacha20` (0.10.1 → 0.10.2; reached via `rand 0.10.2` from `google-cloud-gax` and `sqlx-postgres`)
- [ ] Delete `ignore = ["RUSTSEC-2026-0235"]` and its comment from `deny.toml`; cargo-deny reports the advisory matches nothing
- [ ] `cargo deny check advisories bans licenses sources` clean locally
- [ ] Yank policy, written as a comment above `[advisories]`: `yanked = "deny"` stays; a yank is expected to turn the Monday scheduled run red and is fixed with `cargo update -p <crate>`; HYG-06 makes that weekly instead of incidental

## CI-03 — Firefox screenshot baseline (S)

- [ ] `make playwright-install BROWSER=firefox`
- [ ] Build the console and regenerate against the prebuilt bundle as CI does: `CI=true pnpm --dir console exec playwright test --project=firefox --update-snapshots tests/e2e/operations.spec.ts`
- [ ] Run all four projects; confirm only `console/tests/e2e/operations.spec.ts-snapshots/request-explorer-firefox-linux.png` changed
- [ ] Decide the policy and record it in `console/playwright.config.ts` next to `maxDiffPixels`: recommended — `toHaveScreenshot` on Chromium only, `AxeBuilder` on all four projects. If cross-browser snapshots stay, switch to `maxDiffPixelRatio: 0.01` with the anti-aliasing rationale in the comment
- [ ] `.github/PULL_REQUEST_TEMPLATE.md` screenshot line mentions all four Playwright baselines, not only `docs/assets/screenshots/`

## CI-04 — Deterministic shared-Valkey isolation proof (S)

- [ ] In `tests/e2e/tests/ha.rs` `prove_shared_valkey_isolation`, collect `(key, PTTL)` pairs and return only keys with `PTTL == -1` (the request-metadata stream and consumer-group state); expiring limiter keys are asserted *before* teardown, where `limiter_b.reserve` already proves they exist and are isolated
- [ ] `assert_valkey_keys_exist` message names the durable keys it checked
- [ ] Prove it: run `make worker-ha` three times, each started at `:57` seconds (`sleep $((57 - $(date +%S)))` when needed) so the run straddles a minute boundary
- [ ] `tests/README.md` "End-to-end and HA": one sentence on which Valkey keys are durable and which expire by design

## CI-05 — Dependabot cargo group (S)

- [ ] Read the update log at `github.com/tyk-swe/olp/network/updates/1541862126` (`futures: unknown_error`)
- [ ] If the grouped update trips on the workspace-level `futures = "0.3"`, exclude `futures` from `cargo-minor-patch` in `.github/dependabot.yml`; otherwise file the Dependabot issue and link it here
- [ ] Land the pending bumps once by hand: `cargo update --workspace` in a PR titled `build(deps): cargo minor/patch catch-up 2026-09`, so `deny`, `machete`, and the full gate run against current versions
- [ ] Merge Dependabot #115 (console group) after CI-01 turns `Required` green

## GOV-01 — Protect `main` (S)

- [ ] Repository ruleset for `main`: require the `Required` status check, require linear history, block force pushes, block deletion; maintainer on the bypass list for emergencies only
- [ ] Enable the merge queue and require it for `main` (`ci.yml` already handles `merge_group`)
- [ ] Verify: a direct push with a failing `Required` check is rejected

## GOV-02 — Prune branches (S)

- [ ] Delete the 25 branches already merged (`git branch -r --merged origin/main`)
- [ ] For each remaining `agent/*`, `codex/*`, `jules-*`, `epic1/*`, `fix/*`, `test/*`, `perf/*` branch: `git log --oneline origin/main..<branch>`; keep nothing worth keeping unwritten — open a PR or a backlog item, then delete
- [ ] Turn on "Automatically delete head branches"

## GOV-03 — Backlog hygiene (S)

- [ ] `TODOS.md`: replace "Nothing open" with the open review-derived items and a pointer to `docs/roadmap/backlog.md` for planned work
- [ ] One tracking issue per milestone (or a single pinned issue) so releases can link them
- [ ] `CHANGELOG.md` `[Unreleased]` → "Repository": coverage now includes the DB suites; floor 80; Firefox snapshot policy; isolation-proof determinism

## GOV-04 — Small cleanups (S)

- [ ] Remove `stable` from `on.push.branches` in `ci.yml` (no such branch) or create it and document its meaning in `CONTRIBUTING.md`
- [ ] Fix `user.name` / `user.email` on the dev box (two commits are authored as `Ubuntu <ubuntu@dev-2.1.1.1.1>`)

## Exit criteria

- [ ] The latest push to `main` shows `Required` and `Full` both green
- [ ] Ruleset active and verified against a failing check
- [ ] `cargo deny check` clean; the next scheduled Dependabot cargo run succeeds
- [ ] Remote branch count ≤ 10
- [ ] `TODOS.md` and `CHANGELOG.md` reflect the week

## Carry-over

_None yet. Add unfinished tasks here on Friday and update their week in `backlog.md`._
