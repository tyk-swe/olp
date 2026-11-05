# Contributing

Install Go 1.26.0 or newer, Node.js 22.22.2 or newer, and pnpm 10.26.0 or newer.
Use the current stable Go and pnpm releases and the latest Node.js LTS release
(selected by `.nvmrc`) for development. pnpm 10.26.0 is needed for the workspace's
build-script permissions. Building and checking Go also needs a C compiler/linker
and glibc development headers. Running the development stack needs Docker Compose
and Python 3. Operational scripts need additional tools such as PostgreSQL 18
clients, OpenSSL, curl, jq, bubblewrap and ripgrep. Run commands from the
repository root. GLIDE links a pinned prebuilt native core, so development needs
CGO but no Rust toolchain.
Release platforms are native Linux amd64 and arm64. macOS and musl are
unqualified.

| Command | Purpose |
| --- | --- |
| `make setup` | Install modules, the pnpm workspace and a local Air watcher, then generate contracts |
| `make dev` | Start services, automatically reloaded Go on 8082/private 9092, and Vite on 5173 |
| `make check` | Check contract freshness, Go, console, and the safety net without rewriting tracked files |
| `make check-go` | Check Go formatting, vet, and pinned static analysis |
| `make check-console` | Generate ignored console types, then check formatting, types, and ESLint |
| `make check-contracts` | Compare all tracked contracts with temporary generated outputs |
| `make test-fast` | Fast safety net inside its 60 second budget, in [tests/README.md](tests/README.md) |
| `make test-service` | Database-backed service suite in disposable Docker containers, in [tests/README.md](tests/README.md#service-suite) |
| `make api` | Generate Go and TypeScript types without building the gateway |
| `make build` | Versioned `.local/bin/olp` plus separate `console/build` assets |
| `make build-go` | Build the native Go binary from generated contracts |
| `make catalog` | Rewrite the reference catalog and plugin index in canonical form |
| `make catalog-sign` | Sign the reference catalog and plugin index with the development key |
| `make fmt` | Format Go and console source |
| `make dev-stop` | Stop development containers and preserve data |
| `make dev-reset` | Delete development containers and volumes, preserving local secrets |

`make help` groups everyday commands, focused checks, and maintenance commands.
Run `make setup` after cloning or changing dependencies or the watcher version.
`make dev` and `make build` consume prepared contracts without reinstalling
dependencies or regenerating tracked files. After editing an API schema or
generator, run `make api` explicitly.

Required CI includes `check-go`, `check-console`, `test-fast`, `test-service`,
`contracts`, `helm`, `dependencies`, and `build`,
each gated on the paths a change touches. Helm, dependency and release checks
remain separate from the daily `make check`. Contract generation is reproducible.
`management/management.json` is the checked-in source of truth. Update it
alongside handlers and regenerate with
`make api`. Never edit generated Go or TypeScript files by hand.

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
[architecture map](docs/develop/architecture.md) identifies backend and console owners;
[testing](tests/README.md) covers the safety net, its budget and admission rule.

Use `gofmt`, `go vet`, and `make lint` for Go; console source uses Prettier, two spaces,
single quotes, no trailing commas, `camelCase` helpers and `PascalCase.svelte` components. Keep
Svelte route components thin and use the [console design system](console/README.md#design-system).

Place allowlisted Go tests beside their feature as `*_test.go`, allowlisted
console tests as Vitest `*.test.ts` in the `unit` project, and reactive
Svelte tests as `*.svelte.test.ts` in the `svelte` project.
Cover changed behavior inside the allowlist and preserve protocol fixtures. Tests follow the strict
rules in [testing](tests/README.md#test-rules), meaning behavior assertions
only, no pinned outside facts without a cited source and date, and one focused
regression test per bug fix in the existing mapped test file.
Service and browser behavior has no automated suite. Validate it by running
the affected functionality directly and record the command and output.

Documentation changes also run `node scripts/check-docs.mjs`. The same scoped
link, anchor and handbook-index contract runs through `make test-fast`.

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
`scripts/check-dependencies.sh` checks reachable Go
vulnerabilities, licenses and production JavaScript packages. Image qualification
also scans runtime/build SBOMs and retains native linking/license evidence.

GLIDE's pinned native archive needs CGO, not Rust. The embedded OpenAI token ranks
carry their MIT notice and SHA-256 checks; oracle fixtures use test-only tiktoken
under `tests/fixtures/tokens`.

TypeScript stays on the newest 6.0 patch until both
[typescript-eslint](https://typescript-eslint.io/users/dependency-versions/) and
Svelte support 7.x and the console passes its checks.
Other version exceptions need a concrete incompatibility and removal condition.

## Releases

Root `package.json` owns the version. Console/SDK package versions, chart
metadata, Docker/Compose tags, OpenAPI `info.version` and release tags must agree;
`node scripts/check-release-version.mjs` enforces this. The Makefile injects the
version into the binary. Build Linux amd64 and arm64 images natively; static
console assets ship separately in the nonroot distroless image.

Platform digests form one candidate index. Packaged browser, SDK and recovery
qualification consumes that exact index; promotion tags and attests it without
rebuilding. A manual workflow dispatch qualifies without publishing. A `v0.*`
tag push can publish `:<version>` and `:<major>.<minor>` only after all gates pass.

Provider/protocol/media reviews must cover native conformance, refused or lossy
semantics, body/time/admission limits, pricing and missing usage, cancellation,
privacy, SDK pins and recovery/credential-reference effects. Publish the generated
management OpenAPI contract and record deliberate 0.x breaking changes in the
[changelog](CHANGELOG.md). Weekly scans retain digest-specific released-image
reports; triage through the [security policy](SECURITY.md).
