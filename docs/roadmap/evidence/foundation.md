# Foundation implementation and evidence

The reference is `6c21dfb917c9019161348ea24b532a77b6612e6e`. M1 adds a runnable Go
foundation alongside Rust. M3 owns the first inference path, M6 product parity,
and M7 complete qualification and retirement. HTTP 501 and reserved CLI errors
are intentional evidence of deferred work.

## Frozen capability reference

[Machine-readable inventory](reference-inventory.json) records every exported
management operation, all certified provider/operation/surface/transport tuples,
endpoint paths, CLI modes, browser journeys, test sources, and SHA-256 hashes of
the retained JSON/SSE corpus. [Management progress](management-operations.md)
assigns each operation a completion owner. The sole implemented management
operation is `GET /api/v3/openapi.json`.

The existing [compatibility tables](../../compatibility.md) remain the maintained
support matrix. The inventory expands the frozen conformance matrix, including
streaming images, speech, and transcription and asynchronous video creation.
Model discovery is a gateway operation for every provider; Gemini's v1 and
v1beta endpoints share a surface. OpenAI-compatible providers expose only OpenAI
generation, embeddings, token counting, and moderation. Azure has no media path.
Bedrock uses Converse; other cross-surface generation uses canonical translation.

Documentation must be read with these code/test qualifications:

- `tests/README.md` describes comprehensive fixture coverage, but the separately
  retained JSON/SSE corpus has only 18 files. Many conformance fixtures and
  expectations live in Rust test source. The inventory preserves both sources;
  framing checks do not qualify provider text/usage decoding.
- The compatibility table groups transport modes. Frozen connector conformance
  enumerates exact unary/streaming/async tuples and is the transport authority.
- Native source extensions are replayed; unsupported request extensions refuse
  translation, while response/stream extensions may be dropped. Canonical codecs
  refuse unrepresentable participant names, seeds, multiple choices, tool results,
  media details, and structured formats rather than inventing equivalents.
- Anthropic rejects canonical structured output. Gemini has constraints on tool
  selection, system-message ordering, tool results, MIME types, and media; its
  function-call-only STOP maps to a tool-call finish. Bedrock's frozen conformance
  exemptions explicitly cover structured output, cached usage, request ID
  injection, connector-level response byte limits, and media parts. These are
  retained limitations, not promised capabilities of the rewrite.
- GLIDE's README contains conflicting musl statements. M1 deliberately qualifies
  only distributed `*-unknown-linux-gnu` artifacts on native glibc platforms.

## Ownership and configuration

`cmd/olp` handles signals/CLI and calls `internal/process`; concrete pgx and GLIDE
lifecycle packages have no startup side effects. SQL belongs with features, and
generated transport models live only in `internal/management/contract`. No cloud,
telemetry, framework, ORM, or service-container initialization occurs on imports.

| Mode | Public listener | Private listener | Dependencies |
| --- | --- | --- | --- |
| all | Management, console, reserved inference | live/ready | PostgreSQL; Valkey when configured |
| gateway | Reserved inference | live/ready | PostgreSQL; Valkey when configured |
| control | Management and console | live/ready | PostgreSQL; Valkey when configured |
| worker | None | live/ready | PostgreSQL and required Valkey |

No background product workers exist yet. Private readiness pings owned services;
liveness remains available during dependency outages. Public `/health/…` and
`/metrics` are isolated; exact `/health` remains a console route. Startup validates
configuration/dependencies before binding, rolls back partial listener startup,
and drains listeners concurrently against one shutdown deadline. Requests inherit
process cancellation before clients close.

Flags override corresponding `OLP_*` variables. M1 retains database URL/pool size,
Valkey URL, listen/observability addresses, public origin, console directory, and
mounted auth/bootstrap/master-key file names. Optional mounted keys are checked
for readable, nonempty, non-world-readable files; key interpretation belongs to
M2. New URL `_FILE` alternatives avoid inline credentials. `OLP_LOG_LEVEL`
replaces `RUST_LOG`. Request/startup/shutdown durations use Go duration syntax via
`OLP_DEPENDENCY_REQUEST_TIMEOUT`, `OLP_STARTUP_TIMEOUT`, and
`OLP_SHUTDOWN_TIMEOUT`. `OLP_VALKEY_TLS_CA_FILE` supplies PEM roots with `rediss://`;
pgx uses standard URL `sslmode=verify-full&sslrootcert=…` settings. No TLS-verification
bypass is added. Crypto, admission/media limits, egress, and OTLP settings join with
their feature milestones; the skeleton does not claim to enforce them.

## Contract qualification

The checked-in OpenAPI 3.1 document is byte-for-byte the frozen exporter output.
`make go-api` uses oapi-codegen 2.8.0 and the existing locked openapi-typescript
workflow; openapi-fetch remains unchanged. No database, gateway build, or Cargo
is involved. Generated Go types are checked in; TypeScript remains reproducible
and ignored, matching the existing console workflow.

oapi-codegen cannot merge multiple default annotations in the frozen
`RoutingPreferences` composition. `scripts/generate-go-contract.mjs` removes
schema defaults only from a disposable generation input. It never changes the
served contract or wire constraints. Defaults are annotations and future domain
handlers must implement their behavior explicitly.

Go qualification covers absent versus explicit null versus values, exact decimal
strings, composed unions, and open maps. Generated models are not validators:
M2 handlers must enforce required fields, enums, unions, and closed-object
semantics. `DisallowUnknownFields` is qualified for ordinary closed structs;
raw-JSON union branches require domain validation. Full operation coverage is M7.

## Dependency and native inventory

Go 1.27.1 is pinned in the single root module. pgx 5.11.0 provides PostgreSQL SQL,
pooling, TLS, and cancellation; GLIDE 2.5.2 provides Valkey protocol, native
connection management, streams, Lua, and Pub/Sub. The standard library covers
HTTP/config/logging/testing. oapi-codegen runtime 1.7.0 and nullable 1.2.0 support
generated transport models (unions/UUID/time and three-state nullability), while
oapi-codegen 2.8.0 is a separate `tool` dependency in the same module. Model helper
dependencies do not link into the current application unless imported by a
handler. Module graphs distinguish compiled production, tests, and tooling.

[Dependency inventory](dependencies.json) records every imported application,
generated-model, and generator module, plus the GLIDE archive hashes and all
425 upstream native workspace lock entries. The [module graph](go-module-graph.txt)
also includes test/tool resolution. The application currently imports nine
external modules; that count excludes the root and generated types not yet used
by a handler. Transitive modules have these concrete purposes:

| Modules | Use and build impact |
| --- | --- |
| pgpassfile, pgservicefile, puddle/v2 | pgx connection configuration and pooling; pure Go |
| x/sync, x/text | Pool synchronization and PostgreSQL text handling; pure Go |
| google/uuid, protobuf | GLIDE configuration/callback serialization; pure Go |
| go-jsonmerge/v2 | Generated allOf/union JSON merging; linked when transport models are imported |
| kin-openapi, speakeasy-api, YAML/JSON-schema, x/tools and their graph | Pinned generator only; excluded from the application executable |

The frozen Rust manifest has 50 direct normal dependencies (inline and expanded
`[dependencies.name]` entries). Its lockfile has 467 `[[package]]` entries,
including the root, development, and transitive packages. These counts are not
equivalent to Go module counts.

GLIDE ships `rustbin/x86_64-unknown-linux-gnu/libglide_ffi.a` and
`rustbin/aarch64-unknown-linux-gnu/libglide_ffi.a` inside its checksum-verified Go
module. Normal builds use CGO, C headers/linker, glibc and libm, without Cargo or
rustc. The native Rust dependencies are separate from Go's module graph. Archive
hashes/sizes, upstream lockfile provenance, build-info, dynamic linkage, license
notices, and timing results accompany the measurements. Runtime images use
distroless cc-debian13 with CA certificates and UID/GID 65532; GLIDE is Apache-2.0
and its bundled third-party notices are copied into the image.

`coordination.CommandError` distinguishes pre-dispatch rejection from uncertain
execution after cancellation/timeout/disconnection/close. It retains an
unwrappable cause without logging server text or command arguments. It does not
retry writes. Later reservation/ingestion protocols must reconcile ambiguous
outcomes using their own idempotency rules. Blocking stream consumers must use
finite server-side BLOCK deadlines; M1 tests cancellation completion and close.

Qualification reproduced a GLIDE 2.5.2 Go cleanup leak: canceling an in-flight
command removes its pending callback before `Close`, leaving the library's
cleanup goroutine waiting forever. The concrete wrapper keeps native calls
registered using `context.WithoutCancel`, returns caller cancellation through a
buffered owned result channel, and joins outstanding calls when closing the
native client. Native command deadlines remain enabled. The repeated
cancel-then-close regression test verifies the boundary; no Rust source or
distributed archive is patched.

## Reproduction and completion gates

`scripts/measure-builds.mjs --language=rust|go --case=backend-clean|backend-edit|targeted-test|api|console|check|integration|image --runs=5`
runs in a disposable source snapshot. Rust defaults to the frozen revision; Go
defaults to a content-hashed working snapshot. Download prefetch timings and
dependency graphs are separate. Each clean run empties its private compilation
cache; each edit run changes executable timeout behavior after warmup. Output
contains machine/toolchains/cache definitions, raw logs, every elapsed time, and
only reports medians/ranges after five successful runs. Shared OS page caches
remain warm. Record host load when comparing future runs.

Run `scripts/check-go-contracts.sh`, `make go-check`, and `make go-integration`.
The integration launcher removes its disposable services even on failure and
qualifies both Vite and packaged console hydration, including the CSP hashes
needed by SvelteKit's inline bootstrap. Existing feature journeys remain intact.

Build `docker build -f deploy/go.Dockerfile -t olp-go:candidate .`, then run
`scripts/go-image-smoke.sh olp-go:candidate amd64` (or `arm64` on that native host).
The smoke script rejects emulation as native evidence. The Go foundation workflow
contains independent native amd64/arm64 build/smoke jobs and a Go/console check
job. Candidate images are local qualification artifacts; production release
promotion remains unchanged until M7.

See [validation record](validation.md) for observed results and outstanding gates.
Unmeasured cases and unexecuted native architectures must remain explicit. The
M7 2× target cannot be claimed from this foundation's build times.
