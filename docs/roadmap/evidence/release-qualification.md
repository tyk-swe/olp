# Go release qualification

Qualification is in progress. This record will identify the final candidate and
passing native release runs before M7 closes. No stable release tag has been
published by this work.

The current application is Go 1.27.1 with a separately built SvelteKit console.
Rust application sources and toolchain/build configuration are retired. The
frozen application remains available at
[`6c21dfb917c9019161348ea24b532a77b6612e6e`](https://github.com/tyk-swe/olp/tree/6c21dfb917c9019161348ea24b532a77b6612e6e).
Rust 2.x/3.x storage is incompatible: start a fresh Go database and isolated
Valkey service. Replacement recovery preserves a Go installation's identity.

## Capability and dependency reconciliation

[Release inventory](release-inventory.json) reconciles all 100 frozen management
operations, 77 inference tuples, every retained suite by behavior group, and the
18 unchanged language-neutral fixtures. Actual all/control processes exercise
every management operation to reject missing handlers and server failures;
feature suites exercise authorized behavior and validate response schemas.
The official JavaScript and optional Python SDK launchers use the same Go
fixture. Production credentials and paid provider qualification remain separate.

[Dependency inventory](release-dependencies.json) and [module graph](release-module-graph.txt)
record the completed module, including runtime, test and generator dependencies.
[Native inventory](../../../deploy/native/inventory.json) pins GLIDE's exact Go
release commit, six prebuilt archive hashes and licenses. Its FFI lock and SPDX
inventory cover all 376 locked crates, including optional/build/test crates;
this is a conservative scan set, not a claim that every crate is reachable.
Normal application development/builds require CGO and a C toolchain, without a
Rust compiler. The Docker build asserts native architecture and absence of
Cargo/rustc. CI uses an invocation guard for canonical contributor commands.

## Qualification record

Completed local observations so far:

- The Go application and all 413 console assets build with canonical `make build`.
- `make check` passed Go formatting/vet/unit/protocol checks and 480 console tests;
  final source changes will receive another complete qualification run.
- Focused replacement recovery passes corrupt/incomplete/incompatible/wrong-key
  and initialized-destination rejection, preserved identity/credentials, and
  retained video retrieval/download/deletion after master-key rotation.
- Official JavaScript OpenAI/Anthropic/Gemini and optional Python equivalents pass.
- Access, gateway, accounting and provider browser scenarios pass at packaged
  and Vite origins. The six original journeys and replacement recovery also pass
  at both origins, retaining their behavioral assertions. Four further accounting
  and retained-media browser checks pass, including mobile layout, accessibility,
  filtering and metadata privacy.
- Native amd64 and arm64 images pass all four modes with nonroot, read-only root,
  private health, listener ownership, PostgreSQL/Valkey and bounded shutdown.
- Real OTLP/HTTP export passes metadata/privacy/parent propagation checks; a stalled
  collector test verifies a bounded queue drops observably without blocking requests.
- Go vulnerability and license checks pass after upgrading gRPC/OpenTelemetry.
  Native-crate and final candidate image scans remain required.

Final immutable candidate qualification on both platforms and the final
five-sample build scorecard rerun remain open. The prior full-source CI run
passed canonical checks, integration and both native mode suites; candidate scans
identified dependency patches now applied to this source. They must pass before marking any dependent release gate complete.

## Operational scope

The listener bounds TCP admission, ages connections with HTTP/2 GOAWAY, preserves
admitted work for the configured drain interval, and enforces one process shutdown
deadline across HTTP, metadata delivery, workers and trace flushing. Tests exercise
forced cancellation and connection-capacity release.

Backup requires a drained, fresh accounting checkpoint and explicit traffic
quiescence. Restore validates checksums/history/identity and authenticates the
mounted keys in a disposable staging database before a single transactional write
to the still-empty destination. The restore role needs CREATEDB; the replacement
needs a separate empty Valkey service. A bootstrap-retirement drill verifies that
preparation never regenerates long-lived keys or the retired bootstrap secret.
A completed installation restores and restarts with its configured bootstrap
file absent; a fresh installation still rejects that absence. Secret readers
accept Helm read-only 0400/0440 mounts and private 0600/0640 files.

Resource observations are bounded fixture evidence. They do not establish fleet
throughput, latency, availability, invoice-cap accuracy, or a recovery-point
objective. Valkey persistence and reporting freshness retain the caveats in
[production guarantees](../../production-guarantees.md).
