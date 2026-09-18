# M7: Release qualification and Rust retirement

[Roadmap](README.md) | [Previous: media and console parity](06-media-and-console-parity.md)

**Status:** Complete (2026-09-18). **Prerequisites:** M6 complete.

[Final qualification and native evidence](evidence/release-qualification.md).

Qualify the full Go replacement, switch the development and release workflows,
and retire the Rust application. Feature parity is already required at entry;
this milestone cannot hide unfinished providers, console pages, or media work.

## Backlog

### M7-01

- [x] **Close the behavioral evidence inventory.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#capability-and-dependency-reconciliation).

**Depends on:** Milestone prerequisites.

**Deliver:** Reconcile the M1 capability/endpoint inventory with all completed
Go suites. Port remaining real-process contract and recovery harness behavior,
including mode isolation, management errors, authority convergence, SDK
launching, and packaged/development browser origins.

**Accept:** Every retained scenario has passing Go evidence. Any retired
implementation-specific test has a recorded replacement or reason; valid
behavioral expectations are preserved. All official JavaScript SDK suites pass,
and the optional Python launcher runs against the same Go fixture without Cargo.

**References:** [System suites](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/system),
[contract suites](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract), [HA suites](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/ha),
[Python SDK launcher](../../tests/sdk-smoke-python/run.sh).

### M7-02

- [x] **Qualify installation, maintenance, and replacement restore.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#recovery-and-process-behavior).

**Depends on:** [M7-01](#m7-01).

**Deliver:** Complete Go maintenance commands, backup manifests/checksums,
drained backup and replacement restore, secret/bootstrap lifecycle, and
installation diagnostics. Verify sequential migrations against populated Go
data and preserve installation identity and historical references on restore.

**Accept:** Restore into an empty database with an isolated Valkey service
passes browser and SDK checks. Corrupt/incomplete backups, incorrect keys,
incompatible storage, and already-initialized destinations fail safely.
There is no Rust-to-Go data migration or shared writable installation.
Recovery evidence includes interrupted migration/rotation and retained media jobs.

**References:** [Backup](../../scripts/backup.sh), [restore](../../scripts/restore.sh),
[restore journey](../../console/tests/journeys/recovery.spec.ts),
[maintenance CLI](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/process/cli), [operations](../operations.md).

### M7-03

- [x] **Qualify process lifecycle, load, and dependency outages.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#recovery-and-process-behavior).

**Depends on:** [M7-01](#m7-01).

**Deliver:** Exercise all four modes and replicas under PostgreSQL/Valkey
outages, authority expiry, queue backlogs, maximum configured bodies,
slow consumers, disconnects, and graceful termination. Qualify HTTP/2
connection-age/GOAWAY behavior and draining against deployment grace periods.

**Accept:** No mode exposes another mode's private functionality. Draining
respects active streams and the documented ceiling; shutdown remains bounded.
Connection, goroutine/native-worker, memory, and spool observations fit the
tested configuration. Record measured capacity and limits without inventing
an untested production SLO or recovery RPO.

**References:** [Listener tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/http/listener/tests.rs),
[mode smoke checks](../../scripts/smoke-image-modes.sh),
[service smoke checks](../../scripts/smoke-image-services.sh),
[capacity contract](../production-guarantees.md).

### M7-04

- [x] **Switch canonical developer commands and required CI to Go.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#verification).

**Depends on:** [M7-01](#m7-01).

**Deliver:** Replace the implementations of `make setup`, `dev`, `check`,
`test`, `integration`, `api`, `build`, and `fmt` with the qualified Go
workflows. Keep console verification and service/browser integration.
Replace Rust lint/dependency jobs with Go formatting, vet, unit/protocol,
race, vulnerability/license, and contract checks appropriate to the application.

**Accept:** The canonical commands pass in an environment without Cargo/rustc.
Targeted tests and API generation keep their independent fast paths. Required
CI jobs cover the complete Go and console behavior without launching the frozen
Rust reference. Remove temporary `go-*` targets once callers use canonical ones.

**References:** [Makefile](../../Makefile), [CI](../../.github/workflows/ci.yml),
[tool setup](../../.github/actions/setup/action.yml),
[integration launcher](../../scripts/integration.sh),
[dependency updates](../../.github/dependabot.yml).

### M7-05

- [x] **Finish Go image, Compose, Helm, and release wiring.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#candidate).

**Depends on:** [M7-02](#m7-02), [M7-03](#m7-03), [M7-04](#m7-04).

**Deliver:** Make the Go image path canonical. Preserve native amd64/arm64
builds, separate static assets, nonroot execution, required GLIDE/CA libraries,
secret mounts, health/pre-stop commands, and process resource settings.
Update Compose overlays and Helm values/schema/templates together.
Use the workspace package version as the release-version source and embed it
in Go, keeping the console, chart, image, and tag checks synchronized.

**Accept:** Both native candidates pass packaged qualification. Provenance,
licenses, SBOMs, and scans include native dependencies. Release checks enforce
the same qualified digest through candidate assembly and promotion; no
recompilation occurs between qualification and promotion. Fresh-install and
storage incompatibility are explicit in release metadata.

**References:** [Dockerfile](../../deploy/Dockerfile), [Compose](../../deploy/compose.yaml),
[Helm](../../deploy/helm/), [release workflow](../../.github/workflows/release.yml),
[version checks](../../scripts/check-release-version.mjs).

### M7-06

- [x] **Prove the final build and dependency outcome.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#build-and-dependency-results).

**Depends on:** [M7-04](#m7-04), [M7-05](#m7-05).

**Deliver:** Repeat the M1 procedure against the complete application on the
same runner class and documented toolchains/cache conditions. Report all raw
measurements, medians/ranges, module graphs, native linking, API generation,
console/full-check duration, and image build/size results.

**Accept:** Clean backend builds and warm implementation-edit rebuilds each
take at most 50% of the Rust baseline median. API generation and ordinary
development require no Rust compiler; API generation also requires no gateway
build. Review every retained runtime dependency, including cloud and GLIDE
native components. A missed target keeps this ticket open; a small skeleton
measurement cannot close it.

**References:** [Measurement contract](README.md#build-and-dependency-scorecard),
[baseline ticket](01-foundation.md#m1-02),
[cloud dependency review](05-provider-and-routing-parity.md#m5-04).

### M7-07

- [x] **Update contributor and operator documentation.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#verification).

**Depends on:** [M7-02](#m7-02), [M7-03](#m7-03), [M7-05](#m7-05), [M7-06](#m7-06).

**Deliver:** Update the root README, AGENTS/contributor guidance, architecture,
configuration, compatibility, operations, recovery, deployment, and provider
guides for Go. Document CGO/native prerequisites, the log setting change,
fresh storage, process modes, contracts, current SDK evidence, and measured
build results.

**Accept:** A new contributor can follow setup without Rust. Operator examples
match the packaged CLI, mounts, and listeners. Budget/queue/authority/privacy
limitations remain accurate. Historical Rust source links in this roadmap
resolve to the frozen revision once their working-tree paths retire.

**References:** [Repository guide](../../AGENTS.md), [contributing](../../CONTRIBUTING.md),
[root README](../../README.md), [configuration](../configuration.md),
[operations](../operations.md), [deployment](../deployment.md).

### M7-08

- [x] **Retire the Rust application and obsolete tooling.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#capability-and-dependency-reconciliation).

**Depends on:** [M7-01](#m7-01), [M7-04](#m7-04), [M7-05](#m7-05),
[M7-06](#m7-06), [M7-07](#m7-07).

**Deliver:** Remove superseded Rust application source, manifests/lockfile,
toolchain/lint/dependency configuration, exporter, benches, test launchers,
Cargo path helpers, cargo-chef layers, and CI caches. Preserve useful
language-neutral fixtures, SDK suites, the SvelteKit console, and historical
SQL migration evidence in the frozen reference. Keep the Go migration history intact.

**Accept:** No active development, API generation, test, image, or release path
depends on the Rust toolchain. Reference code remains recoverable from the
recorded commit. GLIDE's prebuilt native core remains an explicitly inventoried
dependency. Removal does not discard any retained behavior or qualification fixture.

**References:** [Frozen baseline](README.md#using-the-backlog),
[Rust manifest](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/Cargo.toml), [Cargo path helper](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/scripts/lib/cargo-target-dir.sh),
[Rust exporter](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/bin/export_openapi.rs), [protocol fixtures](../../tests/fixtures/).

### M7-09

- [x] **Qualify the final replacement from a fresh checkout.**

**Evidence:** [Completed qualification](evidence/release-qualification.md#verification).

**Depends on:** [M7-08](#m7-08).

**Deliver:** Run the canonical checks and both native packaged qualifications
after Rust retirement. Assemble the final capability map, build/dependency
scorecard, fresh-install/restore evidence, SDK lockfile versions, and release
notes around the exact final candidate.

**Accept:** `make check`, `make integration`, contract generation, release
builds, Helm checks, and candidate smoke/restore journeys all pass without
Rust installed. Every current capability is accounted for. All seven
milestones close with linked evidence; paid provider qualification remains a
separate, explicit record when performed.

**References:** [Image qualification](../../scripts/qualify-image.sh),
[Helm checks](../../scripts/check-helm.sh),
[browser integration](../../scripts/browser-integration.sh),
[release workflow](../../.github/workflows/release.yml).

## Exit scenarios

- Start from a fresh checkout and complete setup, checks, integration, and builds without Rust.
- Qualify native amd64/arm64 candidates with the packaged console and all process modes.
- Install fresh storage, rotate secrets, back up, restore, and exercise SDK/browser workflows.
- Re-run outage, authority, queue, streaming/drain, and media recovery scenarios.
- Attach full-capability build results meeting both 2x targets and the final dependency inventory.
- Verify the completed capability map, final documentation, and recoverable Rust reference.
