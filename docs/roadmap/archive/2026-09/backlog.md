# Backlog

Every roadmap work item, in one place. Milestone files carry the detailed
task checklists; this file carries the identity, priority, size, week, and
status of each item. Change the week here when an item moves.

Status values: `planned`, `in progress`, `done`, `dropped` (with a reason).

## Summary

| Week | Items | P0 | P1 | P2 | P3 |
|---|---|---|---|---|---|
| 1 — Green and protected `main` | CI-01 … CI-05, GOV-01 … GOV-04 | 3 | 3 | 2 | 1 |
| 2 — First published release | REL-01 … REL-05 | — | 5 | — | — |
| 3 — Test the real risks | TEST-01, TEST-02, CI-06 … CI-08 | — | 2 | 2 | 1 |
| 4 — Onboarding and docs | DOC-01 … DOC-04, TEST-03, GOV-05 | — | 1 | 5 | — |
| 5 — Distributed tracing | OTEL-01 … OTEL-04 | — | — | 4 | — |
| 6 — Spend controls | SPEND-01 … SPEND-05 | — | — | 5 | — |
| 7 — Performance evidence | PERF-01 … PERF-04 | — | — | 2 | 2 |
| 8 — Release 2.3.0 and hardening | REL-06, HYG-01 … HYG-03 | — | 1 | 2 | 1 |
| Next period, week 1 | TEST-01, REL-07 | — | 2 | — | — |
| Next period, week 2 | DOC-05, OTEL-05, SPEND-06, SPEND-07 | — | — | 4 | — |
| Next period, week 3 | OTEL-06 | — | — | 1 | — |
| Unscheduled | TEST-04, HYG-04 … HYG-08, FLEX-01 … FLEX-06 | — | 3 | 3 | 6 (incl. P4) |

60 items: 48 scheduled, 12 unscheduled; 49 done. [`deferred.md`](deferred.md) lists what
is intentionally not here.

## CI — the gates themselves

- [x] **CI-01** Coverage measures the DB suites, prints its number, floor 80 — P0 · M · week 1 · done
  Root cause and numbers in [`baseline.md`](baseline.md#rust--coverage--floor-failure-caused-by-measurement-not-by-tests). Unblocks every pull request. → [milestone 1](milestones/01-green-main.md)
- [x] **CI-02** Bump yanked `chacha20`, drop the stale `RUSTSEC-2026-0235` ignore, write down the yank policy — P0 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **CI-03** Refresh the Firefox `request-explorer` baseline and decide the cross-browser snapshot policy — P1 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **CI-04** Make the shared-Valkey isolation proof immune to UTC-minute rollover — P1 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **CI-05** Repair the Dependabot cargo group (`futures: unknown_error`) and land the pending bumps once — P1 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **CI-06** `Required` tier under 8 minutes: stable fuzz compile required, nightly replay full-tier, E2E parallelized — P2 · S · week 3 · done → [milestone 3](milestones/03-risk-tests.md)
- [x] **CI-07** Flake policy: no retries outside live-provider tests; time-dependent assertions must be deterministic — P3 · S · week 3 · done → [milestone 3](milestones/03-risk-tests.md)
- [x] **CI-08** Correlate shared-Valkey runtime hints by generation UUID instead of timing — P1 · S · week 3 · done → [milestone 3](milestones/03-risk-tests.md)

## GOV — governance and repository hygiene

- [x] **GOV-01** Protect `main` with a ruleset requiring `Required`, linear history, and deletion/force-push blocks; merge queue unavailable for this personal-account-owned public repository — P0 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **GOV-02** Prune 62 remote branches to ≤ 10; auto-delete merged heads — P2 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **GOV-03** Backlog hygiene: review tracking reflects reality, tracking issues per milestone, CHANGELOG entry for the CI changes — P2 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **GOV-04** Remove the dead `stable` branch trigger; fix the dev-box git identity — P3 · S · week 1 · done → [milestone 1](milestones/01-green-main.md)
- [x] **GOV-05** Repository presentation: description, topics, issue templates, CODEOWNERS, code of conduct, sharper `SECURITY.md` — P2 · S · week 4 · done → [milestone 4](milestones/04-onboarding.md)

## REL — releases

- [x] **REL-01** `release.yml` with tag/version verification, Make lockstep, and supply-chain pins — P1 · M · week 2 · done → [milestone 2](milestones/02-first-release.md)
- [x] **REL-02** Native amd64 + arm64 image on GHCR by digest, cosign-signed, SBOM attested, Trivy-scanned — P1 · M · week 2 · done → [milestone 2](milestones/02-first-release.md)
- [x] **REL-03** Helm chart on `oci://ghcr.io/tyk-swe/charts`, signed, render-identical to the repo — P1 · S · week 2 · done → [milestone 2](milestones/02-first-release.md)
- [x] **REL-04** Cut `v2.2.0`: version bump, CHANGELOG, tag, release notes from CHANGELOG, assets — P1 · S · week 2 · done → [milestone 2](milestones/02-first-release.md)
- [x] **REL-05** Quick start pulls the published image; source build becomes a Compose overlay — P1 · S · week 2 · done → [milestone 2](milestones/02-first-release.md)
- [x] **REL-06** Cut `v2.3.0` with tracing, budgets, bench, and NetworkPolicy — P1 · S · week 8 · done → [milestone 8](milestones/08-release-and-retro.md)
- [ ] **REL-07** Reproduce the 2.2.1-chart to 2.3.0 fresh-cluster Helm upgrade and record migration 0049 — P1 · S · next period, week 1 · planned

## TEST — tests that guard external risk

- [ ] **TEST-01** Weekly live-provider job with environment-scoped secrets, cloud OIDC federation, auto-filed `provider-drift` issue — P1 · M · next period, week 1 · in progress → [milestone 3](milestones/03-risk-tests.md)
- [x] **TEST-02** Python SDK smoke (`openai`, `anthropic`, `google-genai`) mirroring the JavaScript cases — P2 · M · week 3 · done → [milestone 3](milestones/03-risk-tests.md)
- [x] **TEST-03** The README first-request example executes verbatim in the e2e suite — P2 · S · week 4 · done → [milestone 4](milestones/04-onboarding.md)
- [ ] **TEST-04** Property-based round-trip tests for the protocol translators (OpenAI ↔ canonical ↔ Anthropic/Gemini) — P3 · M · unscheduled · planned

## DOC — onboarding and documentation

- [x] **DOC-01** "Your first request": curl plus Python/JavaScript for all three surfaces, `model` = route slug explained — P1 · S · week 4 · done → [milestone 4](milestones/04-onboarding.md)
- [x] **DOC-02** `docs/concepts.md` with one request-lifecycle diagram — P2 · M · week 4 · done → [milestone 4](milestones/04-onboarding.md)
- [x] **DOC-03** `docs/compatibility.md`: generated surface × operation table plus per-provider notes, drift-checked by `make compat-check` — P2 · M · week 4 · done → [milestone 4](milestones/04-onboarding.md)
- [x] **DOC-04** Helm NetworkPolicy template, values, schema, and docs — P2 · S · week 4 · done → [milestone 4](milestones/04-onboarding.md)
- [ ] **DOC-05** Record an unseen user's elapsed time from README checkout to first successful request — P2 · S · next period, week 2 · planned

## OTEL — distributed tracing

- [x] **OTEL-01** Design record: crates, ownership, configuration surface, span attribute allowlist — P2 · S · week 5 · done → [milestone 5](milestones/05-tracing.md)
- [x] **OTEL-02** Request and attempt spans, OTLP/HTTP exporter, W3C propagation in and out, off by default — P2 · L · week 5 · done → [milestone 5](milestones/05-tracing.md)
- [x] **OTEL-03** Proof: allowlist unit test, e2e collector assertions, HA trace continuity — P2 · M · week 5 · done → [milestone 5](milestones/05-tracing.md)
- [x] **OTEL-04** Configuration docs, Helm values, Jaeger compose overlay, CHANGELOG — P2 · S · week 5 · done → [milestone 5](milestones/05-tracing.md)
- [ ] **OTEL-05** Record an end-to-end trace in the Jaeger Compose overlay — P2 · S · next period, week 2 · planned
- [ ] **OTEL-06** Compare the tracing-disabled benchmark against the pre-tracing baseline on the same host — P2 · S · next period, week 3 · planned

## SPEND — spend controls

- [x] **SPEND-01** Budget semantics written before code: windows, accrual, unpriced attempts, fail-closed, error shape — P2 · S · week 6 · done → [milestone 6](milestones/06-spend-controls.md)
- [x] **SPEND-02** Migration 0049, cost reservation script, limiter dimension, PostgreSQL reconciliation task — P2 · M · week 6 · done → [milestone 6](milestones/06-spend-controls.md)
- [x] **SPEND-03** Engine admission, terminal accrual, management API fields, audit events, metrics — P2 · M · week 6 · done → [milestone 6](milestones/06-spend-controls.md)
- [x] **SPEND-04** Console: key form, list column, detail view, usage filter — P2 · M · week 6 (stretch) · done → [milestone 6](milestones/06-spend-controls.md)
- [x] **SPEND-05** Integration and e2e proofs, docs, CHANGELOG semantics sentence — P2 · M · week 6 · done → [milestone 6](milestones/06-spend-controls.md)
- [ ] **SPEND-06** Demonstrate priced budget exhaustion through the Compose stack — P2 · S · next period, week 2 · planned
- [ ] **SPEND-07** Run and record the Toxiproxy HA budget-outage scenario — P2 · S · next period, week 2 · planned

## PERF — performance evidence

- [x] **PERF-01** `make bench`: `oha` against the loopback mock, added-latency p50/p95/p99, JSON results — P2 · M · week 7 · done → [milestone 7](milestones/07-performance.md)
- [x] **PERF-02** Criterion micro-benchmarks on the SSE decoder, translators, and codecs — P3 · S · week 7 · done → [milestone 7](milestones/07-performance.md)
- [x] **PERF-03** Non-blocking `perf` job with artifact and regression warning — P3 · S · week 7 · done → [milestone 7](milestones/07-performance.md)
- [x] **PERF-04** Act on the numbers: profile once, revalidate admission defaults, replace the provisional latency objectives — P2 · M · week 7 · done → [milestone 7](milestones/07-performance.md)

## HYG — hardening and hygiene

- [x] **HYG-01** Size-baseline burn-down: split the 9 oversized test files and the 15 longest functions — P3 · M · week 8 · done → [milestone 8](milestones/08-release-and-retro.md)
- [x] **HYG-02** Security pass on 2.3.0: headers, cookies, auth throttling, egress rules for the collector, audit tools — P2 · M · week 8 · done → [milestone 8](milestones/08-release-and-retro.md)
- [x] **HYG-03** Retrospective and the next roadmap — P2 · S · week 8 · done → [milestone 8](milestones/08-release-and-retro.md)
- [x] **HYG-04** `deploy/Dockerfile` `USER 65532:65532` to clear hadolint DL3066 and match the Helm security context — P4 · S · unscheduled · done
- [ ] **HYG-05** Retire the duplicate `jsonwebtoken` line once `google-cloud-auth` moves off 10.x — P4 · S · unscheduled · planned (watch Dependabot)
- [ ] **HYG-06** Transitive lockfile maintenance: a scheduled `cargo update` PR so yanks and patch releases surface weekly, not on the next unrelated push — P3 · S · unscheduled · planned
- [x] **HYG-07** Toolchain bump procedure documented in `CONTRIBUTING.md` (Rust pin in three places, Node, pnpm, fuzz nightly, sqlx-cli, nextest, llvm-cov) — P3 · S · unscheduled · done
- [ ] **HYG-08** Node 26 / corepack plan for the console build stage (majors are excluded from Dependabot on purpose) — P4 · S · unscheduled · planned

## FLEX — restrictive where it matters, flexible where it does not

Loosens restrictions that carried no security or integrity benefit. The
default-deny, certification-before-activation, never-store-content and
immutable-generation invariants are untouched.

- [x] **FLEX-01** Operator egress allowlist (`OLP_PROVIDER_EGRESS_ALLOW_CIDRS`, `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS`) replaces the debug-only insecure-endpoint hatch — P1 · M · unscheduled · done
- [x] **FLEX-02** Certification survives renames, credential rotation, model disable and unchanged tuple re-review — P1 · M · unscheduled · done
- [x] **FLEX-03** Connection age never cuts an in-flight stream; age and drain configurable — P1 · M · unscheduled · done
- [x] **FLEX-04** Request, inline-media and provider-response caps configurable with unchanged defaults — P2 · M · unscheduled · done
- [x] **FLEX-05** `limits.valkey_unavailable` setting: opt-in fail-open for rate/concurrency-limited keys; cost-budgeted keys remain fail-closed — P2 · M · unscheduled · done
- [x] **FLEX-06** Small fixes: models listing covers every operation, page cap 200 everywhere, gzip JSON bodies, 64 tuples / 8 probes, gateway CORS, activate validates inline — P2 · M · unscheduled · done
