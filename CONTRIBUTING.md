# Contributing

Install Go 1.27.1, a C compiler/linker and glibc development headers, Node.js 26,
pnpm 11.24.0, Docker Compose, PostgreSQL 18 client tools, OpenSSL, curl, jq,
Python 3 and ripgrep. Run commands from the repository root. GLIDE ships a pinned
prebuilt native core; normal development requires CGO but no Rust compiler.
Release platforms are native Linux amd64 and arm64. macOS and musl are unqualified.

| Command | Purpose |
| --- | --- |
| `make setup` | Download modules, install the pnpm workspace, generate contracts |
| `make dev` | Start PostgreSQL, Valkey, Go on 8082/private 9092, and Vite on 5173 |
| `make check` | gofmt, vet, unit/protocol tests, ESLint, Svelte/type checks, Vitest |
| `make test` | Go unit and protocol suites without containers |
| `make integration` | Disposable services, race/process/recovery/SDK/Chromium suites |
| `make api` | Generate Go and TypeScript types without building the gateway |
| `make build` | Versioned `.local/bin/olp` plus separate `console/build` assets |
| `make fmt` | Format Go and console source |

Required CI includes `check`, `integration`, `dependencies`, and both native
image jobs. Contract generation is reproducible; `openapi/management.json` is
the checked-in source of truth. Update it alongside handlers and regenerate
with `make api`. Never edit generated Go or TypeScript files by hand.
`scripts/without-rust.sh COMMAND` catches accidental Cargo/rustc invocations.

## Local development

Open http://127.0.0.1:5173 after `make dev`. Use this configured origin for browser
access so management requests pass the origin check. The one-time bootstrap token is in
`.local/go-secrets/bootstrap.token`. Vite proxies management, OIDC callbacks,
and inference through the browser origin. Console edits use hot reload;
restart after backend edits. Development services use isolated volumes and
loopback ports 54321/63791. Stop them with
`docker compose -f deploy/compose.dev.yaml stop`.

Go uses schema `olp_go` and Valkey namespace `olp:go:v1:<installation UUID>:`.
Provision fresh storage when replacing any Rust release, including Rust 3.0.
Startup rejects Rust schemas before writes. Back up the old installation with
its own version and retain it until the independent replacement is verified.

## Tests and changes

Keep feature types, validation, SQL, handlers and workflows together. See the
[architecture map](docs/architecture.md). Unit tests live beside their owners;
service suites live under `tests/integration`. Run `make integration` for
persistence, authentication, inference, runtime publication, limits or browser
changes. It provisions disposable TLS/authenticated services and runs the full
console journey and replacement restore at packaged and Vite origins. Install
Chromium with `pnpm --dir console exec playwright install --with-deps chromium`.
Preserve meaningful fixture expectations. Include validation and screenshots
for visible console changes in PRs.

`tests/sdk-smoke/run.sh` runs all pinned JavaScript SDKs against the Go fixture.
`tests/sdk-smoke-python/run.sh` uses the same fixture with uv/Python 3.14. These
checks use deterministic local providers. Paid cloud checks are separate:
`OLP_LIVE_PROVIDER=... go test -tags=liveproviders ./internal/connectors` or the
manual main-branch `live-providers` workflow with selected-provider credentials.

## Dependency policy

Use current stable dependencies and update `go.mod`, `go.sum` and the pnpm
workspace lockfile with the relevant package managers. Runtime dependencies,
API-generation tools, GLIDE's native archive and native notices are inventoried
separately. `scripts/check-dependencies.sh` checks reachable Go vulnerabilities,
licenses and production JavaScript packages. Candidate image qualification also
scans runtime/build-stage SBOMs and retains the native linking/license inventory.

TypeScript stays on the newest 6.0 patch. The [TypeScript ESLint support range](https://typescript-eslint.io/users/dependency-versions/) excludes 7.x, and the Svelte toolchain must support the same compiler. Remove this exception when both support TypeScript 7 and the console passes `make check` and the Chromium journey. Other version exceptions require a concrete incompatibility or regression and a stated removal condition.

## Release evidence

Root `package.json` owns the stable 3.x version. The console package, chart
version/appVersion, binary linker value, image label and release tag must agree;
`scripts/check-release-version.mjs` enforces this. Build images natively on
amd64 and arm64. Dependency layers are cached independently of application
source; static assets remain separate from the Go binary. Native GLIDE code
and CA libraries ship in a nonroot distroless image.

Candidate platform digests form one multi-architecture index. Packaged browser,
SDK and recovery qualification consumes that exact index; promotion only tags
and attests it and never rebuilds. A manual release-workflow dispatch qualifies
a candidate without publishing stable version tags. Only a `v3.*` push can
promote. Fresh storage requirements are recorded in release metadata.

The [release evidence](docs/roadmap/evidence/release-qualification.md) records
actual qualification and full-application five-sample build measurements. A
missing platform result or missed build target keeps its release gate open.

A provider, protocol or media feature is not release-ready until its review
includes native conformance, unsupported/lossy semantics, body/time/admission
limits, pricing and missing-usage treatment, failure/cancellation behavior,
privacy, documented SDK versions and any recovery/credential-reference impact.
The reviewer can block expansion when this evidence is missing even if the
happy path works. Keep the single-package feature ownership and immutable
publication model; do not add speculative services, tenant abstractions or
optimization gates to satisfy a review checklist.

Publish the generated management OpenAPI contract with every release. Breaking
changes require an explicit compatibility/versioning decision, not silently
updating a fixture. Weekly released-image scans retain a digest-specific vulnerability report; triage
findings through SECURITY.md and update the supported patch release.
