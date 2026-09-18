# Behavioral validation

After `make setup`, `make test` runs all container-free behavioral suites:
Go unit tests and the language-neutral protocol corpus under `fixtures/`,
console unit/component tests, and `scripts/*.test.mjs`. Failures return a
nonzero exit status. Keep valid expectations for unary/streaming replies,
malformed input, cancellation, timeouts, failover, egress, media and pricing.

`make check` generates contracts, checks formatting, vet, console types and
lint, then runs these same suites once. `make test-race` runs uncached Go tests
with race detection and is also required by CI. Normal Go runs retain caching.
Go-only targets require Go and the native prerequisites from
[CONTRIBUTING.md](../CONTRIBUTING.md); console tests also need Node.js 26 and
the installed pnpm workspace. Console tests synchronize SvelteKit themselves,
so no prior console build is required. Focused `.only` tests fail validation.

Run individual suites or pass runner arguments through Make:

```sh
make test-go GO_TEST_PACKAGES='./internal/protocols/... ./internal/gateway'
make test-go GO_TEST_PACKAGES=./internal/coordination GO_TEST_ARGS='-run TestConfiguration -v -count=1'
make test-console CONSOLE_TEST_ARGS='--project unit src/lib/format.test.ts'
make test-scripts
make test-race GO_TEST_PACKAGES=./internal/gateway GO_TEST_TIMEOUT=10m
make test-go GO_TEST_ARGS=-cover
```

`GO_TEST_PACKAGES` defaults to `./...`, `GO_TEST_TIMEOUT` to `5m` per package,
and `GO_TEST_ARGS` and `CONSOLE_TEST_ARGS` to empty. Go tests use
`-mod=readonly`. Coverage reporting is optional; there is no numeric threshold.

`make integration` provisions disposable TLS/authenticated PostgreSQL and
Valkey, then runs race-enabled process/service/provider/media scenarios,
official JavaScript SDKs, and Chromium journeys at packaged and Vite origins.
Both origins run replacement recovery into an empty database with a separate
Valkey service. Failure-path restores assert that the destination stays empty.
Each test installation has an independent database and installation namespace.
Service-dependent Go tests require the `integration` build tag, even when
they live beside feature code. They fail with setup guidance when their
required service or recovery configuration is missing. Ordinary `make test`
does not select them, including when service environment variables are set.

```sh
./tests/sdk-smoke/run.sh
./tests/sdk-smoke-python/run.sh
```

Both SDK launchers use `tests/sdkfixture`, disable retries and exercise native
OpenAI, Anthropic and Gemini success/typed-error contracts. Python is optional
and uses the pinned uv project. Recovery journeys additionally call the
restored gateway through the official OpenAI SDK.

Paid provider checks require explicit credentials and are excluded from normal
CI. Set `OLP_LIVE_PROVIDER` and run
`go test -tags=liveproviders -count=1 ./internal/connectors`, or dispatch the
main-branch `live-providers` workflow. Its configuration lists required secrets
and cloud identity variables. Live calls consume provider quota.

The retired Rust scenario inventory and its Go replacements are preserved in
[release evidence](../docs/roadmap/evidence/release-qualification.md).
