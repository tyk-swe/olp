# Contributing

Thank you for contributing to OpenLLMProxy. This guide covers the development
environment, the architectural rules every change must respect, and the
validation required before review.

## Development environment

Use Rust 1.97, Node.js 24 or newer, pnpm 11, Clang with LLD, and PostgreSQL 18
for the full local suite. Linux builds select Clang and LLD through
`.cargo/config.toml`. The Compose stack supplies
PostgreSQL 18 and Valkey 9.1 for integration work. The stable Rust toolchain
(1.97.1) installs automatically via `rust-toolchain.toml`.

The broad local gate (`make check-local`, with `make check` retained as an alias) needs ripgrep (for
`scripts/check-boundaries.sh`) and cargo-nextest
(`cargo install --locked cargo-nextest@0.9.140`, the `make test` runner).
Matching CI's full validation additionally needs, at the versions CI pins:

- `cargo install --locked cargo-llvm-cov@0.8.7` — the coverage gate
  (`make coverage`, also run through nextest).
- `cargo install --locked sqlx-cli@0.9.0` — regenerating `.sqlx/` metadata
  (`make sqlx-prepare`).
- `cargo install --locked cargo-fuzz@0.13.2` plus
  `rustup toolchain install nightly-2026-05-15 --profile minimal` — fuzz
  targets only (`make fuzz-replay`).
- `shellcheck` and `jq` — the CI quality job (`make shellcheck`,
  `scripts/backup-manifest.sh`).

## Architectural rules

Keep changes within the component that owns the behavior: `domain` owns
canonical policy, `protocols` owns wire translation, `providers` owns
outbound provider/OIDC networking, `storage` owns PostgreSQL and Valkey, and
`inference` owns transport-neutral runtime pinning, selection, failover,
limits, and terminal accounting. `apps/olp` owns HTTP delivery and process
composition. The console remains a static client-only application.

### Dependency rules

Dependencies point toward `crates/domain`, which must not acquire
infrastructure dependencies. Cargo path dependencies stay in this workspace.
Do not add console server routes or a production Node adapter. Keep
dependencies locked and third-party Actions and container images pinned.

The production Cargo DAG is:

```text
olp-domain
olp-protocols -> olp-domain
olp-providers -> {olp-domain, olp-protocols}
olp-storage -> olp-domain
olp-inference -> {olp-domain, olp-protocols, olp-providers, olp-storage}
olp -> {olp-domain, olp-protocols, olp-providers, olp-storage, olp-inference}
```

Axum, Tower, and Clap stay in `olp`; SQLx/Redis in storage; Reqwest, AWS, and
Google authentication in providers. The app has only `bootstrap`,
`public_http`, `gateway`, `management`, `observability`, and `console`
production roots. Boundary checks reject a return to flat app modules or
production wildcard re-exports. The optional `ProcessComposition` assembly
input stays private to bootstrap in normal builds; integration fixtures use
the `test-util`-gated `olp::test_support` namespace.

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
  authentication choices, field applicability, defaults, and
  complete-candidate validation. Provider factories own transport
  construction, not a parallel capability matrix.
- `apps/olp/src/gateway/endpoint_policy.rs` owns the inference endpoint
  registry: method, path, surface, operation, handler, admission, routing,
  and token-estimation association.
- `crates/domain/src/routing/` owns runtime capability eligibility and weighted
  rendezvous scoring behind the narrow `routing.rs` facade. Connector
  certification filters those domain capabilities before activation.
- `crates/inference/src/` owns runtime generation pinning, selection/failover,
  circuit state, distributed limit lease lifetime, canonical event collection,
  and terminal request/attempt/usage accounting. HTTP adapters must call the
  shared `InferenceService` instead of duplicating this orchestration.
- `crates/providers/src/http_egress.rs` owns public IP classification.
  Provider and OIDC modules own URL policy, DNS pinning, bounded bodies, and
  error mapping.
- Update `openapi/management.json` and regenerate the console schema whenever
  a management endpoint changes.
- Regenerate the published console screenshots after visible UI changes with
  `pnpm --dir console screenshots` and commit the updated PNGs under
  `docs/assets/screenshots/`.

## Validation

Run the full suite before requesting review (first time:
`make console-install`):

```sh
make check-local
```

which expands to:

```sh
./scripts/check-boundaries.sh
./scripts/check-storage-sqlx.sh
cargo fmt --all --check
SQLX_OFFLINE=true cargo clippy --locked --workspace --all-targets --all-features -- -D warnings
SQLX_OFFLINE=true cargo nextest run --locked --workspace --all-features
pnpm --dir console verify
scripts/check-release-version.sh
scripts/check-supply-chain-pins.sh
```

CI's Rust test gate is stricter than plain `cargo test`: it runs
`cargo llvm-cov nextest` with a **51% line-coverage floor**. Reproduce it
locally with `make coverage` before pushing test-sensitive changes. The
workspace deliberately has zero doctests (nextest and llvm-cov do not run
them); if you add one, restore a `cargo test --doc` gate in the Makefile and
CI.

The end-to-end contract suite (`make e2e`, `tests/e2e`) drives the real
`olp all` binary against PostgreSQL, Valkey, and a loopback mock upstream.
Every assertion in it is derived from a document — `README.md`, `docs/*.md`,
or `openapi/management.json` — and cites the clause it enforces. It is
**pass-gated**: any failure fails CI, and there is no expected-failure
manifest. A failure means the product and its documentation disagree; resolve
it by fixing one of them, never by weakening the assertion.
The runner requires `psql` so its process-exit trap can sweep only the
run-scoped databases left by a panic, filter, or interruption.
Contract assertions are split under `tests/e2e/tests/contract/` by public
surface, management, provider lifecycle, gateway dialect, data safety,
telemetry, and distributed limits; `contract.rs` owns only the shared
installation lifecycle and final teardown.
The longest PostgreSQL contracts likewise keep their single ordered database
lifecycle while moving provider/route/API-key, OIDC harness, query, and
retention phases into named sibling modules under each integration test.

The suite needs an isolated PostgreSQL **and an isolated Valkey**: the
request-metadata stream key is a fixed global name, so a second `olp` worker
on the same Valkey consumes this installation's telemetry and the request log
silently stays empty. Without `OLP_E2E_VALKEY_URL` the harness atomically
leases and clears one local logical database with a PostgreSQL session
advisory lock; concurrent runs cannot select the same keyspace, and process
exit releases the reservation automatically.

Dev builds use `debug = "line-tables-only"` (workspace `[profile.dev]`):
backtraces keep file:line information, but debuggers lose variable and type
detail. For interactive debugging, override locally in
`~/.cargo/config.toml`:

```toml
[profile.dev]
debug = 2
```

CI runs in two tiers: pull requests and merge queues run the required tier
(quality, Rust lint/coverage/fuzz-replay, console, SDK compatibility,
database integration, end-to-end contract, amd64 image). Cross-browser, HA, arm64,
upgrade-rehearsal, and bounded fuzz campaigns run only on push, schedule, or
manual dispatch.

### Database test environment

`./scripts/run-postgres-tests.sh` (or `make db-test`) runs the `#[ignore]`d
PostgreSQL/Valkey integration tests through nextest (profile `db` in
`.config/nextest.toml`) against PostgreSQL 18 and requires:

- `OLP_TEST_DATABASE_ADMIN_URL` — maintenance database URL with rights to
  create and drop per-test databases. Every test creates its own
  `olp_test_{run}_*` database via `olp_storage::test_support::TestDb`; names
  carry a per-run token, so the script's leftover sweep only ever touches
  databases of its own run.
- `OLP_TEST_DATABASE_URL_PREFIX` — connection prefix **without** a trailing
  database name; each test appends its own database.
- `OLP_TEST_DATABASE_OWNER` (optional, default `olp`).
- `OLP_VALKEY_URL` (optional) — an isolated Valkey; without it the
  `distributed_limits_valkey` suite is skipped with a warning.

Per-test timeouts live in the nextest `db` profile. To run a subset, pass a
nextest filterset through, e.g. `make db-test ARGS="-E 'test(upgrade_0021)'"`.

With the Compose stack from the README quick start running and the default
`olp` password:

```sh
OLP_TEST_DATABASE_ADMIN_URL=postgres://olp:olp@127.0.0.1:5432/postgres \
OLP_TEST_DATABASE_URL_PREFIX=postgres://olp:olp@127.0.0.1:5432/ \
make db-test
```

For database changes, follow with
`cargo sqlx prepare --workspace --check -- --all-targets --all-features`
(`make sqlx-check`) against a migrated development database, and regenerate
metadata with `make sqlx-prepare` after an intentional query or schema
change. For deployment changes, run `make helm-verify`.
