# OpenLLMProxy console

The console is a client-only SvelteKit application. Rust serves its static
`build/` output with `index.html` as the SPA fallback. See
[CONTRIBUTING.md](../CONTRIBUTING.md) for the toolchain and repository setup.

## Local development

Run from the repository root:

```sh
make dev
```

Open http://localhost:5173 and use `.local/dev/bootstrap-token` to create the
first owner. Vite proxies API, OIDC callback, and inference requests to Rust
on port 8081, preserving the browser origin and cookies. Console edits hot
reload; restart `make dev` after Rust changes.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run these from the repository root after `make setup`:

| Command                     | Purpose                                               |
| --------------------------- | ----------------------------------------------------- |
| `pnpm --dir console verify` | Formatting, Svelte/type checks, ESLint, and Vitest    |
| `pnpm --dir console test`   | Unit and component tests                              |
| `pnpm --dir console build`  | Static assets and asset manifest                      |
| `make api`                  | Generate management OpenAPI and the TypeScript client |
| `make check`                | Required Rust and console checks                      |
| `make integration`          | Service suites and Chromium journeys                  |

Management requests use the generated `openapi-fetch` client. Update the Rust
handler annotations and route registration, then run `make api`.
`openapi/management.json` and `src/lib/api/schema.d.ts` are ignored outputs;
do not hand-edit them.

## Feature ownership

`src/lib/features/` groups provider, route, access, settings, inference,
runtime, usage, and media workflows. Keep route components thin and shared UI
in `$lib/components/`; see the [architecture map](../docs/architecture.md).
Keep the application client-only: do not add server routes, server hooks,
`lib/server/`, or a production Node adapter.

## Testing

Vitest runs under `TZ=America/New_York` to exercise local-time formatting and
daylight-saving behavior. Component tests use jsdom and browser exports.

`make integration` provisions disposable services and runs Chromium journeys
against both the packaged Rust origin and the Vite development origin,
including replacement restore. The suite covers setup, provider activation,
routing, inference, history, OIDC, and edit conflicts. It supplies the database,
Valkey, and file-backed secrets required by `pnpm --dir console test:e2e`.
See [tests/README.md](../tests/README.md) for focused suites.
