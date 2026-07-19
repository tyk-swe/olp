# Amazon Bedrock connector

This crate implements Amazon Bedrock through the official AWS SDK for Rust. It
uses Bedrock Runtime for `Converse`, `ConverseStream`, and `CountTokens`, and
the Bedrock control-plane client for foundation-model discovery. The SDK owns
SigV4 signing, credential resolution, and event-stream framing.

Model IDs, cross-Region inference profile IDs, and supported ARNs are passed to
Bedrock unchanged.

## Authentication modes

| Mode | Description |
|---|---|
| `default_chain` | The standard AWS environment, profile, web identity, ECS, and EC2 credential providers |
| `static` | A JSON object containing `access_key_id`, `secret_access_key`, and an optional `session_token` |

The connector disables SDK retries so OpenLLMProxy remains responsible for
retry and failover policy. Streaming calls enforce response-setup, overall,
and event-idle deadlines. Buffered unary calls use the overall attempt deadline
and the SDK's connection and socket-read timeouts.

## Testing

Run the local test suite from the repository root:

```sh
cargo test -p olp-providers bedrock
```

The ignored live tests use the default AWS credential chain:

```sh
OLP_BEDROCK_LIVE_REGION=us-east-1 \
OLP_BEDROCK_LIVE_MODEL=amazon.nova-micro-v1:0 \
cargo test -p olp-providers live_provider -- --ignored
```
