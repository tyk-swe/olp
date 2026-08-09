# `console` SvelteKit guide

The console is a client-only SvelteKit SPA (`adapter-static`, `ssr = false`)
served by the Rust binary at `/`. Never add `+page.server.*`, `+server.*`,
server hooks, or `lib/server/`; the boundary checker rejects them.

## Commands

Run from the repository root with `pnpm --dir console …`, or from this
directory:

| Command | Purpose |
|---|---|
| `pnpm install --frozen-lockfile` | Node 24+/pnpm 11 dependencies |
| `pnpm dev` | UI-only Vite server; no `/api/v1` proxy |
| `pnpm verify` | API drift, formatter scope, Vitest, type/lint, and build |
| `pnpm test:e2e`, `test:integration`, `test:storybook` | Browser, full-stack, and a11y suites |
| `pnpm api:generate` | Regenerate `src/lib/api/schema.d.ts` from OpenAPI |

`pnpm dev` cannot exercise API-backed pages. Build the console and run the
Rust binary for full-stack work. Never hand-edit the generated schema.

## Layout and boundaries

`src/lib/features/` contains independent slices for gateway, access, and
operations; ESLint rejects cross-slice imports. Put shared code in a neutral
`$lib` module and keep `src/routes/(console)/` files thin. API wrappers and
the generated schema live in `src/lib/api/`. Stories colocate with components.

Playwright configs cover e2e, integration, screenshots, and Storybook as
separate suites. Browser contracts are split by product/workflow; shared
provider record construction belongs in `gateway-access-fixtures.ts`.
Integration requires the `OLP_CONSOLE_E2E_*` files described in
[`README.md`](README.md).
