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

`./scripts/code-mode-qualification.sh cli` installs the exact-integrity official
Codex 0.160.0 package and runs controlled HTTP/SSE and WebSocket journeys without
a provider account. It passes only an OLP fixture key into an isolated client
home; local tool execution, continuation/resume and cancellation are exercised
by the real CLI. The fixtures are not live subscription qualification.

With integration PostgreSQL configured, public management checks run with:

```sh
go test -race -tags=integration -count=1 -timeout=5m -v \
  -run '^TestCodeQualificationPublic' ./tests/integration
```

After the supported transport/authorizer and Codex enrollment fixture are
integrated, `./scripts/code-mode-qualification.sh process` runs the real gateway
fleet suite. It requires `OLP_TEST_DATABASE_URL`, `OLP_TEST_VALKEY_URL` and
`OLP_TEST_BINARY`; missing prerequisites fail rather than skip. It uses the
additional `codecli` build tag. `all` runs both CLI and process suites.

See [qualification evidence and integration prerequisites](../docs/qualification/code-mode.md)
for the exact upstream source/package pins and unrun release gates. The foundation
alone returns 404 at the code transport seam; a successful compile is not a
passing fleet test. These shell suites do not run browser tests.

### Other focused suites

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
official JavaScript SDKs, and Chromium journeys at packaged and Vite origins.
Packaged assets run the full feature and hosted-console journeys, followed by
replacement recovery into an empty database with a separate Valkey service.
Vite runs shell hydration and focused setup/login, cookie, CSRF mutation,
protected deep-link and unary/streaming inference proxy checks. Failure-path
restores assert that the destination stays empty.
Each test installation has an independent database and installation namespace.
Code-mode process tests run with a separate 15-minute suite deadline. The
remaining process tests retain their 30-minute deadline; the two selections are
disjoint and cover the complete package, with the same build tags and race
detection.

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
