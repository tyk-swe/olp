# M1 validation record

Recorded on 2026-09-13, Linux amd64. The implementation is complete; M1 remains
open for **native arm64 execution**. This workspace has no native arm64 runner.
The [native CI matrix](../../../.github/workflows/ci.yml) builds and
smokes both architectures; its arm64 artifact must pass before M1-05/M1-09 close.
Emulation is explicitly rejected by the image qualification script.

## Completed qualification

| Check | Result and evidence |
| --- | --- |
| Go formatting, vet, unit/protocol checks; console formatting, Svelte/types/ESLint and Vitest | [Go check output](go-check.txt); 51 console files, 460 tests |
| PostgreSQL transactions, rollback, cancellation/recovery, bad credentials, verified TLS | Passed under `go test -race -tags=integration`; [service/process log](integration-amd64.txt) |
| GLIDE authenticated TLS, EVAL/TIME, stream group pending recovery, Pub/Sub, ambiguous writes, cancellation, reconnect and repeated close | Passed with race detector and goroutine convergence; same integration log |
| Four process modes, real dependency readiness, public/private route ownership, invalid config before bind, bounded shutdown | Passed against disposable PostgreSQL/Valkey; same integration log |
| Independent Go/TypeScript generation twice under Rust invocation tripwires | Byte-identical Go/TS output; [generation log](contracts.txt) |
| Frozen OpenAPI definition | Byte-identical to the exporter at `6c21dfb917c9019161348ea24b532a77b6612e6e`; SHA-256 `26a86670d520dab86deda608b6d03ff257bbd9f4bfbd27bd91fc33289e92fa48` |
| Go SDK fixture metadata/readiness through the existing launcher | Passed under Rust tripwire; full SDK protocol behavior remains M3–M6 |
| Chromium packaged assets and Vite proxy/hydration | Two passing browser projects; [packaged screenshot](console-go-packaged.png), [Vite screenshot](console-go-vite.png) |
| Native amd64 image, all/gateway/control/worker | Passed; [Rust-free build log](native-amd64-build.txt), [mode smoke log](native-amd64-smoke.txt) |
| Native arm64 image | **Not executed** locally; native CI job added, pending evidence |

The console screenshots show the retained sign-in shell and its honest HTTP 501
response from unimplemented authentication. They establish hydration, asset
loading, CSP and proxy behavior, not successful sign-in or product parity.
Original Rust workflows remain available; their full suites were not rerun for
this Go foundation change. No migrations or product handlers were introduced.

## Build and dependency scorecard

Every reported median/range below has **five successful samples**. Durations
include process startup and linking where applicable. These are development
builds, not release performance comparisons. Raw commands, source hashes or
frozen commits, environment/toolchain details, prefetch/warmup records, stdout,
stderr, statuses and dependency graphs are in the
[raw baseline archive](build-baselines.tar.gz). Individual JSON records are linked
below and remain readable without unpacking the archive.

| Measurement | Frozen Rust median (min–max) | Initial Go median (min–max) |
| --- | --- | --- |
| Clean backend build, empty private compilation cache | [228.062 s (226.766–234.552)](rust-backend-clean.json) | [28.778 s (28.181–30.886)](go-final-backend-clean.json) |
| Rebuild after actual implementation edit, warm cache | [21.121 s (20.086–21.916)](rust-backend-edit.json) | [4.145 s (4.025–4.232)](go-final-backend-edit.json) |
| Targeted SSE behavioral tests, warm cache, tests execute each run | Unmeasured | [0.642 s (0.635–0.680)](go-targeted-test.json) |
| API generation, warm generator cache | Unmeasured | [3.749 s (3.216–5.510)](go-api.json) |
| Console production build | Unmeasured | Unmeasured |
| Complete check / service+browser integration | Unmeasured | Unmeasured |
| Complete release image build | Unmeasured | Unmeasured |
| Cold dependency downloads | Unmeasured; sources already cached | Unmeasured; module/store sources already cached |

Machine: Intel Core Processor (Haswell, no TSX), 8 logical CPUs, 24,598,114,304
bytes RAM, Linux 7.0.0-31-generic x86_64. Rust 1.98.1/Cargo 1.98.1,
Go 1.27.1, GCC 15.2.0, Node 26.8.1 and pnpm 11.24.0. Dependency prefetch was
measured separately using existing download caches; those timings are not cold
network download costs. OS page caches were shared. Rust and early Go API/test
measurements overlapped other development/build work on this shared host. Final
Go backend samples ran sequentially after native and integration builds ended.
These load differences and the different application capabilities prevent a
controlled speedup claim. Re-run the procedure against the complete M7 product.

Rust edit samples change `BACKGROUND_SHUTDOWN_TIMEOUT` in [src/process/cli.rs](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/process/cli.rs)
on every run. Go edit samples change `ReadHeaderTimeout` in
`internal/process/run.go`; neither is a no-op rebuild. Go records hash their
uncommitted source snapshots and record base HEAD; timing artifacts correspond
to those snapshots, not a later invented commit. API and targeted-test snapshots
precede final lifecycle polish, which does not change those operations.

Native amd64 compilation/linking in the final candidate builder was one observed
39.310-second sample; **no median is claimed** for it. The glibc runtime links
libm, libgcc_s and libc: [linker output](native-amd64-native-link.txt) and
[binary build info](native-amd64-go-build-info.txt). Image content size is
92,038,070 bytes according to Docker (not compressed registry transfer size),
with ID `sha256:ca4e0fdea4ac3afbf31a12920ef4b95d4cdcb29ca377cad9bd4a9357e1be9250`. The [dependency inventory](dependencies.json) records
both GNU archives, their hashes/sizes, six distributed native archive variants,
425 upstream core lock entries and the imported Go graphs. Shipped GLIDE notices
include native Rust dependencies as well as Go dependencies. Runtime notices are
under `/usr/share/doc/openllmproxy/`.

## Reproduce

Use the prerequisites and commands in [CONTRIBUTING](../../../CONTRIBUTING.md).
`make go-check` requires no services. `make go-integration` provisions unique
service projects, one-day TLS material, per-run storage and cleanup traps; it
runs the service suite with the race detector, SDK fixture metadata check and
both Chromium projects. A local Docker sudo wrapper was needed in this workspace
because its user cannot access the daemon socket; checked-in tooling uses normal
Docker access and does not require sudo.

```sh
make go-setup
./scripts/go-without-rust.sh make go-check
./scripts/check-go-contracts.sh
pnpm --dir console exec playwright install chromium
./scripts/go-without-rust.sh make go-integration
docker build -f deploy/go.Dockerfile -t olp-go:candidate .
./scripts/go-image-smoke.sh olp-go:candidate amd64
node scripts/measure-builds.mjs --language=rust --case=backend-clean --runs=5
node scripts/measure-builds.mjs --language=go --case=backend-clean --runs=5
node scripts/measure-builds.mjs --language=go --case=backend-edit --runs=5
```

Use `arm64` for the image smoke command on an actual arm64 machine. Do not run
measurement cases concurrently for a controlled comparison. The measurement
runner also supports `targeted-test`, `api`, `console`, `check`, `integration`
and `image`; unmeasured scorecard cells remain explicit until five samples exist.
