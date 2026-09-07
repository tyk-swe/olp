# Agent Guidelines

- Name branches `tyk/{branch-name}`. Never use `codex/`.
- Keep code descriptive and control flow direct. Avoid unnecessary wrappers, facades, indirection, and speculative abstractions.
- Do not add speculative compatibility. Preserve compatibility required by an explicit release, migration, storage, serialization, or deployment contract. Retire it only through a documented migration and verification.
- Avoid comments except to explain an unavoidable workaround or complex algorithm.
- Use `pnpm`. Keep functions under 100 lines and source files under 30 KB.
- Keep feature types, validation, management handlers, SQL, and administrative workflows together.

# Repository

One Rust 2024 package under `src/`. Product owners are `providers`, `routes`, `access`, `settings`, `inference`, `protocols`, `runtime`, `limits`, `usage`, and `media`. Infrastructure lives in `process`, `database`, `http`, `net`, `crypto`, and `observability`. The SvelteKit console uses the same owners in `console/src/lib/features/`. See [docs/architecture.md](docs/architecture.md).

Use parameterized SQLx queries and typed `FromRow` results. Pass pools, transactions, and audit provenance explicitly. The 3.0 schema requires a fresh installation and rejects 2.x storage. Future migrations are forward-only and sequential.

`make setup` installs dependencies and generates contracts. `make dev` runs services, Rust, and Vite. `make check` is the required PR check; `make test` uses standard Cargo tests. Run `make integration` for changes to service behavior. `make api`, `make build`, and `make fmt` handle generation, release builds, and formatting. Details and the TypeScript 6.0 exception are in [CONTRIBUTING.md](CONTRIBUTING.md).

Unit tests belong beside their owner; shared behavioral fixtures and service suites are under `tests/`. Preserve meaningful expectations. Do not hand-edit generated OpenAPI, TypeScript contracts, or screenshots. Do not add custom architecture, source-size, dependency-pin, or CI synchronization gates.

Use focused imperative commit summaries. PRs explain the problem, resulting behavior, and validation; include screenshots for visible changes. Update Helm values, schema, and templates together. Never commit credentials, generated secrets, prompts, outputs, or customer data. Report vulnerabilities through `SECURITY.md`.
