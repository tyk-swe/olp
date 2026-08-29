# Milestone 3 — Test the risks the gates do not cover

| | |
|---|---|
| Dates | Mon 2026-09-14 → Sun 2026-09-20 |
| Goal | Provider drift is detected weekly instead of by users; the dominant client ecosystem is smoke-tested; pull requests get their verdict in under 8 minutes |
| Backlog items | TEST-01, TEST-02, CI-06, CI-07 |
| Prerequisites | Provider accounts with API keys dedicated to CI; AWS role and GCP workload identity for OIDC federation |

## TEST-01 — Weekly live-provider job (M)

The tests exist behind `OLP_LIVE_OPENAI_API_KEY`, `OLP_LIVE_ANTHROPIC_API_KEY`,
`OLP_LIVE_GEMINI_API_KEY`, `OLP_AZURE_OPENAI_LIVE_{ENDPOINT,DEPLOYMENT,API_VERSION,API_KEY}`,
`OLP_VERTEX_LIVE_{PROJECT,LOCATION,MODEL,CREDENTIALS}`, and
`OLP_BEDROCK_LIVE_{REGION,MODEL}`, and have never run in CI.

- [ ] Inventory: `rg -o 'OLP_[A-Z_]*LIVE_[A-Z_]*' crates apps tests | sort -u`; for each variable list the gated test functions and what each call costs; pick the cheapest model per provider
- [ ] Provider keys dedicated to CI with hard monthly caps set in each provider console; stored in a GitHub environment `live-providers` with the maintainer as required reviewer
- [ ] AWS and GCP through OIDC federation (`aws-actions/configure-aws-credentials`, `google-github-actions/auth`, both SHA-pinned) — no static cloud keys in secrets
- [ ] `.github/workflows/live-providers.yml`: weekly `schedule` offset from the Monday CI cron, `workflow_dispatch`; runs `make live-tests` — a new target wrapping `cargo nextest run` with a `live` profile in `.config/nextest.toml` (`retries = 1`, `slow-timeout = 60s`); never part of `Required`
- [ ] Failure routing: the job opens or updates one issue labelled `provider-drift` with the failing test names and the run link (SHA-pinned action); it closes the issue when green
- [ ] `tests/README.md`: what runs live, cost expectations, how to run locally, what to do when the issue fires

## TEST-02 — Python SDK smoke (M)

- [ ] `tests/sdk-smoke-python/` with `pyproject.toml` pinning `openai`, `anthropic`, `google-genai`, and a committed `uv.lock`; a `setup-python-uv` composite action pinning `uv` and Python (SHA-pinned)
- [ ] `smoke.py` mirrors `tests/sdk-smoke/smoke.mjs` case for case: `/v1` and `/openai/v1` bases, trailing slash, `x-litellm-api-key`, streaming, Responses, model list and retrieve, Anthropic messages and `count_tokens`, Gemini `generateContent`; reuses the `sdk_smoke_fixture` example server and the metadata handshake from `tests/sdk-smoke/run.sh`
- [ ] `make sdk-smoke-python` and `make sdk-smoke-python-install`; added to the `sdk-compatibility` job; `make advisories` extended with `uv audit` (or `pip-audit`, pinned)
- [ ] `.github/dependabot.yml`: `pip` ecosystem for `tests/sdk-smoke-python`, weekly, grouped minor/patch
- [ ] `tests/README.md` SDK section lists both runtimes

## CI-06 — `Required` tier under 8 minutes (S)

- [ ] `.github/actions/setup-rust`: add a `cache-targets` input (default `"false"`); `fuzz-replay` passes `"true"` and keys the cache on `fuzz/Cargo.lock` plus the nightly pin from `Makefile`; measure the warm run
- [ ] If still over 8 minutes: keep `make fuzz-check` (stable `cargo check`) in `Required` and move `fuzz-replay` to the full tier beside `fuzz-campaign`; update the tier comments at the top of `ci.yml` and in `Makefile`
- [ ] Record job durations before and after in the PR description

## CI-07 — Flake policy (S)

- [ ] `.config/nextest.toml`: comment stating retries stay at 0 in every profile except `live`; a test that fails once gets an issue, not a retry
- [ ] `CONTRIBUTING.md` "Validation": time-dependent assertions (UTC windows, TTLs, minute boundaries) must be deterministic or must straddle the boundary deliberately, as CI-04 does

## Exit criteria

- [ ] `live-providers.yml` has run green once via `workflow_dispatch`; schedule armed; the `provider-drift` issue mechanism tested with a forced failure
- [ ] `sdk-compatibility` runs JavaScript and Python smokes; both green
- [ ] `Required` wall time ≤ 8 minutes on a warm cache (baseline ~16)

## Carry-over

_None yet._
