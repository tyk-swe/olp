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

- [x] Split the `coverage` recipe in `Makefile` into lockstep-friendly targets:
  - [x] `coverage-unit` — `SQLX_OFFLINE=true NEXTEST_PROFILE=ci cargo llvm-cov nextest --no-report --locked --workspace --all-features`
  - [x] `coverage-db` — the same `--no-report` invocation routed through `scripts/run-postgres-tests.sh`; add an `OLP_DB_TEST_RUNNER` variable (default `cargo nextest run`) so the run-token sweep, `--profile db` parallelism bounds, and the Valkey-less skip list are reused rather than copied
  - [x] `coverage-report` — `cargo llvm-cov report --summary-only` first, then `cargo llvm-cov report --lcov --output-path lcov.info --ignore-filename-regex 'src/test_support\.rs' --fail-under-lines 80`, so a floor failure prints the percentage instead of a bare `make: *** Error 1`
  - [x] `coverage` stays as the umbrella target: `cargo llvm-cov clean --workspace` → unit → db → report
- [x] Give the `Rust / coverage` job services: copy the pinned `postgres:18@sha256:…` and `valkey/valkey:9.1@sha256:…` `services:` block and the `OLP_TEST_DATABASE_ADMIN_URL` / `OLP_TEST_DATABASE_URL_PREFIX` / `OLP_VALKEY_URL` env from `postgres-integration`; add the `scripts/ci/install-postgres-client.sh 18` step
- [x] Floor: 80. Put the measured per-crate numbers (`olp-db` 88.7, `apps/olp` 82.6, `olp-engine` 87.6) in the commit message so the next raise is deliberate
- [x] `make ci-lockstep`, `make script-selftest`, `make shellcheck` pass; `scripts/test-postgres-test-databases.sh` gains a case for the runner override
- [x] `CONTRIBUTING.md` "Validation" and the Makefile header comment say coverage now includes the DB suites and needs the `make db-test` environment
- [x] On [PR #117](https://github.com/tyk-swe/olp/pull/117): the [coverage job](https://github.com/tyk-swe/olp/actions/runs/33252607630/job/99100780659) is green, artifact `rust-coverage-33252607630-1` is present, and the summary reports 86.12% line coverage

## CI-02 — Yanked `chacha20` (S)

- [x] `cargo update -p chacha20` (0.10.1 → 0.10.2; reached via `rand 0.10.2` from `google-cloud-gax` and `sqlx-postgres`)
- [x] Delete `ignore = ["RUSTSEC-2026-0235"]` and its comment from `deny.toml`; cargo-deny reports the advisory matches nothing
- [x] `cargo deny check advisories bans licenses sources` clean locally
- [x] Yank policy, written as a comment above `[advisories]`: `yanked = "deny"` stays; a yank is expected to turn the Monday scheduled run red and is fixed with `cargo update -p <crate>`; HYG-06 makes that weekly instead of incidental

## CI-03 — Firefox screenshot baseline (S)

- [x] `make playwright-install BROWSER=firefox`
- [x] Build the console and regenerate against the prebuilt bundle as CI does: `CI=true pnpm --dir console exec playwright test --project=firefox tests/e2e/operations.spec.ts --update-snapshots=all`
- [x] Run all four projects; confirm only `console/tests/e2e/operations.spec.ts-snapshots/request-explorer-firefox-linux.png` changed
- [x] Keep screenshots and `AxeBuilder` on all four projects; use `maxDiffPixelRatio: 0.01` with the anti-aliasing rationale recorded in `console/playwright.config.ts`
- [x] `.github/PULL_REQUEST_TEMPLATE.md` screenshot line mentions all four Playwright baselines, not only `docs/assets/screenshots/`

## CI-04 — Deterministic shared-Valkey isolation proof (S)

- [x] In `tests/e2e/tests/ha.rs` `prove_shared_valkey_isolation`, collect `(key, PTTL)` pairs and return only keys with `PTTL == -1` (the request-metadata stream and consumer-group state); expiring limiter keys are asserted *before* teardown, where `limiter_b.reserve` already proves they exist and are isolated
- [x] `assert_valkey_keys_exist` message names the durable keys it checked
- [x] Prove it: run `make worker-ha` three times, each started at `:57` seconds (`sleep $((57 - $(date +%S)))` when needed) so the run straddles a minute boundary
- [x] `tests/README.md` "End-to-end and HA": one sentence on which Valkey keys are durable and which expire by design

## CI-05 — Dependabot cargo group (S)

- [x] Read [Dependabot update 1541862126](https://github.com/tyk-swe/olp/network/updates/1541862126) (maintainer access required; `futures: unknown_error`)
- [x] Exclude `futures` from `cargo-minor-patch`; the standalone lockstep updater defect is tracked in [dependabot-core#16092](https://github.com/dependabot/dependabot-core/issues/16092)
- [x] Land the pending cargo bumps in [PR #117](https://github.com/tyk-swe/olp/pull/117) and advance the lockstep `futures` family in [PR #122](https://github.com/tyk-swe/olp/pull/122)
- [x] Merge [Dependabot #115](https://github.com/tyk-swe/olp/pull/115) after CI-01 turns `Required` green

## GOV-01 — Protect `main` (S)

- [x] [Repository ruleset for `main`](https://github.com/tyk-swe/olp/rules/21799519): require the `Required` status check, require linear history, block force pushes, block deletion; maintainer on the pull-request-only bypass list for emergencies
- [-] Enable the merge queue and require it for `main` — dropped: GitHub offers merge queues only for organization-owned repositories, while this public repository is owned by a personal account
- [x] Verify: a direct push with a failing `Required` check is rejected

## GOV-02 — Prune branches (S)

- [x] Delete the 25 branches already merged (`git branch -r --merged origin/main`)
- [x] For each remaining `agent/*`, `codex/*`, `jules-*`, `epic1/*`, `fix/*`, `test/*`, `perf/*` branch: `git log --oneline origin/main..<branch>`; keep nothing worth keeping unwritten — open a PR or a backlog item, then delete
- [x] Turn on "Automatically delete head branches"

Closed PRs #3 and #80 were deliberately not retained: their provider
compatibility and distributed-circuit proposals add unapproved product
semantics and critical-path complexity without recorded demand. PR #81 was a
broad, stale refactor; the other unmerged branches were superseded by `main`.

## GOV-03 — Backlog hygiene (S)

- [x] Review tracking: replace "Nothing open" with the open review-derived items in `docs/roadmap/backlog.md`
- [x] [Pinned roadmap tracking issue #116](https://github.com/tyk-swe/olp/issues/116) so releases can link it
- [x] `CHANGELOG.md` `[Unreleased]` → "Repository": coverage now includes the DB suites; floor 80; Firefox snapshot policy; isolation-proof determinism

## GOV-04 — Small cleanups (S)

- [x] Remove `stable` from `on.push.branches` in `ci.yml` (no such branch) or create it and document its meaning in `CONTRIBUTING.md`
- [x] Fix `user.name` / `user.email` on the dev box (two commits are authored as `Ubuntu <ubuntu@dev-2.1.1.1.1>`)

## Exit criteria

- [x] The post-implementation push to `main` shows `Required` and `Full` both green ([run 33256090035](https://github.com/tyk-swe/olp/actions/runs/33256090035))
- [x] Ruleset active and verified against a failing check
- [x] `cargo deny check` is clean; the [fresh Dependabot cargo run](https://github.com/tyk-swe/olp/actions/runs/33256092124) triggered by [PR #123](https://github.com/tyk-swe/olp/pull/123) succeeds
- [x] Remote branch count ≤ 10
- [x] `backlog.md` and `CHANGELOG.md` reflect the week

## Carry-over

_None. GOV-01's merge-queue subtask was dropped because GitHub does not offer
merge queues to public repositories owned by personal accounts._
