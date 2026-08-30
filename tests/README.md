# Compatibility and contract tests

The repository's test assets include the framework-independent wire corpus,
official SDK smoke tests, end-to-end contracts, HA proof, and a separate fuzz
workspace. Repository-wide Rust validation is `make test` (locked nextest);
use the focused commands below when changing one suite.

## Conformance corpus

`fixtures/` holds bounded JSON and UTF-8 SSE examples for protocol translation,
fragmented streams, routing/retry decisions, and custom-endpoint security.
`conformance/` replays them without live DNS or provider requests. Fixture
changes are contract changes: add a case rather than replacing an expectation
unless it is demonstrably wrong.

```sh
SQLX_OFFLINE=true cargo nextest run --locked -p olp-conformance
```

The provider conformance matrix exercises every connector transport,
deadlines, malformed bodies, error mapping, and failover, including Bedrock
responses with missing error codes or malformed bodies.

## SDK smoke

Official OpenAI, Anthropic, and Google GenAI JavaScript and Python clients run
against the same ephemeral local server with retries disabled and no live
provider access. Both runtimes cover `/v1`, `/openai/v1`, trailing-slash bases,
`x-litellm-api-key`, streaming, Responses, model list and retrieve, Anthropic
messages and token counting, Gemini generation, and typed error contracts:

```sh
make sdk-smoke
make sdk-smoke-python
```

The Python suite needs Python 3.14 and uv 0.12.7. Both commands build and reuse
the `sdk_smoke_fixture` example; neither needs PostgreSQL, Valkey, or provider
credentials.

## Live providers

`make live-tests` runs seven ignored provider tests. The weekly workflow runs
Wednesday at 04:43 UTC in the protected `live-providers` environment. Catalog
and token-count calls have no inference charge; the two generation probes cap
output at one token. A failed test is retried once, so a provider can receive
at most two executions of that test per workflow run.

| Environment | Test and call | Expected cost / model |
|---|---|---|
| `OLP_LIVE_OPENAI_API_KEY` | `live_provider_discovers_openai_models`: one `GET /v1/models` | No inference charge; no model selected |
| `OLP_LIVE_ANTHROPIC_API_KEY` | `live_provider_discovers_anthropic_models`: paginated `GET /v1/models` | No inference charge; normally one page |
| `OLP_LIVE_GEMINI_API_KEY` | `live_provider_discovers_gemini_models`: paginated `GET /v1beta/models` | No inference charge; normally one page |
| `OLP_AZURE_OPENAI_LIVE_ENDPOINT`, `OLP_AZURE_OPENAI_LIVE_DEPLOYMENT`, `OLP_AZURE_OPENAI_LIVE_API_VERSION`, `OLP_AZURE_OPENAI_LIVE_API_KEY` | `live_provider_azure_chat_smoke`: one short Chat Completions request | Deploy `gpt-5-nano`; one output token is typically below $0.00001, but Azure pricing is regional |
| `OLP_VERTEX_LIVE_PROJECT`, `OLP_VERTEX_LIVE_LOCATION`, `OLP_VERTEX_LIVE_MODEL` | `live_provider_vertex_adc_smoke`: one `countTokens` probe using ADC | No inference charge; use `us` and `gemini-3.1-flash-lite` |
| `OLP_BEDROCK_LIVE_REGION` | `live_provider_discovers_models_with_default_chain`: one `ListFoundationModels` call | No inference charge |
| `OLP_BEDROCK_LIVE_REGION`, `OLP_BEDROCK_LIVE_MODEL` | `live_provider_runs_converse_with_default_chain`: one short `Converse` request | Use `us-east-1` and `amazon.nova-micro-v1:0`; one output token is below $0.000001 at the published rate |

Store the four API keys as environment secrets. Store endpoints, deployment,
API version, cloud project/location/model, region/model, AWS role ARN, GCP
workload identity provider, and GCP service account as environment variables.
Provider identities must be dedicated to CI. Use a provider-enforced cap or a
fixed prepaid balance without automatic reload where available. Otherwise use
the lowest practical API/model permissions and quotas, spend alerts with
automatic disablement, and record the remaining overshoot bound in the
milestone evidence. The `live-providers` environment requires `tyk-swe`
approval and allows deployments only from `main`. CI gets AWS and GCP
credentials through GitHub OIDC; do not add a service-account JSON or static
AWS keys.

For a local run, export the listed values, establish Google application-default
credentials and an AWS default credential chain, then run:

```sh
make live-tests
```

When the `provider-drift` issue opens, inspect the linked run and reproduce the
named test. Separate expired credentials, quota, and provider outages from an
actual contract change. Update the connector and add a fixture when the
provider contract changed; do not weaken an expectation or add a retry. The
workflow updates the same issue while failures continue and closes it after a
green run.

## End-to-end and HA

`make e2e` drives the real `olp` binary against PostgreSQL, Valkey, and a
loopback mock provider. `make worker-ha` verifies shared-Valkey installation
isolation and three-worker crash recovery. The full CI tier separately runs a
two-gateway HA job through PostgreSQL and Valkey Toxiproxy partitions. The
request-metadata stream and its consumer-group state are durable; limiter
rate-window and concurrency keys expire by design. The suites assert public
paths, routing/capability policy, generation pinning,
usage completeness, data-safety, recovery, and distributed limits. The
end-to-end suite also executes the README "Your first request" `curl` blocks
verbatim, so a README example that stops working is a build failure.

## Fuzzing

The separate `fuzz/` workspace covers `sse_decoder`, `protocol_json`,
`media_metadata`, and `multipart_parser`. Use the pinned nightly replay gate:

```sh
make fuzz-replay
```

Deterministic cancellation and staged-file cleanup remain Rust unit tests.
