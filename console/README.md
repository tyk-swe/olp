# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. Start with the
[repository prerequisites and setup](../CONTRIBUTING.md).

## Local development

The [development workflow](../CONTRIBUTING.md#local-development) starts Vite
with `make dev`. It proxies `/api/`, `/v1/`, `/native/`, `/anthropic/`,
`/gemini/`, `/v1beta/`, and `/bedrock/` to Go on port 8082, preserving the
browser origin and cookies. The checked-in proxy does not enable WebSocket
proxying; use the Go listener directly for realtime clients.

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

## API wiring

- `apiClient` is imported only by feature api modules (plus `$lib/api/**` and
  tests). A slice with one API module uses `api.ts`; a slice with several uses
  an `api/` folder with one file per domain (`api/<domain>.ts`). Api modules
  hold the API-call functions and the schema-derived types they return; pure
  helpers stay in the slice's domain modules.
- Every call is resolved with `unwrap(fetched)`, `unwrapPage(fetched)` or
  `ensureOk(fetched)` from `$lib/api/http`.
- Every query key comes from the slice's `*Keys.ts` module.
- The browser reaches the inference gateway only through `gatewayFetch` in
  `$lib/api/gateway.ts`, which omits the session cookie and refuses redirects.
- ESLint enforces the `apiClient` and `fetch` restrictions.

## Feature ownership

`src/lib/features/` groups product workflows. Keep route components thin and
shared UI in `$lib/components/`; see the [architecture map](../docs/architecture.md).
Keep the application client-only: do not add server routes, server hooks,
`lib/server/`, or a production Node adapter.

## Testing

See the [testing guide](../tests/README.md) for Vitest configuration, focused
runs, and Chromium service setup.
