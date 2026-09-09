# Contributing

OpenLLMProxy 3.0 is one Rust 2024 package and a SvelteKit console. Install the Rust toolchain from `rust-toolchain.toml`, Node.js 26, pnpm 11, Docker Compose, PostgreSQL 18 client tools, OpenSSL, curl, and jq. Run commands from the repository root.

| Command | Purpose |
| --- | --- |
| `make setup` | Install the pnpm workspace and generate API contracts |
| `make dev` | Start PostgreSQL, Valkey, Rust, and Vite |
| `make check` | Formatting, Clippy, Rust tests, ESLint, Svelte/type checks, and Vitest |
| `make test` | Rust unit and protocol tests using `cargo test` |
| `make integration` | PostgreSQL/Valkey, gateway contracts, recovery, SDKs, and Chromium journeys |
| `make api` | Generate OpenAPI and the TypeScript client |
| `make build` | Build the release binary and static console |
| `make fmt` | Format Rust and console source |

`check`, `integration`, and `dependencies` are the CI qualification jobs; configure repository branch protections to require them. Run integration locally when changing persistence, inference, authentication, runtime publication, distributed limits, or browser journeys. SQLx queries do not require offline metadata preparation.

`openapi/management.json` and `console/src/lib/api/schema.d.ts` are ignored outputs. Setup, development, checking, integration, and builds use `make api`. Change the handler's `#[utoipa::path]` annotation and its feature's `utoipa_axum::routes!` registration together; generation obtains paths and schemas from the router. Do not hand-edit generated files.

## Local development

Open http://localhost:5173 after `make dev`. The bootstrap token is in `.local/dev/bootstrap-token`; the console asks for it once when creating the first owner. Credentials and the master key stay in `.local/dev`. Vite proxies API, OIDC callback, and inference traffic to Rust on port 8081, preserving the browser origin and cookies. Console edits use hot reload. Restart `make dev` after Rust changes.

Development services bind to loopback ports 54320 and 63790. `docker compose -f deploy/compose.dev.yaml stop` stops them. Their named volumes preserve the development installation. Use a distinct database for other installations. Never point development or integration commands at customer storage.

3.0 requires a fresh database. Startup rejects 2.x storage before modifying it. PostgreSQL objects live in `olp_v3`, and Valkey keys include the 3.0 prefix and durable installation UUID. There is no in-place 2.x upgrade. Back up 2.x with its own version before retiring it, and provision 3.0 independently. Future 3.x schema changes use forward-only sequential migrations.

## Making changes

See [the architecture map](docs/architecture.md) for feature ownership. Keep types, validation, SQL, handlers, and workflows together. Use concrete PostgreSQL pools and transactions; pass audit provenance explicitly to mutations. Retain traits for real connector implementations and meaningful test substitutions. Prefer descriptive names, direct control flow, and small functions. Do not add compatibility without an explicit supported contract.

Tests assert behavior and keep meaningful protocol fixtures. Unit tests belong beside their owner. Service tests use disposable installations, and the Chromium suite exercises both the development proxy and the packaged Rust origin, followed by replacement restore. Do not replace valid fixture expectations merely to make a failure pass. Include the checks run and screenshots for visible console changes in PR descriptions.

## Dependency policy

Use current stable dependencies and update the lockfiles through Cargo and pnpm. The JavaScript projects share one workspace and lockfile. Python/uv is only used for the optional Python SDK test: `tests/sdk-smoke-python/run.sh`.

TypeScript stays on the newest 6.0 patch. The [TypeScript ESLint support range](https://typescript-eslint.io/users/dependency-versions/) excludes 7.x, and the Svelte toolchain must support the same compiler. Remove this exception when both support TypeScript 7 and the console passes `make check` and the Chromium journey. Other version exceptions require a concrete incompatibility or regression and a stated removal condition.

## Release evidence

The release tag, Cargo, workspace/console package versions, chart version and
appVersion use the same stable 3.x version; `scripts/check-release-version.mjs`
checks them before candidate publication. The pinned Rust toolchain is used in
required checks. Tags require source qualification and packaged candidate
qualification on both published architectures. Candidate tags are provisional;
production consumes only promoted, attested digests. Paid provider tests remain
manual, main-branch-only and receive only the selected provider's credentials.

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
