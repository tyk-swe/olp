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
| Broad local gate | `make check-local` (`make check` remains an alias) |
| Rust format / lint | `make fmt` (`fmt-fix`), `make clippy` |
| Rust unit tests | `make test` |
| CI's real Rust gate | `make coverage` — llvm-cov nextest with a **51% line floor**. Plain `cargo test` is not what CI enforces. The workspace has zero doctests by policy; if you add one, restore a `cargo test --doc` gate. |
| Postgres/Valkey integration tests | `make db-test` — requires `OLP_TEST_DATABASE_ADMIN_URL` and `OLP_TEST_DATABASE_URL_PREFIX`, optional `OLP_VALKEY_URL` (see CONTRIBUTING.md) |
| Console | `make console-install`, `make console-verify`, `make console-e2e` |
| Regenerate contracts | `make openapi`, `make sqlx-prepare`, `make screenshots` |

The CI required tier additionally runs service-, browser-, image-, and
coverage-dependent jobs that `make check-local` does not reproduce.
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
crates/inference transport-neutral runtime pinning, selection, execution, failover, accounting
tests/conformance cross-protocol conformance harness over tests/fixtures/
```

Every workspace package declares `[package.metadata.olp] role = "…"` using
`domain`, `protocol`, `provider`, `storage`, `inference`, `delivery`, or `test`.
`scripts/check-boundaries.sh` enforces role-compatible non-dev/build edges and
role-based dependency ownership (`sqlx`/`redis` in storage; `reqwest`/`aws-*`/
`google-cloud-auth` in provider; `axum`/`tower*`/`clap` in delivery). Dev edges
are excluded, production cannot depend on test, and same-role decomposition is
allowed. Current package and source-root names are descriptive, not snapshots.

The app source has six production roots: `bootstrap/`, `public_http/`,
`gateway/`, `management/`, `observability/`, and `console/`. Transport-neutral
inference behavior belongs in `crates/inference`, never in an Axum handler.

## Where does X live

| Change | Owner |
|---|---|
| Provider kinds, auth choices, field applicability, validation | `crates/domain/src/provider_configuration.rs` |
| Inference endpoint registry (method, path, admission, routing) | `apps/olp/src/gateway/endpoint_policy.rs` |
| Runtime capability eligibility, weighted rendezvous scoring | `crates/domain/src/routing/{selection,route}.rs` via the `routing` facade |
| Runtime pinning, failover, limits, terminal accounting | `crates/inference/src/` |
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

- When adding a workspace package, declare its OLP role and verify the workspace
  COPY scope in `deploy/Dockerfile`; semantic boundary policy discovers it
  automatically.
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

## Tool preference

- Prefer built-in Read, Edit, Write tools for file operations.
- Avoid shell-based file reading, searching, or editing when a built-in tool can perform the operation.
- Avoid complex inline shell, heredocs, nested quoting, and multi-stage pipelines.
- Keep tool output bounded; save full logs to a file and return only relevant diagnostics.
