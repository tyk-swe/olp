# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. Start with the
[repository prerequisites and setup](../CONTRIBUTING.md).

## Local development

The [development workflow](../CONTRIBUTING.md#local-development) starts Vite
with `make dev`. It proxies `/api/`, `/v1/`, `/anthropic/`, `/gemini/`, and
`/v1beta/` to Go on port 8082, preserving the browser origin and cookies.
The checked-in proxy does not include `/bedrock/` or enable WebSocket proxying;
use the Go listener directly for Bedrock and realtime clients.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run from the repository root after setup:

| Command                     | Purpose                                            |
| --------------------------- | -------------------------------------------------- |
| `pnpm --dir console verify` | Formatting, Svelte/type checks, ESLint, and Vitest |
| `pnpm --dir console build`  | Static assets and asset manifest                   |

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

See the [testing guide](../tests/README.md) for Vitest configuration, focused
runs, and Chromium service setup.
