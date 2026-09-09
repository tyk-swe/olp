# Repository Guidelines

## Project Structure & Module Organization

OpenLLMProxy combines a Rust 2024 gateway with a client-only SvelteKit console.

- `src/`: backend feature modules, including `providers/`, `routes/`, `access/`, `inference/`, and `protocols/`. Keep each feature's types, SQL, handlers, and workflows together.
- `console/src/lib/features/`: matching console features; shared UI lives in `console/src/lib/components/`, pages in `console/src/routes/`, and static assets in `console/static/`.
- `tests/`: Rust integration/protocol suites, fixtures, and SDK smoke tests; `console/tests/journeys/`: browser journeys; `benches/`: Criterion benchmarks.
- `migrations/`: database changes; `deploy/`: Compose and Helm configuration; `scripts/`: automation; `docs/`: architecture and operations guides.

## Build, Test, and Development Commands

Use the pinned `rust-toolchain.toml`, Node.js 26, pnpm 11, Docker Compose, and prerequisites in `CONTRIBUTING.md`. Run from the repository root:

| Command | Purpose |
| --- | --- |
| `make setup` | Install workspace dependencies and generate API contracts. |
| `make dev` | Start PostgreSQL, Valkey, Rust, and Vite at localhost:5173. |
| `make check` | Run formatting, Clippy, Rust tests, ESLint, Svelte/type checks, and Vitest. |
| `make test` | Run Rust unit and protocol tests. |
| `make integration` | Run disposable-service, recovery, SDK, and Chromium suites. |
| `make api` | Regenerate OpenAPI and TypeScript contracts. |
| `make build` | Build the release binary and static console. |
| `make fmt` | Format Rust and console source. |

## Coding Style & Naming Conventions

Rust uses four-space indentation, a 100-column rustfmt limit, `snake_case` functions/modules, and `PascalCase` types. Console code uses two-space indentation, single quotes, and no trailing commas through Prettier; use `camelCase` helpers and `PascalCase.svelte` components. Follow Clippy and ESLint; keep route components thin.

## Testing Guidelines

Place Rust unit tests beside their feature and register new integration targets in `Cargo.toml` (`autotests = false`). Use descriptive behavior names, Vitest `*.test.ts`/`*.svelte.test.ts`, and Playwright `*.spec.ts`. Run console tests with `pnpm --dir console test`. No numeric coverage threshold is configured; preserve meaningful fixtures and cover changed behavior. Run integration checks for service or browser changes.

## Commit & Pull Request Guidelines

Use short imperative subjects, following history such as `Simplify CI`; occasional prefixes like `docs:` are also used. Name Codex-created branches `tyk/{branch-name}`, never `codex/`.

Follow `.github/PULL_REQUEST_TEMPLATE.md`: explain the problem and resulting behavior, record validation, and include screenshots for visible console changes. Regenerate affected API contracts rather than editing generated files. Keep migrations forward-only and sequential; update Helm values, schema, and templates together.
