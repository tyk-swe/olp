# Contributing

Thank you for contributing to OpenLLMProxy. This guide describes the
development environment, the architectural rules every change must respect,
and the validation required before review.

## Development environment

Use Rust 1.97, Node.js 24 or newer, pnpm 11.10, and PostgreSQL 18 for the
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
- `release-metadata.env` records the migration included in the last completed
  release and is the CI upgrade-rehearsal baseline.
- Helm defaults, schema, and templates in `deploy/helm/` change together.

### Change maps

- `crates/providers/src/factory.rs` is the provider lifecycle assembly site.
  Keep caller-owned HTTP errors, AAD decryption, and mounted-file I/O outside
  it.
- `crates/domain/src/routing.rs` owns capability eligibility and weighted
  rendezvous scoring. The management capability endpoint is generated from its
  policy and filtered by the connector certification contract.
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
cargo fmt --all --check
cargo clippy --locked --workspace --all-targets --all-features -- -D warnings
cargo test --locked --workspace --all-features
pnpm --dir console install --frozen-lockfile
pnpm --dir console verify
scripts/check-release-version.sh
scripts/check-supply-chain-pins.sh
```

For database changes, run `./scripts/run-postgres-tests.sh` against PostgreSQL
18. For deployment changes, run `scripts/verify-helm-contract.sh deploy/helm`.
