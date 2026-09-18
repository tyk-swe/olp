# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. `make dev` starts
the Go gateway and Vite. See
[CONTRIBUTING.md](../CONTRIBUTING.md) for the toolchain and repository setup.

## Local development

Run from the repository root:

```sh
make dev
```

Open http://127.0.0.1:5173 and use `.local/go-secrets/bootstrap.token` to create
the first owner. Vite proxies API, OIDC callback, and inference requests to Go
on port 8082, preserving the browser origin and cookies. Console edits hot
reload; restart `make dev` after backend changes.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run these from the repository root after `make setup`:

| Command                     | Purpose                                              |
| --------------------------- | ---------------------------------------------------- |
| `pnpm --dir console verify` | Formatting, Svelte/type checks, ESLint, and Vitest   |
| `pnpm --dir console test`   | Synchronize SvelteKit and run unit/component tests   |
| `pnpm --dir console build`  | Static assets and asset manifest                     |
| `make api`                  | Generate Go and TypeScript management contract types |
| `make test`                 | All container-free Go, console, and script tests     |
| `make test-console`         | Console unit and component tests                     |
| `make check`                | Static checks and all local tests                    |
| `make integration`          | Service suites and Chromium journeys                 |

Management requests use the generated `openapi-fetch` client. Update the
checked-in `openapi/management.json` contract alongside the Go handlers, then
run `make api`. Do not hand-edit the generated `src/lib/api/schema.d.ts`.

## Feature ownership

`src/lib/features/` groups provider, route, access, settings, inference,
runtime, usage, and media workflows. Keep route components thin and shared UI
in `$lib/components/`; see the [architecture map](../docs/architecture.md).
Keep the application client-only: do not add server routes, server hooks,
`lib/server/`, or a production Node adapter.

## Testing

Vitest runs under `TZ=America/New_York` to exercise local-time formatting and
daylight-saving behavior. Component tests use jsdom and browser exports.
The test command synchronizes SvelteKit, so no prior build is needed. Focused
`.only` tests fail validation; use runner filters for local work instead:

```sh
make test-console CONSOLE_TEST_ARGS='--project unit src/lib/format.test.ts'
```

`make integration` provisions disposable services and runs Chromium journeys
against both the packaged Go origin and the Vite development origin,
including replacement restore. The suite covers setup, provider activation,
routing, inference, history, OIDC, and edit conflicts. It supplies the database,
Valkey, and file-backed secrets required by `pnpm --dir console test:e2e`.
See [tests/README.md](../tests/README.md) for focused suites.
