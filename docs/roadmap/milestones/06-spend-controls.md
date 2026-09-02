# Milestone 6 — Spend controls

| | |
|---|---|
| Dates | Mon 2026-10-05 → Sun 2026-10-11 |
| Goal | An API key can carry a hard cost budget per UTC day and per UTC month; exhaustion is rejected fail-closed exactly like RPM/TPM, using the pricing the gateway already attributes to every attempt |
| Backlog items | SPEND-01, SPEND-02, SPEND-03, SPEND-04 (stretch), SPEND-05 |
| Prerequisites | Semantics (SPEND-01) agreed before storage changes; local PostgreSQL and Valkey for the suites |

## SPEND-01 — Semantics, decided before code (S)

- [x] Budgets live on the API key beside `requests_per_minute` and `tokens_per_minute` in `crates/olp-engine/src/domain/auth.rs`: `daily_cost_limit` and `monthly_cost_limit`, `rust_decimal` in the pricing currency, both optional
- [x] Accrual is the sum of priced usage facts for the key in the fixed UTC window. Unpriced attempts (no observed usage or no pricing revision) accrue **0** and are counted separately as `unpriced_attempts` — cost is never invented, consistent with the data-safety invariants
- [x] Enforcement is pre-admission against a Valkey counter (`…:limits:{key}:cost:<window>`), advanced by cumulative snapshots from the terminal accounting path and reconciled from PostgreSQL usage facts by the maintenance worker, so a lost Valkey cannot lose spend
- [x] Fail-closed: a key with any budget and an unavailable limiter is rejected even when the installation opts rate/concurrency-only keys into `fail_open`
- [x] Rejection: HTTP 429, `error.code = "budget_exhausted"`, `Retry-After` = seconds to the window end; mapped onto the Anthropic and Gemini error shapes the same way RPM rejections are

## SPEND-02 — Storage (M)

- [x] Migration `0049_api_key_cost_budgets.sql` (forward-only, sequential): two nullable numeric columns with `CHECK (… > 0)`; add an index for "spend by key by window" only if `EXPLAIN` on the reconciliation query shows the usage-fact indexes do not cover it
- [x] `make sqlx-prepare` → commit `.sqlx/`; `make sqlx-check` green
- [x] A separate `crates/olp-db/scripts/reserve_cost.lua` invoked in the same admission step, so `reserve_limits.lua` keeps its argument contract and unit tests untouched
- [x] `crates/olp-db/src/limits.rs`: daily/monthly cost dimensions in `DistributedLimiter::reserve`; `LimitError::Exceeded { dimension: DailyCost | MonthlyCost, .. }`
- [x] Reconciliation task in the maintenance worker with a PostgreSQL checkpoint like the other tasks; `olp_worker_task_healthy{task="cost_reconciliation"}`

## SPEND-03 — Engine and delivery (M)

- [x] `crates/olp-engine/src/inference/limits.rs`: include the cost dimensions in the reservation; the accounting path advances cumulative accrued cost when the terminal envelope can be priced
- [x] Management API: create, update, and rotate accept the two fields; the key read returns `budget: { daily: { limit, accrued, window_ends_at }, monthly: { … }, unpriced_attempts }`; audit events for budget changes; `make openapi` regenerated; the pagination and declared-400 contract tests still pass
- [x] Metrics: `olp_key_budget_rejections_total{window}` only — a per-key spend gauge is too high-cardinality for Prometheus; top-N spend is exposed through the usage endpoint instead

## SPEND-04 — Console (M, stretch)

- [x] API-key create/edit form: two optional currency inputs with the server's validation mirrored; list column "Budget" showing accrued / limit; key detail shows both windows and `unpriced_attempts`
- [x] Usage page: filtering by key shows the budget line
- [x] Vitest for the form state factory; `api-keys.spec.ts` extended; `make screenshots` and all four Playwright baselines if the visible layout changed

## SPEND-05 — Proof and docs (M)

- [x] `crates/olp-db/tests/integration`: cost reservation; window rollover at the UTC boundary done deterministically by injecting the window id, never by sleeping; reconciliation after a simulated Valkey loss; engine and HA proof that budgeted keys remain fail-closed with the limiter down
- [x] `tests/e2e`: a key with a $0.01 daily budget on a priced route — second request is 429 `budget_exhausted` with `Retry-After`; an unpriced route never trips the budget but increments `unpriced_attempts`
- [x] README feature bullet; `docs/concepts.md` limits section; `docs/operations.md` (what to do when reconciliation lags); CHANGELOG `[Unreleased]` with the exact semantics — the "unpriced attempts accrue 0" sentence is the one people will search for

## Exit criteria

- [ ] Budget exhaustion demonstrable in the compose stack from the console (or via the API if SPEND-04 slips)
- [x] Migration 0049 passes the N-1 upgrade rehearsal (`make upgrade-rehearsal`)
- [x] The coverage floor holds with the new DB suites included (`make coverage`: 85.61% lines)

## Carry-over

- Compose-stack UI/API smoke remains environment-only exit evidence because this environment cannot access the Docker API. The real `olp` binary E2E contract passed, including priced exhaustion and unpriced accrual semantics.
- The Toxiproxy HA budget-outage scenario compiles and passes Clippy, but its live run also requires Docker API access. Engine fail-closed tests, real Valkey recovery tests, and the three-worker HA suite passed.
