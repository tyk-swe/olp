# Go + SvelteKit rewrite roadmap

OLP will replace its Rust gateway with Go to reduce compile time and dependency
maintenance. The project has zero users, so the rewrite can start with fresh
storage and change management contracts without building migration adapters.
All current product capabilities must be restored by milestone 7. The existing
SvelteKit console will be reused and simplified as each backend feature lands.

This directory is the implementation backlog. Ticket status and linked
qualification evidence record progress; a milestone closes only after its gates pass.

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
| M1 | [Go foundation and build economics](01-foundation.md) | None | Implemented; native arm64 qualification pending |
| M2 | [Installation, identity, and management](02-access-and-control.md) | M1 | Not started |
| M3 | [Complete OpenAI request path](03-core-gateway.md) | M2 | Not started |
| M4 | [Distributed limits, pricing, and recovery](04-limits-and-accounting.md) | M3 | Not started |
| M5 | [Remaining protocols, providers, and routing](05-provider-and-routing-parity.md) | M4 | Not started |
| M6 | [Media and operational completeness](06-media-and-console-parity.md) | M5 | Not started |
| M7 | [Release qualification and Rust retirement](07-release-and-rust-retirement.md) | M6 | Not started |

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

Temporary `make go-*` commands will isolate rewrite work from Cargo starting in
M1. Rust may be run explicitly for baseline measurements or differential
investigation. M7 replaces the canonical Make targets with the Go workflows.
The Rust reference and the Go application never share a writable installation.

## Capability ownership

This maps existing behavior to its completion milestone. The M1
[frozen inventory](evidence/reference-inventory.json) adds concrete endpoint,
fixture, and journey evidence. The existing
compatibility tables remain the operation/provider support matrix.

| Existing capability and source | Completion owner |
| --- | --- |
| [Process configuration, listeners, lifecycle, and mode composition](../../src/process/) | M1 foundation; M7 qualification |
| [Database transactions, migrations, pagination, and idempotency](../../src/database.rs) | M2 |
| [Bootstrap, local accounts, roles, invitations, sessions, profiles, and API keys](../../src/access/) | M2 |
| [OIDC configuration, login, role mappings, and linked identities](../../src/access/oidc/) | M2 |
| [Secret files, hashing, encryption, and master-key rotation](../../src/crypto/) | M2 |
| [Management contracts and response policies](../../src/http/control/) and [settings](../../src/settings/) | M2 |
| [Provider drafts, discovery, certification, revisions, and credential pools](../../src/providers/) | M3 core; M5 remaining connectors/options |
| [Route drafts, publication, history, and weighted selection](../../src/routes/) | M3 |
| [Atomic runtime publication, pinned snapshots, and independent authority refresh](../../src/runtime/) | M3 |
| [OpenAI Chat Completions, Responses, and model discovery](../../src/inference/http/endpoint_policy/registry.rs) | M3 |
| [Admission, bounded execution, retries, circuit health, cancellation, and SSE](../../src/inference/) | M3 |
| [Egress validation and DNS pinning](../../src/net/) and [HTTP resource limits](../../src/http/) | M3; media extensions M6 |
| [Key, connection, and slot limits and spend controls](../../src/limits/) | M4 |
| [Pricing, accounting, ingestion, history, reports, completeness, and retention](../../src/usage/) | M4 |
| [Distributed recovery and multiple-installation isolation](../../tests/ha/) | M4; final process qualification M7 |
| [Anthropic/Gemini surfaces, translations, token counting, embeddings, and moderation](../compatibility.md) | M5 |
| [Azure, Vertex, Bedrock, and compatible-vendor profiles](../../src/providers/) | M5 |
| [Custom endpoints/auth, mounted connectors, model facts, bulk workflows, policies, and routing preferences](../provider-routing.md) | M5 |
| [Images, audio, uploads, video jobs, historical credentials, and reconciliation](../../src/media/) | M6 |
| [Metrics, optional OTLP tracing, health, and worker diagnostics](../../src/observability/) | M1/M3 foundations; M6 complete |
| [Console access/settings](../../console/src/lib/features/access/), [providers](../../console/src/lib/features/providers/), [routes](../../console/src/lib/features/routes/), and [playground](../../console/src/lib/features/inference/) | M2, M3, and M5 alongside their APIs |
| [Console usage/history](../../console/src/lib/features/usage/), [media](../../console/src/lib/features/media/), [overview](../../console/src/lib/features/overview/), and [health](../../console/src/lib/features/runtime/) | M4 and M6 |
| [Compose/Helm](../../deploy/), [backup/restore and qualification scripts](../../scripts/), [CI/releases](../../.github/workflows/), and [operations](../operations.md) | M7 |

## Build and dependency scorecard

The [M1 measurement record](evidence/validation.md) contains timing samples,
commands, machine/cache details, and native build evidence. Dependency counts
below come from the frozen source.
There are 50 direct production dependencies in `Cargo.toml` and 467 resolved
package entries in `Cargo.lock`; the latter includes transitive and development
dependencies. API generation currently compiles the Rust exporter through
`make api`. [Build commands](../../Makefile), [manifest](../../Cargo.toml),
[lockfile](../../Cargo.lock)

| Measurement | Rust baseline | Go completion target or evidence |
| --- | --- | --- |
| Clean backend development build | 228.062 s median (226.766–234.552) | M1: 28.778 s (28.181–30.886); full-capability M7 target remains at most 50% of Rust |
| Rebuild after a small backend implementation edit | 21.121 s median (20.086–21.916) | M1: 4.145 s (4.025–4.232); full-capability M7 target remains at most 50% of Rust |
| Targeted behavioral test | Unmeasured | M1 SSE: 0.642 s median (0.635–0.680), including startup/link checks |
| API contract generation | Unmeasured; invokes Cargo | M1: 3.749 s median (3.216–5.510); no gateway compilation, services, or Rust |
| Console build, complete checks, integration, and release image build | Unmeasured | Record separately so backend gains do not conceal shifted work |
| Direct production dependency inventory | 50 Rust crates | Four direct library requirements; nine external modules imported by the current executable; [purposes and impact](evidence/foundation.md#dependency-and-native-inventory) |
| Transitive and development inventory | 467 resolved Cargo package entries in total | [Separate Go graphs and 425 native workspace lock entries](evidence/dependencies.json) |
| Native dependencies and artifact size | Unmeasured | amd64 image: 92,038,070 bytes; [GLIDE, glibc, archives and licenses](evidence/validation.md) |
| Ordinary development requires a Rust compiler | Yes | No |

M1 records the runner CPU, memory, OS/architecture, toolchains, commands, source
revisions, cache states, and at least five successful measurements per timing
case. Report medians and ranges. Prefetch dependencies and measure downloads
separately. A clean build uses an empty compilation cache; a warm rebuild uses
populated caches and a repeatable small implementation edit in a disposable
checkout. A no-op build is not the rebuild measurement. Include CGO linking.
Measure the backend binary alone and report API/console work separately.

The two build targets are initial acceptance targets, not measured speedup
claims. Re-run the same procedure against the complete M7 application.
The dependency inventory is reviewed after adding cloud connectors and
observability as well as at release: module counts across languages are not
equivalent measurements. Every additional runtime dependency needs a concrete
capability, a reason the standard library or an existing dependency does not
suffice, and recorded direct/transitive/native impact.

## Completion evidence

Reuse the language-neutral JSON/SSE corpus, official JavaScript SDK checks,
optional Python SDK checks, and browser journeys. Replace their Rust launchers
as the relevant Go implementation lands. Use disposable PostgreSQL, Valkey,
and fault-injection services for persistence and recovery scenarios.

Preserve the [production contracts](../production-guarantees.md): authority
expiry, accrued-cost budget semantics, metadata privacy, egress restrictions,
queue-loss visibility, and installation identity during restore. Deterministic
fixtures establish tested compatibility. Paid live-provider qualification
remains optional, credential-scoped, and separately recorded.

M7 requires the completed capability map, passing feature and process evidence,
both native image qualifications, fresh-install and restore journeys, the
completed build scorecard, and a working development/release path without the
Rust toolchain.
