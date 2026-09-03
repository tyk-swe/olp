# Roadmap — September and October 2026

This directory is the working plan for OpenLLMProxy from **2026-08-31 to
2026-10-25**: eight one-week milestones, the backlog they draw from, the
evidence the plan rests on, and the risks that could derail it.

Weeks 1–2 are corrective: a green, protected `main` and a first published
release. Weeks 3–4 close the gaps the current gates do not cover. Weeks 5–7
deliver the three capabilities that matter most against peer gateways —
distributed tracing, spend controls, and performance evidence. Week 8 ships
all of it as 2.3.0 and resets the plan.

Progress is mirrored in the
[pinned roadmap issue](https://github.com/tyk-swe/olp/issues/116). The checked-in
backlog is the sole source of work-item status; milestone files hold detail and
evidence.

## Layout

| File | Holds | Update when |
|---|---|---|
| [`baseline.md`](baseline.md) | The measured state on 2026-08-29 and the CI forensics behind weeks 1–2 | Never — it is the reference point |
| [`backlog.md`](backlog.md) | Every work item with an ID, priority, size, week, and status | An item starts, finishes, or is dropped |
| [`milestones/`](milestones/) | One file per week: goal, task checklists, exit criteria, carry-over | Daily, as tasks close |
| [`deferred.md`](deferred.md) | What is deliberately outside these two months, and what would reopen it | A reopen trigger fires |
| [`risks.md`](risks.md) | Risk register with early-warning signals and mitigations | A risk materialises or retires |

## Milestones

| # | Dates | Theme | Ships |
|---|---|---|---|
| [1](milestones/01-green-main.md) | Aug 31 – Sep 6 | Green and protected `main` | Five CI fixes, protected `main`, pruned branches |
| [2](milestones/02-first-release.md) | Sep 7 – Sep 13 | First published release | `v2.2.0`, plus `v2.2.1` quick-start repair: signed image, OCI chart, GitHub Release, pull-based setup |
| [3](milestones/03-risk-tests.md) | Sep 14 – Sep 20 | Test the real risks | Weekly live-provider job, Python SDK smoke, `Required` tier under 8 minutes |
| [4](milestones/04-onboarding.md) | Sep 21 – Sep 27 | Onboarding and docs | "Your first request", concepts page, generated compatibility matrix, Helm NetworkPolicy, repository presentation |
| [5](milestones/05-tracing.md) | Sep 28 – Oct 4 | Distributed tracing | OpenTelemetry request and attempt spans, OTLP export, W3C propagation, content-free by construction |
| [6](milestones/06-spend-controls.md) | Oct 5 – Oct 11 | Spend controls | Per-key daily and monthly cost budgets: migration 0049, fail-closed enforcement, API, console |
| [7](milestones/07-performance.md) | Oct 12 – Oct 18 | Performance evidence | `make bench`, Criterion micro-benchmarks, non-blocking perf job, measured SLO numbers |
| [8](milestones/08-release-and-retro.md) | Oct 19 – Oct 25 | Release 2.3.0 and hardening | Release, size-baseline burn-down, security pass, retrospective, next plan |

## Conventions

- **IDs.** Every backlog item has an ID (`CI-01`, `REL-03`, …). Milestone
  sections, commits, PRs, and issues reference the ID so the trail is
  searchable.
- **Checkboxes.** `[ ]` open, `[x]` done, `[-]` dropped — a dropped box gets
  one line saying why, on the same line.
- **Size.** S under half a day, M one to two days, L three days or more.
- **Priority.** P0 blocks release, P1 next, P2 soon, P3 when convenient, P4
  someday.
- **Gates.** Every change passes `make check` locally and the `Required` job
  in CI. Nothing here overrides `AGENTS.md`, `CONTRIBUTING.md`, the
  forward-only migration rule, or the generated-artifact rule.
- **Floors only rise.** Coverage floors, size baselines, and pinned tool
  versions are never lowered or loosened to make a week pass.

## Weekly cadence

- [ ] Monday — triage the scheduled CI run (03:17 UTC) and the Dependabot
  results the same day; merge or explain every dependency PR.
- [ ] Wednesday — check the live-provider job (from week 3) and the
  `provider-drift` issue.
- [ ] Friday — score the milestone against its exit criteria; move leftovers
  into the next milestone's carry-over section and re-assign the week in
  `backlog.md`.
- [ ] Every PR — regenerate `.sqlx/`, `openapi/management.json`, the console
  schema, screenshots, and all four Playwright baselines when the change
  touches them.
- [ ] Every release — `release-metadata.env` moves only in the release commit;
  `make release-version` guards it.

## Relationship to the other tracking files

- `CHANGELOG.md` records what shipped. This directory records what is planned.
  When an item ships, its CHANGELOG entry is the closing evidence.
- `backlog.md` holds review-derived defects and roadmap work on the same
  priority scale; cross-reference by ID rather than duplicating text.
- `release-metadata.env` is release bookkeeping, not roadmap state.

## Updating the plan

1. Exit criteria decide whether a week is done, not the number of ticked boxes.
2. Unfinished tasks go to the next milestone's **Carry-over** section and get
   their week changed in `backlog.md`; they are never silently dropped.
3. A new item enters `backlog.md` first, with an ID, and is only then
   scheduled into a milestone.
4. The week-8 retrospective rewrites this README for the next period and
   moves this one under `docs/roadmap/archive/` with its final state intact.

## Final score

Scored against each milestone's exit criteria after the 2.3.0 tag workflow.

| Milestone | Score | Decision |
|---|---:|---|
| 1 — Green `main` | 5 / 5 | Complete |
| 2 — First release | 4 / 4 | Complete |
| 3 — Risk tests | 2 / 3 | Carry TEST-01; the workflow exists but provider credentials and cloud identities are not provisioned |
| 4 — Onboarding | 3 / 4 | Carry DOC-05; no unseen-user elapsed-time observation exists |
| 5 — Tracing | 1 / 3 | Carry OTEL-05 and OTEL-06; Docker-backed Jaeger and benchmark dependencies are unavailable in this environment |
| 6 — Spend controls | 2 / 3 | Carry SPEND-06 and SPEND-07; Docker-backed Compose and Toxiproxy proofs are unavailable |
| 7 — Performance | 3 / 3 | Complete; `main` run 33738308399 published a green performance artifact |
| 8 — Release and retro | 4 / 4 | Complete; REL-07 carries the environment-blocked fresh-cluster Helm proof |

## Decisions

- **Completed:** all repository work scheduled for CI, SDK parity,
  documentation, tracing, spend controls, performance, source-size reduction,
  and the published 2.3.0 release.
- **Carried, not dropped:** every missed exit criterion appears in the final
  [`backlog.md`](backlog.md) with a new week and priority.
- **Dropped:** GitHub merge queue enablement because public repositories owned
  by personal accounts do not support it. The required ruleset remains active.
- **Candidates, trigger-gated:** exact-match response caching, guardrail hooks,
  provider- and route-level budgets, OpenAI Files and Batch, console i18n, and
  the licensing question remain in [`deferred.md`](deferred.md). None has the
  evidence required to schedule implementation yet.

## Health at close

- The twelve most recent inspected `Required` jobs are green, ending with
  [run 33783393182](https://github.com/tyk-swe/olp/actions/runs/33783393182/job/100750001133).
- Time-to-first-request remains unmeasured by a person new to the repository;
  DOC-05 carries that signal into the next period.
- [`provider-drift` issue #134](https://github.com/tyk-swe/olp/issues/134) is
  open and the protected environment has no credentials or cloud identity
  variables; TEST-01 remains the top external-risk item.
