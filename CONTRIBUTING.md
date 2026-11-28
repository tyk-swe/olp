# Contributing

Install Go 1.26.0 or newer, Node.js 22.22.2 or newer, and pnpm 10.26.0 or newer.
Use the current stable Go and pnpm releases and the latest Node.js LTS release
(selected by `.nvmrc`) for development. pnpm 10.26.0 is needed for the workspace's
build-script permissions. Building and checking Go also needs a C compiler and
linker plus glibc development headers. Running the development stack needs Docker
Compose and Python 3. Dependency, end-to-end and image scripts use `jq` and
`curl`, operational scripts need additional tools such as PostgreSQL 18 clients
and OpenSSL, and the code-mode fixture qualification lanes (`make qualify-code`)
need `bubblewrap`. Run commands from the repository root. Release platforms are
native Linux amd64 and arm64. macOS and musl are unqualified.

| Command | Purpose |
| --- | --- |
| `make setup` | Install modules, the pnpm workspace and a local Air watcher, then generate contracts |
| `make dev` | Start services, automatically reloaded Go on 8082/private 9092, and Vite on 5173 |
| `make check` | Check contract freshness, Go, console, and the unit tier without rewriting tracked files |
| `make check-go` | Check migration numbers, Go formatting, vet, and pinned static analysis |
| `make check-console` | Generate ignored console types, then check formatting, types, and ESLint |
| `make check-contracts` | Compare all tracked contracts with temporary generated outputs |
| `make test` | Every test tier with the cheapest first, in [tests/README.md](tests/README.md) |
| `make test-unit` | Go and console unit tests, which need no services, in [tests/README.md](tests/README.md#unit-tier) |
| `make test-integration` | Tests against PostgreSQL and Valkey in disposable Docker containers, in [tests/README.md](tests/README.md#integration-tier) |
| `make test-e2e` | API journeys against the real binary and browser journeys against the built console, in [tests/README.md](tests/README.md#end-to-end-tier) |
| `make qualify-code` | Code mode fixture qualification lanes (release gate) in disposable Docker containers, in [tests/README.md](tests/README.md#qualification-lanes) |
| `make qualify-code-live` | Code mode live qualification lanes against real vendor accounts (release gate when `OLP_LIVE_QUALIFICATION=required`) in disposable Docker containers, in [tests/README.md](tests/README.md#qualification-lanes) |
| `make api` | Generate Go and TypeScript types without building the gateway |
| `make build` | Versioned `.local/bin/olp` plus separate `console/build` assets |
| `make build-go` | Build the native Go binary from generated contracts |
| `make catalog` | Rewrite the reference catalog and plugin index in canonical form |
| `make catalog-sign` | Sign the reference catalog and plugin index with the development key |
| `make fmt` | Format Go and console source |
| `make dev-stop` | Stop development containers and preserve data |
| `make dev-reset` | Delete development containers and volumes, preserving local secrets |

`make help` groups targets into four sections, covering Everyday commands,
Focused checks, Tests, and Services and maintenance. Run `make setup` after
cloning or changing dependencies or the watcher version. `make dev` and
`make build` consume prepared contracts without reinstalling dependencies or
regenerating tracked files. After editing an API schema or generator, run
`make api` explicitly.

Required CI includes `check-go`, `check-console`, `test-unit-go`,
`test-unit-console`, `test-integration`, `test-e2e-api`, `test-e2e-browser`,
`contracts`, `helm`, `workflows`, `dependencies`, and `build`,
each gated on the paths a change touches. Helm, dependency and release checks
remain separate from the daily `make check`. Contract generation is reproducible.
`management/management.json` is the checked-in source of truth. Update it
alongside handlers and regenerate with `make api`. Never edit generated Go or
TypeScript files by hand.

## Local development

After `make setup` and `make dev`, open http://127.0.0.1:5173 and use
`.local/secrets/bootstrap.token` to create the first owner. Use this origin
so management requests pass the origin check. Vite proxies management, OIDC
callbacks, and the configured inference paths. Console edits hot reload.
Air watches backend source, embedded assets and build metadata, rebuilds Go,
stops the old backend, applies pending migrations, and starts the new binary.
See the [console guide](console/README.md) for proxy limits and running Vite alone.

The development backend has a two-second shutdown deadline, so reloading can
interrupt active requests. A compilation failure stops the old backend and
prints the error while the watcher and console remain running. Fix the source
to rebuild and resume serving. A migration failure leaves the backend stopped.
Fix the migration and save it to retry. Applied migrations remain immutable.
Number a new migration above the highest one on `origin/main`, or `make check-go`
fails. Set `MIGRATIONS_BASE` to compare against another ref.

`make dev` creates missing private secrets and initializes the schema. Existing
secrets and data survive restarts. One development stack runs at a time. Vite
requires port 5173 and fails if it is occupied. PostgreSQL, Valkey and response
cache Valkey use loopback ports 54321, 63791 and 63792. Ctrl-C stops the watcher,
backend and Vite while leaving containers running for the next start. Stop
`make dev` before stopping the containers with the following command.

```sh
make dev-stop
```

Development storage uses schema `olp` and Valkey namespace
`olp:<installation UUID>:`. OLP makes no upgrade promises during 0.x, so a change
may require a fresh database. Stop `make dev`, then remove development volumes
and start again. This deletes local database and Valkey data but retains
`.local/secrets`.

```sh
make dev-reset
make dev
```

## Tests and changes

Keep feature types, validation, SQL, handlers and workflows together. The
[architecture map](docs/develop/architecture.md) identifies backend and console
owners, and [testing](tests/README.md) covers the test tiers, their budgets and
the placement rules.

Use `gofmt`, `go vet`, and `make lint` for Go. Console source uses Prettier, two
spaces, single quotes, no trailing commas, `camelCase` helpers and
`PascalCase.svelte` components. Keep Svelte route components thin and use the
[console design system](console/README.md#design-system).

Place Go unit tests beside their feature as `*_test.go`, console unit tests
as Vitest `*.test.ts` in the `unit` project, and reactive Svelte tests as
`*.svelte.test.ts` in the `svelte` project. Tests that need PostgreSQL or
Valkey go in `tests/integration`, journeys against the real binary go in
`tests/e2e`, and browser journeys go in `console/tests/e2e`. Put a test in
the cheapest tier that can catch the failure.
Cover changed behavior and preserve protocol fixtures. Tests follow the strict
rules in [testing](tests/README.md#test-rules), meaning behavior assertions
only, no pinned outside facts without a cited source and date, and one focused
regression test per bug fix in the existing mapped test file.
The end-to-end tier holds one journey per flow, so most service and browser
behavior has no journey of its own. Validate it by running the affected
functionality directly and record the command and output.

Documentation changes are validated by `make check-docs` (running
`node scripts/check-docs.mjs`), and the same scoped link, anchor and
handbook-index contract runs inside `make test-unit-console`.

Use conventional commit subjects and name agent-created branches `tyk-{name}`.
Follow the [PR template](.github/PULL_REQUEST_TEMPLATE.md) and describe the problem,
the resulting behavior and the validation performed. Include screenshots for
visible console changes. Regenerate affected contracts with `make api`.
Migrations are forward-only and sequential, and Helm values, schema and templates
change together.

During 0.x, target the current storage, configuration and management API formats.
Compatibility with provider protocols and official OpenAI, Anthropic and Gemini
SDKs remains part of the product contract.

## Dependency policy

Use current stable dependencies and update Go and pnpm through their package
managers. Commit the corresponding lockfiles with each update.
`scripts/check-dependencies.sh` verifies that `deploy/Dockerfile` matches the
Go toolchain in `go.mod`, the Node.js release in `.nvmrc`, and `packageManager`
in root `package.json`, then checks reachable Go vulnerabilities, licenses and
production JavaScript packages. A reachable advisory that was reviewed and does
not apply is accepted there by pinning its ID and `modified` time, so a revised
record fails the check again. Image qualification also scans runtime and build
SBOMs and retains native linking and license evidence.

The embedded OpenAI token ranks carry their MIT notice and SHA-256 checks,
while oracle fixtures use test-only tiktoken under `tests/fixtures/tokens`.

TypeScript stays on the newest 6.0 patch until both
[typescript-eslint](https://typescript-eslint.io/users/dependency-versions/) and
Svelte support 7.x and the console passes its checks.
Other version exceptions need a concrete incompatibility and removal condition.

## Releases

Root `package.json` owns the version. `node scripts/check-release-version.mjs`
enforces version parity across `console/package.json`, `deploy/helm/Chart.yaml`,
`deploy/Dockerfile`, `deploy/compose.yaml`, `management/management.json`, the
two versioned documentation pins in `docs/operate/deployment.md` and
`docs/use/plugins.md`, and the release tag. The Makefile injects the version
into the binary. Build Linux amd64 and arm64 images natively because static
console assets ship separately in the nonroot distroless image.

Platform digests form one candidate index. Packaged image mode, service,
compose and vulnerability qualification (`scripts/qualify-image.sh`) consumes
that exact index, and promotion tags and attests it without rebuilding. A manual
workflow dispatch qualifies without publishing. A `v0.*` tag push can publish
`:<version>` and `:<major>.<minor>` only after all gates pass.

Provider, protocol and media reviews must cover native conformance, refused or
lossy semantics, body, time and admission limits, pricing and missing usage,
cancellation, privacy, SDK pins and recovery or credential-reference effects.
Publish the generated management OpenAPI contract and record deliberate 0.x
breaking changes in the [changelog](CHANGELOG.md). Weekly scans retain
digest-specific released-image reports, and findings are triaged through the
[security policy](SECURITY.md).
