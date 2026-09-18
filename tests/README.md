# Behavioral validation

`make test` runs Go unit tests and the language-neutral protocol corpus under
`fixtures/`. Keep valid expectations for unary/streaming replies, malformed
input, cancellation, timeouts, failover, egress, media and pricing.

`make integration` provisions disposable TLS/authenticated PostgreSQL and
Valkey, then runs race-enabled process/service/provider/media scenarios,
official JavaScript SDKs, and Chromium journeys at packaged and Vite origins.
Both origins run replacement recovery into an empty database with a separate
Valkey service. Failure-path restores assert that the destination stays empty.
Each test installation has an independent database and installation namespace.

```sh
go test ./internal/protocols/... ./internal/gateway
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
