# Behavioral validation

Pre-merge validation is one fast safety net. After `make setup`, run
`make test-fast`. The net is the explicit allowlist in
`scripts/test-fast.sh`, and the script fails when the run takes longer than
60 seconds of wall time. `make check` and `make check-fast` run formatting,
vet, lint, console types and formatting checks, contract generation where
applicable, and this same net. CI runs those gates as separate jobs and runs
the net in the `test-fast` job.

The net needs no containers, services, network, or provider accounts. Go
packages run with `-count=1`, so cached results never hide budget growth.
Console tests run through the installed pnpm workspace and need the
generated API types, which CI creates with
`pnpm --dir console api:generate` before the run.

## What the net contains

The Go allowlist covers authorization derivation and its golden file,
secret sealing and signing, billing and usage math, protocol translation,
token estimation against its oracle fixtures, catalog validation, limits,
route planning, guardrail detection, MCP registration validation, tool
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
Those areas now have no automated behavioral signal. In particular, nothing
in the net exercises PostgreSQL or Valkey behavior, cross-project isolation
at the service layer, backup and restore, browser rendering, or real client
and provider compatibility. The deleted suites remain available in git
history before commit `99234cb0`. Release image qualification still runs
the image mode and service smokes and the vulnerability scan through
`scripts/qualify-image.sh`.

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
exists outside the allowlist is a defect, because nothing runs it. A
shallow test that cannot fail gets rewritten as a behavior test or
deleted. No signal beats false signal.

## Focused runs

Run one allowlisted package or file directly while iterating, for example
`go test -count=1 ./internal/gateway` or
`pnpm --dir console test --project unit src/lib/features/usage/priceEntry.test.ts`.
Focused `.only` tests must never land. `make test-fast` is the only test
target, and it always runs the whole net.

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
