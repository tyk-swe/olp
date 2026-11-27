# Qualification

The qualification lanes were subtracted with the rest of the test system.
Service and process suites, browser journeys, client and agent qualification,
SDK smoke suites and live-provider checks no longer run in CI or as release
gates. Their evidence, pins and reproduction commands remain in git history at
commit `99234cb0`, before commit `dbce24318`, scoped to the revisions that
produced them.

Pre-merge validation is now the unit, integration and end-to-end tiers in
[testing](../../tests/README.md) plus formatting, vet, lint, type checks,
contract generation and a full build. Release qualification still builds the
candidate image and runs the image mode and service smokes, the vulnerability
scan and a Compose quickstart smoke that creates the first owner through
`olp setup-token` and checks it survives a restart. The code-mode fixture lanes
below also run again as a release gate. Nothing else in this page describes a
check that still runs. Each section below records what its lane used to
establish and what, if anything, still covers part of it. Performance claims
follow [Performance](performance.md#performance-budget).

## Providers and token estimates

Paid live-provider checks no longer run, including the token comparison against
a live API. The token estimator still runs in the unit tier against its offline
oracle fixtures, which pin the reference encoder output without any provider
account. Provider certification through the management API remains a product
feature, and its former automated evidence is historical.

## Guardrail and capture qualification

Guardrail detection, masking and streaming behavior still run in the unit tier
through the guardrails package and the gateway slice. Against the real binary in
`tests/e2e`, `testGuardrailJourney` covers guardrail credentials, definitions,
revisions, policies, attachments, evaluation, synthetic fixture runs, retirement
and audit records, while its `testGuardrailPluginPins` helper verifies that a
confined guardrail plugin digest pinned by an active or retired guardrail
definition refuses uninstall with `plugin_pinned`. For capture,
`TestCapturePolicyExpiryLifecycle` in `tests/e2e/capture_journey_test.go`
exercises enabling capture, setting and updating capture policy expiry in UTC,
clearing expiry and verifying audit rows against the real binary. In the browser
tier, one Playwright journey builds a keyword guardrail and tries it. Broader
platform activation, ownership, migration, SDK journeys and browser checks for
capture no longer run, so validate changes in those areas by running the
affected functionality directly.

## Operator ecosystem

The former operator and ecosystem process suites no longer run. Management
contract generation and the in-process authorization golden matrix still run
pre-merge, so contract shape and route admission keep automated signal. The
end-to-end API tier in [testing](../../tests/README.md#end-to-end-tier) covers
port collision retries and split-mode listener boundaries, restricted runtime
database role grants and Valkey-less epoch recovery, first-run bootstrap setup,
origin and CSRF transport guards, route publication to durable usage, playground
streaming, management write idempotency, ETag preconditions and audit trails,
secured owner read and cross-project isolation sweeps with guardrail and
configuration promotion journeys, plus provider lifecycle, billing imports,
capture policy expiry, code accounts and budgets, route fixture comparisons,
identity writes, key overlap and pause, and SCIM provisioning. The browser
end-to-end tier also covers first-run setup, provider activation, route
publishing, sign-in, bookmarks, branding, API keys, usage, unary and streamed
playground requests and keyword guardrails. Other operator workflows against
real services have no automated coverage.

## Code-mode qualification

Fixture qualification lanes for code mode run again as a release gate. Run them
with `make qualify-code`, which builds the real `olp` binary, starts disposable
PostgreSQL and Valkey containers, and runs `tests/qualification` behind the
`qualification` build tag through `scripts/qualification.sh`. The lanes live
outside the three tiers in [testing](../../tests/README.md), so the pre-merge
budgets carry no timed result for this suite.

Each lane seeds one subscription code account, publishes a code route onto it
and drives one client journey end to end over HTTP against the real binary.
Codex uses `responses` through the `codex` adapter, Claude Code uses
`v1/messages` through the `zai_coding` coding-plan adapter, and OpenCode uses
`v1/chat/completions` through `opencode_go`. The driver runs each installed
client in isolation. The account, its credential and grant, and its plugin
provider are seeded directly in the database because grant enrollment needs a
real vendor account. The frozen connection keeps the adapter's own vendor
address, as production requires. Its network options route through a fixture
CONNECT proxy that terminates TLS for the vendor hostnames under a test CA and
serves the scripted vendor, which pins the upstream paths and credential headers
each adapter must produce. A regression in an adapter's endpoint, credential
placement or profile therefore fails its lane. The run needs outbound DNS for
the vendor hostnames because egress resolves the destination before it tunnels
through the proxy. Fixture evidence must never be presented as live acceptance
of a client, an adapter or a vendor service.

Every lane writes one run record as a JSON line, to `OLP_QUALIFY_REPORT` when
set and to stdout otherwise, and `scripts/qualification.sh` prints the report
before it cleans up, pass or fail. A record carries client, client version,
platform, adapter, profile, model, protocol, transport, result and evidence,
where evidence is always `fixture` in this suite. The adapter is the domain
adapter the lane exercises. The record also carries `driver` as `client-fixture`
and `client_executed` as `true` when the journey starts the client. A lane whose
client binary is unavailable or whose journey fails reports `no_signal` and
fails the run.

The fixture gate needs Linux, Python 3, Bubblewrap, Docker and native Codex,
Claude Code and OpenCode executables on `PATH`. Bubblewrap must be able to
create network and PID namespaces. `scripts/qualification-clients.sh <directory>`
installs the client versions pinned in
`tests/qualification/clients/pnpm-lock.yaml` and links their native Linux amd64
binaries into that directory, which then goes on `PATH`. The release workflow
sets up these prerequisites on Linux amd64. Each version probe and journey runs
in an isolated network namespace with a temporary home and a cleared environment.
A Unix socket connects the namespace only to the local OLP server, and the
upstream proxy serves scripted responses.

The Codex streaming fixture uses the event and message fields defined by the
OpenAI SDK for
[output item events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_output_item_added_event.py),
[content part events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_content_part_added_event.py),
[text delta events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_text_delta_event.py)
and
[output messages](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_output_message.py).

The release workflow installs the pinned Codex, Claude Code and OpenCode clients
with that script and runs `make qualify-code` as a gate on promotion. Fleet
process suites still do not run. Generated client configuration and protocol
translation keep partial signal through the unit tests. Behavior against real
subscription services is covered only by the live lanes below, and only where a
lane has actually run green.

### Live lanes

Live lanes run the same real binary against real vendor accounts and real vendor
upstreams. Run them with `make qualify-code-live`, which runs
`scripts/qualification-live.sh` and the live tests in `tests/qualification`
behind the `qualification` and `qualificationlive` build tags. Run
`make qualify-code` separately for the fixture lanes. There are ten required
live lanes, one per adapter and profile in stage one. Each lane reads its
account key and its native model from the environment variables in the table
below. Only the variable names appear in the repository and in workflow files.
Values are never printed, logged or committed, and run records and check details
name variables and status codes only.

| Adapter | Profile | Key variable | Model variable |
| --- | --- | --- | --- |
| Codex | `codex-subscription` | `OLP_LIVE_CODEX_AUTH_JSON` | `OLP_LIVE_CODEX_MODEL` |
| GLM Coding Plan | `zai-coding-plan` | `OLP_LIVE_GLM_GLOBAL_KEY` | `OLP_LIVE_GLM_GLOBAL_MODEL` |
| GLM Coding Plan | `bigmodel-coding-plan` | `OLP_LIVE_GLM_CN_KEY` | `OLP_LIVE_GLM_CN_MODEL` |
| MiniMax M Plan | `minimax-plan` | `OLP_LIVE_MINIMAX_GLOBAL_KEY` | `OLP_LIVE_MINIMAX_GLOBAL_MODEL` |
| MiniMax M Plan | `minimax-plan-cn` | `OLP_LIVE_MINIMAX_CN_KEY` | `OLP_LIVE_MINIMAX_CN_MODEL` |
| Kimi Code | `kimi-code` | `OLP_LIVE_KIMI_GLOBAL_KEY` | `OLP_LIVE_KIMI_GLOBAL_MODEL` |
| Kimi Code | `kimi-code-cn` | `OLP_LIVE_KIMI_CN_KEY` | `OLP_LIVE_KIMI_CN_MODEL` |
| Qwen Token Plan | `qwen-token-plan` | `OLP_LIVE_QWEN_GLOBAL_KEY` | `OLP_LIVE_QWEN_GLOBAL_MODEL` |
| Qwen Token Plan | `qwen-token-plan-cn` | `OLP_LIVE_QWEN_CN_KEY` | `OLP_LIVE_QWEN_CN_MODEL` |
| OpenCode Go | `opencode-go` | `OLP_LIVE_OPENCODE_GO_KEY` | `OLP_LIVE_OPENCODE_GO_MODEL` |

The Codex key variable holds the contents of the account `auth.json` document
described in the code mode guide. Every other key variable holds the coding plan
key for its profile. The optional `OLP_LIVE_CODEX_AUTH_JSON_2` variable holds a
second Codex account, which gives the route a recovery target for the failover
check. A model variable names one native model the account serves because vendor
model catalogues change outside this repository and no lane pins one.

Each lane creates its code account, pool and route through the management API
and publishes the route, then sends one real inference request through the
gateway to the adapter vendor address. There is no scripted vendor and no proxy
in the live path. The provider and grant rows are inserted in the database in
the form grant enrollment stores because headless plugin enrollment is outside
this harness, and the credential is encrypted under the test master key like any
enrolled grant. Dispatch, observation, the ledger and diagnostics all run in the
real binary against the real upstream. A missing client binary, a missing
variable or a failed setup reports `no_signal` for that lane, and a lane never
reports a pass it did not earn.

Every live record carries client, client version, platform, adapter, profile,
model, protocol, transport, result and evidence (where evidence is always
`live`), plus a checks list. The inference check passes only when the journey
returns success and attempt diagnostics show the settled attempt. The Codex
allowance check passes only when account diagnostics carry passive allowance
windows after inference, while active reads remain disabled pending live
qualification. Other adapters record `no_signal` for allowance in stage one. The
cooldown and failover checks pass only on evidence the lane actually observed in
diagnostics. The lane induces neither exhaustion nor rejection, so both normally
record `no_signal`, withholding the matching claim instead of passing it. A lane
result is `pass` only when inference passes and, for Codex, the allowance check
passes.

The last report line is a claims summary. It names the adapter and profile pairs
allowed to claim live inference, cooldown evidence and failover evidence from
this run. When lanes are missing or red, the matching lists are empty, and an
empty list means no claim. The full reliability gate requires every live lane to
execute its client, record its scope and pass inference, cooldown and failover
checks, and the Codex lane also needs its allowance check. The current live
lanes send protocol requests without executing the clients, so the full gate
cannot pass yet, while the report still preserves the scoped partial
observations. The script exits non-zero whenever this evidence is missing or a
required lane is red. The live report goes to `OLP_QUALIFY_LIVE_REPORT`, the
harness refuses a live report path that matches the fixture report path, and the
script fails if fixture evidence appears in the live report. Fixture evidence is
never substituted for a live record.

The `qualify-code-live` workflow runs on manual dispatch because push and pull
request runs have no account secrets. It maps repository secrets of the same
names to the variables above and runs `make qualify-code-live` with no step
allowed to continue on error. Dispatch it before making any reliability claim
for a release, and scope the claim to exactly the pairs the green run summary
names. A release with no green live run makes no such claim. The workflow is
also reusable through `workflow_call`. When the repository variable
`OLP_LIVE_QUALIFICATION` is set to `required`, the release workflow calls it and
forwards the live-lane secrets explicitly, and promotion needs it, so a red or
missing live run blocks the release. When the variable is unset, that release
job is skipped, claims remain fixture-scoped, and the `check-docs` guard in
`scripts/check-docs.mjs` enforces that scope. Specifically, the guard rejects
the hyphenated phrase `live-proven only`, the phrases `is live-proven`,
`is live proven`, `are live-proven` and `are live proven`, and the phrases
`live-proven ordering`, `live proven ordering`, `live-proven signal` and
`live proven signal` (case-insensitively outside code spans) in scoped
documentation, and its test applies the same rule to the code mode guide and
console copy. Unhyphenated definitional wording such as "counts as live proven
only when a lane runs green" stays allowed. Claiming live status for a pair
therefore takes a green live run and a deliberate change to the guard in the
same change.

### Latest official clients

The fixture lanes execute each installed official client and record its version
against scripted upstreams. The live lanes record client versions through the
same sandboxed version probe, so the live gate has the same Bubblewrap and
native executable prerequisites, and no client process can read the live account
secrets. The live lanes send the live requests through protocol fixtures, so
they do not qualify the client program itself against a real account. Client
version pins in user documentation describe configuration written against those
clients rather than a live qualification result. Requalifying a client against a
real account still means running it by hand against a local gateway and
recording the client version, platform, adapter, profile, model, protocol and
transport with the result.

## Historical qualification

Historical repair and qualification evidence stays in git history at commit
`99234cb0`, before commit `dbce24318`. Read it as a dated record for its stated
revision and scope. It does not qualify later revisions, and timing diaries from
that record are not current suite budgets. The current budgets are the tier
budgets in [testing](../../tests/README.md#budgets).
