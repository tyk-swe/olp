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

Each lane seeds one subscription code account, publishes a code route
onto it, and drives one client journey end to end over HTTP against the
real binary. Codex uses `responses` through the `codex` adapter, Claude
Code uses `v1/messages` through the `zai_coding` coding-plan adapter,
and OpenCode uses `v1/chat/completions` through `opencode_go`. The
driver runs each installed client in isolation. The account,
its credential and grant, and its plugin provider are seeded directly in
the database, because grant enrollment needs a real vendor account. The
frozen connection keeps the adapter's own vendor address, as production
requires. Its network options route through a fixture CONNECT proxy
that terminates TLS for the vendor hostnames under a test CA and serves
the scripted vendor, which pins the upstream paths and credential
headers each adapter must produce. A regression in an adapter's
endpoint, credential placement, or profile therefore fails its lane.
The run needs outbound DNS for the vendor hostnames, because egress
resolves the destination before it tunnels through the proxy. Fixture
evidence must never be presented as live acceptance of a client, an
adapter, or a vendor service.

Every lane writes one run record as a JSON line, to
`OLP_QUALIFY_REPORT` when set and to stdout otherwise, and
`scripts/qualification.sh` prints the report before it cleans up, pass
or fail. A record carries client, client version, platform, adapter,
profile, model, protocol, transport, result, and evidence, where
evidence is always `fixture` in this suite. The adapter is the domain adapter the
lane exercises. The record also carries `driver` as `client-fixture` and
`client_executed` as `true` when the journey starts the client. A lane
whose client binary is unavailable or whose journey fails reports
`no_signal` and fails the run.

The fixture gate needs Linux, Python 3, Bubblewrap, Docker, and native
Codex, Claude Code, and OpenCode executables on `PATH`. Bubblewrap must be
able to create network and PID namespaces. npm installs wrapper scripts,
so expose the platform package binaries on `PATH` when using npm. The
release workflow sets up these prerequisites on Linux amd64. Each version
probe and journey runs in an isolated network namespace with a temporary
home and a cleared environment. A Unix socket connects the namespace only
to the local OLP server, and the upstream proxy serves scripted responses.

The Codex streaming fixture uses the event and message fields defined by
the OpenAI SDK for [output item events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_output_item_added_event.py),
[content part events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_content_part_added_event.py),
[text delta events](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_text_delta_event.py),
and [output messages](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_output_message.py).

The release workflow installs the latest Codex, Claude Code, and
OpenCode clients from npm and runs `make qualify-code` as a gate on
promotion. Fleet process suites still do not run. Generated client
configuration and protocol translation keep partial signal through the
allowlisted unit tests. Behavior against real subscription services is
covered only by the live lanes below, and only where a lane has actually
run green.

### Live lanes

Live lanes run the same real binary against real vendor accounts and
real vendor upstreams. Run them with `make qualify-code-live`, which
runs `scripts/qualification-live.sh` and the live tests in
`tests/qualification` behind the `qualification` and
`qualificationlive` build tags. Run `make qualify-code` separately for
the fixture lanes. There are ten required live lanes, one per
adapter and profile in stage one. Each lane reads its account key and
its native model from the environment variables in the table below.
Only the variable names appear in the repository and in workflow files.
Values are never printed, logged, or committed, and run records and
check details name variables and status codes only.

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

The Codex key variable holds the contents of the account `auth.json`
document described in the code mode guide. Every other key variable
holds the coding plan key for its profile. The optional
`OLP_LIVE_CODEX_AUTH_JSON_2` variable holds a second Codex account for
the band check. A model variable names one native model the account
serves, because vendor model catalogues change outside this repository
and no lane pins one.

Each lane creates its code account, pool, and route through the
management API and publishes the route, then sends one real inference
request through the gateway to the adapter vendor address. There is no
scripted vendor and no proxy in the live path. The provider and grant
rows are inserted in the database in the form grant enrollment stores,
because headless plugin enrollment is outside this harness, and the
credential is encrypted under the test master key like any enrolled
grant. Dispatch, observation, the ledger, and diagnostics all run in
the real binary against the real upstream. A missing client binary, a
missing variable, or a failed setup reports `no_signal` for that lane.
A lane never reports a pass it did not earn.

Every live record carries client, client version, platform, adapter,
profile, model, protocol, transport, result, and evidence, where
evidence is always `live`, plus a checks list. The inference check
passes only when the journey returns success and attempt diagnostics
show the settled attempt. The Codex allowance check passes only when
account diagnostics carry passive allowance windows after inference. Active
reads remain disabled pending live qualification.
Other adapters record `no_signal` for allowance in stage one. The band
check needs the second Codex account and passes only when a selection
decision carries allowance evidence. Without the second account it
records `no_signal`. The cooldown and failover checks pass only on
evidence the lane actually observed in diagnostics. The lane induces
neither exhaustion nor rejection, so both normally record `no_signal`,
and that record withholds the matching claim instead of passing it. A
lane result is `pass` only when inference passes and, for Codex, the
allowance check passes.

The last report line is a claims summary. It names the adapter and
profile pairs allowed to claim reset aware ordering, live inference,
cooldown evidence, and failover evidence from this run. Reset aware
ordering needs a passing lane with passing allowance and band checks,
so Codex is the only adapter that can earn it in stage one. When lanes
are missing or red the matching lists are empty, and an empty list
means no claim. The script exits non-zero and prints every lane that
is not green whenever any required lane is missing or red. The live
report goes to `OLP_QUALIFY_LIVE_REPORT`, the harness refuses a live
report path that matches the fixture report path, and the script fails
if fixture evidence appears in the live report. Fixture evidence is
never substituted for a live record.

The `qualify-code-live` workflow runs on manual dispatch, because push
and pull request runs have no account secrets. It maps repository
secrets of the same names to the variables above and runs
`make qualify-code-live` with no step allowed to continue on error.
Dispatch it before making any reset aware ordering or reliability
claim for a release, and scope the claim to exactly the pairs the
green run summary names. A release with no green live run makes no
such claim. The workflow is also reusable through `workflow_call`.
When the repository variable `OLP_LIVE_QUALIFICATION` is set to
`required`, the release workflow calls it with inherited secrets and
promotion needs it, so a red or missing live run blocks the release.
When the variable is unset, that release job is skipped, claims remain
fixture-scoped, and the `check-docs` guard in
`scripts/check-docs.mjs` enforces that scope. The guard rejects the
exact claiming phrases `live-proven only`, `is live proven`,
`are live proven`, and `live proven ordering`, plus their hyphenated
and band or signal variants, in scoped documentation, and its test
applies the same rule to the code mode guide and console copy.
Wording that defines the term without claiming it, such as counts as
live proven only when a lane runs green, stays allowed. Claiming live
status for a pair therefore takes a green live run and a deliberate
change to the guard in the same change.

### Latest official clients

The fixture lanes execute each installed official client and record its
version against scripted upstreams. The live lanes record client versions
but send the live requests through protocol fixtures, so they do not
qualify the client program itself against a real account. Client version
pins in user documentation describe configuration written against those
clients, not a live qualification result. Requalifying a client against a
real account still means running it by hand against a local gateway and
recording the client version, platform, adapter, profile, model, protocol,
and transport with the result.

## Historical qualification

Historical repair and qualification evidence stays in git history before
commit `99234cb0`. Read it as a dated record for its stated revision and
scope. It does not qualify later revisions, and timing diaries from that
record are not current suite budgets. The current budget is the safety
net budget in [testing](../../tests/README.md).
