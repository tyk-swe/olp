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

Immediate directories under `src/lib/features/` are isolated feature areas;
their immediate child directories are isolated slices. ESLint discovers both
levels automatically. Areas cannot import other areas. Child slices cannot
import siblings through aliases or relative parent paths, but may use
area-level shared files; area-level files may compose child slices. Put general
shared utilities outside `features/`.

- `src/lib/features/gateway/{providers,routes,models}` — provider lifecycle,
  stable route drafts/revisions, and global model inventory.
- `src/lib/features/access/{api-keys,users,invitations,sessions,oidc}` — each
  access workflow owns its queries, mutations, pagination, and write-only
  secret state. `AccessPage.svelte` only composes tabs.
- `src/lib/features/operations/{requests,usage,media-jobs,audit,health}` and
  `settings/{installation,profile}` — resource-oriented operations/settings
  slices. Profile security and API-key secret workflows use focused components.
- Route files under `src/routes/(console)/` remain thin shims; SvelteKit still
  requires the `+page.svelte` files.
- `src/lib/api/` — generated `schema.d.ts` + typed fetch wrappers.
- `*.stories.ts` colocate with components; Storybook's glob picks up
  anything under `src/`.
- Four Playwright configs at the root cover e2e / integration /
  screenshots / storybook — they are distinct suites, not duplicates.
- Browser contracts are split by product and workflow under `tests/e2e/`;
  provider wizard, endpoint, probe, model, activation, and inventory behavior
  has its own spec. Shared provider record construction lives in
  `gateway-access-fixtures.ts` only.
