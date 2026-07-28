# OpenLLMProxy — agent guide

OpenLLMProxy is a single-binary Rust AI gateway (OpenAI/Anthropic/Gemini
surfaces, management API, background worker) that also serves an embedded
SvelteKit console as static assets. One Cargo workspace plus three deliberate
islands with their own lockfiles: `console/` (pnpm), `tests/sdk-smoke/` (pnpm),
`fuzz/` (separate Cargo workspace).

## Commands

`make help` lists everything. The Makefile mirrors CI
(`.github/workflows/ci.yml`); keep both in lockstep when either changes.

| Task | Command |
|---|---|
| Full PR gate | `make check` |
| Rust format / lint | `make fmt` (`fmt-fix`), `make clippy` |
| Rust unit tests | `make test` |
| CI's real Rust gate | `make coverage` — llvm-cov nextest with a **51% line floor**. Plain `cargo test` is not what CI enforces. The workspace has zero doctests by policy; if you add one, restore a `cargo test --doc` gate. |
| Postgres/Valkey integration tests | `make db-test` — requires `OLP_TEST_DATABASE_ADMIN_URL` and `OLP_TEST_DATABASE_URL_PREFIX`, optional `OLP_VALKEY_URL` (see CONTRIBUTING.md) |
| Console | `make console-install`, `make console-verify`, `make console-e2e` |
| Regenerate contracts | `make openapi`, `make sqlx-prepare`, `make screenshots` |

CI tiers: pull requests run the required tier; cross-browser, HA, arm64,
upgrade-rehearsal, and fuzz campaigns run only on push/schedule/dispatch.

## Generated files — never hand-edit

| Path | Regenerate with | Drift gate in CI |
|---|---|---|
| `.sqlx/` (offline query metadata) | `make sqlx-prepare` | `cargo sqlx prepare --check` (postgres-integration job) |
| `openapi/management.json` | `make openapi` | `apps/olp/tests/openapi_drift.rs` |
| `console/src/lib/api/schema.d.ts` | `make openapi` | `pnpm api:check` (inside `make console-verify`) |
| `docs/assets/screenshots/*.png` | `make screenshots` | manual — regenerate after visible UI changes |
| `Cargo.lock`, `pnpm-lock.yaml` | locked installs only | `--locked` / `--frozen-lockfile` everywhere |

## Crate map

```
apps/olp        delivery: HTTP surfaces, CLI, process composition (axum, clap)
crates/domain   canonical model, routing policy — no infrastructure deps
crates/protocols vendor wire translation (OpenAI/Anthropic/Gemini DTOs, SSE)
crates/providers outbound provider + OIDC networking (reqwest, aws-*, google-cloud-auth)
crates/storage  PostgreSQL (sqlx) + Valkey; migrations; Lua scripts
tests/conformance cross-protocol conformance harness over tests/fixtures/
```

Dependencies point toward `crates/domain`:
`olp → {domain, protocols, providers, storage}`, `storage → domain`,
`providers → {domain, protocols}`, `protocols → domain`.
`scripts/check-boundaries.sh` enforces the DAG, the exact package set, and
dependency ownership (`sqlx`/`redis` only in storage; `reqwest`/`aws-*`/
`google-cloud-auth` only in providers; `axum`/`clap` only in apps/olp).

## Where does X live

| Change | Owner |
|---|---|
| Provider kinds, auth choices, field applicability, validation | `crates/domain/src/provider_configuration.rs` |
| Inference endpoint registry (method, path, admission, routing) | `apps/olp/src/gateway/endpoint_policy.rs` |
| Runtime capability eligibility, weighted rendezvous scoring | `crates/domain/src/routing.rs` |
| Public IP classification / egress policy | `crates/providers/src/http_egress.rs` |
| SQL migrations (forward-only, sequential) | `crates/storage/migrations/` |
| Management API contract | `openapi/management.json` → `make openapi` after endpoint changes |
| Helm defaults + schema + templates (change together) | `deploy/helm/` → `make helm-verify` |
| Runtime configuration reference | `docs/configuration.md` |

## Testing model

- Rust unit tests live in `src/**/tests.rs` modules next to the code.
- The `#[ignore]`d PostgreSQL/Valkey suites live in the consolidated
  `tests/integration/` binaries of `crates/storage` and `apps/olp` and run
  via `make db-test` (nextest, one database per test from
  `olp_storage::test_support`) against PostgreSQL 18 and Valkey.
- `tests/conformance` replays the fixture corpus in `tests/fixtures/`.
- Console: Vitest colocated `*.test.ts`, Playwright suites under
  `console/tests/`, Storybook a11y. `fuzz/` needs nightly (see Makefile).

## Hazards

- Never move `apps/`, `crates/*`, or `console/src/routes`, and never add or
  remove a workspace package, without co-updating `scripts/check-boundaries.sh`
  and the COPY list in `deploy/Dockerfile`.
- Two dockerignore files exist; BuildKit uses `deploy/Dockerfile.dockerignore`
  for the production image. Keep the root `.dockerignore` a synchronized copy.
- Migrations are forward-only; `release-metadata.env` pins the last released
  migration for CI's upgrade rehearsal.
- The console is client-only: no `+page.server.*`, `+server.*`, or server
  hooks; the adapter must stay `adapter-static` (enforced by
  `scripts/check-boundaries.sh`).

Deeper rules: `CONTRIBUTING.md` (architecture rules, sources of truth, change
maps, validation) and `docs/{architecture,configuration,deployment,operations}.md`.
Subtree guides: `apps/olp/AGENTS.md`, `crates/AGENTS.md`, `console/AGENTS.md`.
