# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. Start with the
[repository prerequisites and setup](../CONTRIBUTING.md).

## Local development

The [development workflow](../CONTRIBUTING.md#local-development) starts Vite
with `make dev`. It proxies `/api/`, `/v1/`, `/native/`, `/code/`, `/anthropic/`,
`/gemini/`, `/v1beta/`, and `/bedrock/` to Go on port 8082, preserving the
browser origin and cookies. WebSocket proxying is enabled for `/code/`.
Console edits hot reload and backend edits trigger a Go rebuild and restart.
`make dev` requires port 5173 so the browser origin stays consistent.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run from the repository root after setup:

| Command                    | Purpose                                                       |
| -------------------------- | ------------------------------------------------------------- |
| `make check-console`       | Generate types and check formatting, Svelte types, and ESLint |
| `pnpm --dir console build` | Static assets and asset manifest                              |

Management requests use the generated `openapi-fetch` client. Update
`management/management.json` alongside Go handlers and run `make api`; never edit
`src/lib/management/schema.d.ts` by hand. See [Contributing](../CONTRIBUTING.md) for
repository-wide commands.

## API wiring

- `apiClient` is imported only by feature api modules (plus `#lib/management/**` and
  tests). A slice with one API module uses `api.ts`; a slice with several uses
  an `api/` folder with one file per domain (`api/<domain>.ts`). Api modules
  hold the API-call functions and the schema-derived types they return; pure
  helpers stay in the slice's domain modules.
- Every call is resolved with `unwrap(fetched)`, `unwrapPage(fetched)` or
  `ensureOk(fetched)` from `#lib/management/http.js`.
- Every query key comes from the slice's `*Keys.ts` module.
- The browser reaches the inference gateway only through `gatewayFetch` in
  `#lib/management/gateway.ts`, which omits the session cookie and refuses redirects.
- ESLint enforces the `apiClient` and `fetch` restrictions.

## Feature ownership

`src/lib/features/` owns product workflows; shared UI lives in
`src/lib/components/` and pages in `src/routes/`. Keep route components thin.
The application is client-only: use the static adapter and avoid server routes,
server hooks or `lib/server/`. See the [architecture map](../docs/develop/architecture.md).

## Design system

The console is dark-only: black canvas, carbon surfaces and electric blue as the
brand accent. Use semantic tokens from `src/app.css`, never raw colors;
`.card-light` remaps tokens for the paper hero card. Blue means active/primary,
green success, amber warning and red danger. Text must meet WCAG AA.
No browser suite enforces that today, so check contrast when changing color.

Use CSS motion on `transform` and `opacity`, with shared keyframes and
`--ease-out`/`--dur-*` tokens. Disable hover/entrance effects under
`prefers-reduced-motion`.
Keep text readable on hover, preserve its wording and avoid gradients behind copy.

## Testing

See the [testing guide](../tests/README.md) for the console unit project
and focused runs.
