# Agent Guidelines

- Do not add speculative compatibility. Preserve compatibility explicitly required by release, migration, storage, serialization, or deployment contracts. Remove it only through a documented retirement migration after its support window and verification gates have passed.
- Prefer self-explanatory code with clear, descriptive names and obvious control flow.
- Avoid comments. Add them only when necessary to explain an unavoidable non-obvious workaround or genuinely complex algorithm.
- Use `pnpm`.
- Keep functions under 100 lines and source files under 30 KB.
- Avoid unnecessary wrappers, facades, indirection, and speculative abstractions.
- Prefer the simplest structure that clearly expresses intent.
- Do not make maintainability more complex

# Repository Guidelines

## Structure
Rust 2024 workspace: `apps/olp/` (HTTP/CLI binary), `crates/olp-engine/` (domain/provider logic), `crates/olp-db/` (PostgreSQL/Valkey persistence). Dependencies flow toward the engine—Axum/Clap stay in delivery, SQLx/Redis in the db crate. SvelteKit UI in `console/`; unit tests colocated, broader suites in `tests/`; fuzzing/deploy/docs/automation in `fuzz/`, `deploy/`, `docs/`, `scripts/`.

## Commands
- `make console-install`: install pinned pnpm deps on first setup.
- `make check`: `check-static` (boundaries, scripts, formatting, repo checks; ~10 s) then `check-heavy` (Clippy, nextest, console verify in parallel). Use `make check-static` as the quick loop.
- `cargo run -p olp -- all`: run locally; `OLP_CONSOLE_DIR=console/build` serves built console.
- `pnpm --dir console dev`: UI-only Vite server (no API proxy).
- `make e2e`: contract tests vs PostgreSQL, Valkey, mock provider.
- `make db-test`: ignored DB suites; required vars in `CONTRIBUTING.md`.
- `make help`: OpenAPI, SQLx, Helm, SDK, coverage, fuzz targets.

## Style
Rust: 4-space indent, `rustfmt` (`max_width = 100`), Clippy warnings denied, no unsafe. `snake_case` functions/modules, `PascalCase` types, behavior-focused test names. TS/Svelte: ESLint + Prettier; feature code in `console/src/lib/features/`, thin routes. Never hand-edit generated OpenAPI types, `.sqlx/` metadata, or screenshots—use matching Make targets.

## Testing
`make test` (locked nextest) is the gate, not plain `cargo test`. Add unit tests beside owning modules; add contract fixtures as new cases, don't replace valid expectations. `make coverage` includes the database-backed suites and enforces an 80% line-coverage floor; it needs the `make db-test` environment. Run browser/db/e2e suites when touched areas require.

## Commits & PRs
Concise imperative commit summaries, optional `test:`/`build:`/`refactor:` prefix; keep commits focused. PRs: explain what/why, link issues, report validation, include UI screenshots when visible. PR checklist: regenerate affected artifacts, migrations forward-only and sequential, update Helm values/schema/templates together.

## Security
Copy `.env.example` for local config. Never commit credentials, generated secrets, prompts, outputs, or customer data; report vulnerabilities via `SECURITY.md`.
