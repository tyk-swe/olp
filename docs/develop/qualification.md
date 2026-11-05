# Qualification

The qualification lanes were subtracted with the rest of the test system.
Service and process suites, browser journeys, client and agent
qualification, SDK smoke suites, and live-provider checks no longer run in
CI or as release gates. Their evidence, pins, and reproduction commands
remain in git history before commit `99234cb0`, scoped to the revisions
that produced them.

Pre-merge validation is now the fast safety net and the small service suite in
[testing](../../tests/README.md) plus formatting, vet, lint, type checks,
contract generation, and a full build. Release qualification still builds
the candidate image and runs the image mode and service smokes and the
vulnerability scan. The code-mode fixture lanes below also run again as a
release gate. Nothing else in this page describes a check that still runs.
Each section below records what its lane used to establish and what, if
anything, still covers part of it. Performance claims follow
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

Fixture qualification lanes for code mode run again as a release gate.
Run them with `make qualify-code`, which builds the real `olp` binary,
starts disposable PostgreSQL and Valkey containers, and runs
`tests/qualification` behind the `qualification` build tag through
`scripts/qualification.sh`. The lanes live outside the fast and service
allowlists in [testing](../../tests/README.md). Restoring them changed
neither allowlist, so the pre-merge budgets carry no timed result for
this suite.

Each lane publishes a code route onto a scripted upstream and drives one
client journey end to end over HTTP. Codex uses `responses`, Claude Code
uses `v1/messages`, and OpenCode uses `v1/chat/completions`. The driver
is a protocol fixture that speaks each client path against the real
binary. Subscription adapters pin their vendor hosts in code, so a
scripted upstream cannot stand in for them, and the fixture routes
therefore serve their models through published metered targets. The
lanes qualify the code route journeys on current behavior. They do not
qualify subscription accounts, and fixture evidence must never be
presented as live acceptance of a client, an adapter, or a vendor
service.

Every lane writes one run record as a JSON line, to
`OLP_QUALIFY_REPORT` when set and to stdout otherwise. A record carries
client, client version, platform, adapter, model, protocol, transport,
result, and evidence, where evidence is always `fixture` in this suite.
The client version comes from the installed client binary. A lane whose
client binary is unavailable reports `no_signal` and fails the run. It
never reports a pass.

Fleet process suites and coding-plan subscription adapter journeys
still do not run. Generated client configuration and protocol
translation keep partial signal through the allowlisted unit tests.
Behavior against real coding clients and subscription services has no
automated coverage.

### Latest official clients

The fixture lanes record the version of each installed official client
but do not execute it. Client version pins that remain in user
documentation describe configuration that was written against those
clients, not a live qualification result. Requalifying a client against
a real account means running it by hand against a local gateway and
recording the client version, platform, adapter, model, protocol, and
transport with the result.

## Historical qualification

Historical repair and qualification evidence stays in git history before
commit `99234cb0`. Read it as a dated record for its stated revision and
scope. It does not qualify later revisions, and timing diaries from that
record are not current suite budgets. The current budget is the safety
net budget in [testing](../../tests/README.md).
