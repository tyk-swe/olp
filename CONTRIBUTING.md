# Contributing

Thank you for contributing to OpenLLMProxy. This guide describes the
development environment, the architectural rules every change must respect,
and the validation required before review.

## Development environment

Use Rust 1.97, Node.js 24 or newer, pnpm 11, and PostgreSQL 18 for the
full local suite. The Compose stack supplies PostgreSQL 18 and Valkey 9.1 for
integration work.

### Toolchain

The standard gate (`make check`) needs ripgrep (for
`scripts/check-boundaries.sh`). Matching CI's full validation additionally
needs, with the versions CI pins:

- `cargo install --locked cargo-nextest@0.9.140 cargo-llvm-cov@0.8.7` — the
  coverage gate (`make coverage`).
- `cargo install --locked sqlx-cli@0.9.0` — regenerating `.sqlx/` metadata
  (`make sqlx-prepare`).
- `cargo install --locked cargo-fuzz@0.13.2` plus
  `rustup toolchain install nightly-2026-05-15 --profile minimal` — fuzz
  targets only (`make fuzz-replay`).
- `shellcheck` and `jq` — the CI quality job (`make shellcheck`,
  `scripts/backup-manifest.sh`).

The stable Rust toolchain (1.97.0) installs automatically via
`rust-toolchain.toml`.

## Architectural rules

Keep changes within the component that owns the behavior: `domain` owns
canonical policy, `protocols` owns wire translation, `providers` owns outbound
provider/OIDC networking, `storage` owns PostgreSQL and Valkey, and `apps/olp`
owns delivery and process composition. The console remains a static
client-only application.

### Dependency rules

Dependencies point toward `crates/domain`; it must not acquire infrastructure
dependencies. Cargo path dependencies must stay in this workspace. Do not add
console server routes or a production Node adapter. Keep dependencies locked
and third-party Actions and container images pinned.

### Sources of truth

- `Cargo.toml` owns the workspace version.
- `openapi/management.json` owns the tracked management API contract.
  Regenerate it and the console schema together with `make openapi`
  (= `cargo run -p olp --example export_openapi` +
  `pnpm --dir console api:generate`) after management endpoint changes.
- SQL migrations in `crates/storage/migrations/` are forward-only.
- `.sqlx/` owns the checked PostgreSQL query metadata. Static production SQL
  uses `query!`, `query_as!`, or `query_scalar!`; dynamic filters use
  `QueryBuilder::build_query_as` with a cohesive `FromRow` model. Manual
  string-key `Row::get`/`try_get` decoding is not allowed.
- `release-metadata.env` records the migration included in the last completed
  release and is the CI upgrade-rehearsal baseline.
- Helm defaults, schema, and templates in `deploy/helm/` change together.

### Change maps

- `crates/domain/src/provider_configuration.rs` owns provider kinds,
  authentication choices, field applicability, defaults, and complete-candidate
  validation. Provider factories own transport construction, not a parallel
  capability matrix.
- `apps/olp/src/gateway/endpoint_policy.rs` owns the inference endpoint
  registry: method, path, surface, operation, handler, admission, routing, and
  token-estimation association.
- `crates/domain/src/routing.rs` owns runtime capability eligibility and
  weighted rendezvous scoring. Connector certification filters those domain
  capabilities before activation.
- `crates/providers/src/http_egress.rs` owns public IP classification. Provider
  and OIDC modules own URL policy, DNS pinning, bounded bodies, and error
  mapping.
- Update `openapi/management.json` and regenerate the console schema whenever
  a management endpoint changes.
- Regenerate the published console screenshots after visible UI changes with
  `pnpm --dir console screenshots` and commit the updated PNGs under
  `docs/assets/screenshots/`.

## Validation

Run the full suite before requesting review (first time:
`make console-install`):

```sh
make check
```

which expands to:

```sh
./scripts/check-boundaries.sh
./scripts/check-storage-sqlx.sh
cargo fmt --all --check
SQLX_OFFLINE=true cargo clippy --locked --workspace --all-targets --all-features -- -D warnings
SQLX_OFFLINE=true cargo test --locked --workspace --all-features
pnpm --dir console verify
scripts/check-release-version.sh
scripts/check-supply-chain-pins.sh
```

Note that CI's Rust test gate is stricter than plain `cargo test`: it runs
`cargo llvm-cov nextest` with a **51% line-coverage floor**. Reproduce it
locally with `make coverage` before pushing test-sensitive changes. The
workspace deliberately has zero doctests (nextest and llvm-cov do not run
them); if you add one, restore a `cargo test --doc` gate in the Makefile
and CI.

CI runs in two tiers: pull requests and merge queues run the required tier
(quality, Rust, console, SDK compatibility, database integration, amd64
image). Cross-browser, HA, arm64, upgrade-rehearsal, and bounded fuzz
campaigns run only on push, schedule, or manual dispatch.

### Database test environment

`./scripts/run-postgres-tests.sh` (or `make db-test`) runs the `#[ignore]`d
PostgreSQL/Valkey integration tests against PostgreSQL 18 and requires:

- `OLP_TEST_DATABASE_ADMIN_URL` — maintenance database URL with rights to
  create and drop per-test databases.
- `OLP_TEST_DATABASE_URL_PREFIX` — connection prefix **without** a trailing
  database name; each test appends its own database.
- `OLP_TEST_DATABASE_OWNER` (optional, default `olp`) and
  `OLP_POSTGRES_TEST_TIMEOUT_SECONDS` (optional, default 900).

With the Compose stack from the README quick start running and the default
`olp` password:

```sh
OLP_TEST_DATABASE_ADMIN_URL=postgres://olp:olp@127.0.0.1:5432/postgres \
OLP_TEST_DATABASE_URL_PREFIX=postgres://olp:olp@127.0.0.1:5432/ \
make db-test
```

For database changes, follow with
`cargo sqlx prepare --workspace --check -- --all-targets --all-features`
(`make sqlx-check`) against a migrated development database. Regenerate
metadata with `make sqlx-prepare` after an intentional query or schema
change. For deployment changes, run `make helm-verify`.
