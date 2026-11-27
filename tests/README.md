# Behavioral validation

Tests run in three tiers. Each suite has its own command, its own requirements,
and a budget on the time it spends running tests. A test belongs in the cheapest
tier that can catch the failure it guards against.

| Command | Suite | Needs | Run budget |
| --- | --- | --- | --- |
| `make test-unit-go` | Every Go test without a build tag | Go | 30s |
| `make test-unit-console` | Vitest `unit` and `svelte` projects and `scripts/*.test.mjs` | pnpm workspace, Node.js, Python 3 | 30s |
| `make test-integration` | `tests/integration` | Docker, Go | 30s |
| `make test-e2e-api` | `tests/e2e` | Docker, Go with a C compiler, Node.js, `jq` | 45s |
| `make test-e2e-browser` | `console/tests/e2e` | Docker, Go with a C compiler, `pnpm` and Node.js, Chromium, Python 3, `jq`, `curl` | 60s |

`make test-unit` runs both unit suites side by side, `make test-e2e` runs both
end-to-end suites, and `make test` runs every tier with the cheapest first.
`make check` runs contract freshness, Go formatting, vet and lint, console
formatting, types and lint, then the unit tier. CI runs each suite as its own
job, named after its make target.

`make check-contracts` generates temporary outputs and compares all tracked
management, guardrail and inference artifacts with the working tree. It accepts
correct uncommitted generated changes and reports stale or missing files with
an instruction to run `make api`. `make check-console` regenerates ignored
console types before validating them. Checks may write ignored caches and build
metadata, but they do not rewrite tracked files.

## Budgets

A budget covers the time a suite spends running tests. For a Go suite that is
the sum of the package times `go test` reports, and for Vitest and Playwright
it is the wall time of the runner. Container startup, building the `olp` and
fixture vendor binaries, and building the console all run before
`scripts/lib/test.sh` records `began=$SECONDS`, so they stay outside both the
enforced run budget and the printed wall time. For Go suites, the printed wall
time covers `go test` package compilation plus execution and is not enforced,
because compile times follow the cache of the machine and say nothing about the
tests. For Vitest and Playwright, the printed wall time equals the runner time.

Each suite ends with one line such as
`integration: ran 8.5s of a 30s budget (10s wall with builds)` and fails when
the first number passes the second. Go suites run with `-count=1`, so cached
results never hide growth, and with `-shuffle=on`, so a test that leans on
another test's leftovers fails early.

## Unit tier

The unit tier needs no containers, services, network, or provider accounts.
Membership is automatic. Every Go `_test.go` file without a build tag runs,
every file matched by the Vitest `unit` and `svelte` projects runs, and every
`scripts/*.test.mjs` file runs via `node --test`. Console tests need the
generated API types, which `pnpm --dir console api:generate` creates.

The Go unit tests span `cmd/`, `plugin/`, `tests/harness`, and `internal/`
packages. They cover authorization derivation and its golden file, secret
sealing and signing, notification validation and signal boundaries, billing and
usage math, protocol translation, token estimation against its oracle fixtures,
catalog validation and drift tooling, limits, route planning, code mode
adapters and forwarding, agent dispatch and continuation ownership, A2A wire
versions, publication isolation, guardrail detection and previews, MCP
registration and tool pricing, grant binding, gateway execution kernel and
surfaces, staging spool behavior, promotion snapshot guards, telemetry metric
exposition, provider egress policy, SCIM parsing, SAML verification, admission
queuing, response caching including `internal/gateway/kernel/cache_test.go`,
secret resolution against a scripted Vault, operator CLI setup, and harness
startup failure cleanup.

The console unit suite separates pure logic tests (`*.test.ts` in the Node
`unit` project) from reactive Svelte component tests (`*.svelte.test.ts` in the
jsdom `svelte` project), backed by shared helpers in `console/tests/unit/support/`
(imported via `#test/*`). The script unit tests cover documentation checks,
release version parity, `dev.sh` port wiring, management operation generation,
configuration promotion review and comment helpers, `restore.sh` URL preflight
checks, and `secrets.sh` provisioning.

## Integration tier

`make test-integration` runs `tests/integration` behind the `integration` build
tag against disposable PostgreSQL and Valkey containers. Tests call project
packages in process, so this is the tier for behavior that only a real
database or a real Valkey can decide.

The suite is organized by domain across `tests/integration/`, covering
migrations concurrency and checksum history (`migrations_test.go`), provider
drafts, revisions, external secrets and grant enrollments (`providers_test.go`,
`providers_grants_test.go`), retained provider resources across OpenAI,
Anthropic, Gemini and vector stores (`resources_test.go`,
`resources_openai_test.go`, `resources_anthropic_gemini_test.go`,
`resources_vector_stores_test.go`), gateway workflows and health probes
(`gateway_workflows_test.go`), media and video jobs (`media_jobs_test.go`),
retrieval bindings (`retrieval_bindings_test.go`), MCP catalog and tool
settlement (`mcp_catalog_test.go`), usage ingest, retention, rollups,
comparisons and FOCUS exports (`usage_test.go`, `usage_comparison_test.go`,
`focus_exports_test.go`), billing imports and money ledger invariants
(`billing_test.go`, `money_test.go`), metering and export sink delivery
(`metering_delivery_test.go`), budget increases, limits, shared quotas and
traces (`budget_increase_requests_test.go`, `limits_test.go`,
`shared_quota_test.go`, `traces_test.go`), notifications and destination
testing (`notifications_test.go`, `notification_destination_test.go`,
`route_latency_silence_test.go`), code mode decisions, capacity, drain,
cooldowns and allowances (`code_test.go`, `code_capacity_test.go`,
`code_cooldowns_test.go`), response cache isolation and purge (`cache_test.go`),
and PostgreSQL-backed SCIM filtering and sorting (`scim_test.go`).

`tests/harness` gives every test its own state. `harness.Database` clones a
migrated template database, so a test pays for a copy instead of every
migration, and `harness.EmptyDatabase` hands migration tests a blank one.
`harness.Valkey` opens a client, and each test works under its own key
namespace. `harness.StartProcess` runs the built binary on free ports and
`harness.Service` drives it over HTTP, which `tests/e2e` and
`tests/qualification` share.

## End-to-end tier

`make test-e2e-api` runs `tests/e2e` behind the `e2e` build tag. It builds the
real `olp` binary and drives it over HTTP, so requests cross the same
composition a deployment runs. Each test starts its own process on its own
database and ports, and provider calls go to the scripted vendor in
`tests/harness/vendor`. The suite covers port collision retries and split-mode
listener boundaries (`harness_test.go`), restricted runtime role grants and
Valkey-less epoch recovery (`runtime_role_test.go`), route publication to
durable usage, bootstrap token waiting, origin and CSRF transport checks and
playground streaming (`journey_test.go`), management write idempotency, ETag
preconditions and audit trails (`management_writes_test.go`), secured read and
cross-project isolation sweeps with guardrail and configuration promotion
journeys (`sweep_test.go`, `guardrail_journey_test.go`,
`promotion_journey_test.go`), provider, billing, capture, code account, fixture,
identity, key overlap and pause, and SCIM journeys.

`make test-e2e-browser` builds the console, starts one `olp all` that serves
it, and runs the Playwright journeys in `console/tests/e2e` in Chromium. The
`setup` project is the first journey. It creates the owner through first-run
setup, connects and activates a provider, publishes a route, and saves the
signed-in session. The other journeys start from that session and run in
parallel. They sign in from a fresh browser, test installation bookmarks and
branding (restoring the original branding afterward), create an API key, call
the gateway with it until its usage shows up and save a key edit, send unary
and streamed playground requests, and build a keyword guardrail and try it.
Journeys build their state through the console and never seed it through the
API, so the screens a new operator meets first are the ones under test.

Install the browser once locally with
`pnpm --dir console exec playwright install chromium`. CI installs the headless
shell and its system dependencies with
`pnpm --dir console exec playwright install --with-deps --only-shell chromium`.
A failed journey leaves a trace under `console/test-results`, and CI uploads it.

## Qualification lanes

`make qualify-code` and `make qualify-code-live` are further test targets.
They run `tests/qualification` in disposable Docker containers as release
gates rather than pre-merge validation. `make qualify-code` runs behind the
`qualification` build tag through `scripts/qualification.sh`.
`make qualify-code-live` runs behind the `qualification` and
`qualificationlive` build tags through `scripts/qualification-live.sh` and
needs real vendor account secrets from the environment. Both targets sit
outside the three tiers as the one stated exception to the defect rule below,
and neither carries a budget. The release workflow runs `make qualify-code`
before a release can be promoted, and it runs `make qualify-code-live` as a
promotion gate when the repository variable `OLP_LIVE_QUALIFICATION` is set to
`required`. What the lanes establish is recorded in
[qualification](../docs/develop/qualification.md#code-mode-qualification).

The lanes are also the one stated exception to the rule that a test fails only
when project code changes. Each fixture lane runs an installed third-party
client in an isolated network namespace against the scripted upstream and
records the client version. A missing or failing client reports `no_signal`
and fails the run. Fixture results do not establish live vendor behavior.

## Test rules

A test asserts behavior, never structure or implementation. It must fail
when the feature breaks or is removed, and keep passing through any
refactor that preserves behavior. Break the code once while writing the test
and watch it fail. A test must only fail when project code
changes. Never pin a fact the project does not own as a literal or as a
must-be-absent assertion. Assert the invariant our code guarantees instead.
When an outside fact is truly load-bearing, cite its source and observation
date next to the assertion. Protocol fixtures, signed catalog data with
enforced provenance, and the token oracle fixtures are conformance evidence
under this exception.

Placement follows cost. A test that needs no service is a unit test and sits
beside its code. A test whose answer depends on PostgreSQL or Valkey is an
integration test. A test goes end to end only when the composition is the
behavior, such as process startup, a request crossing HTTP and authentication,
or a screen an operator uses. One journey per flow is enough, and the cases of
that flow belong in a cheaper tier.

A bug fix extends the existing mapped test file for the code it touches, with
one focused regression test that fails without the fix. Create a new test file
only for a new feature that has no mapped test yet, and follow the naming
convention of the directory.

Tests share no state. Every top-level integration and end-to-end test calls
`t.Parallel()` (enforced by `paralleltest`) and works on its own database, key
namespace, process and ports, and browser journeys create records under their
own names and restore any installation-wide setting they mutate. A test that
passes only alone or only in one order is a defect.

Budgets are not raised to admit a test. A new test must leave its suite inside
the budget with headroom. When a suite gets tight, move the test down a tier,
make it faster, or displace a weaker test, and state the timed result in the
change. A test file that no tier runs is a defect, because nothing runs it.
That means a Go test behind any build tag other than `integration` or `e2e`,
and a console test outside the Vitest projects and `console/tests/e2e`. The
qualification lanes are the stated exception, because `make qualify-code`
and `make qualify-code-live` and the release workflow run them. A shallow test
that cannot fail gets rewritten as a behavior test or deleted. No signal beats
false signal.

## What has no signal

Client and SDK compatibility outside the qualification lanes, benchmarks,
live-provider checks, backup and restore, race detection, and console rendering
beyond the retained Vitest files and the browser journeys have no automated
signal. Release image qualification still runs the image mode and service
smokes and the vulnerability scan through `scripts/qualify-image.sh`. Broader
suites that were removed for speed remain in git history at commit `99234cb0`,
before commit `dbce24318`.

## Focused runs

Use `make check-go`, `make check-console`, `make check-contracts`, or
`make check-docs` for focused static validation. Helm (`make check-helm`),
dependency (`make check-dependencies`), workflow, and release checks run
separately in CI.

Run one package, file or test directly while iterating. Unit tests run with
`go test -count=1 ./internal/gateway/...`,
`./scripts/test-unit.sh go ./internal/gateway/...`, or
`pnpm --dir console test --project unit src/lib/features/usage/priceEntry.test.ts`.
The service-backed scripts pass extra arguments to their runner, as in
`./scripts/test-integration.sh -run TestSCIM -v`,
`./scripts/test-e2e.sh api -run TestPublishedRoute -v`, and
`./scripts/test-e2e.sh browser guardrail`. Focused `.only` tests must never
land.

Authorization changes surface as diffs to
`internal/access/testdata/authorization.golden.json`, which records the
callers every management route admits, derived from the contract and the
policy, and is maintained by hand. Review such a diff as a security change.
Secret-purpose changes surface in the generated table in the security
architecture documentation. Regenerate that table deliberately with
`go test ./internal/platform/secrets -run TestPurposeDocumentation -update` after
reviewing the purpose change.

## Retained fixtures

The language-neutral corpus under `tests/fixtures` (embedded via
`tests/fixtures/fixtures.go` and `tests/fixtures/fidelity/fixtures.go`) stays as
reference data for the routing, stream, batch reader, protocol fidelity, vendor
batch and job payloads (`tests/fixtures/vendors`), and token tests. The
comparator in `tests/fidelity` stays because the protocol tests use it. Neither
is a test suite on its own.

`tests/fixtures/tokens` holds the ground truth for the token estimator in
`internal/domain/operations/tokenization/estimate`. `generate.py` is the only
thing that writes those fixtures. It runs offline and is deterministic, so
a clean rerun leaves no diff. `calibrate.py` is a manual calibration helper for
measuring vendor token-estimation ratios into the reference catalog.

```sh
uv sync --project tests/fixtures/tokens --frozen
uv run --project tests/fixtures/tokens --frozen python tests/fixtures/tokens/generate.py
```
