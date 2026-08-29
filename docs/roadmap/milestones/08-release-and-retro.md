# Milestone 8 — Release 2.3.0, hardening, retrospective

| | |
|---|---|
| Dates | Mon 2026-10-19 → Sun 2026-10-25 |
| Goal | Everything from weeks 3–7 ships as `v2.3.0` through the pipeline built in week 2; the repository's own rules are back to zero grandfathered debt where it is cheap; the next plan exists |
| Backlog items | REL-06, HYG-01, HYG-02, HYG-03 |
| Prerequisites | Milestones 5–7 exit criteria, or their carry-over explicitly excluded from 2.3.0 |

## REL-06 — Cut `v2.3.0` (S)

- [ ] Same procedure as REL-04; `release-metadata.env` advances to `0047` (the last migration shipped by 2.2.0) in the release commit; `make release-version` passes
- [ ] CHANGELOG `[2.3.0]` groups tracing, budgets, Python SDK smoke, bench, NetworkPolicy, and docs, each with its backlog ID
- [ ] Upgrade rehearsal 2.2.0 → 2.3.0 green in CI before tagging
- [ ] `helm upgrade` from the 2.2.0 chart on the kind cluster used in week 2; migration Job runs 0048

## HYG-01 — Size-baseline burn-down (M)

- [ ] `scripts/source-size-baseline.txt` holds 9 files over 30 KB (all test files) and 65 functions over 100 lines; this week: split the 9 files by concern and the 15 longest functions without behaviour change (list function lengths with a one-off `awk` using the same `fn` counting the script applies)
- [ ] The baseline only shrinks: `make source-size` green after each split, no new entries

## HYG-02 — Security pass on 2.3.0 (M)

- [ ] Public routes: CSP, HSTS, `X-Frame-Options`, `Referrer-Policy` present on every response class (tests exist in `apps/olp/src/public_http/router.rs`; extend for the new tracing-enabled configuration)
- [ ] Session cookie flags, login and invitation throttling (`public_auth_rate_limits`) unchanged and tested
- [ ] Egress classification for the OTLP collector endpoint: it may be private (it is our collector) — document the explicit exception and keep provider egress rules unchanged
- [ ] The tracing layer never logs or exports secrets — review every `span.record` call added in milestone 5
- [ ] `cargo deny`, `pnpm audit`, `uv audit`, and Trivy on the release image: clean, or ignored with an expiry date and a reason
- [ ] MFA decision recorded in `docs/deployment.md`: production disables local login and uses OIDC; revisit only on request (see `deferred.md`)

## HYG-03 — Retrospective and the next plan (S)

- [ ] Score all eight milestones against their exit criteria; write every miss into `backlog.md` with a new week or into `TODOS.md` with a priority
- [ ] Update `README.md` in this directory: tick, drop with reasons, and list candidates for the next period (exact-match response cache, guardrail hooks, provider-level budgets, OpenAI Files and Batch, console i18n, the licensing question)
- [ ] Health signals to keep watching: `Required` green streak, time-to-first-request for a new user, `provider-drift` issue streak
- [ ] Move this period's files under `docs/roadmap/archive/2026-09/` with their final state intact and start the next period's `README.md`

## Exit criteria

- [ ] `v2.3.0` published with image, chart, signatures, SBOMs, and release notes
- [ ] Baseline file shorter than at the start of the week; `make check` green
- [ ] Security pass recorded with no open HIGH/CRITICAL findings
- [ ] Next period's roadmap README exists

## Carry-over

_None yet._
