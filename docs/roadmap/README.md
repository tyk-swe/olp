# Go + SvelteKit rewrite roadmap

OLP has replaced its Rust gateway with Go and retained the SvelteKit console.
All seven milestones are complete. [Final release qualification](evidence/release-qualification.md)
records the exact source/candidate, both native architectures, recovery and SDK/
browser evidence, dependency scans, and five-sample build results. Rust storage
is incompatible; Go deployments start with fresh storage. This directory retains
the original ticket IDs, explicit scope corrections and historical milestone evidence.

## Decisions

| Area | Decision |
| --- | --- |
| Backend | One Go module and application binary, with feature packages owning types, SQL, handlers, and workflows. Use concrete dependencies and interfaces at actual transport or test boundaries. |
| Standard library | Prefer standard-library HTTP, routing, configuration, logging, and testing. Keep application composition explicit. |
| Storage | PostgreSQL remains authoritative. Use pgx and direct SQL with feature-owned transactions. |
| Coordination | Retain Valkey for distributed limits, hints, and event delivery; use GLIDE as the Go client. |
| Console | Keep client-only SvelteKit, existing components and useful journeys, same-origin APIs, and separately built static assets. Vite remains the development server. |
| Management contract | Keep `/api/v3` and its OpenAPI endpoint. A checked-in OpenAPI definition generates Go management types and the TypeScript contract independently of gateway compilation. Update changed shapes and console consumers together. |
| Client protocols | Preserve `/v1`, `/anthropic/v1`, and both `/gemini/v1` and `/gemini/v1beta`, including native SDK authentication, errors, and streaming. |
| Process topology | Retain `all`, `gateway`, `control`, and `worker` modes, a separate private observability listener, Compose, and Helm. |
| Storage transition | Use a fresh Go installation with an isolated database and Valkey namespace. Keep a separate Go migration history, with forward-only sequential changes. Existing Rust storage must be rejected before mutation. |
| Compatibility | Preserve product behavior and documented limitations. Old management payloads, Rust storage, and mixed Rust/Go deployments receive no compatibility layer. |
| Release platforms | Retain native Linux amd64 and arm64 image builds. Qualify GLIDE and the runtime libraries on both platforms in M1. |
| Scope | Full existing capabilities; no new tenancy model, provider families, SSR application, or infrastructure services are required. |

GLIDE's Go client uses CGO and a Rust core distributed as prebuilt static
libraries. The build therefore needs a C toolchain and compatible native
libraries. Normal OLP development must use those artifacts without compiling
Rust; the release inventory must still account for their dependencies. Use
glibc-based Linux build/runtime images and native architecture runners as the
initial packaging choice. [GLIDE Go documentation](https://github.com/valkey-io/valkey-glide/blob/main/go/README.md)

## Milestones

| ID | Milestone | Required predecessor | Status |
| --- | --- | --- | --- |
| M1 | [Go foundation and build economics](01-foundation.md) | None | Complete; [final evidence](evidence/release-qualification.md) |
| M2 | [Installation, identity, and management](02-access-and-control.md) | M1 | Complete; [final evidence](evidence/release-qualification.md) |
| M3 | [Complete OpenAI request path](03-core-gateway.md) | M2 | Complete; [final evidence](evidence/release-qualification.md) |
| M4 | [Distributed limits, pricing, and recovery](04-limits-and-accounting.md) | M3 | Complete; [final evidence](evidence/release-qualification.md) |
| M5 | [Remaining protocols, providers, and routing](05-provider-and-routing-parity.md) | M4 | Complete; [final evidence](evidence/release-qualification.md) |
| M6 | [Media and operational completeness](06-media-and-console-parity.md) | M5 | Complete; [final evidence](evidence/release-qualification.md) |
| M7 | [Release qualification and Rust retirement](07-release-and-rust-retirement.md) | M6 | Complete; [final evidence](evidence/release-qualification.md) |

M3 provides the first usable Go inference path. M4 supplies the accounting and
measurements that advanced routing needs in M5. M6 completes product parity;
M7 qualifies the complete replacement and removes the old build workflow.
Milestones are completion gates, not calendar estimates.

## Using the backlog

Each ticket has a stable ID, checkbox, prerequisites, deliverable, acceptance
criteria, and references. Tickets inherit their milestone's prerequisites;
their explicit dependencies add ordering within the milestone. Preserve IDs
when splitting work into issues, and link implementation PRs and validation
evidence beside the ticket before checking it off.

Tests and console changes belong with the feature they exercise. A milestone
closes only when every ticket and its exit scenarios pass. Record measured
results and remaining limitations; do not replace valid fixture expectations
merely to make a port pass. A capability removal or weaker guarantee requires an
explicit roadmap revision and cannot be recorded as completed parity.

The frozen Rust reference is commit
`6c21dfb917c9019161348ea24b532a77b6612e6e`. Source links in these files describe
that baseline. If working-tree behavior later changes, inspect the reference
with `git show 6c21dfb917c9019161348ea24b532a77b6612e6e:<path>`.
M7 converts links to retired files into links to that revision. The existing
[architecture map](../architecture.md), [compatibility tables](../compatibility.md),
and [behavioral suites](../../tests/README.md) are the starting inventory.

Canonical `make setup/dev/check/test/integration/api/build/fmt` commands use Go.
Rust sources are retained only through the frozen reference; the two storage
formats must never share a writable installation.

## Capability ownership

This maps existing behavior to its completion milestone. The M1
[frozen inventory](evidence/reference-inventory.json) adds concrete endpoint,
fixture, and journey evidence. The existing
compatibility tables remain the operation/provider support matrix.

| Existing capability and source | Completion owner |
| --- | --- |
| [Process configuration, listeners, lifecycle, and mode composition](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/process) | M1 foundation; M7 qualification |
| [Database transactions, migrations, pagination, and idempotency](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/database.rs) | M2 implemented; [Go code and qualification](evidence/access-and-control.md) |
| [Bootstrap, local accounts, roles, invitations, sessions, profiles, and API keys](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access) | M2 implemented; [Go code and qualification](evidence/access-and-control.md) |
| [OIDC configuration, login, role mappings, and linked identities](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/oidc) | M2 implemented; [Go code and qualification](evidence/access-and-control.md) |
| [Secret files, hashing, encryption, and master-key rotation](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/crypto) | M2 implemented; [Go code and qualification](evidence/access-and-control.md) |
| [Management contracts and response policies](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/http/control) and [settings](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/settings) | M2 implemented; [Go code and qualification](evidence/access-and-control.md) |
| [Provider drafts, discovery, certification, revisions, and credential pools](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers) | M3 implemented for OpenAI-compatible connections; [Go code and qualification](evidence/core-gateway.md); M5 remaining connectors/options |
| [Route drafts, publication, history, and weighted selection](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/routes) | M3 implemented; [Go code and qualification](evidence/core-gateway.md) |
| [Atomic runtime publication, pinned snapshots, and independent authority refresh](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/runtime) | M3 implemented; [Go code and qualification](evidence/core-gateway.md) |
| [OpenAI Chat Completions, Responses, and model discovery](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/inference/http/endpoint_policy/registry.rs) | M3 implemented; [Go code and qualification](evidence/core-gateway.md) |
| [Admission, bounded execution, retries, circuit health, cancellation, and SSE](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/inference) | M3 implemented; [Go code and qualification](evidence/core-gateway.md) |
| [Egress validation and DNS pinning](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/net) and [HTTP resource limits](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/http) | M3 implemented; [Go code and qualification](evidence/core-gateway.md); media extensions implemented in M6 |
| [Key, connection, and slot limits and spend controls](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits) | M4 implemented as [`internal/limits/`](../../internal/limits/); [Go code and qualification](evidence/limits-and-accounting.md) |
| [Pricing, accounting, ingestion, history, reports, completeness, and retention](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage) | M4 implemented as [`internal/usage/`](../../internal/usage/); [Go code and qualification](evidence/limits-and-accounting.md) |
| [Distributed recovery and multiple-installation isolation](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/ha) | M4 implemented as the [`tests/integration/m4_*` process suites](../../tests/integration/); [Go code and qualification](evidence/limits-and-accounting.md); final process qualification M7 |
| [Anthropic/Gemini surfaces, translations, token counting, embeddings, and moderation](../compatibility.md) | M5 |
| [Azure, Vertex, Bedrock, and compatible-vendor profiles](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/providers) | M5 |
| [Custom endpoints/auth, mounted connectors, model facts, bulk workflows, policies, and routing preferences](../provider-routing.md) | M5 |
| [Images, audio, uploads, video jobs, historical credentials, and reconciliation](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/media) | M6 implemented as [`internal/media/`](../../internal/media/) and the gateway media handlers; M6-09 failure/resource/browser qualification is complete |
| [Metrics, optional OTLP tracing, health, and worker diagnostics](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/observability) | M1/M3 foundations; M6 implemented as [`internal/observability/`](../../internal/observability/) and [`internal/telemetry/`](../../internal/telemetry/) |
| [Console access/settings](../../console/src/lib/features/access/), [providers](../../console/src/lib/features/providers/), [routes](../../console/src/lib/features/routes/), and [playground](../../console/src/lib/features/inference/) | M2 and M3 implemented; [Go code and qualification](evidence/core-gateway.md); M5 alongside its APIs |
| [Console usage/history](../../console/src/lib/features/usage/), [media](../../console/src/lib/features/media/), [overview](../../console/src/lib/features/overview/), and [health](../../console/src/lib/features/runtime/) | Usage, history, budgets, and overview implemented in M4; [Go code and qualification](evidence/limits-and-accounting.md); media and health implemented in M6 |
| [Compose/Helm](../../deploy/), [backup/restore and qualification scripts](../../scripts/), [CI/releases](../../.github/workflows/), and [operations](../operations.md) | M7 |

## Build and dependency scorecard

The [complete M7 scorecard](evidence/release-build-scorecard.json) records five
successful measurements of source `ccd138f` on the same runner class/cache
procedure as the frozen Rust baseline. These are full-application measurements.

| Measurement | Frozen Rust median (range), seconds | Go M7 median (range), seconds |
| --- | --- | --- |
| Clean backend build | 228.062 (226.766–234.552) | 46.214 (43.621–49.054) |
| Actual implementation edit rebuild | 21.121 (20.086–21.916) | 5.791 (5.553–5.933) |
| Uncached targeted SSE test | Unmeasured | 0.682 (0.660–0.787) |
| API generation | Unmeasured | 2.304 (2.183–2.361) |
| Console build | Unmeasured | 10.583 (10.296–11.092) |
| Complete checks | Unmeasured | 56.801 (56.402–59.422) |
| Image build | Unmeasured | 70.231 (68.592–70.962) |

Both required backend medians are below 50% of Rust: 20.26% clean and 27.42%
with an implementation edit. API generation uses checked OpenAPI and pinned Go/
TypeScript generators without compiling the gateway. Image builds disable layer
reuse while retaining the explicit BuildKit Go cache mount; Docker-internal
downloads are included. Uncompressed local amd64 images measure approximately
111.50 MB. CI separately qualifies integration rather than presenting it as a
five-sample timing case.

[Dependencies](evidence/release-dependencies.json) contain 23 direct Go module
requirements and 216 resolved modules, including tests/tools. The frozen Rust
baseline has 50 direct production crates and 467 resolved entries; these are not
equivalent maintenance metrics. GLIDE's prebuilt native component is separately
inventoried and scanned, including its conservative 376-crate FFI lock. See the
[release report](evidence/release-qualification.md#build-and-dependency-results)
for raw samples, commands, toolchains, linking, native libraries, licenses and
resource-observation limits. Ordinary development requires no Rust compiler.

## Completion evidence

The replacement reuses the neutral JSON/SSE corpus, official JavaScript SDKs,
optional Python SDKs and browser journeys through Go launchers. Disposable
PostgreSQL, Valkey and controlled dependency failures qualify persistence and
recovery behavior.

Preserve the [production contracts](../production-guarantees.md): authority
expiry, accrued-cost budget semantics, metadata privacy, egress restrictions,
queue-loss visibility, and installation identity during restore. Deterministic
fixtures establish tested compatibility. Paid live-provider qualification
remains optional, credential-scoped, and separately recorded.

M7 closes with the linked capability map, passing feature/process evidence,
both native image qualifications, fresh-install and restore journeys, completed
build scorecard, and canonical development/release workflows without Rust.
