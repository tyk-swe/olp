# M1: Go foundation and build economics

[Roadmap](README.md) | [Next: access and control](02-access-and-control.md)

**Status:** Not started. **Prerequisites:** None.

Establish a small, runnable Go application and prove the build path before
porting features. This milestone produces the measurements, contracts, and
test infrastructure used by the remaining six milestones.

## Backlog

### M1-01

- [ ] **Freeze the behavioral reference and capability inventory.**

**Depends on:** None.

**Deliver:** Expand the roadmap's capability ownership table with current
management operations, inference operation/provider combinations, CLI modes,
console journeys, and their supporting tests at the frozen reference commit.
Capture the generated management contract once from that reference.

**Accept:** Every existing feature has a completion milestone and evidence
source. Record documentation/code disagreements against behavioral tests,
including documented unsupported combinations and translation exceptions.
Preserve JSON/SSE fixtures independently of their Rust runners.

**References:** [Architecture](../architecture.md),
[endpoint registry](../../src/inference/http/endpoint_policy/registry.rs),
[compatibility](../compatibility.md), [tests](../../tests/README.md).

### M1-02

- [ ] **Record reproducible build and dependency baselines.**

**Depends on:** [M1-01](#m1-01).

**Deliver:** Implement the measurement procedure in the
[scorecard](README.md#build-and-dependency-scorecard), using disposable
checkouts and recording commands, machine details, caches, raw timings, and
dependency graphs. Separate backend builds from API generation and console work.

**Accept:** At least five successful measurements support each reported timing
median and range. Rebuild measurements contain an actual implementation edit.
Record the 50 direct production crates and 467 resolved Cargo package entries
with their counting definitions. Missing results remain explicitly unmeasured.

**References:** [Makefile](../../Makefile), [Cargo manifest](../../Cargo.toml),
[Docker build](../../deploy/Dockerfile), [CI](../../.github/workflows/ci.yml).

### M1-03

- [ ] **Create the Go module and dependency boundaries.**

**Depends on:** [M1-01](#m1-01).

**Deliver:** Create the application entrypoint under `cmd/olp` and feature
packages under `internal`, with explicit composition and feature-owned SQL.
Pin a supported Go toolchain, pgx, and GLIDE; record the initial dependency
inventory and supported native artifacts. Keep tooling dependencies distinct.

**Accept:** Feature unit tests do not import the application entrypoint or
initialize cloud clients, database connections, or telemetry. Add no generic
repository framework, ORM, service container, or second application module.
Each nonstandard dependency has a documented use and build impact.

**References:** [Feature ownership](../architecture.md),
[current composition](../../src/lib.rs),
[pgx](https://github.com/jackc/pgx).

### M1-04

- [ ] **Implement configuration, process composition, and listeners.**

**Depends on:** [M1-03](#m1-03).

**Deliver:** Establish standard-library configuration/logging, context-owned
startup and shutdown, the four process modes, public listener isolation, and
private liveness/readiness. Preserve applicable `OLP_*` settings and file-based
secret inputs; replace `RUST_LOG` with `OLP_LOG_LEVEL`. Reserve the existing
maintenance command names for their later implementations.

**Accept:** Invalid configuration fails before binding. Modes expose only their
owned listeners and initialize only needed dependencies. Readiness reports
actual dependency state, and shutdown closes owned resources within a bounded
deadline. Unimplemented commands return an explicit error.

**References:** [CLI](../../src/process/cli.rs),
[mode dependencies](../../src/process/mode_dependencies.rs),
[configuration](../configuration.md), [observability](../../src/observability.rs).

### M1-05

- [ ] **Qualify PostgreSQL and GLIDE integration.**

**Depends on:** [M1-03](#m1-03), [M1-04](#m1-04).

**Deliver:** Add concrete pgx/GLIDE client lifecycle code and disposable-service
fixtures. Exercise authenticated/TLS connections, Lua scripts, server time,
Streams consumer groups and pending recovery, publication hints, request
timeouts, cancellation, reconnects, and closing clients with work in flight.

**Accept:** Both native release architectures build and link with CGO using
GLIDE's distributed artifacts while Cargo and rustc are absent. Capture C/native
prerequisites and linking cost. Repeated disconnect/shutdown scenarios terminate
without abandoned work; ambiguous command outcomes are exposed to callers so
later reservation and ingestion code can handle retries safely.

**References:** [Valkey integration](../../src/limits/valkey.rs),
[queue protocol](../../src/usage/queue/protocol.rs),
[GLIDE Go documentation](https://github.com/valkey-io/valkey-glide/blob/main/go/README.md).

### M1-06

- [ ] **Make management contracts independent of gateway compilation.**

**Depends on:** [M1-01](#m1-01), [M1-03](#m1-03).

**Deliver:** Seed the checked-in OpenAPI source for `/api/v3` from the frozen
contract so existing console consumers remain typed during the rewrite.
Generate Go management types with pinned oapi-codegen tooling and TypeScript
types with the existing openapi-typescript workflow. Preserve openapi-fetch,
regenerate current outputs, and serve the definition at the management
OpenAPI endpoint.

**Accept:** Generation is deterministic and runs without PostgreSQL, Valkey,
gateway compilation, or Rust. Generated types stay outside domain/storage
models. Qualify the pinned generator against the document dialect, optional/
null fields, unions, decimal strings, and additional properties. Check each
implemented operation against the definition as handlers land, and track
remaining operations in the capability inventory. Full operation coverage is
an M7 gate; placeholder handlers must never return a fabricated success.

**References:** [Current exporter](../../src/bin/export_openapi.rs),
[console client](../../console/src/lib/api/client.ts),
[console tooling](../../console/package.json),
[oapi-codegen](https://github.com/oapi-codegen/oapi-codegen).

### M1-07

- [ ] **Create reusable Go behavioral and process harnesses.**

**Depends on:** [M1-03](#m1-03), [M1-05](#m1-05), [M1-06](#m1-06).

**Deliver:** Add Go runners for retained JSON/SSE fixtures, controlled upstream
HTTP servers, fragmented streams, and disposable process/service lifecycles.
Establish a Go SDK-fixture launcher with the metadata/readiness contract
consumed by the existing JavaScript and Python suites. Add a Go binary option
to the browser launcher.

**Accept:** Unit/protocol tests need no containers. Service suites isolate
installations and clean up on failure. Harness startup and SDK/browser fixture
launching invoke no Cargo command. Feature suites join these harnesses in their
own milestones; a fixture scaffold alone does not count as protocol parity.

**References:** [Fixtures](../../tests/fixtures/),
[process harness](../../tests/contract/harness.rs),
[SDK launcher](../../tests/sdk-smoke/run.sh),
[browser launcher](../../console/tests/journeys/run-olp.sh).

### M1-08

- [ ] **Provide a complete Go development workflow.**

**Depends on:** [M1-04](#m1-04), [M1-06](#m1-06), [M1-07](#m1-07).

**Deliver:** Add temporary `make go-setup`, `go-dev`, `go-api`, `go-check`,
`go-test`, `go-integration`, `go-build`, and `go-fmt` targets. Use gofmt,
go vet, meaningful Go tests, and the existing console checks. Run Vite against
Go with isolated development services and same-origin proxying.

**Accept:** A fresh development environment can generate contracts, run the
console shell, and execute the initial tests without Rust. Console edits keep
hot reload and do not rebuild the backend. Standard Go feature tests remain
fast; service and browser suites have explicit integration entrypoints.

**References:** [Makefile](../../Makefile), [dev script](../../scripts/dev.sh),
[development Compose](../../deploy/compose.dev.yaml),
[console configuration](../../console/svelte.config.js).

### M1-09

- [ ] **Build and smoke-test the first native Go images.**

**Depends on:** [M1-02](#m1-02), [M1-05](#m1-05), [M1-08](#m1-08).

**Deliver:** Add a Go candidate build path with separate backend and console
stages, native Linux amd64/arm64 execution, glibc-compatible runtime libraries,
CA certificates, and a nonroot runtime. Inventory the GLIDE native component
and record initial Go build timings alongside the reference.

**Accept:** Each architecture starts the application, connects to disposable
services, serves console assets, and responds on the private health listener.
Build logs show no Rust compilation. Any timing or native-linking problem is
recorded before feature implementation increases the cost of resolving it.

**References:** [Current image](../../deploy/Dockerfile),
[native release runners](../../.github/workflows/release.yml),
[image mode checks](../../scripts/smoke-image-modes.sh).

## Exit scenarios

- Run initial Go unit/protocol checks and PostgreSQL/GLIDE service scenarios.
- Generate Go/TypeScript contracts twice and verify deterministic outputs.
- Exercise Vite proxying and packaged static assets against the Go skeleton.
- Run both architecture smoke tests with the Rust toolchain absent.
- Attach baseline measurements, dependency inventory, and native build evidence.

The 2x build target remains a final, full-capability M7 gate; a fast skeleton
alone does not satisfy it.
