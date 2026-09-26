# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. Start with the
[repository prerequisites and setup](../CONTRIBUTING.md).

## Local development

After `make setup`, run `make dev` from the repository root and open
http://127.0.0.1:5173. Use `.local/secrets/bootstrap.token` for first-owner
setup. Console edits hot reload; restart after backend changes.

Vite proxies `/api/`, `/v1/`, `/anthropic/`, `/gemini/`, and `/v1beta/` to Go
on port 8082 under `make dev`, preserving the browser origin and cookies.
The checked-in proxy does not include `/bedrock/` or enable WebSocket proxying;
use the Go listener directly for Bedrock and realtime clients.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run from the repository root after setup:

| Command                     | Purpose                                              |
| --------------------------- | ---------------------------------------------------- |
| `pnpm --dir console verify` | Formatting, Svelte/type checks, ESLint, and Vitest   |
| `make test-console`         | Console unit and component tests                     |
| `pnpm --dir console build`  | Static assets and asset manifest                     |
| `make api`                  | Generate Go and TypeScript management contract types |
| `make integration`          | Service suites and Chromium journeys                 |

Management requests use the generated `openapi-fetch` client. Update
`openapi/management.json` alongside Go handlers and run `make api`; never edit
`src/lib/api/schema.d.ts` by hand. See [Contributing](../CONTRIBUTING.md) for
repository-wide commands.

## Feature ownership

`src/lib/features/` groups product workflows. Keep route components thin and
shared UI in `$lib/components/`; see the [architecture map](../docs/architecture.md).
Keep the application client-only: do not add server routes, server hooks,
`lib/server/`, or a production Node adapter.

## Testing

Vitest uses `TZ=America/New_York` to exercise local-time and daylight-saving
behavior. Component tests use jsdom and browser exports. Tests synchronize
SvelteKit themselves; no prior build is needed. Focused `.only` tests fail
validation. Filter runs instead:

```sh
make test-console CONSOLE_TEST_ARGS='--project unit src/lib/format.test.ts'
```

`make integration` supplies disposable services and secrets for Chromium
journeys at both packaged and Vite origins, including replacement restore.
See [the testing guide](../tests/README.md) for suite selection and qualification
limits; standalone `pnpm --dir console test:e2e` requires that service setup.
