# Amazon Bedrock connector

The `olp-engine` Bedrock provider uses the official AWS SDK for Rust: Bedrock
Runtime for `Converse`, `ConverseStream`, and `CountTokens`, and the
control-plane client for foundation-model discovery. The SDK owns SigV4,
credential resolution, and event framing; model IDs and supported ARNs pass
through unchanged.

## Authentication

| Mode | Credentials |
|---|---|
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, optional `session_token` |

SDK retries are disabled so inference owns retry/failover policy. Streaming
calls enforce setup, overall, and event-idle deadlines; unary calls use the
attempt deadline and SDK connection/socket-read timeouts. Error mapping treats
malformed bodies and missing error codes as provider failures rather than
successful empty responses.

## Testing

Run the focused locked nextest filter or the full gate from the repository root:

```sh
SQLX_OFFLINE=true cargo nextest run --locked -p olp-engine -E 'test(/bedrock/)'
make test
```
