# OpenLLMProxy console

The console is a client-only SvelteKit application. The backend serves its
static `build/` output with `index.html` as the SPA fallback. Start with the
[repository prerequisites and setup](../CONTRIBUTING.md).

## Local development

The [development workflow](../CONTRIBUTING.md#local-development) starts Vite
with `make dev`. It proxies `/api/`, `/v1/`, `/native/`, `/code/`, `/anthropic/`,
`/gemini/`, `/v1beta/`, and `/bedrock/` to the Go backend, preserving the
browser origin and cookies. WebSocket proxying is enabled for `/code/`.
Console edits hot reload and backend edits trigger a Go rebuild and restart.
Each checkout keeps its own fixed ports, so the browser origin stays
consistent, and `make dev` fails if one of them is occupied.

For an already running backend, `pnpm --dir console dev` starts Vite alone.
Set `OLP_DEV_API_ORIGIN` to override its default `http://127.0.0.1:8081` target.

## Commands

Run these commands from the repository root after setup.

| Command                    | Purpose                                                       |
| -------------------------- | ------------------------------------------------------------- |
| `make check-console`       | Generate types and check formatting, Svelte types, and ESLint |
| `make test-unit-console`   | Run Vitest `unit` and `svelte` projects plus script tests     |
| `make test-e2e-browser`    | Run Playwright browser journeys against a built console       |
| `pnpm --dir console build` | Static assets                                                 |

Management requests use the generated `openapi-fetch` client and route
authorization metadata. Update the fragments in `management/openapi/` alongside
Go handlers and run `make api`. Never edit `src/lib/management/schema.d.ts` or
`src/lib/management/requirements.ts` by hand. See
[Contributing](../CONTRIBUTING.md) for repository-wide commands.

## API wiring

`apiClient` lives in `#lib/session/client.ts`, which gives the management
transport its session authentication, and is imported only by API modules and
tests. Every file named `api.ts` under `src/lib` is an API module, and a new
one takes that name. The branding module in `shell/` and the modules that some
slices split by resource have other names, so `eslint.config.mjs` lists them
one by one. API modules hold the API-call functions and the schema-derived types
they return, while pure helpers stay in other modules of the same folder.

Every call is resolved with `unwrap(fetched)`, `unwrapPage(fetched)` or
`ensureOk(fetched)` from `#lib/management/http.js`, and every query key comes
from a `*Keys.ts` module in the slice or in the [domain layer](#domain-layer).
The browser reaches the inference gateway only through `gatewayFetch` in
`#lib/management/gateway.ts`, which omits the session cookie and refuses
redirects. Writes build their `Idempotency-Key` and `If-Match` headers with
`ifMatch`, `mutationHeaders` or `createHeaders` from `#lib/management/http.js`.
ESLint enforces the `apiClient`, `fetch` and header restrictions, and the
`apiClient` rule lives in `eslint/feature-boundary.mjs` so it holds for any
spelling of the path.

## Feature ownership

`src/lib/features/` owns product workflows, shared UI lives in
`src/lib/components/`, the application frame lives in `src/lib/shell/` and
pages live in `src/routes/`. Keep route components thin. The application is
client-only, so use the static adapter and avoid server routes, server hooks or
`lib/server/`. See the [architecture map](../docs/develop/architecture.md).

A feature is one management resource family and the pages that read and edit
it, so a feature can own several top-level routes. The folders under
`src/lib/features/` and the routes they render are listed below.

| Feature         | Routes                                                                                                          | Owns                                                                                                                                                                                             |
| --------------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `access`        | `/organizations`, `/projects`, `/keys`, `/audit`                                                                | Organizations, projects with their members and policies (limit templates, attribution budgets, catalog publication), API keys, attribution, end users, route groups and the audit log.           |
| `agents`        | `/agents`, `/mcp-servers`, `/activity`, `/investigations`, `/budgets/exposure`                                  | Agent publications, grants and delegations, MCP servers, runs, tasks, tool calls, investigations and provisional exposure.                                                                       |
| `budgets`       | `/budgets`                                                                                                      | The installation budget, budget groups, budget increases, aggregate budgets and the limit fields other features embed.                                                                           |
| `code-mode`     | `/code-mode`                                                                                                    | Code mode accounts, pools, routes, bindings, budgets and diagnostics.                                                                                                                            |
| `exports`       | `/exports`                                                                                                      | Capture and telemetry export settings at the root and signed export sinks in `signed/`.                                                                                                          |
| `guardrails`    | `/guardrails`, `/guardrails/preview`, `/guardrails/try`                                                         | Guardrail definitions, policies, credentials, attachments, pre-dispatch preview and the Try it evaluator.                                                                                        |
| `health`        | `/health`                                                                                                       | Readiness, provider health, gateway epochs and runtime generations.                                                                                                                              |
| `identity`      | `/login`, `/setup`, `/invitations/accept`, `/members`, `/members/invitations`, `/identity`, `/settings/profile` | Sign-in, setup, MFA, OIDC, SAML, single sign-on, SCIM, sessions, tokens, workload issuers, users, invitations and the signed-in profile.                                                         |
| `inference`     | `/playground`                                                                                                   | The playground.                                                                                                                                                                                  |
| `media`         | `/media-jobs`                                                                                                   | Media job metadata.                                                                                                                                                                              |
| `notifications` | `/notifications`                                                                                                | Notification rules, destinations and deliveries.                                                                                                                                                 |
| `overview`      | `/`                                                                                                             | The landing page and its summaries.                                                                                                                                                              |
| `plugins`       | `/plugins`                                                                                                      | The plugin index, installed plugins and the unconfined plugin and MCP executable reviews.                                                                                                        |
| `providers`     | `/providers`, `/models`, `/models/catalog`, `/catalog/public/[projectId]`                                       | Provider connections, credentials, grants, billing imports, commitments and models, with cross-provider model comparison in `provider-models/` and published model catalogs in `model-catalog/`. |
| `resources`     | `/provider-resources`                                                                                           | Retained provider resources and retrieval bindings.                                                                                                                                              |
| `routes`        | `/routes`, `/routes/templates`                                                                                  | Route drafts, route templates, activation, bulk creation from models, routing policy and inspection, with the route cache editor in `cache/`.                                                    |
| `settings`      | `/settings`, `/settings/routing`                                                                                | Installation settings, default routing and the branding write, with declarative configuration in `configuration/`.                                                                               |
| `usage`         | `/usage`, `/usage/routing`, `/usage/calculator`, `/requests`, `/pricing`                                        | Usage reports, routing savings, cost calculator, request records in `history/`, pricing, rate cards and reconciliation.                                                                          |

Two pages sit under another feature's route section. `agents` renders
`/budgets/exposure`, because the page lists agent exposure, and `identity`
renders `/settings/profile`, because the page edits the signed-in user. The
root layouts also mount the MFA confirmation host and read the setup status
from `identity`.

New code goes in the feature that owns the resource it reads or writes. A new
resource in an existing family gets its own subfolder in that feature, and a
new feature folder is for a resource family that no feature owns yet. A
component with one consumer lives next to that consumer, even when its name
suggests another feature. A read or a picker that two or more features need goes
into the [domain layer](#domain-layer). Other code that two features need stays
in the feature that owns its resource and is exported from that feature's
`index.ts`. A pure helper that belongs to no resource, such as `shortDigest` in
`src/lib/format.ts`, goes into a shared layer. The shell reads branding itself
because a shared layer cannot import a feature, which is why `settings` owns
only the branding write. An `index.ts` lists only what other features import,
so a feature that no other feature imports has none.

## Boundaries

ESLint enforces the structural rules below. `eslint.config.mjs` is their source.
The first five and the feature order resolve alias and relative imports through
`eslint/imports.mjs` in `import` and `export` statements, dynamic imports and
inline `import('...')` types, in both `.ts` and `.svelte` files.

- The shared layers (`src/lib/*.ts`, `management/`, `session/`, `editing/`,
  `listing/`, `components/`, `domain/` and `shell/`) never import from
  `src/lib/features/`. "Feature ownership" above says where code that two
  features need goes.
- A shared layer imports only itself and the layers below it. See "Shared
  layers" below.
- A feature imports another feature only through that feature's `index.ts`.
  Add the export there instead of importing one of its files.
- Nothing under `src/lib` imports from `src/routes`.
- Only a test file imports a `*.test.ts` file, a `test/` folder, `#test/*`, or
  `console/tests/`. Test files are exempt from these rules, so code that
  production needs moves out of them.
- Non-CSS local side-effect imports are forbidden by `console/side-effect-import`
  (`eslint/side-effect-import.mjs`) because `package.json` marks only `**/*.css`
  as having side effects, so the build drops bare JS or TS imports.
- Programmatic navigation in Svelte files must use `$app/paths` `resolve` under
  `svelte/no-navigation-without-resolve` (with `ignoreLinks: true`).
- A `.svelte` file holds at most 400 lines of code. Split a larger one into
  parts beside it.
- The `Idempotency-Key` and `If-Match` header names appear only in
  `src/lib/management/`.
- A `.svelte` file has no `try` block. See "Writes" below.
- Test files outside `tests/unit/support/eventually.ts` must use `eventually`
  from `#test/eventually.js` instead of `vi.waitFor` or `vi.waitUntil`, because
  Vitest's 50 ms polling interval dominates the console unit budget.
- A feature imports only the features listed before it. See "Feature order"
  below.
- A `.ts` file holds at most 500 lines of code. Test files and generated files
  are exempt.
- A function holds at most 100 lines, counting blank lines but not comments.
  Test files and generated files are exempt.
- A binding that is never reassigned is declared with `const`. In a `.svelte`
  file `$props()` and `$derived()` may stay `let`.

### Feature order

The `orderedFeatures` list in `eslint.config.mjs` orders the feature folders,
and `console/feature-order` (`eslint/feature-order.mjs`) lets a feature import
only the features listed before it, so a new import cannot close a cycle between
features. Type-only imports count. When the order forbids an import, move the
shared part into a shared layer or pass it in. To pass a component in, the page
under `src/routes/` imports it and hands it to the earlier feature as a prop, as
`src/routes/(console)/models/+page.svelte` does with `BulkRouteCreation`. A new
feature goes into the list after every feature it imports, and a feature can
move within the list when no new violation appears. Test files are exempt.
Imports between the subfolders of one feature are not checked.

### Suppressed violations

`eslint-suppressions.json` is ESLint's
[bulk suppressions](https://eslint.org/docs/latest/use/suppressions) file. It
records how many violations of a rule each file had when that rule arrived, so
the list can only shrink. `pnpm --dir console lint` fails when a file goes over
its count, which then reports every violation of that rule in the file. It also
fails when a count is higher than what the file still has, or when a listed file
no longer exists. After fixing a violation, or after deleting or renaming a
listed file, remove the stale entries and commit the result.

```sh
pnpm --dir console exec eslint . --prune-suppressions
```

Never add an entry or raise a count to get a change through. Fix the violation,
also when listed code moves to another file. The counts are per file and rule, so
they do not stop a listed file or function from growing further.

### Shared layers

The shared layers form one strict order, so no two of them import each other.
The list runs from the lowest layer to the highest, and a layer imports only
itself and the layers listed before it.

1. `src/lib/*.ts` holds pure helpers such as formatting and static data such as
   the request templates, and imports no other layer.
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
7. `domain/` holds the reads and pickers that features share, one folder per
   entity. See "Domain layer" below. It imports `components/` and below.
8. `shell/` is the application frame, which is the sidebar, navigation and
   section layout. It imports `domain/` and below.

Features and routes sit above all eight and import any of them. The order is
the `sharedLayers` list in `eslint.config.mjs`. A new folder under `src/lib`
ranks above `shell/` until it is added to that list, so no listed layer can
import it.

## Domain layer

`src/lib/domain/<entity>/` holds what two or more features need to read one
kind of resource. `domain/projects/` covers projects, project members and the
signed-in user's project memberships, and it is the example to copy.

| File          | Holds                                                                                       |
| ------------- | ------------------------------------------------------------------------------------------- |
| `api.ts`      | The read calls and the schema-derived types they return                                     |
| `*Keys.ts`    | The query keys, including the root a feature nests under                                    |
| `queries.ts`  | One `queryOptions` factory for each shared query                                            |
| Other modules | Pickers, the option sources that `EntityPicker` takes and selections over what reads return |

Writes stay in the feature that owns the resource, and so does a read that only
that feature runs. A feature imports domain files directly, so a domain folder
has no `index.ts`.

A factory decides the key, the call and the cache policy together, so two
screens cannot disagree about how fresh the same data is. Pass it to
`createQuery`, and spread it only to add what belongs to one screen, such as
`enabled` or `select`. A screen never overrides `staleTime`, `gcTime` or
`retry`. The one exception is a `fetchQuery` that decides what a write does and
so has to see what the server holds now, such as the live grant check in the
provider wizard, which passes `staleTime: 0`.

`projectQueries.memberships` is never served from cache, because memberships
decide what the signed-in user is offered. That makes a refresh happen often,
so a page that holds unsaved input under that query treats only
`isLoadingError` as a failed page and keeps the input when a later refresh
fails.

## Writes

A component runs every write through `useAction` or `useMutation` from
`src/lib/editing/action.svelte.ts` and never keeps its own busy flag, error
string or `try` block. Both give the component `busy`, `error` and
`fieldErrors` to render, plus `key` naming the write in flight (`''` when idle)
and `conflict` for a stale `If-Match`. They ignore a second submit while one is
running and drop a response that arrives after the signed-in user changed.

`useAction` runs `run(send, accept?, feedback?)` and returns `true` when the
write succeeded and still belongs to the mounted principal. `send` makes the one
API call, and `accept` does everything that follows a success, such as updating
the query cache, showing a toast and navigating. `feedback` supplies an
optional `key`, a `fallback` message, a `failure(cause, stage)` mapper for
`'send'` or `'accept'` failures, and the `serverFields` described under
[Edit forms](#edit-forms). Only a send-stage ETag mismatch sets
`conflict` and invokes the optional refresh callback. `useMutation` adds an
`Idempotency-Key` through `submit(input, send, accept?, feedback?)`, giving
`send` a request ID that stays the same when the user retries the same input so
a retry cannot create a second resource. When the submitted payload includes
generated values such as timestamps that must also stay fixed across retries of
the same logical form input, `submitBuilt(input, build, send, accept?, feedback?)`
runs `build` once per logical input and reuses both the built payload and the
request ID on retry until the input changes or the write commits.

An action is principal-scoped by default, so a principal change or unmount
retires it. Lifecycle-authorized sign-in and sign-out pass `'authentication'`
to stay alive through their own transition, ending only at unmount.
`dismiss()` clears the reported errors, and `reset()` also clears the conflict.
A mutation's `reset()` drops the saved retry intent as well, so use `dismiss()`
when the user may still retry the same input. Reads that can fail return the
problem as a value through `captureResult` or `readResult` in
`src/lib/editing/result.ts` rather than throwing through the component. Other
logic that can fail, such as parsing what the user typed, belongs in a `.ts`
module that returns the problem as a value, like `src/lib/editing/json.ts`.

### Edit forms

`src/lib/features/exports/signed/SinkEditor.svelte` and its `sinkDraft.ts` are
the example to copy for a form that creates and updates.

A form keeps what the user typed in one draft object. Its validator is a plain
function in a `.ts` module that returns a `Validation` from
`src/lib/editing/validation.ts`, which is either `{ ok: true, value }` with the
request body or `{ ok: false, fieldErrors }` keyed by the form's own field
names. `validated(fieldErrors, value)` builds one and calls `value` only when no
field has an error. `formProblem(message)` builds a refusal that belongs to no
single field.

The component names those fields in `useMutation<SinkField>()`, passes the
result to `action.check`, and writes `value` only when `check` returns `true`.
To report a refused request on the same inputs, pass `serverFields` in
`feedback`, a map from the API's field names to the form's. `action.error` is
the message for the whole form. It holds the `formProblem` message after a
failed check, and it is empty when every field the server named is in the map. A
form never assigns `action.error`.

`FormField.svelte` renders a labelled control with its error and hands the
control the `id` and ARIA attributes to spread. Set `novalidate` on the form so
the browser does not report a second set of errors, give the form-level message
`tabindex="-1"` and `data-error-summary`, and call `focusFormError(form)` after
a failed check or write. `hasChanges(saved, draft)` tells the unsaved changes
guard whether the draft differs from the saved record.

## Page building blocks

Pages are built from the shared components in `src/lib/components/` rather than
hand-rolled markup. A standalone page starts with `PageHeader`. A page with
several views is a route folder whose `+layout.svelte` renders `SectionLayout`,
which reads the title, description and tabs from the navigation tree in
`src/lib/shell/navigationTree/`, so each tab is its own URL. A query-backed
region goes through `QueryView`, which renders the loading status, a danger
`Callout` with the failure message and a retry button, an optional empty state,
or its children. Lists open create and edit forms in a `Dialog` with
`placement="side"`. Errors and empty states use `Callout` and `EmptyState`.
Rarely used fields go in `Advanced`. Destructive or hard-to-undo actions await
`confirmAction` from `confirm.ts`, and successful saves call `notify` from
`toasts.svelte.ts` instead of leaving a notice on the page. A new page also
needs an entry in the file for its area under `src/lib/shell/navigationTree/` so
it appears in the sidebar, breadcrumbs and command palette.

## Design system

The console is dark-only, using a near-black canvas, carbon surfaces and
electric blue as the default brand accent. An installation's configured brand
color dynamically replaces the `--color-accent-*` token scale at runtime. Use
semantic tokens from `src/app.css`, never raw colors, because `.card-light`
remaps those tokens for the paper hero card. Blue (or the active installation
brand accent) means active or primary, green means success, amber means warning
and red means danger. Text must meet WCAG AA contrast across every surface. Go
unit tests in `internal/settings/accent_test.go` check WCAG AA contrast for
every accepted installation brand color and its derived accent shades against
the surfaces in `src/app.css`, while no browser suite enforces contrast for
other style changes, so verify contrast whenever editing palette tokens or
component colors.

Use CSS motion on `transform` and `opacity`, with shared keyframes and
`--ease-out` and `--dur-*` tokens. Disable hover and entrance effects under
`prefers-reduced-motion`. Keep text readable on hover, preserve its wording and
avoid gradients behind copy.

## Testing

See the [testing guide](../tests/README.md) for the console `unit` and `svelte`
Vitest projects, Playwright browser journeys, and focused runs. Shared Vitest
helpers in `tests/unit/support/` are imported through the `#test/*` alias.
