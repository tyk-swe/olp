# Go release qualification

Completed on 2026-09-18 for source
[`ccd138f288a99f66a2bb1145eb75d5ce57398416`](https://github.com/tyk-swe/olp/commit/ccd138f288a99f66a2bb1145eb75d5ce57398416).
[Release run 35334054213](https://github.com/tyk-swe/olp/actions/runs/35334054213) passed canonical CI, both native
image builds, vulnerability scans, packaged browser journeys and replacement
restore. [Machine-readable candidate record](release-candidate.json) links every
job, scan report and artifact checksum. Later evidence/documentation commits do
not change the qualified application.

## Candidate

```text
ghcr.io/tyk-swe/olp@sha256:0c7495977bf5a837c7d3d0fa9d21c557a7ddbabb5eb08213ec51d45a458de09d
```

This index contains native Linux amd64 and arm64 images. The same index digest
was pulled and qualified on both native runners. Tag-push promotion is wired to
attest and tag that qualified digest without recompilation. This manual run
qualified the candidate; stable promotion was intentionally skipped.

The application version is 3.0.0, sourced from root `package.json`, embedded in
the Go binary, and checked against console, image defaults, Helm and release tag.
The runtime is nonroot with a read-only filesystem, separate static assets,
private health/metrics, writable bounded spool mounts and Helm-compatible secret
permissions. Native [amd64](release-native-amd64/smoke.log) and
[arm64](release-native-arm64/smoke.log) logs qualify all four process modes.
Packaged [amd64](release-packaged-amd64.log) and
[arm64](release-packaged-arm64.log) logs each show six fresh browser journeys and
one restored journey, with durable request history increasing from four to six.

[Runtime/build/native scan results](release-candidate.json) contain no HIGH or
CRITICAL findings at qualification time. The [OCI build SBOM bundle](release-image-sbom.json.gz)
is gzip-compressed JSON; its hashes are in the candidate record. Scans include
Go, console/build dependencies and GLIDE's native Rust crate inventory. Findings
are time-dependent; the scheduled released-image scanner remains enabled.

## Capability and dependency reconciliation

[Release inventory](release-inventory.json) reconciles 100 frozen management
operations, 77 inference tuples, all retained suites by behavior group and 18
unchanged neutral fixtures. Actual all/control processes exercise every management
operation to reject missing handlers and server failures; authorized feature
suites validate behavior and OpenAPI response schemas.

[Dependency inventory](release-dependencies.json) and [module graph](release-module-graph.txt)
record 23 direct module requirements and 216 resolved modules, including test and
generator dependencies. These counts are not equivalent to the frozen Rust
baseline's 50 production requirements and 467 resolved Cargo entries.
[Native inventory](../../../deploy/native/inventory.json) pins GLIDE's exact
Go release commit, six prebuilt archive hashes and license notices. The FFI
lock/SPDX covers 376 crates, including optional/build/test crates; this conservative
scan set does not imply every crate is reachable. Native link/build information
is retained for [amd64](release-native-amd64/native-link.txt) and
[arm64](release-native-arm64/native-link.txt). Ordinary development needs Go,
CGO and a C toolchain. Image builds assert that Cargo/rustc are absent, and CI
uses a Rust-invocation guard for canonical commands.

The Rust application/toolchain, exporter, source, tests, migrations, benches and
Cargo helpers are retired. The frozen reference remains recoverable at
[`6c21dfb917c9019161348ea24b532a77b6612e6e`](https://github.com/tyk-swe/olp/tree/6c21dfb917c9019161348ea24b532a77b6612e6e).
Neutral fixtures, Go migrations, SDKs and the console remain. Source links for
retired files point to that revision. Rust 2.x/3.x storage is incompatible:
new Go installations require fresh PostgreSQL storage and isolated Valkey.

## Verification

[CI record](release-ci.json) and raw [checks](release-ci-check.log),
[integration](release-ci-integration.log) and [dependencies](release-ci-dependencies.log)
show passing formatting, vet, unit/protocol tests, race checks, contract
reproducibility, OpenAPI response validation, 480 console tests, service/process
recovery suites, Helm validation and vulnerability/license checks.

The integration run passed 26 browser scenarios: twelve access/gateway/accounting/
retained-media checks across packaged assets and Vite, twelve original fresh
journeys, and two replacement-recovery journeys. Retained-media tests exercise
real list/detail APIs, filters, reloads, authorization, metadata privacy and Axe
checks at mobile/desktop sizes. Screenshots: packaged [320](release-media-packaged-320.png)/
[1440](release-media-packaged-1440.png), Vite [320](release-media-vite-320.png)/
[1440](release-media-vite-1440.png).

Official JavaScript SDKs passed: OpenAI 7.4.0, Anthropic 0.116.0 and Google GenAI
2.16.0. Optional Python SDKs passed against the same Go fixture: OpenAI 3.8.0,
Anthropic 1.4.0 and google-genai 2.22.0. Restored browser journeys additionally
exercise the official OpenAI SDK against restored gateways.
[Fresh-source qualification](release-fresh-checkout.log) starts with an archive
of the exact source and no workspace dependencies/assets/secrets, runs guarded
`make setup`/`make dev`, verifies Vite proxying, private liveness and embedded
version, runs the Python SDKs, and packages the Helm chart. The candidate record
contains chart and management-contract hashes. Paid live-provider runs were not
requested; their manual credential-scoped Go workflow remains separate.

## Recovery and process behavior

Replacement recovery passes checksummed/drained snapshots, corrupt/incomplete/
incompatible/wrong-key rejection, initialized-destination refusal, identity and
history preservation, interrupted migration and key rotation, and retained video
refresh/download/deletion after rotation. Restore authenticates keys and supported
migrations in a disposable staging database before a single transaction rechecks
and writes the empty destination. It requires CREATEDB and a separate empty
Valkey service. Bootstrap retirement preserves long-lived keys: a completed
installation restores/restarts with its configured bootstrap file absent, while
a fresh installation rejects that absence. Secret modes 0400/0440 and 0600/0640
are accepted; writable group/world files are rejected.

The service suites cover multiple replicas, PostgreSQL/Valkey failures, authority
expiry, bounded queues, duplicate stream delivery, leader loss, spool recovery,
slow readers, cancellation and distributed reservations/accounting. HTTP/2 tests
observe GOAWAY on connection age while admitted work survives; forced shutdown
cancels overdue work. One configured shutdown deadline covers HTTP, metadata,
workers and trace flushing. Real OTLP/HTTP tests verify metadata-only export,
validated traceparent propagation without tracestate leakage, and observable
drops from a bounded queue when a collector stalls.

[Media capacity observations](release-media-capacity.json) hold eight 64 MiB
uploads (512 MiB total) under a 1 GiB spool bound, then exercise slow reads and
mass disconnect cleanup to zero reservations. Final-source CI observed 67,008 KiB
RSS, 3,044,296 heap bytes and ten descriptors at the held checkpoint.
[Native process snapshots](release-process-resources.json) record Docker memory,
CPU and thread counts for each mode on each architecture. These observations are
not peak-memory, throughput, latency, fleet availability, invoice-cap or RPO
guarantees; filesystem/page-cache accounting and Valkey persistence retain the
caveats in [production guarantees](../../production-guarantees.md).

## Build and dependency results

[Scorecard](release-build-scorecard.json) records five successful samples per
case for the complete source above. The Linux amd64 runner has eight Haswell
logical CPUs and approximately 24.6 GB memory, matching the frozen M1 runner
class/cache procedure. JSON files record exact toolchains, source, timestamps,
download times, commands and raw log paths.

| Measurement | Median seconds | Range seconds |
| --- | ---: | ---: |
| [Clean backend build](go-release-backend-clean.json) | 46.214 | 43.621–49.054 |
| [Implementation edit rebuild](go-release-backend-edit.json) | 5.791 | 5.553–5.933 |
| [Uncached SSE test](go-release-targeted-test.json) | 0.682 | 0.660–0.787 |
| [API generation](go-release-api.json) | 2.304 | 2.183–2.361 |
| [Console build](go-release-console.json) | 10.583 | 10.296–11.092 |
| [Complete checks](go-release-check.json) | 56.801 | 56.402–59.422 |
| [Image build](go-release-image.json) | 70.231 | 68.592–70.962 |

The clean median is 20.26% of Rust's 228.062 seconds; the implementation-edit
median is 27.42% of Rust's 21.121 seconds. Both required thresholds are at most
50%, and both pass. Clean samples clear the private Go compilation cache;
edit samples alter implementation code after warmup, including CGO linking.
API/console/check/image work is measured separately. Image builds disable layer
reuse with `--no-cache`; the explicit BuildKit Go cache mount remains warm, and
Docker-internal downloads are included. Local uncompressed amd64 images ranged
111,497,855–111,500,108 bytes. Integration is separately evidenced by CI and is
not presented as a five-sample timing benchmark.
