# Conformance and compatibility tests

This directory contains the framework-independent conformance corpus, the
official SDK smoke test, and pointers to the fuzz targets.

## Operational qualification

`qualification/run.sh` is the stable local and CI driver for `clean-install`,
`backup-restore`, `n-minus-one`, `load`, and `soak`. Each target fails rather
than skipping a missing runtime, tool, credential, or baseline. Thresholds,
evidence, workflow tiers, and failure owners are defined in
[the qualification matrix](../docs/qualification.md).

## Conformance harness and corpus

`fixtures/` is a framework-independent corpus of bounded JSON and UTF-8 SSE
examples. It covers protocol translation, fragmented streams, routing and retry
decisions, and custom-endpoint security. `conformance/` is the Rust reference
conformance harness for that corpus; it never makes live DNS or provider
requests.

Run it from the repository root:

```sh
cargo test -p olp-conformance
```

Each fixture is limited to 64 KiB. Treat fixture changes as contract changes:
add a case instead of replacing an existing expectation unless that expectation
is incorrect.

## Official SDK smoke test

`sdk-smoke/` runs the official OpenAI, Anthropic, and Google GenAI JavaScript
clients against an ephemeral local OpenLLMProxy server. It generates a local
proxy key, disables SDK retries, and cannot contact a live provider.

Node.js 24 or newer and pnpm 11 are required:

```sh
pnpm --dir tests/sdk-smoke install --frozen-lockfile
./tests/sdk-smoke/run.sh
```

## Fuzz targets

The `fuzz/` workspace provides these `cargo-fuzz` targets:

| Target | Coverage |
|---|---|
| `sse_decoder` | Fragmented vendor streams |
| `protocol_json` | Request and response codecs |
| `media_metadata` | Bounded media handles |
| `multipart_parser` | Multipart limits, cleanup, and spooling |

Run a target with `cargo fuzz run <target>`. Deterministic HTTP lifecycle tests,
including cancellation and staged-file cleanup, remain in:

```sh
cargo test -p olp --lib
```
