# Contributing

Thank you for contributing to OpenLLMProxy. This guide describes the
development environment, the architectural rules every change must respect,
and the validation required before review.

## Development environment

Use Rust 1.97, Node.js 24 or newer, pnpm 11, and PostgreSQL 18 for the
full local suite. The Compose stack supplies PostgreSQL 18 and Valkey 9.1 for
integration work.

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
- `openapi/management.json` owns the tracked management API contract and
  requires `pnpm --dir console api:generate` after changes.
- SQL migrations in `crates/storage/migrations/` are forward-only.
- `.sqlx/` owns the checked PostgreSQL query metadata. Static production SQL
  uses `query!`, `query_as!`, or `query_scalar!`; dynamic filters use
  `QueryBuilder::build_query_as` with a cohesive `FromRow` model. Manual
  string-key `Row::get`/`try_get` decoding is not allowed.
- `release-metadata.env` records the migration included in the last completed
  release and is the strict CI N-1 baseline. Bootstrap metadata is valid only
  for 2.0.0; later versions require a verified released image digest.
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

Run the full suite before requesting review:

```sh
./scripts/check-boundaries.sh
./scripts/check-storage-sqlx.sh
cargo fmt --all --check
SQLX_OFFLINE=true cargo clippy --locked --workspace --all-targets --all-features -- -D warnings
SQLX_OFFLINE=true cargo test --locked --workspace --all-features
pnpm --dir console install --frozen-lockfile
pnpm --dir console verify
scripts/check-release-version.sh
scripts/check-supply-chain-pins.sh
scripts/check-docs.py
tests/qualification/run.sh load
```

For database changes, run `./scripts/run-postgres-tests.sh` against PostgreSQL
18, then run `cargo sqlx prepare --workspace --check -- --all-targets --all-features`
against a migrated development database. Regenerate metadata without `--check`
after an intentional query or schema change. For deployment changes, run
`scripts/verify-helm-contract.sh deploy/helm`.

Operational, deployment, connector, performance, image, and release changes
must also update [the qualification matrix](docs/qualification.md) when their
command, tier, threshold, evidence, or failure owner changes. Run the relevant
stable target (`clean-install`, `backup-restore`, `n-minus-one`, `load`, or
`soak`) exactly as CI does. Never use live-provider credentials in a pull
request; the scheduled and tagged workflows own the mandatory canaries.
