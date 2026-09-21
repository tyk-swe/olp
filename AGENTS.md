# Repository Guidelines

## Project Structure & Module Organization

OpenLLMProxy combines a Go gateway with a client-only SvelteKit console.
Independent conformance inputs live in `tests/fixtures/reference-inventory.json`
and `tests/release-behaviors.json`; `docs/roadmap/README.md` retains dated
qualification provenance.

- `internal/`: backend feature packages (`access/`, `providers/`, `routes/`, `gateway/`, `media/`, `observability/`); keep types, SQL, handlers and workflows together. `cmd/olp` is the binary entrypoint; `openapi/management.json` owns the management contract and `openapi/document.go` embeds it.
- `console/src/lib/features/`: matching console features; shared UI lives in `console/src/lib/components/`, pages in `console/src/routes/`, and static assets in `console/static/`.
- `tests/fixtures/`: language-neutral protocol corpus; `tests/integration/`: process and service suites; `tests/sdk*`: official SDK qualification; `console/tests/`: browser journeys.
- `internal/database/migrations/`: forward-only Go SQL history; `deploy/`: Compose and Helm configuration; `scripts/`: automation; `docs/`: architecture and operations guides.

## Build, Test, and Development Commands

Use Go 1.27.1, a C compiler/linker and glibc headers, Node.js 26, pnpm 11,
Docker Compose, and the [contributor prerequisites](CONTRIBUTING.md). Run from
the repository root:

| Command | Purpose |
| --- | --- |
| `make setup` | Install workspace dependencies and generate API contracts. |
| `make dev` | Start PostgreSQL, Valkey, Go, and Vite at http://127.0.0.1:5173. |
| `make check` | Generate contracts; check formatting, vet, ESLint, types, and local tests. |
| `make test` | Run Go, console unit/component, and script tests without containers. |
| `make test-go` | Run Go unit and protocol tests. |
| `make test-console` | Run console unit and component tests. |
| `make test-scripts` | Run automation script tests. |
| `make test-race` | Run uncached Go tests with race detection. |
| `make integration` | Run disposable-service, recovery, SDK, and Chromium suites. |
| `make api` | Generate Go and TypeScript types from the checked-in OpenAPI contract. |
| `make build` | Build the release binary and static console. |
| `make fmt` | Format Go and console source. |

## Coding Style & Naming Conventions

Go code uses standard `gofmt` formatting and `go vet` cleanliness. Console code
uses two-space indentation, single quotes, and no trailing commas through
Prettier; use `camelCase` helpers and `PascalCase.svelte` components. Follow go
vet and ESLint; keep route components thin.

## Testing Guidelines

Place Go unit tests beside their feature as `*_test.go`. Process and service
suites live in `tests/integration/` or beside their feature, use the
`integration` build tag, and run under `make integration`. An explicitly
selected integration test must fail when required service configuration is
missing. Use descriptive behavior names, Vitest `*.test.ts`/`*.svelte.test.ts`,
and Playwright `*.spec.ts`. Run console tests with `make test-console` and Go
tests with `make test-go`. See the [testing guide](tests/README.md) for
filtering, timeouts, and coverage. No numeric coverage threshold is configured;
preserve meaningful fixtures and cover changed behavior. Run integration checks
for service or browser changes.

## Commit & Pull Request Guidelines

Use short imperative subjects, following history such as `Simplify CI`;
occasional prefixes like `docs:` are also used. Name Codex-created branches
`tyk/{branch-name}`, never `codex/`.

Follow `.github/PULL_REQUEST_TEMPLATE.md`: explain the problem and resulting
behavior, record validation, and include screenshots for visible console
changes. Regenerate affected API contracts rather than editing generated files.
Keep migrations forward-only and sequential; update Helm values, schema, and
templates together.

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues. Read `docs/agents/issue-tracker.md` before tracker operations.

### Triage labels

Use the five default triage labels. Read `docs/agents/triage-labels.md` before assigning triage roles.

### Domain docs

Single-context: root `CONTEXT.md` and `docs/adr/`. Before exploring the codebase, read `docs/agents/domain.md`.
