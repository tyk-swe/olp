# Amazon Bedrock connector

The Bedrock provider uses AWS SDK for Go v2 credential providers, SigV4 signing,
partition-aware endpoint resolution and event-stream decoding. Native
`Converse`, `ConverseStream`, `CountTokens` and foundation-model discovery use
OLP's bounded HTTP transport. Model IDs and supported ARNs pass through
unchanged.

## Authentication

| Mode | Credentials |
|---|---|
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, optional `session_token` |

OLP owns retry/failover policy; the connector does not create an SDK inference
client with a separate retry loop. Streaming
calls enforce setup, overall, and event-idle deadlines; unary calls use the
attempt deadline and bounded HTTP connection/response timeouts. Error mapping treats
malformed bodies and missing error codes as provider failures rather than
successful empty responses.

## Testing

Run focused protocol/connector tests or the complete local suite:

```sh
go test ./internal/connectors ./internal/protocols -run Bedrock
make test
```

Opt-in paid live qualification uses the default AWS credential chain:

```sh
OLP_LIVE_PROVIDER=bedrock \
OLP_BEDROCK_LIVE_REGION=us-east-1 \
OLP_BEDROCK_LIVE_MODEL=amazon.nova-micro-v1:0 \
go test -tags=liveproviders -count=1 -timeout=2m ./internal/connectors \
  -run TestLiveProviderNativeGeneration
```

The manual `live-providers` workflow runs the same credential-scoped Go test.
Deterministic protocol, routing and SigV4 checks remain in ordinary qualification.
