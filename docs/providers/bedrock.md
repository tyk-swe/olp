# Amazon Bedrock connector

The Bedrock provider uses AWS SDK for Go v2 credential providers, SigV4 signing,
partition-aware endpoint resolution and event-stream decoding. Native
`Converse`, `ConverseStream`, `CountTokens` and foundation-model discovery use
OLP's bounded HTTP transport. Model IDs and supported ARNs pass through
unchanged.

Beyond Converse, two InvokeModel families qualify by model prefix:
`amazon.titan-embed-text-*` models serve single-input embeddings and
`amazon.titan-image-generator-*` models serve `image_generation` with the
`TEXT_IMAGE` task. Both normalize to the OpenAI surface and are described in
[the compatibility matrix](../compatibility.md).

## Bedrock SDK ingress

AWS SDK clients can also call the gateway's Bedrock surface directly under
`/bedrock/model/{route}/converse`, `converse-stream`, `invoke`, and
`invoke-with-response-stream`, where `{route}` is an OLP route slug rather
than a provider model ID. Incoming SigV4 signatures are never trusted as
gateway authentication: send `X-OLP-API-Key` (or an ordinary bearer key that
is not a SigV4 header), and CORS preflights must allow `X-OLP-API-Key`. The
gateway strips all inbound `Authorization` and `X-Amz-*` headers, rewrites
only the URL's route slug to the upstream model, and re-signs with the
configured Bedrock credential. Converse targets must hold a certified
`bedrock`/unary-or-streaming generation tuple; InvokeModel passes through
only for the qualified model families above. Capability certification
refuses an unqualified Invoke model first (`capability_unavailable`), so the
data plane's explicit 422 for other model IDs is defense-in-depth rather
than the reachable boundary. Event-stream responses are CRC- and
size-validated frame by
frame and re-encoded, so malformed or oversized frames terminate the stream
instead of forwarding unchecked bytes.

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
