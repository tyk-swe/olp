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
| `pnpm --dir console build` | Static assets                                                 |

Management requests use the generated `openapi-fetch` client. Update
`management/management.json` alongside Go handlers and run `make api`; never edit
`src/lib/management/schema.d.ts` by hand. See [Contributing](../CONTRIBUTING.md) for
repository-wide commands.

## API wiring

- `apiClient` lives in `#lib/session/client.ts`, which gives the management
  transport its session authentication, and is imported only by feature api
  modules (plus the session and branding api modules and tests). A slice with
  one API module uses `api.ts`; a slice with several uses an `api/` folder with
  one file per domain (`api/<domain>.ts`). Api modules hold the API-call
  functions and the schema-derived types they return; pure helpers stay in the
  slice's domain modules.
- Every call is resolved with `unwrap(fetched)`, `unwrapPage(fetched)` or
  `ensureOk(fetched)` from `#lib/management/http.js`.
- Every query key comes from the slice's `*Keys.ts` module.
- The browser reaches the inference gateway only through `gatewayFetch` in
  `#lib/management/gateway.ts`, which omits the session cookie and refuses redirects.
- Writes build their `Idempotency-Key` and `If-Match` headers with `ifMatch`,
  `mutationHeaders` or `createHeaders` from `#lib/management/http.js`.
- ESLint enforces the `apiClient`, `fetch` and header restrictions. The
  `apiClient` one is part of `eslint/feature-boundary.mjs`, so it holds for any
  spelling of the path.

## Feature ownership

`src/lib/features/` owns product workflows; shared UI lives in
`src/lib/components/`, the application frame in `src/lib/shell/` and pages in
`src/routes/`. Keep route components thin.
The application is client-only: use the static adapter and avoid server routes,
server hooks or `lib/server/`. See the [architecture map](../docs/develop/architecture.md).

A feature is one route subtree plus the management resource family behind it.
The folders under `src/lib/features/` and the routes they render are:

| Feature         | Routes                                                                           | Owns                                                                                                                                         |
| --------------- | -------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `access`        | `/organizations`, `/projects`, `/keys`, `/audit`                                 | Organizations, projects and their members, API keys, attribution, end users, route groups and the audit log.                                 |
| `agents`        | `/agents`, `/mcp-servers`, `/activity`, `/investigations`                        | Agent publications, grants and delegations, MCP servers, runs, tasks, tool calls and investigations, plus the exposure tab under `/budgets`. |
| `budgets`       | `/budgets`                                                                       | The installation budget, budget groups, budget increases, limit templates, attribution budgets and the limit fields other features embed.    |
| `code-mode`     | `/code-mode`                                                                     | Code mode accounts, pools, routes, bindings, budgets and diagnostics.                                                                        |
| `exports`       | `/exports`                                                                       | Capture and telemetry export settings at the root and signed export sinks in `signed/`.                                                      |
| `guardrails`    | `/guardrails`                                                                    | Guardrail definitions, policies, credentials and attachments.                                                                                |
| `health`        | `/health`                                                                        | Readiness, provider health, gateway epochs and runtime generations.                                                                          |
| `identity`      | `/login`, `/setup`, `/invitations`, `/members`, `/identity`, `/settings/profile` | Sign-in, setup, MFA, OIDC, SAML, single sign-on, SCIM, sessions, tokens, workload issuers, users, invitations and the signed-in profile.     |
| `inference`     | `/playground`                                                                    | The playground and its request templates.                                                                                                    |
| `media`         | `/media-jobs`                                                                    | Media job metadata.                                                                                                                          |
| `notifications` | `/notifications`                                                                 | Notification rules, destinations and deliveries.                                                                                             |
| `overview`      | `/`                                                                              | The landing page and its summaries.                                                                                                          |
| `plugins`       | `/plugins`                                                                       | The plugin index, installed plugins and the unconfined plugin review.                                                                        |
| `providers`     | `/providers`, `/models`, `/catalog`                                              | Provider connections, credentials, grants and models, with the published model catalog in `model-catalog/`.                                  |
| `resources`     | `/provider-resources`                                                            | Retained provider resources and retrieval bindings.                                                                                          |
| `routes`        | `/routes`                                                                        | Route drafts, activation, routing policy and inspection, with the route cache editor in `cache/`.                                            |
| `settings`      | `/settings`                                                                      | Installation settings and the branding write, with declarative configuration in `configuration/`.                                            |
| `usage`         | `/usage`, `/requests`, `/pricing`                                                | Usage reports, request records, pricing, rate cards and reconciliation.                                                                      |

New code goes in the feature whose route subtree shows it. A new resource that
appears under an existing top-level route gets a subfolder in that feature, and
a new top-level route gets a new feature folder. A component with one consumer
lives next to that consumer, even when its name suggests another feature. Code
that two features need goes into the feature that owns the resource and is
exported from that feature's `index.ts`, or into a shared layer when it knows
about no resource at all. An `index.ts` lists only what other features import,
so a feature nothing imports has none.

## Boundaries

ESLint enforces eight structural rules. `eslint.config.mjs` is their source,
and `eslint/feature-boundary.mjs` resolves alias and relative imports for the
first five, in `import` and `export` statements, dynamic imports and inline
`import('...')` types:

- The shared layers (`src/lib/*.ts`, `management/`, `session/`, `editing/`,
  `listing/`, `components/` and `shell/`) never import from
  `src/lib/features/`. When two features need the same thing, move it into a
  shared layer.
- A shared layer imports only itself and the layers below it. See "Shared
  layers" below.
- A feature imports another feature only through that feature's `index.ts`.
  Add the export there instead of importing one of its files.
- Nothing under `src/lib` imports from `src/routes`.
- Only a test file imports a `*.test.ts` file, a `test/` folder or
  `console/tests/`. Test files are exempt from these rules, so code that
  production needs moves out of them.
- A `.svelte` file holds at most 400 lines of code. Split a larger one into
  parts beside it.
- The `Idempotency-Key` and `If-Match` header names appear only in
  `src/lib/management/`.
- A `.svelte` file has no `try` block. See "Writes" below.

Components that broke the size rule or the `try` rule before those rules
existed are listed in `eslint-legacy.json`, and the other six rules have no
exemptions. The lists only shrink: `scripts/check-exemptions.sh` fails
`make check-console` when a list has an entry the default branch does not. Fix
a new violation instead of listing it, and remove a file from its list in the
change that fixes it.

### Shared layers

The shared layers form one strict order, so no two of them import each other.
The list runs from the lowest layer to the highest, and a layer imports only
itself and the layers listed before it:

1. `src/lib/*.ts` holds pure helpers such as formatting and imports no other
   layer.
2. `management/` is the management API transport, its generated schema and its
   query and header helpers. It imports `src/lib/*.ts`.
3. `session/` is the signed-in principal, its permissions and the authenticated
   `apiClient`. It imports `management/` and below.
4. `editing/` is the write and concurrent-edit machinery. It imports `session/`
   and below.
5. `listing/` is the list filter and page state. It imports `editing/` and
   below.
6. `components/` is the generic UI that knows nothing about the application
   frame. It imports `listing/` and below.
7. `shell/` is the application frame, which is the sidebar, navigation and
   section layout. It imports `components/` and below.

Features and routes sit above all seven and import any of them. The order is
the `sharedLayers` list in `eslint.config.mjs`. A new folder under `src/lib`
ranks above `shell/` until it is added to that list, so no listed layer can
import it.

## Writes

A component runs every write through `useAction` or `useMutation` from
`src/lib/editing/action.svelte.ts` and never keeps its own busy flag, error
string or `try` block. Both give the component `busy` and `error` to render,
plus `key` naming the write in flight (`''` when idle) and `conflict` for a
stale `If-Match`. They ignore a second submit while one is running and drop a
response that arrives after the signed-in user changed.

`useAction` runs `run(send, accept?, feedback?)` and returns whether the write
still belonged to the mounted principal when it settled. `send` makes the one
API call; `accept` does everything that follows a success, such as updating
the query cache, showing a toast and navigating. `feedback` supplies an
optional `key`, a `fallback` message, and a `failure(cause, stage)` mapper for
`'send'` or `'accept'` failures. Only a send-stage ETag mismatch sets
`conflict` and invokes the optional refresh callback. `useMutation` adds an
`Idempotency-Key`: its `submit(input, send, accept?, feedback?)` gives `send`
a request ID that stays the same when the user retries the same input, so a
retry cannot create a second resource.

An action is principal-scoped by default: a principal change or unmount
retires it. Lifecycle-authorized sign-in and sign-out pass `'authentication'`
to stay alive through their own transition, ending only at unmount.
`reset()` clears the reported error and conflict; a mutation's `reset()`
also drops the saved retry intent. Reads that can fail return the problem as
a value through `captureResult`/`readResult` in `src/lib/editing/result.ts`
rather than throwing through the component.

`src/lib/features/exports/signed/SinkEditor.svelte` is the example to copy for a form
that creates and updates. Other logic that can fail, such as parsing what the
user typed, belongs in a `.ts` module that returns the problem as a value, like
`src/lib/editing/json.ts`.

## Page building blocks

Pages are built from the shared components in `src/lib/components/` rather than
hand-rolled markup. A standalone page starts with `PageHeader`. A page with
several views is a route folder whose `+layout.svelte` renders
`SectionLayout`, which reads the title, description and tabs from
`src/lib/shell/navigation.ts`, so each tab is its own URL. A query-backed
region goes through `QueryView`, which renders the loading status, a danger
`Callout` with the failure message and a retry button, an optional empty
state, or its children. Lists open create and
edit forms in a `Dialog` with `placement="side"`. Errors and empty
states use `Callout` and `EmptyState`. Rarely used fields go in
`Advanced`. Destructive or hard-to-undo actions await `confirmAction` from
`confirm.ts`, and successful saves call `notify` from `toasts.svelte.ts` instead
of leaving a notice on the page. A new page also needs an entry in
`navigation.ts` so it appears in the sidebar, breadcrumbs and command palette.

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
