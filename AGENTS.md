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
| `domain` | Canonical model, provider configuration, routing; no infrastructure |
| `protocols` | Vendor DTOs, translation, bounded SSE |
| `providers` | Outbound/OIDC networking, authentication, egress, error mapping |
| `storage` | PostgreSQL, Valkey, encryption, migrations, Lua |
| `inference` | Pinning, selection, failover, limits, event collection, accounting |
| `delivery` | HTTP, CLI, process composition, workers |

Every workspace package declares `[package.metadata.olp]`; the boundary checker
enforces semantic role edges and infrastructure ownership. Domain is the base;
storage owns SQLx/Redis, providers own Reqwest/AWS/Google auth, and delivery
owns Axum/Tower/Clap. Test harnesses never enter production dependency graphs.
Transport-neutral behavior belongs in an inference-role crate. Delivery roots
are `bootstrap/`, `public_http/`, `gateway/`, `management/`,
`observability/`, and `console/`.

### Source map

- Provider kinds, auth, fields, presets: `crates/domain/src/provider_configuration.rs`.
- Endpoint method/path/admission/routing: `apps/olp/src/gateway/endpoint_policy.rs`.
- Routing eligibility/scoring: `crates/domain/src/routing/`.
- Pinning, failover, limits, terminal accounting: `crates/inference/src/`.
- Public egress classification: `crates/providers/src/http_egress.rs`.
- SQL migrations: `crates/storage/migrations/` (forward-only, sequential).
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
