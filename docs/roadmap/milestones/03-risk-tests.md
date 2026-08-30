# Milestone 3 — Test the risks the gates do not cover

| | |
|---|---|
| Dates | Mon 2026-09-14 → Sun 2026-09-20 |
| Goal | Provider drift is detected weekly instead of by users; the dominant client ecosystem is smoke-tested; pull requests get their verdict in under 8 minutes |
| Backlog items | TEST-01, TEST-02, CI-06, CI-07, CI-08 |
| Prerequisites | Provider accounts with API keys dedicated to CI; AWS role and GCP workload identity for OIDC federation |

## TEST-01 — Weekly live-provider job (M)

The tests exist behind `OLP_LIVE_OPENAI_API_KEY`, `OLP_LIVE_ANTHROPIC_API_KEY`,
`OLP_LIVE_GEMINI_API_KEY`, `OLP_AZURE_OPENAI_LIVE_{ENDPOINT,DEPLOYMENT,API_VERSION,API_KEY}`,
`OLP_VERTEX_LIVE_{PROJECT,LOCATION,MODEL}`, and `OLP_BEDROCK_LIVE_{REGION,MODEL}`.
The static Vertex service-account live test was retired because CI uses OIDC;
the mocked service-account flow remains covered locally.

- [x] Inventory: `tests/README.md` maps every variable to its gated test and call cost; catalog and token-count probes are free, Azure uses `gpt-5-nano`, Vertex uses `gemini-3.1-flash-lite`, and Bedrock uses `amazon.nova-micro-v1:0`
- [x] GitHub environment `live-providers` requires `tyk-swe` review and permits deployments only from `main`
- [ ] Provider identities dedicated to CI and stored in `live-providers`; use a
  provider-enforced cap or fixed prepaid balance without automatic reload where
  available, otherwise the lowest practical permissions and quotas, spend
  alerts with automatic disablement, and a recorded residual-overshoot bound
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

- [x] `.github/actions/setup-rust`: `cache-targets` defaults to `"false"`;
  `fuzz-replay` writes and `fuzz-campaign` reads the schema-versioned target
  cache. Cold [run 33303620675](https://github.com/tyk-swe/olp/actions/runs/33303620675)
  took 15m10s for replay; warm [run 33305412730](https://github.com/tyk-swe/olp/actions/runs/33305412730)
  restored the exact 877 MB entry but still took 7m32s.
- [x] Keep `make fuzz-check` (stable `cargo check`) in `Required`; move nightly
  replay to the full tier beside the campaign; run E2E in parallel with the
  independently required PostgreSQL integration job; update CI/Make tier text
- [x] Record before/after job durations in
  [PR #133](https://github.com/tyk-swe/olp/pull/133)

## CI-07 — Flake policy (S)

- [x] `.config/nextest.toml`: retries stay at 0 in every profile except `live`; a test that fails once gets an issue, not a retry
- [x] Playwright retries are 0 for required and full-tier browsers; required
  Chromium is green without a retry in
  [run 33308319108](https://github.com/tyk-swe/olp/actions/runs/33308319108/job/99248820696)
- [x] `CONTRIBUTING.md` "Validation": time-dependent assertions (UTC windows, TTLs, minute boundaries) must be deterministic or deliberately straddle the boundary

## CI-08 — Deterministic shared-Valkey hint attribution (S)

- [x] [Issue #132](https://github.com/tyk-swe/olp/issues/132) records both
  failures instead of retrying them for green
- [x] The isolation proof correlates a runtime hint with the exact generation
  UUID returned by installation A; delayed bootstrap hints from installation B
  no longer get attributed to A by timing
- [x] The complete three-test worker-HA suite passes three consecutive local
  runs against PostgreSQL and Valkey
- [x] The backported release-candidate proof is green in
  [run 33309244354](https://github.com/tyk-swe/olp/actions/runs/33309244354/job/99251507975)

## Exit criteria

- [ ] `live-providers.yml` has run green once via `workflow_dispatch`; schedule armed; the `provider-drift` issue mechanism tested with a forced failure
- [x] `sdk-compatibility` runs JavaScript and Python smokes; both green in
  [run 33305412730](https://github.com/tyk-swe/olp/actions/runs/33305412730/job/99240982566)
- [x] `Required` wall time ≤ 8 minutes on a warm cache: 6m20s in
  [run 33308319108](https://github.com/tyk-swe/olp/actions/runs/33308319108)
  and 6m47s in
  [run 33308637297](https://github.com/tyk-swe/olp/actions/runs/33308637297)

Repository validation and hosted CI are green for both SDK runtimes. The first
fallback [PR run](https://github.com/tyk-swe/olp/actions/runs/33308319108)
completed `Required` in 6m20s: stable fuzz compile 1m53s, PostgreSQL 6m06s,
test-util 3m03s, E2E 2m36s after test-util, and coverage—the final gate—6m14s.
The second warm measurement completed in 6m47s. TEST-01 still needs provider
identities, cloud federation, and the green/failure/recovery runs.

## Carry-over

_None yet._
