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

Official OpenAI, Anthropic, and Google GenAI JavaScript clients run against an
ephemeral local server with retries disabled and no live provider access. The
OpenAI checks cover `/v1`, `/openai/v1`, trailing-slash bases, and
`x-litellm-api-key` authentication:

```sh
make sdk-smoke
```

## End-to-end and HA

`make e2e` drives the real `olp` binary against PostgreSQL, Valkey, and a
loopback mock provider. `make worker-ha` verifies shared-Valkey installation
isolation and three-worker crash recovery. The full CI tier separately runs a
two-gateway HA job through PostgreSQL and Valkey Toxiproxy partitions. The
suites assert public paths, routing/capability policy, generation pinning,
usage completeness, data-safety, recovery, and distributed limits.

## Fuzzing

The separate `fuzz/` workspace covers `sse_decoder`, `protocol_json`,
`media_metadata`, and `multipart_parser`. Use the pinned nightly replay gate:

```sh
make fuzz-replay
```

Deterministic cancellation and staged-file cleanup remain Rust unit tests.
