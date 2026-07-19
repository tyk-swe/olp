# OpenLLMProxy console

The console is a client-only SvelteKit application served by OpenLLMProxy. Its
static build is written to `build/` with `index.html` as the SPA fallback.

## Requirements

- Node.js 24 or newer
- pnpm 11

## Getting started

From this directory:

```sh
pnpm install --frozen-lockfile
pnpm dev
```

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
`pnpm api:check` verifies that the checked-in TypeScript schema is current.

## Integration tests

`pnpm test:integration` exercises the production build through the Rust
server. It requires `OLP_CONSOLE_E2E_DATABASE_URL` and
`OLP_CONSOLE_E2E_MASTER_KEY_FILE`.

## Documentation screenshots

The screenshots published in the root [README](../README.md) and under
[`../docs/`](../docs/) are captured by `tests/screenshots/` against mocked API
responses — no backend is required. Run `pnpm screenshots` and commit the
updated PNGs whenever the UI changes visibly.
