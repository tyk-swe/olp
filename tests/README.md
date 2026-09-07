# Behavioral validation

`make test` runs the Rust unit tests and protocol conformance corpus. Fixtures
under `fixtures/` cover every retained connector, unary and streaming replies,
malformed input, timeouts, cancellation, failover, and egress restrictions.
Add meaningful cases without replacing valid expectations.

`make integration` starts disposable PostgreSQL and Valkey services, runs the
persistence and HTTP suites, real-process contracts and HA recovery with
Toxiproxy dependency outages, official
JavaScript SDK checks, then one Chromium journey suite. The process suites
share `contract/harness`; the browser uses the same services and starts Rust
behind Vite to exercise the development origin, setup, provider activation,
routing, inference, history, OIDC, and conflicts. Each installation has its own
database and Valkey namespace.

For a focused run after installing dependencies:

```sh
cargo test --locked --all-features --test conformance
./tests/sdk-smoke/run.sh
./tests/sdk-smoke-python/run.sh
```

The optional Python suite uses uv and the Python version declared in its
project. Both SDK suites use the same local Rust fixture, disable retries, and
cover native OpenAI, Anthropic, and Gemini success and typed error contracts.
They also verify that the retired OpenAI prefix and LiteLLM authentication
header are unavailable.

Live provider tests are optional and require explicitly supplied credentials.
Run `cargo test --locked --all-features --lib live_provider -- --ignored`
or dispatch the `live-providers` workflow for one provider. Its configuration
lists the required secrets and cloud identity variables. Live calls can consume provider quota.
