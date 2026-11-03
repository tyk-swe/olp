# Qualification

The qualification lanes were subtracted with the rest of the test system.
Service and process suites, browser journeys, client and agent
qualification, SDK smoke suites, and live-provider checks no longer run in
CI or as release gates. Their evidence, pins, and reproduction commands
remain in git history before commit `99234cb0`, scoped to the revisions
that produced them.

Pre-merge validation is now the fast safety net in
[testing](../../tests/README.md) plus formatting, vet, lint, type checks,
contract generation, and a full build. Release qualification still builds
the candidate image and runs the image mode and service smokes and the
vulnerability scan. Nothing else in this page describes a check that still
runs. Each section below records what its lane used to establish and what,
if anything, still covers part of it. Performance claims follow
[Performance](performance.md#performance-budget).

## Providers and token estimates

Paid live-provider checks no longer run, including the token comparison
against a live API. The token estimator still runs in the safety net
against its offline oracle fixtures, which pin the reference encoder
output without any provider account. Provider certification through the
management API remains a product feature. Its former automated evidence
is historical.

## Guardrail and capture qualification

Guardrail detection, masking, and streaming behavior still run in the
safety net through the guardrails package and the reduced gateway slice.
Platform activation, ownership, migration, SDK journeys, and browser
checks for guardrails and capture no longer run. Validate changes in
those areas by running the affected functionality directly.

## Operator ecosystem

The operator and ecosystem process suites no longer run. Management
contract generation and the authorization golden still run pre-merge, so
contract shape and route admission keep automated signal. End-to-end
operator workflows against real services have no automated coverage.

## Code-mode qualification

Controlled client journeys, fleet process suites, and coding-plan adapter
journeys no longer run. Generated client configuration and protocol
translation keep partial signal through the allowlisted unit tests.
Behavior against real coding clients and subscription services has no
automated coverage, and fixture results never established it in the first
place.

### Latest official clients

No job resolves or runs the official coding clients. Client version pins
that remain in user documentation describe configuration that was written
against those clients, not a current qualification result. Requalifying a
client means running it by hand against a local gateway and recording the
client version, platform, adapter, model, protocol, and transport with
the result.

## Historical qualification

Historical repair and qualification evidence stays in git history before
commit `99234cb0`. Read it as a dated record for its stated revision and
scope. It does not qualify later revisions, and timing diaries from that
record are not current suite budgets. The current budget is the safety
net budget in [testing](../../tests/README.md).
