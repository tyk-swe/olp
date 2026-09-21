# Contributing

Install Go 1.27.1, a C compiler/linker and glibc development headers, Node.js
26, pnpm 11.24.0, Docker Compose, PostgreSQL 18 client tools, OpenSSL, curl, jq,
Python 3 and ripgrep. Run commands from the repository root. GLIDE ships a
pinned prebuilt native core; normal development requires CGO but no Rust
compiler. Release platforms are native Linux amd64 and arm64. macOS and musl are
unqualified.

| Command | Purpose |
| --- | --- |
| `make setup` | Download modules, install the pnpm workspace, generate contracts |
| `make dev` | Start PostgreSQL, Valkey, Go on 8082/private 9092, and Vite on 5173 |
| `make check` | Generate contracts; check formatting, vet, ESLint, types, and local tests |
| `make test` | Go, console unit/component, and script suites without containers |
| `make test-go` | Go unit and protocol suites |
| `make test-console` | Console unit and component suites |
| `make test-scripts` | Automation script tests |
| `make test-race` | Uncached Go tests with race detection, also required in CI |
| `make integration` | Disposable services, race/process/recovery/SDK/Chromium suites |
| `make api` | Generate Go and TypeScript types without building the gateway |
| `make build` | Versioned `.local/bin/olp` plus separate `console/build` assets |
| `make fmt` | Format Go and console source |

Required CI includes `check`, `integration`, `dependencies`, and both native
image jobs. Contract generation is reproducible; `openapi/management.json` is
the checked-in source of truth. Update it alongside handlers and regenerate with
`make api`. Never edit generated Go or TypeScript files by hand.
`scripts/without-rust.sh COMMAND` catches accidental Cargo/rustc invocations.

## Local development

After `make setup` and `make dev`, open http://127.0.0.1:5173 and use
`.local/go-secrets/bootstrap.token` to create the first owner. Use this origin
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

Go uses schema `olp_go` and Valkey namespace `olp:go:v1:<installation UUID>:`.
Provision fresh storage when replacing any Rust release, including Rust 3.0.
Startup rejects Rust schemas before writes. Back up the old installation with
its own version and retain it until the independent replacement is verified.

## Tests and changes

Keep feature types, validation, SQL, handlers and workflows together. See the
[architecture map](docs/architecture.md). Unit tests live beside their owners;
service suites live under `tests/integration` or beside their owners with the
`integration` build tag. Run `make test` after setup for all container-free
behavioral checks; `make check` adds contract generation and static checks.
Neither command installs dependencies or starts services. Go-only targets need
Go and its native prerequisites, without Node or pnpm. See
[tests/README.md](tests/README.md) for focused runs, timeouts, and coverage.

Run `make integration` for persistence, authentication, inference, runtime
publication, limits, or browser changes. It provisions disposable
TLS/authenticated services and runs the full console journey and replacement
restore at packaged and Vite origins. Install Chromium with
`pnpm --dir console exec playwright install --with-deps chromium`. Preserve
meaningful fixture expectations. Include validation and screenshots for visible
console changes in PRs.

See [SDK and live-provider tests](tests/README.md#sdk-and-live-provider-tests)
for deterministic JavaScript/Python checks and opt-in paid qualification.

## Dependency policy

Use current stable dependencies and update `go.mod`, `go.sum` and the pnpm
workspace lockfile with the relevant package managers. Runtime dependencies,
API-generation tools, GLIDE's native archive and native notices are inventoried
separately. `scripts/check-dependencies.sh` checks reachable Go vulnerabilities,
licenses and production JavaScript packages. Candidate image qualification also
scans runtime/build-stage SBOMs and retains the native linking/license
inventory.

TypeScript stays on the newest 6.0 patch. The
[TypeScript ESLint support range](https://typescript-eslint.io/users/dependency-versions/)
excludes 7.x, and the Svelte toolchain must support the same compiler. Remove
this exception when both support TypeScript 7 and the console passes
`make check` and the Chromium journey. Other version exceptions require a
concrete incompatibility or regression and a stated removal condition.

## Release evidence

Root `package.json` owns the stable 3.x version. The console package, chart
version/appVersion, binary linker value, image label and release tag must agree;
`scripts/check-release-version.mjs` enforces this. Build images natively on
amd64 and arm64. Dependency layers are cached independently of application
source; static assets remain separate from the Go binary. Native GLIDE code and
CA libraries ship in a nonroot distroless image.

Candidate platform digests form one multi-architecture index. Packaged browser,
SDK and recovery qualification consumes that exact index; promotion only tags
and attests it and never rebuilds. A manual release-workflow dispatch qualifies
a candidate without publishing stable version tags. Only a `v3.*` push can
promote. Fresh storage requirements are recorded in release metadata.

The [dated completion record](docs/roadmap/README.md) preserves qualification of
the September 18, 2026 candidate, not newer source. Current inventories are
`deploy/release-inventory.json`, `deploy/release-dependencies.json`, and
`deploy/release-module-graph.txt`; regenerate them with
`node scripts/release-inventory.mjs`. The frozen reference is never regenerated
from the application being tested. CI checks these generated paths;
qualification runs remain separate. A missing platform result or missed build
target keeps that candidate's release gate open.

A provider, protocol or media feature is not release-ready until its review
includes native conformance, unsupported/lossy semantics, body/time/admission
limits, pricing and missing-usage treatment, failure/cancellation behavior,
privacy, documented SDK versions and any recovery/credential-reference impact.
Missing evidence can block release. Preserve feature ownership and immutable
publication; avoid speculative services, tenant abstractions, or optimization
gates added only to satisfy a checklist.

Publish the generated management OpenAPI contract with every release. Breaking
changes require an explicit compatibility/versioning decision, not silently
updating a fixture. Weekly released-image scans retain a digest-specific
vulnerability report; triage findings through [the security policy](SECURITY.md)
and update the supported patch release.
