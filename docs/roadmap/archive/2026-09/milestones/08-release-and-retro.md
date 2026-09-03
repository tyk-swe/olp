# Milestone 8 — Release 2.3.0, hardening, retrospective

| | |
|---|---|
| Dates | Mon 2026-10-19 → Sun 2026-10-25 |
| Goal | Everything not already released from weeks 3–7 ships as `v2.3.0` through the pipeline built in week 2; the repository's own rules are back to zero grandfathered debt where it is cheap; the next plan exists |
| Backlog items | REL-06, HYG-01, HYG-02, HYG-03 |
| Prerequisites | Milestones 5–7 exit criteria, or their carry-over explicitly excluded from 2.3.0 |

## REL-06 — Cut `v2.3.0` (S)

- [x] Same procedure as REL-04; `release-metadata.env` remains `0048` (the last migration shipped by 2.2.1) in the release commit; `make release-version` passes
- [x] CHANGELOG `[2.3.0]` groups tracing, budgets, bench, NetworkPolicy, and docs, each with its backlog ID
- [x] Upgrade rehearsal 2.2.1 → 2.3.0 green in CI before tagging in
  [run 33738308399](https://github.com/tyk-swe/olp/actions/runs/33738308399/job/100594824226)
- [ ] `helm upgrade` from the 2.2.1 chart on a fresh cluster reproducing the
  week-2 topology; migration Job runs 0049

## HYG-01 — Size-baseline burn-down (M)

- [x] The historical source-size baseline held 9 files and 62 functions (71
  total entries; the planned count of 65 functions was a documentation error).
  The 9 files and 15 longest functions were split without behaviour changes.
- [x] The baseline only shrank, from 71 to 47 entries; `make source-size` passes

## HYG-02 — Security pass on 2.3.0 (M)

- [x] Public routes: CSP, HSTS, `X-Frame-Options`, `Referrer-Policy` present on every response class, with tracing disabled and enabled
- [x] Session cookie flags, login and invitation throttling (`public_auth_rate_limits`) unchanged and tested
- [x] Egress classification for the OTLP collector endpoint: it may be private (it is our collector) — the explicit exception is documented and provider egress rules are unchanged
- [x] Every tracing `span.record` call exports only the documented attribute allowlist and never content, credentials, headers, or raw errors
- [ ] `cargo deny`, `pnpm audit`, `uv audit`, and Trivy on the release image: clean, or ignored with an expiry date and a reason
- [x] MFA decision recorded in `docs/deployment.md`: production disables local login and uses OIDC; revisit only on request (see `deferred.md`)

## HYG-03 — Retrospective and the next plan (S)

- [x] Score all eight milestones against their exit criteria; write every miss into `backlog.md` with a new week and priority
- [x] Update `README.md` in this directory: tick, drop with reasons, and list candidates for the next period (exact-match response cache, guardrail hooks, provider-level budgets, OpenAI Files and Batch, console i18n, the licensing question)
- [x] Health signals to keep watching: `Required` green streak, time-to-first-request for a new user, `provider-drift` issue streak
- [x] Move this period's files under `docs/roadmap/archive/2026-09/` with their final state intact and start the next period's `README.md`

## Exit criteria

- [ ] `v2.3.0` published with image, chart, signatures, SBOMs, and release notes
- [x] Baseline file shorter than at the start of the week; `make check` green
- [ ] Security pass recorded with no open HIGH/CRITICAL findings
- [x] Next period's roadmap README exists

## Carry-over

- REL-07 records the fresh-cluster Helm upgrade after publication. This
  environment cannot access the Docker API, so it cannot create the disposable
  kind cluster used by the release procedure.
