# OpenLLMProxy console

The console is a client-only SvelteKit application served by OpenLLMProxy.
Its static build is written to `build/` with `index.html` as the SPA
fallback. Requires Node.js 24 or newer and pnpm 11.

## Getting started

From this directory:

```sh
pnpm install --frozen-lockfile
pnpm dev
```

`pnpm dev` serves the UI only: there is no API proxy in `vite.config.ts`, so
`/api/v1` requests have nowhere to go and API-backed pages stay in their
error/empty states. For full-stack work, build the console (`pnpm build`) and
run the Rust binary, which serves the build output — for example the Compose
quick start in the root README, or `cargo run -p olp -- all` with
`OLP_CONSOLE_DIR` pointing at `console/build`.

## Commands

| Command | Purpose |
|---|---|
| `pnpm verify` | API drift, unit tests, linting, type checks, and build |
| `pnpm test:e2e` | Chromium, Firefox, WebKit, and mobile browser tests |
| `pnpm test:storybook` | Component interaction and accessibility tests |
| `pnpm test:integration` | Production build exercised through the Rust server |
| `pnpm screenshots` | Regenerate the documentation screenshots in `../docs/assets/screenshots/` |

## Management API client

Management requests go through the generated `openapi-fetch` client. After
changing `../openapi/management.json`, run `pnpm api:generate`;
`pnpm api:check` verifies the checked-in TypeScript schema is current.

## Integration tests

`pnpm test:integration` exercises the production console build through the
complete Rust control, gateway, and worker process. The browser configures a
loopback Azure OpenAI provider and proves OpenAI, Anthropic, and Gemini
inference plus persisted request telemetry. It requires a disposable PostgreSQL database in
`OLP_CONSOLE_E2E_DATABASE_URL`, an isolated Valkey instance or logical database
in `OLP_VALKEY_URL`, and
`OLP_CONSOLE_E2E_MASTER_KEY_FILE`, `OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE`, and
`OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE`. Set `OLP_CONSOLE_E2E_BIN` to a
prebuilt debug `olp` executable compiled with `--features test-util` to skip
the harness build. That test-only feature permits the provider mock's loopback
HTTP endpoint; release builds do not include the escape hatch. The harness
refuses to start if the selected Valkey database already contains keys, because
stale stream events would make telemetry assertions misleading.

## Documentation screenshots

The screenshots in the root [README](../README.md) and under
[`../docs/`](../docs/) are captured by `tests/screenshots/` against mocked
API responses — no backend required. Run `pnpm screenshots` and commit the
updated PNGs whenever the UI changes visibly.
