# Contributing

Install Go 1.26.0 or newer, Node.js 22.22.2 or newer, and pnpm 10.26.0 or newer.
Use the current stable Go and pnpm releases and the latest Node.js LTS release
(selected by `.nvmrc`) for development. pnpm 10.26.0 is needed for the workspace's
build-script permissions. Also install a C compiler/linker and glibc development
headers, Docker Compose, PostgreSQL 18 client tools, OpenSSL, curl, jq, Python 3,
bubblewrap and ripgrep. Run commands from the repository root. GLIDE links a
pinned prebuilt native core, so development needs CGO but no Rust toolchain.
Release platforms are native Linux amd64 and arm64. macOS and musl are
unqualified.

| Command | Purpose |
| --- | --- |
| `make setup` | Download modules, install the pnpm workspace, generate contracts |
| `make dev` | Start PostgreSQL, Valkey, Go on 8082/private 9092, and Vite on 5173 |
| `make check` | Generate contracts; check formatting, vet, Go lint, ESLint, types, and local tests |
| `make lint` | Pinned Go static analysis gate (see `staticcheck.conf`) |
| `make test` | Go, benchmark harness, console unit/component, and script suites without containers |
| `make test-go` | Go unit and protocol suites |
| `make test-bench` | The benchmark harness's own tests, which need no services |
| `make test-console` | Console unit and component suites |
| `make test-scripts` | Automation script tests |
| `make test-race` | Uncached Go tests with race detection, also required in CI |
| `make test-ratchet` | Mechanical test rules on the test files a change touches |
| `make test-coverage-gate` | Coverage comparison for changed Go unit packages against the merge base |
| `make test-mutation-go` | Mutation gate on changed Go code against the merge base |
| `make test-mutation-console` | Mutation gate on changed console modules |
| `make integration` | Disposable services, race/process/recovery/SDK/Chromium suites |
| `make bench` | Gateway benchmark scenarios against a mock upstream, in [docs/develop/performance.md](docs/develop/performance.md) |
| `make bench-compare` | The same scenarios against OLP and the pinned LiteLLM release |
| `make bench-gate` | Hot-path `testing.B` benchmarks against the merge base, as the CI `bench` job runs them |
| `make api` | Generate Go and TypeScript types without building the gateway |
| `make build` | Versioned `.local/bin/olp` plus separate `console/build` assets |
| `make build-go` | Build the native Go binary from generated contracts |
| `make catalog` | Rewrite the reference catalog and plugin index in canonical form |
| `make catalog-sign` | Sign the reference catalog and plugin index with the development key |
| `make fmt` | Format Go and console source |

Required CI includes `check`, `integration`, `dependencies`, and both native
image jobs. Contract generation is reproducible; `openapi/management.json` is
the checked-in source of truth. Update it alongside handlers and regenerate with
`make api`. Never edit generated Go or TypeScript files by hand.

## Local development

After `make setup` and `make dev`, open http://127.0.0.1:5173 and use
`.local/secrets/bootstrap.token` to create the first owner. Use this origin
so management requests pass the origin check. Vite proxies management, OIDC
callbacks, and the configured inference paths; console edits hot reload, while
backend edits require a restart. See the [console guide](console/README.md) for
proxy limits and running Vite alone.

`make dev` creates missing private secrets and initializes the schema. Existing
secrets and data survive restarts. PostgreSQL and Valkey use isolated volumes
and loopback ports 54321/63791. Stop them with:

```sh
docker compose -f deploy/compose.dev.yaml stop
```

Development storage uses schema `olp` and Valkey namespace
`olp:<installation UUID>:`. OLP makes no upgrade promises during 0.x, so a change
may require a fresh database. Remove the development volumes and start again:

```sh
docker compose -f deploy/compose.dev.yaml down -v
make dev
```

## Tests and changes

Keep feature types, validation, SQL, handlers and workflows together. The
[architecture map](docs/develop/architecture.md) identifies backend and console owners;
[testing](tests/README.md) covers focused runs, timeouts and qualification.

Use `gofmt`, `go vet`, and `make lint` for Go; console source uses Prettier, two spaces,
single quotes, no trailing commas, `camelCase` helpers and `PascalCase.svelte` components. Keep
Svelte route components thin and use the [console design system](console/README.md#design-system).

Place Go unit tests beside their feature as `*_test.go`. Service tests use the
`integration` build tag and fail when explicitly selected without prerequisites.
Use Vitest `*.test.ts`/`*.svelte.test.ts` and Playwright `*.spec.ts` for the console.
Cover changed behavior and preserve protocol fixtures. Tests follow the strict
rules in [testing](tests/README.md#test-rules), meaning behavior assertions
only, no pinned outside facts without a cited source and date, and one focused
regression test per bug fix in the existing mapped test file. Coverage on
changed packages must not drop against the merge base, the comparison in the
[test baseline](tests/baseline.md) lanes, so the floor ratchets with every merge.
Run `make integration` for service or browser behavior changes.

Use short imperative commit subjects and name agent-created branches `tyk/{name}`.
Follow the [PR template](.github/PULL_REQUEST_TEMPLATE.md): describe the problem,
resulting behavior and validation; include screenshots for visible console changes.
Regenerate affected contracts with `make api`. Migrations are forward-only and
sequential; update Helm values, schema and templates together.

During 0.x, target the current storage, configuration and management API formats.
Compatibility with provider protocols and official OpenAI, Anthropic and Gemini
SDKs remains part of the product contract.

## Dependency policy

Use current stable dependencies; update Go and pnpm through their package managers.
Commit the
corresponding lockfiles. `scripts/check-dependencies.sh` checks reachable Go
vulnerabilities, licenses and production JavaScript packages. Image qualification
also scans runtime/build SBOMs and retains native linking/license evidence.

GLIDE's pinned native archive needs CGO, not Rust. The embedded OpenAI token ranks
carry their MIT notice and SHA-256 checks; oracle fixtures use test-only tiktoken
under `tests/fixtures/tokens`.

Client qualification packages in `tests/clients` are exact development pins,
subject to pnpm's release-age gate. Update their manifests and
[client compatibility](docs/use/clients.md) together. Codex, Claude Code and OpenCode
instead resolve the publisher's npm `latest` channel on each CI job, without a
release-age delay. Code-mode and broader client suites share those verified
binaries; exact versions and integrity values are retained in CI evidence.

TypeScript stays on the newest 6.0 patch until both
[typescript-eslint](https://typescript-eslint.io/users/dependency-versions/) and
Svelte support 7.x and the console passes checks and Chromium qualification.
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
