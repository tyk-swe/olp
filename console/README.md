# OpenLLMProxy console

The console is a client-only SvelteKit application served as static assets by
OpenLLMProxy. Its build output is `build/` with `index.html` as the SPA
fallback. Use Node.js 24+ and pnpm 11; repository-wide setup is in
[`../CONTRIBUTING.md`](../CONTRIBUTING.md).

## Local development

```sh
pnpm install --frozen-lockfile
pnpm dev
```

`pnpm dev` serves only the UI. There is no API proxy in `vite.config.ts`, so
API-backed pages remain empty/error states unless a Rust server serves the
production build. For full-stack work, run `pnpm build` and `cargo run -p olp
-- all` with `OLP_CONSOLE_DIR=console/build`, or use the root Compose quick
start.

## Commands

| Command | Purpose |
|---|---|
| `pnpm verify` | API drift, tests, lint, types, and build |
| `pnpm test:e2e` | Chromium, Firefox, WebKit, and mobile browser tests |
| `pnpm test:storybook` | Component interaction and accessibility |
| `pnpm test:integration` | Production build through the Rust server |
| `pnpm screenshots` | Regenerate `../docs/assets/screenshots/` |

Management requests use the generated `openapi-fetch` client. After changing
`../openapi/management.json`, run `pnpm api:generate`; `pnpm api:check` fails
when the checked-in TypeScript schema is stale.

## Integration environment

`pnpm test:integration` starts a complete Rust control/gateway/worker process,
uses a loopback Azure OpenAI provider, and checks OpenAI, Anthropic, Gemini,
and persisted telemetry. Provide a disposable PostgreSQL URL in
`OLP_CONSOLE_E2E_DATABASE_URL`, an isolated `OLP_VALKEY_URL`, and file-backed
`OLP_CONSOLE_E2E_MASTER_KEY_FILE`, `OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE`, and
`OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE`. Set `OLP_CONSOLE_E2E_BIN` to a debug
`olp` built with `--features test-util` to skip the harness build. The harness
refuses a Valkey database containing existing keys because stale stream events
would invalidate telemetry assertions.

## Screenshots

`pnpm screenshots` captures mocked API responses; no backend is required.
Commit regenerated PNGs when the UI changes visibly. The root
[`README.md`](../README.md) is the index for product screenshots and links.
