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
`OLP_VERTEX_LIVE_{PROJECT,LOCATION,MODEL}`, and `OLP_BEDROCK_LIVE_{REGION,MODEL}`.
The static Vertex service-account live test was retired because CI uses OIDC;
the mocked service-account flow remains covered locally.

- [x] Inventory: `tests/README.md` maps every variable to its gated test and call cost; catalog and token-count probes are free, Azure uses `gpt-5-nano`, Vertex uses `gemini-3.1-flash-lite`, and Bedrock uses `amazon.nova-micro-v1:0`
- [x] GitHub environment `live-providers` requires `tyk-swe` review and permits deployments only from `main`
- [ ] Provider keys dedicated to CI with hard monthly caps set in each provider console and stored in `live-providers`
- [ ] AWS and GCP through OIDC federation (`aws-actions/configure-aws-credentials`, `google-github-actions/auth`, both SHA-pinned) — no static cloud keys in secrets
- [x] `.github/workflows/live-providers.yml`: Wednesday `schedule` offset from the Monday CI cron plus `workflow_dispatch`; runs `make live-tests` with the `live` nextest profile (`retries = 1`, 60-second slow timeout); never part of `Required`
- [x] Failure routing: the job opens or updates one issue labelled `provider-drift` with the failing test names and run link; it closes the issue when green
- [x] `tests/README.md`: what runs live, cost expectations, how to run locally, and what to do when the issue fires

## TEST-02 — Python SDK smoke (M)

- [x] `tests/sdk-smoke-python/` with `pyproject.toml` pinning `openai`, `anthropic`, `google-genai`, and a committed `uv.lock`; a `setup-python-uv` composite action pinning uv and Python by immutable action SHA
- [x] `smoke.py` mirrors `tests/sdk-smoke/smoke.mjs` case for case: `/v1` and `/openai/v1` bases, trailing slash, `x-litellm-api-key`, streaming, Responses, model list and retrieve, Anthropic messages and `count_tokens`, Gemini `generateContent`; both reuse `sdk_smoke_fixture` and `tests/sdk-smoke/run.sh`
- [x] `make sdk-smoke-python` and `make sdk-smoke-python-install`; added to `sdk-compatibility`; `make advisories` extended with the pinned `uv audit`
- [x] `.github/dependabot.yml`: `uv` ecosystem for `tests/sdk-smoke-python`, weekly, grouped minor/patch
- [x] `tests/README.md` SDK section lists both runtimes

## CI-06 — `Required` tier under 8 minutes (S)

- [ ] `.github/actions/setup-rust`: `cache-targets` now defaults to `"false"`; `fuzz-replay` writes and `fuzz-campaign` reads the schema-versioned target cache keyed by `fuzz/Cargo.lock` and the nightly pin; a pushed cold/warm pair still needs measuring
- [ ] If still over 8 minutes: keep `make fuzz-check` (stable `cargo check`) in `Required` and move `fuzz-replay` to the full tier beside `fuzz-campaign`; update the tier comments at the top of `ci.yml` and in `Makefile`
- [ ] Record job durations before and after in the PR description

## CI-07 — Flake policy (S)

- [x] `.config/nextest.toml`: retries stay at 0 in every profile except `live`; a test that fails once gets an issue, not a retry
- [x] `CONTRIBUTING.md` "Validation": time-dependent assertions (UTC windows, TTLs, minute boundaries) must be deterministic or deliberately straddle the boundary

## Exit criteria

- [ ] `live-providers.yml` has run green once via `workflow_dispatch`; schedule armed; the `provider-drift` issue mechanism tested with a forced failure
- [ ] `sdk-compatibility` runs JavaScript and Python smokes; both green
- [ ] `Required` wall time ≤ 8 minutes on a warm cache (baseline ~16)

Repository validation is green for both SDK runtimes. Hosted evidence remains
open: configure provider secrets and cloud federation, dispatch a green and a
forced-failure live run, run `sdk-compatibility`, then measure a cold
cache-writing fuzz replay followed by a warm run. The baseline successful
[Required run](https://github.com/tyk-swe/olp/actions/runs/33292128914) took
15m20s; its fuzz replay took 15m15s.

## Carry-over

_None yet._
