# Behavioral validation

After `make setup`, `make test` runs all container-free behavioral suites: Go
unit tests beside their features and the language-neutral protocol corpus under
`fixtures/`, console unit/component tests, and `scripts/*.test.mjs`. Failures
return a nonzero exit status. Keep valid expectations for unary/streaming
replies, malformed input, cancellation, timeouts, failover, egress, media and
pricing.

`make check` generates contracts, checks formatting, vet, console types and
lint, then runs these same suites once. CI uses
`make check GO_TEST_ARGS='-race -count=1'` so container-free Go tests run once
with race detection. `make test-race` remains available for local runs. Normal
Go runs retain caching.
Neither `make test` nor `make check` installs dependencies or starts services.
Go-only targets need Go and its native prerequisites, without Node or pnpm; see
[CONTRIBUTING.md](../CONTRIBUTING.md). Console tests also need Node.js 26 and
the installed pnpm workspace. They synchronize SvelteKit themselves, so no
prior console build is required. Focused `.only` tests fail validation.

Vitest uses `TZ=America/New_York` to exercise local-time and daylight-saving
behavior. Component tests use jsdom and browser exports.

## Focused runs

### Code-mode qualification

`./scripts/code-mode-qualification.sh cli` resolves official Codex, Claude Code
and OpenCode from npm `latest`, verifies their published SHA-512 values and runs
controlled Codex HTTP/SSE and WebSocket journeys without a provider account.
`install codex|claude-code|opencode` freshly resolves and prints one client's verified binary.
`source "$(./scripts/code-mode-qualification.sh resolve)"` resolves all three once
and exports shared binary paths and exact versions for both harnesses. Run metadata
and failures are retained under `.local/codecli/evidence/` and uploaded by CI. It passes only an OLP fixture key into an isolated client
home; local tool execution, continuation/resume and cancellation are exercised
by the real CLI. The fixtures are not live subscription qualification.

With integration PostgreSQL configured, public management checks run with:

```sh
go test -race -tags=integration -count=1 -timeout=5m -v \
  -run '^TestCodeQualificationPublic' ./tests/integration
```

`./scripts/code-mode-qualification.sh process` runs the real gateway fleet suite
through the supported transport, authorizer and enrollment fixture. It requires
`OLP_TEST_DATABASE_URL`, `OLP_TEST_VALKEY_URL` and
`OLP_TEST_BINARY`; missing prerequisites fail rather than skip. It uses the
additional `codecli` build tag, and includes the Claude Code and OpenCode
journeys through the OpenCode Go and GLM Coding Plan adapters, which also need
`OLP_CLAUDE_CODE_BINARY` and `OLP_OPENCODE_BINARY`. `all` runs both CLI and
process suites.

See [qualification evidence and integration prerequisites](../docs/develop/qualification.md)
for the exact upstream source/package pins and recorded qualification scope.
A successful compile is not a passing fleet test. These shell suites do not run
browser tests.

### Guardrails

`go test ./internal/guardrails ./internal/gateway -run 'Guardrail|Detector|PhaseAction|Streaming'`
checks detection, action/phase compatibility, masking, split streams, cancellation
and native errors.

With disposable PostgreSQL and Valkey, run the `integration`-tagged
`TestGuardrailPlatform`, `TestNamedGuardrail`, `TestContentPolicy` and
`TestConfigurationPromotion` tests for activation, ownership, migration and SDK
journeys. The normal management authorization and isolation sweeps include the
new management endpoints.

`pnpm --dir console exec playwright test --config playwright.guardrails.config.ts`
runs the keyboard, accessibility and no-storage browser checks against bounded
API fixtures. No provider account or persistent database is used by this check.
Live-vendor certification and the reference-hardware S3 load run remain release
qualification steps; microbenchmarks are development evidence only.

### Other focused suites

Run individual suites or pass runner arguments through Make:

```sh
make test-go GO_TEST_PACKAGES='./internal/protocols/... ./internal/gateway'
make test-go GO_TEST_PACKAGES=./internal/coordination GO_TEST_ARGS='-run TestConfiguration -v -count=1'
make test-console CONSOLE_TEST_ARGS='--project unit src/lib/features/inference/playground/browserContinuation.test.ts'
make test-scripts
make test-race GO_TEST_PACKAGES=./internal/gateway GO_TEST_TIMEOUT=10m
make test-go GO_TEST_PACKAGES='./internal/access ./internal/gateway' GO_TEST_ARGS='-race -count=10 -run "Test(MachinePrincipalResolution|ManagementTokenValidation|DuplexCancellationJoinsProviderObservationBeforeReturning|ForcedStreamFailuresAfterAcceptance)"'
make test-go GO_TEST_ARGS=-cover
```

Authorization, key-location, and secret-purpose changes surface as diffs to
golden files under `internal/*/testdata` and to the generated tables in
[security architecture](../docs/develop/security.md). Review such a diff as a security
change, then regenerate deliberately with
`go test ./internal/access ./internal/secrets -update`.

`GO_TEST_PACKAGES` defaults to `./...`, `GO_TEST_TIMEOUT` to `5m` per package,
and `GO_TEST_ARGS` and `CONSOLE_TEST_ARGS` to empty. Go tests use
`-mod=readonly`. Coverage reporting is optional; there is no numeric threshold.

## Integration

Install Chromium with
`pnpm --dir console exec playwright install --with-deps chromium`.

`make integration` provisions disposable TLS/authenticated PostgreSQL and
Valkey, then runs race-enabled process/service/provider/media scenarios,
official JavaScript SDKs, [client and agent qualification](#client-and-agent-qualification),
and Chromium journeys at packaged and Vite origins.
Packaged assets run the full feature and hosted-console journeys, followed by
replacement recovery into an empty database with a separate Valkey service.
Vite runs shell hydration and focused setup/login, cookie, CSRF mutation,
protected deep-link and unary/streaming inference proxy checks. Failure-path
restores assert that the destination stays empty.
Each test installation has an independent database and installation namespace.
Code-mode process tests run with a separate 25-minute suite deadline. The
remaining process tests retain their 30-minute deadline in three disjoint groups:
names starting `TestA` through `TestF`, `TestG` through `TestM`, and every other
test name. All exclude `TestCode`, which runs in the first phase. The four
selections cover the complete package, with the same build tags and race
detection. Separating the two alphabetic cohorts leaves room for intentional
recovery, enrollment and certification waits without extending operation or
package deadlines.

Service-dependent Go tests live in `tests/integration/` or beside feature code
and require the `integration` build tag. Internal service tests use the
`TestIntegration` prefix; the runner selects those explicitly with
`-run '^TestIntegration'` and runs the complete external integration package.
Container-free tests belong to `make check`, rather than being repeated by the
service runner. The trusted registry extension test runs in its own process.
Service tests fail with setup guidance when
their required service or recovery configuration is missing. Ordinary
`make test` does not select them, including when service environment variables
are set.

Standalone `pnpm --dir console test:e2e` requires the same service and secret
setup; use `make integration` to provision it automatically.

The Valkey cost of a priced request on a key with a cost budget, which is the
time the server spends inside the three cost scripts on the one thread every
replica shares, is measured by a benchmark that reports it as `valkey-us/op`. It
needs only a Valkey, named by `OLP_TEST_VALKEY_URL`:

```sh
go test -tags=integration -run '^$' -bench LimitsPricedRequest ./tests/integration
```

With that configuration already provided, focused browser selections are:

```sh
pnpm --dir console test:e2e --project=packaged tests/gateway/openai-route.spec.ts
pnpm --dir console test:e2e --project=vite
OLP_CONSOLE_E2E_PACKAGED=true pnpm --dir console exec playwright test --config playwright.journeys.config.ts
```

Feature prerequisites share owner authentication and management API helpers.
Owner cookies are cached only within a browser run's output directory, checked
against the backend before reuse, and renewed through the UI after revocation.

## Benchmarks

`make bench` runs the gateway benchmark scenarios of
[the performance guide](../docs/develop/performance.md) against a disposable
PostgreSQL and Valkey, a deterministic mock upstream and a real `olp all`
process, and writes one result per scenario to `.local/bench/`. The suites under
`tests/bench/` carry the `bench` build tag, so `make test-go` and
`make integration` do not run them. The harness's own tests, which need no
services, are what `make test-bench` runs; `make test` and `make check` include
it, so CI runs them with `-race`, and only the scenarios are left to
`make bench`. Like the integration suites, the scenarios fail, rather than skip,
when the services they need are not configured. Use `BENCH_SCENARIOS=S1,S2` and
`OLP_BENCH_SCALE=0.05` for a smoke run; its numbers are not reference numbers.
The [benchmark guide](bench/README.md) has the scenarios, the settings and how
to read a result, and `scripts/bench-compare.sh` runs the same scenarios against
LiteLLM; see the [performance guide](../docs/develop/performance.md).

The hot paths have `testing.B` benchmarks beside the code, in the packages the
[microbenchmark table](#microbenchmarks) lists, which `make bench-gate` compares
with the merge base and CI runs on every pull request. They are ordinary test
files, so `make test-go` compiles them and runs none of them.

## Suite ownership

Go unit tests own algorithms/protocol fixtures; internal service tests own
PostgreSQL/Valkey; external process suites own public API/SDK/recovery contracts.
Keep distinct security, billing, concurrency, cancellation and failure regressions.
Immutable plugin build artifacts
are shared per process; callers/runtimes receive independent mutable state.

Console helpers test problem/null decoding and bounded cursors; resource tests own
URL/query adaptation/cancellation. Mounted components, protected layouts and
browser deep links own routing behavior; do not replace them with source-import
assertions.

Run `./scripts/check-contracts.sh` standalone or with `--already-generated` after
`make check` to verify deterministic regeneration and checked-in Go artifacts.
Historical repair evidence is [recorded separately](../docs/develop/qualification.md#historical-qualification);
timing diaries are not current suite budgets.

## Token oracle fixtures

`fixtures/tokens/` holds the ground truth for the token estimator in
`internal/operations/tokenization/estimate`: the token ids OpenAI's reference
`tiktoken` produces for English prose, code, JSON and tool definitions,
multilingual text, identifiers, digits, whitespace, special-token text, long
unbroken runs, a 50 KB document and 320 seeded fuzz strings, for `o200k_base`
and `cl100k_base`, plus chat-message counts by the OpenAI Cookbook formula.
`TestOracleFixtures` requires every id to match, and the framing test the
cookbook counts.

`generate.py` is the only thing that writes them. `tiktoken` is pinned in
`pyproject.toml` and `uv.lock`, and the script serves it the rank bytes embedded
in the estimator, after checking them against the hashes tiktoken pins, so that
the oracle and the estimator cannot disagree about the data. It runs offline and
is deterministic, so a clean rerun leaves no diff:

```sh
uv sync --project tests/fixtures/tokens --frozen
uv run --project tests/fixtures/tokens --frozen python tests/fixtures/tokens/generate.py
```

## Microbenchmarks

The hot path is measured by `testing.B` benchmarks that sit beside the code in
ordinary `_test.go` files, with no build tag. They use in-memory fakes in place
of Valkey, PostgreSQL and the network, so they need no services. `make test`
compiles them and never runs them.

| Package | Benchmarks |
| --- | --- |
| `internal/runtime` | `BenchmarkAuthenticate` (key digest and lookup), `BenchmarkEligibility`, `BenchmarkPlanRequest` and `BenchmarkSelectSlots` (route planning and credential selection) |
| `internal/gateway` | `BenchmarkAdmission` (limit and budget reservation and settlement against a stateless limiter client), `BenchmarkStreamWriter`, `BenchmarkGateway`, the whole request path with the upstream and the client held in memory: unary, streamed, and an OpenAI stream translated for an Anthropic client, and `BenchmarkGatewayLargePrompt`, the same for the 100,000-token prompts of the high-throughput scenario |
| `internal/protocols` | `BenchmarkTranslateRequest`, `BenchmarkTranslateResponse` and `BenchmarkTranslateStream` for OpenAI Chat, Anthropic Messages and Gemini: each dialect to itself, as a route to a provider of the caller's own dialect is served, and each of Anthropic and Gemini to and from OpenAI Chat |
| `internal/operations/tokenization/estimate` | `BenchmarkEstimate` (a short prompt for every family) beside the encoder benchmarks, of which `BenchmarkMeter` and `BenchmarkHeuristic` measure the long prompts admission counts |
| `internal/usage`, `internal/plugins` | `BenchmarkCostBound` and `BenchmarkPriceCost`; `BenchmarkSign` |
| `internal/oif` | `BenchmarkPlainEnd` (the end of the plain run of a 405,000-byte JSON string, eight bytes at a time beside one at a time) |

```sh
go test -run='^$' -bench=. -benchmem ./internal/...
go test -run='^$' -bench='BenchmarkAdmission|BenchmarkPlanRequest' -benchmem -count=10 ./internal/gateway ./internal/runtime
```

`-run='^$'` keeps the unit tests out of the run, and `-benchmem` reports bytes
and allocations per operation. The first command spends about three minutes in
the benchmarks at the default `-benchtime` of one second, and a minute and a
half at `-benchtime=500ms`, on an 8-vCPU machine, with up to a minute more for
`go test` to vet and link the packages that have none. Ten counts of it take ten
times as long. A third of that is in the estimate package, in four benchmarks
that measure the encoder itself rather than what a request pays: `BenchmarkLoad`
is its first use, `BenchmarkCount` is a whole prompt of 50K and 100K tokens
counted exactly where admission counts a bounded part, and `BenchmarkUnbrokenPieces`
and `BenchmarkMeterWorstCase` are input chosen to be slow. The hot path is
everything else, which skipping them selects, and which takes about two minutes
at the default `-benchtime` and about 80 seconds at `-benchtime=500ms`:

```sh
go test -run='^$' -skip='^(BenchmarkLoad|BenchmarkCount|BenchmarkUnbrokenPieces|BenchmarkMeterWorstCase)$' -bench=. -benchmem ./internal/...
```

A benchmark that is added later is in that selection unless it is named in the
skip. Name the package and benchmark of the code you changed to get an answer
sooner. A benchmark checks once, before it is timed, that it exercises what its
name says, such as an admitted request, a plan that carries the prices it was
given or a stream that ended, so a change that turns one into an error path, or
into a case that is no longer the one it names, fails it instead of making it
fast.

The performance budget of [Performance](../docs/develop/performance.md)
is that a change must not slow a hot-path benchmark, or add allocations to it,
by more than 10%. Compare a change with its merge base by running the same
benchmarks on both with `-count=10` and giving the two outputs to
[`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat), which marks
a difference only when it is statistically significant:

```sh
skip='^(BenchmarkLoad|BenchmarkCount|BenchmarkUnbrokenPieces|BenchmarkMeterWorstCase)$'
git switch --detach "$(git merge-base origin/main HEAD)"
go test -run='^$' -skip="$skip" -bench=. -benchmem -count=10 ./internal/... > base.txt
git switch -
go test -run='^$' -skip="$skip" -bench=. -benchmem -count=10 ./internal/... > head.txt
go run golang.org/x/perf/cmd/benchstat@latest base.txt head.txt
```

Time on a shared machine is noisy. Across ten counts the codec, planning and
admission benchmarks have a standard deviation of 6 to 15% of their mean, and the
token counts 10 to 30%, so repeat a suspect time on an idle machine, pinned to
one CPU with `taskset -c 5` where there is more than one: in a trial that took
the token counts from 10 to 20% down to under 10%. Allocations per operation
are exact for most benchmarks and vary by well under 1% for the streams, and
bytes per operation by about as much, so read them first.

The budget says that a feature that is not configured adds no allocations and
no Valkey or PostgreSQL round trip to a request. Unconfigured-admission
allocation accounting is covered by benchmark and integration evidence rather
than a dedicated container-free unit test. The read of the shared cooldown that
precedes each attempt when a limiter is configured costs one Valkey round trip
an attempt whatever the key and the target limit.

## SDK and live-provider tests

```sh
./tests/sdk-smoke/run.sh
./tests/sdk-smoke-python/run.sh
```

Both SDK launchers use `tests/sdkfixture`, disable retries and exercise native
OpenAI, Anthropic and Gemini success/typed-error contracts. Python is optional
and uses Python 3.14 with the pinned uv project. Recovery journeys additionally
call the restored gateway through the official OpenAI SDK.

Paid provider checks require explicit credentials and are excluded from normal
CI. Set `OLP_LIVE_PROVIDER` and run
`go test -tags=liveproviders -count=1 ./internal/connectors`, or dispatch the
main-branch `live-providers` workflow. Its configuration lists required secrets
and cloud identity variables. Live calls consume provider quota.

## Client and agent qualification

[Pinned clients](../docs/use/clients.md) run real headless CLIs/frameworks/Go SDKs
against deterministic gateway/upstream fixtures, asserting client results and
recorded requests. This does not qualify paid providers.

```sh
pnpm install --frozen-lockfile
tests/clients/run.sh
tests/clients/run.sh --list
CLIENTS=claude-code,codex tests/clients/run.sh
```

Requires Go, Node 26, jq/curl, not containers/database/Valkey. Each suite gets an
empty environment, isolated home/XDG, telemetry off and loopback-only proxy.
Missing, skipped, empty or unfinished tests fail unless the runner table declares
an explicit open item. Startup/suite/test deadlines default 60/900/180 seconds;
signals stop child clients. Go SDK calls additionally have individual deadlines.

`make integration` runs these lanes. `make check` covers fixture/harness/doc-pin
checks; the runner vets its separate Go SDK module. Fix exposed incompatibility
in gateway behavior with a regression, not a client workaround.
See [harness ownership](clients/README.md) and
[client limits](../docs/use/clients.md#open-items).

## Contract and browser coverage

The contract test in `internal/access` checks that every operation in the
embedded OpenAPI contract carries truthful requirements, and the isolation
sweep reads every operation as members of another project and fails on any
disclosure of a canary project. `internal/access/testdata/authorization.golden.json`
still gates generated management clients and MCP discovery. A test's existence
does not prove it ran; use CI results for the commit you are checking. Browser
journeys cover the hosted console and replacement recovery through deterministic
local providers, not paid accounts.

### Response caching

`go test ./internal/cache ./internal/gateway -run 'Cache|Sealed|Canonical'`
checks canonical identity, sealing, controls, native replay and key-policy opt-out.
The `integration`-tagged `TestIntegrationCache*`,
`TestIntegrationSemanticCache*` and `TestResponseCache*` tests use
`OLP_TEST_CACHE_VALKEY_URL` for a separate Valkey Search service, alongside the
normal PostgreSQL and coordination Valkey variables. `scripts/integration.sh`
provisions all three services and runs these tests. The hosted Chromium suite
also exercises cache configuration, activation, miss/hit, purge and accessibility.

To measure the cache path against its uncached counterpart on the same machine:

```sh
go test -tags=integration ./internal/gateway -run '^$' \
  -bench '^BenchmarkIntegrationResponseCache$' -benchtime=1s -count=6
```

Run latency comparisons without other builds or load generators competing for
CPU. The output includes `p95-ns/op`; the cache budget allows at most 1 ms of
additional p95 latency. The ordinary `BenchmarkGatewayResponseCache` uses an
in-memory backing store and is included in the regression gate.
