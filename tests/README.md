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

Run individual suites or pass runner arguments through Make:

```sh
make test-go GO_TEST_PACKAGES='./internal/protocols/... ./internal/gateway'
make test-go GO_TEST_PACKAGES=./internal/coordination GO_TEST_ARGS='-run TestConfiguration -v -count=1'
make test-console CONSOLE_TEST_ARGS='--project unit src/lib/format.test.ts'
make test-scripts
make test-race GO_TEST_PACKAGES=./internal/gateway GO_TEST_TIMEOUT=10m
make test-go GO_TEST_PACKAGES='./internal/access ./internal/gateway' GO_TEST_ARGS='-race -count=10 -run "Test(AccessBodyReadDeadline|IncompleteBodies|CompletedUpload|UnreadUnary|RealtimeBoundedWriter|MediaStreamCancellationAndDeadline)"'
make test-go GO_TEST_ARGS=-cover
```

Authorization, key-location, and secret-purpose changes surface as diffs to
golden files under `internal/*/testdata` and to the generated tables in
[security architecture](../docs/security.md). Review such a diff as a security
change, then regenerate deliberately with
`go test ./internal/access ./internal/secrets ./internal/gateway -update`.

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
pnpm --dir console test:e2e --project=packaged tests/gateway/accounting.spec.ts
pnpm --dir console test:e2e --project=vite
OLP_CONSOLE_E2E_PACKAGED=true pnpm --dir console exec playwright test --config playwright.journeys.config.ts
```

Feature prerequisites share owner authentication and management API helpers.
Owner cookies are cached only within a browser run's output directory, checked
against the backend before reuse, and renewed through the UI after revocation.
Setup, authentication, admission retry, invitation and provider/route onboarding
journeys continue to exercise their real UI flows.

## Benchmarks

`make bench` runs the gateway benchmark scenarios of
[the M1 roadmap](../docs/roadmap/m01-measured-advantage.md) against a disposable
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
LiteLLM; see the [performance guide](../docs/performance.md).

The hot paths have `testing.B` benchmarks beside the code
(in `internal/runtime`, `internal/protocols`, `internal/gateway` and
`internal/plugins`), which `make bench-gate` compares with the merge base and CI runs on every
pull request. They are ordinary test files, so `make test-go` compiles them and
runs none of them.

## Suite ownership

Go unit tests own algorithms and protocol fixtures; internal service tests own
database/Valkey behavior, and the external process suite owns API, SDK and
recovery contracts. Keep distinct security, billing, concurrency, protocol,
failure and recovery regressions at these boundaries.

Socket deadline tests keep real incomplete uploads, unread responses, admission
release and keep-alive checks. Gateway writer wrappers assert the requested
production deadlines before shortening them to 250 ms; access tests use their
existing timeout parameter. Five-second failure guards bound stalled tests.
Plugin fixture builds share immutable artifacts per test process, keyed by
package, target, build flags and linker flags. Each caller gets independent
module bytes or an executable copy; runtimes and mutable state stay isolated.

Console HTTP helper tests own common problem decoding and missing/null response
cases. Cursor helper tests own collection bounds and cursor cycles, using small
values rather than serializing thousands of resource records. Resource API
tests retain URL/query mapping, response adaptation and cancellation coverage.
The 29 filesystem source-import assertions were removed: mounted components, protected
layouts and browser deep links cover routing behavior. The bounded-writer test
uses one slow peer fixture because the former client/provider labels exercised
the same function. Security architecture guards and protocol fixtures remain.

Contract verification uses `./scripts/check-contracts.sh` standalone, or
`./scripts/check-contracts.sh --already-generated` after `make check`. The latter
compares that generation with one more pass, retaining determinism and the
checked-in Go contract check with two total CI generation passes.

### Timing comparison

Measured on the same local runner with uncached Go tests (`-count=1`), normal
Vitest settings, and one Chromium worker with no retries:

| Suite | Before | After |
| --- | ---: | ---: |
| Container-free Go (`make test-go GO_TEST_ARGS=-count=1`) | 52.2 s | 27.5 s |
| Console (Vitest duration) | 20.9 s, 816 tests | 20.3 s, 786 tests |
| External process package (`-race -tags=integration,oidctest,pythonsdk -count=1`) | 26.6 min | 23.5 min |
| Feature browsers | 11.8 min, 44 tests | 4.5 min, 24 tests |
| Browser provisioning, build, feature/hosted journeys and replacement recovery | 17.9 min | 7.8 min |

Browser comparisons use fresh databases and Valkey services; the earlier full
feature, hosted and recovery runs covered both origins. The updated run uses
full packaged coverage and focused Vite smoke checks. Qualification used
disk-backed temporary directories after shared `/tmp` space exhausted a
linker and the baseline media spool. That unchanged capacity test passed on
retry. The external process package, all 32 selected internal service tests,
the separate extension process, SDK smoke, and browser/recovery stages passed;
the final browser stages were rerun separately after fixing their helpers.
Deadline and cancellation tests additionally passed ten race-enabled repeats,
and the concurrent log-budget fixture passed 100 repeats.

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
| `internal/gateway` | `BenchmarkAdmission` (limit and budget reservation and settlement against a stateless limiter client), `BenchmarkStreamWriter`, and `BenchmarkGateway`, the whole request path with the upstream and the client held in memory: unary, streamed, and an OpenAI stream translated for an Anthropic client |
| `internal/protocols` | `BenchmarkTranslateRequest`, `BenchmarkTranslateResponse` and `BenchmarkTranslateStream` for OpenAI Chat, Anthropic Messages and Gemini: each dialect to itself, as a route to a provider of the caller's own dialect is served, and each of Anthropic and Gemini to and from OpenAI Chat |
| `internal/operations/tokenization/estimate` | `BenchmarkEstimate` (a short prompt for every family) beside the encoder benchmarks, of which `BenchmarkMeter` and `BenchmarkHeuristic` measure the long prompts admission counts |
| `internal/usage`, `internal/plugins` | `BenchmarkCostBound` and `BenchmarkPriceCost`; `BenchmarkSign` |

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
at the default `-benchtime` and one at `-benchtime=500ms`:

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

The performance budget of [M1](../docs/roadmap/m01-measured-advantage.md#performance-budget)
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
no Valkey or PostgreSQL round trip to a request. An ordinary unit test holds the
admission side to it, so `make test` does: `TestUnconfiguredFeaturesAddNoAllocations`
in `internal/gateway` counts allocations with `testing.AllocsPerRun`, and the
commands a counting limiter client was sent, for the admission and settlement of
a key without limits or a cost budget and of a provider target without a quota.
The request of the key has a price list to be estimated from, so that only the
guard of the cost budget spares it the estimate. The test then admits a key and a
target that are limited and expects the key to allocate and each to send its
command, and estimates the same request for a key with a cost budget and expects
that to allocate, to prove the zeros are not a request that never reaches the
limiter or one that nothing could price. The test counts the whole process's
allocations, so it does not run in parallel. It does not cover the read of the shared cooldown
that precedes each attempt when a limiter is configured, which costs one Valkey
round trip an attempt whatever the key and the target limit.

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

The SDK smoke suites check the native contracts. Client qualification checks
the software operators actually deploy: pinned releases of Claude Code, Codex
CLI and Gemini CLI run headless, the OpenAI Agents SDK, Vercel AI SDK, LangChain
and LlamaIndex.TS, and the official OpenAI, Anthropic and Google Go SDKs. Each
runs against a gateway whose upstream is a deterministic scripted fixture, and
each test asserts both the client's own outcome and the requests the upstream
recorded, so a client that bypasses OLP, or a gateway that mangles a request,
fails.

```sh
pnpm install --frozen-lockfile                       # the npm clients, once
tests/clients/run.sh                                 # every suite, in table order
CLIENTS=claude-code,codex tests/clients/run.sh       # only these
tests/clients/run.sh --list                          # the suite names
```

The runner needs Go, Node.js 26, `jq` and `curl`, and no containers, database
or Valkey: it builds `tests/clientfixture`, which hosts the gateway in-process
with a static runtime, and starts it beside the scripted upstream. The Go SDK
module downloads its dependencies once, before the isolation below begins. Each
suite then runs from `env -i` with a private home and XDG tree, telemetry and
update checks off, and a proxy that refuses everything but loopback, so a
client that reaches past the gateway fails. A suite that cannot run, runs no
tests, has a test file that defines none, or skips or leaves a test or a group
of tests to do fails. A suite may be skipped only by a row of the table in
`run.sh` with a stated reason, which the summary prints as an open item.

`OLP_CLIENTS_READY_TIMEOUT_SECONDS` (60), `OLP_CLIENTS_SUITE_TIMEOUT_SECONDS`
(900) and `OLP_CLIENTS_TEST_TIMEOUT_SECONDS` (180) bound the gateway start, a
suite and a test of a Node suite. The Go SDK suite is one package that the
toolchain bounds as a whole, so the suite timeout bounds it, and each SDK call
in it has a deadline of its own. A signal to `run.sh` stops the running suite
and the clients it started. The full run takes about two minutes on a developer
machine.

`make integration` runs it after the SDK smoke suites, so a client release that
breaks compatibility fails the pull request that proposes it. `make check`
covers what needs no client: the scripted upstream's unit tests under
`go test ./...`, `gofmt` of the fixture and Go SDK suite, and
`scripts/check-clients-doc.test.mjs`, which keeps
[client compatibility](../docs/clients.md) equal to the pinned manifests, and
`scripts/clients-run.test.mjs`, which runs the harness suite under a `TMPDIR`
that is a symlink with a `..` in it (about fifteen seconds; it is skipped, and
says why, where the toolchain or the client packages are missing). Root
`go vet ./...` does not reach the Go SDK module, so `run.sh` vets it.

A gateway incompatibility a client exposes is fixed in the gateway with a Go
test beside the code, not patched around in the suite. See the
[client qualification guide](clients/README.md) for the harness contract,
the scripted upstream and how to add a suite, and
[client compatibility](../docs/clients.md) for the pinned releases, each
client's tested configuration and the open items.

## Contract and browser coverage

The release contract test, run inside the process suite, checks that every
operation in the embedded OpenAPI contract reaches a handler. The
authorization sweep calls every secured management operation as every caller
in `internal/access/testdata/authorization.golden.json` and holds each
outcome to that file, and the isolation sweep reads every operation as members
of another project and fails on any disclosure of a canary project. A test's existence
does not prove it ran; use CI results for the commit you are checking. Browser
journeys cover accounting, cloud configuration, bulk certification, grouped
routes, pools, policy exclusions, preview, publication, playground, and
replacement recovery through deterministic local providers, not paid accounts.
