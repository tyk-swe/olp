# OpenLLMProxy agent guide

OpenLLMProxy is a single Rust binary serving OpenAI/Anthropic/Gemini surfaces,
management, workers, and an embedded static SvelteKit console. The main Cargo
workspace has two pnpm islands (`console/`, `tests/sdk-smoke/`) and a separate
nightly Cargo workspace (`fuzz/`).

## Commands

`make help` is the CI-aligned index. Common gates are:

| Concern | Target |
|---|---|
| Broad local gate | `make check-local` (`make check` is an alias) |
| Format/lint/tests | `make fmt`, `make clippy`, `make test` (locked nextest) |
| CI Rust gate | `make coverage` (llvm-cov nextest, 51% line floor) |
| PostgreSQL/Valkey | `make db-test` (see `CONTRIBUTING.md`) |
| Console | `make console-install`, `make console-verify`, `make console-e2e` |
| Contracts/generated files | `make openapi`, `make sqlx-prepare`, `make screenshots` |

Rust compilations use sccache 0.17.0 through `.cargo/config.toml`; install it
as described in `CONTRIBUTING.md`, or set `RUSTC_WRAPPER=` to bypass it for a
single command.

The workspace has zero doctests by policy; restore a `cargo test --doc` gate if
one is added. CI also runs service, browser, image, HA, upgrade, supply-chain,
Helm, SDK, and fuzz jobs beyond the local gate.

## Generated files

Never hand-edit `.sqlx/`, `openapi/management.json`,
`console/src/lib/api/schema.d.ts`, or `docs/assets/screenshots/*.png`; use
`make sqlx-prepare`, `make openapi`, or `make screenshots`. Lockfiles change
only through locked/frozen installs.

## Roles and ownership

| Role | Responsibility |
|---|---|
| `engine` | Canonical model, vendor protocols, provider networking, routing, inference, and persistence ports |
| `db` | PostgreSQL, Valkey, encryption, migrations, Lua, and engine port implementations |
| `delivery` | HTTP, CLI, process composition, workers |
| `test-harness` | Conformance and end-to-end support that production packages never depend on |

Every workspace package declares `[package.metadata.olp]`; the boundary checker
enforces semantic role edges and infrastructure ownership. The engine is the
base; the database crate depends on engine-defined ports, and delivery composes
both. Within the engine, dependencies point left in `domain <- protocols <-
providers <- inference`; a module may use only itself and modules to its left. Database
code owns SQLx/Redis, `olp_engine::providers` exclusively owns
Reqwest/AWS/Google auth and concrete connector construction, and delivery owns
Axum/Tower/Clap. Test harnesses never enter production dependency graphs.
Transport-neutral behavior belongs in `olp_engine::inference`. Delivery roots
are `bootstrap/`, `public_http/`, `gateway/`, `management/`, `observability/`,
and `console/`.

### Source map

- Provider kinds, auth, fields, presets: `crates/olp-engine/src/domain/provider_configuration.rs`.
- Endpoint method/path/admission/routing: `apps/olp/src/gateway/endpoint_policy.rs`.
- Routing eligibility/scoring: `crates/olp-engine/src/domain/routing/`.
- Pinning, failover, limits, terminal accounting: `crates/olp-engine/src/inference/`.
- Public egress classification: `crates/olp-engine/src/providers/http_egress.rs`.
- SQL migrations: `crates/olp-db/migrations/` (forward-only, sequential).
- Management contract: `openapi/management.json` → `make openapi`.
- Helm values/schema/templates: `deploy/helm/` → `make helm-verify`.
- Runtime settings: `docs/configuration.md`.

## Hazards and tests

Adding a package requires its role and verification that `deploy/Dockerfile`
copies it. BuildKit uses `deploy/Dockerfile.dockerignore`; keep the root
`.dockerignore` synchronized. `release-metadata.env` pins the upgrade baseline.
The console remains `adapter-static` with no server routes/hooks.

Unit tests stay beside owners; ignored service suites use `make db-test`; the
conformance, SDK, HA, browser, and fuzz layouts are indexed in `tests/README.md`.
See `CONTRIBUTING.md` and `docs/{architecture,configuration,deployment,operations}.md`
for contributor and operational detail.

Prefer built-in file tools and bounded output. Avoid shell-based editing,
complex quoting, and commands that rewrite generated or unrelated files.
