# Behavioral validation

Pre-merge validation is one fast safety net and one small service suite. After
`make setup`, run `make test-fast`. The net is the explicit allowlist in
`scripts/test-fast.sh`, and the script fails when the run takes longer than
60 seconds of wall time. `make check` runs contract freshness, Go formatting,
vet and lint, console formatting, types and lint, then this same net. CI uses
the same focused checks in separate jobs and runs the net in the `test-fast` job.
The [service suite](#service-suite) needs Docker and runs in the `test-service`
job.

`make check-contracts` generates temporary outputs and compares all tracked
management, guardrail and inference artifacts with the working tree. It accepts
correct uncommitted generated changes and reports stale or missing files with
an instruction to run `make api`. `make check-console` regenerates ignored
console types before validating them. Checks may write ignored caches and build
metadata, but they do not rewrite tracked files.

The net needs no containers, services, network, or provider accounts. Go
packages run with `-count=1`, so cached results never hide budget growth.
Console tests run through the installed pnpm workspace and need the
generated API types, which CI creates with
`pnpm --dir console api:generate` before the run.

## What the net contains

The Go allowlist covers authorization derivation and its golden file,
secret sealing and signing, billing and usage math, protocol translation,
token estimation against its oracle fixtures, catalog validation, limits,
route planning, agent dispatch and continuation ownership, both A2A wire versions,
publication isolation, guardrail detection, MCP registration validation, tool
pricing and resolution, grant binding, and a reduced gateway slice for
credential redaction, cost reservation, cache replay, and guardrail wiring.
The gateway slice is the files `gateway_test.go`,
`caller_credentials_test.go`, `cache_test.go`,
`cost_reservation_test.go`, `credential_redaction_test.go`, and
`guardrails_test.go` in `internal/gateway`, with shared helpers kept in
`gateway_test.go` and `cache_test.go`. The console allowlist is the Vitest
`unit` and `svelte` projects. The script allowlist is the documentation contract and the
release version checks.

Everything else was subtracted for speed, including the service and process
integration suites, browser journeys, client and code-CLI qualification,
SDK smoke suites, benchmark scenarios and harness, live-provider checks,
component tests, race and ratchet targets, and the tests of that machinery.
Apart from what the service suite and the qualification lanes restore, those
areas have no automated behavioral signal. In particular, nothing exercises
backup and restore, browser rendering, or real client and provider
compatibility. The deleted suites remain
available in git history before commit `99234cb0`. Release image qualification
still runs the image mode and service smokes and the vulnerability scan through
`scripts/qualify-image.sh`.

## Service suite

`make test-service` runs `tests/service` behind the `service` build tag. It
starts disposable PostgreSQL and Valkey containers, builds the real `olp`
binary, and drives it over HTTP, so requests cross the same composition a
deployment runs. Each test gets its own database and process.

The suite covers what the net cannot reach without a database: migrations
applying concurrently and refusing changed history, the process running under
the restricted runtime role, every secured management read answering without a
server error, cross-project isolation of those reads, and one request travelling
from route publication to durable usage.

`scripts/test-service.sh` fails when the tests take longer than its budget. The
budget and admission rules match the net: a test enters only when the suite
still fits with headroom, or when it displaces a weaker test, and the change
states the timed result. Add a service test only for behavior that needs a real
database or Valkey, and keep everything else in the net.

## Qualification lanes

`make qualify-code` and `make qualify-code-live` are further test targets.
They run `tests/qualification` in disposable Docker containers as release
gates rather than pre-merge validation. `make qualify-code` runs behind the
`qualification` build tag through `scripts/qualification.sh`.
`make qualify-code-live` runs behind the `qualification` and
`qualificationlive` build tags through `scripts/qualification-live.sh` and
needs real vendor account secrets from the environment. Both targets sit
outside the fast and service allowlists as the one stated exception to the
defect rule below, and neither carries a wall-time budget. The release
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
refactor that preserves behavior. A test must only fail when project code
changes. Never pin a fact the project does not own as a literal or as a
must-be-absent assertion. Assert the invariant our code guarantees instead.
When an outside fact is truly load-bearing, cite its source and observation
date next to the assertion. Protocol fixtures, signed catalog data with
enforced provenance, and the token oracle fixtures are conformance evidence
under this exception.

Placement follows the mapped file rule inside the allowlist. A bug fix
extends the existing mapped test file for the code it touches, with one
focused regression test that fails without the fix. Create a new test file
only for a new feature that has no mapped test yet, and follow the naming
convention of the directory.

Admission to the net is budgeted. A new test or package enters
`scripts/test-fast.sh` only when the whole net still fits the 60 second
budget with headroom, or when it displaces a weaker test already in the
net. State the timed result in the change that admits it. A test file that
exists outside the allowlist is a defect, because nothing runs it. The
qualification lanes are the stated exception, because `make qualify-code`
and `make qualify-code-live` and the release workflow run them. A shallow test that cannot fail gets rewritten
as a behavior test or deleted. No signal beats false signal.

## Focused runs

Use `make check-go`, `make check-console`, or `make check-contracts` for focused
static validation. Helm, dependency and release checks run separately in CI.

Run one allowlisted package or file directly while iterating, for example
`go test -count=1 ./internal/gateway` or
`pnpm --dir console test --project unit src/lib/features/usage/priceEntry.test.ts`.
Focused `.only` tests must never land. `make test-fast`, `make test-service`,
`make qualify-code`, and `make qualify-code-live` are the only test targets,
and each always runs its whole suite.

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
evidence for the allowlisted protocol, routing, and token tests. The
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
