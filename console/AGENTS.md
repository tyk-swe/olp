# console/ — SvelteKit SPA guide

Client-only SvelteKit app compiled to static assets (`adapter-static`,
`ssr = false`) and served by the Rust binary at `/`. Never add
`+page.server.*`, `+server.*`, server hooks, or `lib/server/` — CI's
boundary check rejects them.

## Commands (`pnpm --dir console …` or from this directory)

| Command | Purpose |
|---|---|
| `pnpm install --frozen-lockfile` | install (pnpm 11, Node ≥ 24) |
| `pnpm dev` | Vite dev server — **no API proxy**: there is no `server.proxy` in `vite.config.ts`, so `/api/v1` calls fail. For full-stack work, build (`pnpm build`) and run the Rust binary (`cargo run -p olp -- all …`), which serves the built console. |
| `pnpm verify` | the CI gate: `api:check` + scoped formatter check + vitest + svelte-check/eslint + build |
| `pnpm format` / `pnpm format:check` | format or verify the incrementally adopted Svelte-compatible formatter scope |
| `pnpm test:e2e` / `test:integration` / `test:storybook` | Playwright suites (integration needs `OLP_CONSOLE_E2E_*` env, see README) |
| `pnpm api:generate` | regenerate `src/lib/api/schema.d.ts` from `../openapi/management.json` — never hand-edit that file |

## Layout

- `src/lib/features/{access,gateway,operations,overview,playground,settings,setup}` —
  feature modules; route files under `src/routes/(console)/` are thin shims
  that re-export a feature page (this is the intended pattern — SvelteKit
  requires the `+page.svelte` files).
- `src/lib/api/` — generated `schema.d.ts` + typed fetch wrappers.
- `*.stories.ts` colocate with components; Storybook's glob picks up
  anything under `src/`.
- Four Playwright configs at the root cover e2e / integration /
  screenshots / storybook — they are distinct suites, not duplicates.
