# Behavioral validation

Tests run in three tiers. Each suite has its own command, its own requirements,
and a budget on the time it spends running tests. A test belongs in the cheapest
tier that can catch the failure it guards against.

| Command | Suite | Needs | Run budget |
| --- | --- | --- | --- |
| `make test-unit-go` | Every Go test without a build tag | Go | 30s |
| `make test-unit-console` | Vitest `unit` and `svelte` projects and the script contract tests | pnpm workspace | 30s |
| `make test-integration` | `tests/integration` | Docker | 30s |
| `make test-e2e-api` | `tests/e2e` | Docker | 45s |
| `make test-e2e-browser` | `console/tests/e2e` | Docker and Chromium | 60s |

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
it is the wall time of the runner. Compiling, building the console and starting
containers are printed next to it and are not enforced, because they follow
the cache of the machine and say nothing about the tests.

Each suite ends with one line such as
`integration: ran 4.9s of a 30s budget (7s wall with builds)` and fails when
the first number passes the second. Go suites run with `-count=1`, so cached
results never hide growth, and with `-shuffle=on`, so a test that leans on
another test's leftovers fails early.

## Unit tier

The unit tier needs no containers, services, network, or provider accounts.
Membership is automatic. Every Go `_test.go` file without a build tag runs, and
so does every file the Vitest `unit` and `svelte` projects match. Console tests
need the generated API types, which `pnpm --dir console api:generate` creates.

The Go tests cover authorization derivation and its golden file, secret sealing
and signing, notification destination-secret validation and signal state
boundaries, billing and usage math, protocol translation, token estimation
against its oracle fixtures, catalog validation, limits, route planning, agent
dispatch and continuation ownership, both A2A wire versions, publication
isolation, guardrail detection, MCP registration validation, tool pricing and
resolution, grant binding, a gateway slice for credential redaction, cost
reservation, cache replay and guardrail wiring, and the business metric
exposition in `internal/telemetry`. They also cover the provider egress policy
that keeps requests off private addresses, SCIM document, patch and filter
parsing, SAML response verification against a test identity provider, the
admission pool and its weighted queue, response cache keys and policy
validation, and secret resolution against a scripted Vault.

## Integration tier

`make test-integration` runs `tests/integration` behind the `integration` build
tag against disposable PostgreSQL and Valkey containers. Tests call project
packages in process, so this is the tier for behavior that only a real
database or a real Valkey can decide. It covers migrations applying
concurrently and refusing changed history, upgrades over legacy rows, usage
settlement and reports, budget notifications, FOCUS exports, code mode
decisions, cooldowns and allowances, the request, token, concurrency and cost
limit scripts under contention, budget exposure surviving a lost Valkey,
response cache isolation, freshness and purge, and SCIM filters and sorting
executed by PostgreSQL.

`tests/harness` gives every test its own state. `harness.Database` clones a
migrated template database, so a test pays for a copy instead of every
migration, and `harness.EmptyDatabase` hands migration tests a blank one.
`harness.Valkey` opens a client, and each test works under its own key
namespace.

## End-to-end tier

`make test-e2e-api` runs `tests/e2e` behind the `e2e` build tag. It builds the
real `olp` binary and drives it over HTTP, so requests cross the same
composition a deployment runs. Each test starts its own process on its own
database and ports, and provider calls go to the scripted vendor in
`tests/harness/vendor`. The suite covers startup failure cleanup, the process
running under the restricted runtime role, every secured management read
answering without a server error, cross-project isolation of those reads, one
guardrail definition, policy, attachment and credential lifecycle with its
audit trail, one promotion round trip, one request travelling from route
publication to durable usage, the code account endpoints, and a SCIM directory
provisioning, searching and removing users. The restore preflight regression
runs the operator script with Bash and Python 3 and requires no database client.

`make test-e2e-browser` builds the console, starts one `olp all` that serves
it, and runs the Playwright journeys in `console/tests/e2e` in Chromium. The
`setup` project is the first journey. It creates the owner through first-run
setup, connects and activates a provider, publishes a route, and saves the
signed-in session. The other journeys start from that session and run in
parallel. They sign in from a fresh browser, create an API key and call the
gateway with it until its usage shows up, send unary and streamed playground
requests, and build a keyword guardrail and try it. Journeys build their state
through the console and never seed it through the API, so the screens a new
operator meets first are the ones under test.

Install the browser once with
`pnpm --dir console exec playwright install chromium`. A failed journey leaves
a trace under `console/test-results`, and CI uploads it.

## Qualification lanes

`make qualify-code` and `make qualify-code-live` are further test targets.
They run `tests/qualification` in disposable Docker containers as release
gates rather than pre-merge validation. `make qualify-code` runs behind the
`qualification` build tag through `scripts/qualification.sh`.
`make qualify-code-live` runs behind the `qualification` and
`qualificationlive` build tags through `scripts/qualification-live.sh` and
needs real vendor account secrets from the environment. Both targets sit
outside the three tiers as the one stated exception to the defect rule below,
and neither carries a budget. The release
workflow runs `make qualify-code` before a release can be promoted, and it
runs `make qualify-code-live` as a promotion gate when the repository
variable `OLP_LIVE_QUALIFICATION` is set to `required`. What the lanes
establish is recorded in
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

Tests share no state. Every integration and end-to-end test calls
`t.Parallel()` and works on its own database, key namespace, process and
ports, and browser journeys create records under their own names. A test that
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
suites that were removed for speed remain in git history before commit
`99234cb0`.

## Focused runs

Use `make check-go`, `make check-console`, or `make check-contracts` for focused
static validation. Helm, dependency and release checks run separately in CI.

Run one package, file or test directly while iterating. Unit tests run with
`go test -count=1 ./internal/gateway` or
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
Secret-purpose changes surface in the generated tables in the security
architecture documentation. Regenerate those tables deliberately with
`go test ./internal/secrets -run TestPurposeDocumentation -update` after
reviewing the purpose change.

## Retained fixtures

The language-neutral corpus under `tests/fixtures` stays as conformance
evidence for the protocol, routing, and token tests. The
comparator in `tests/fidelity` stays because the protocol tests use it.
Neither is a test suite on its own.

`tests/fixtures/tokens` holds the ground truth for the token estimator in
`internal/operations/tokenization/estimate`. `generate.py` is the only
thing that writes those fixtures. It runs offline and is deterministic, so
a clean rerun leaves no diff.

```sh
uv sync --project tests/fixtures/tokens --frozen
uv run --project tests/fixtures/tokens --frozen python tests/fixtures/tokens/generate.py
```
