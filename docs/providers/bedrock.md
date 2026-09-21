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
`invoke-with-response-stream`, where `{route}` is an OLP route slug rather than
a provider model ID. Incoming SigV4 signatures are never trusted as gateway
authentication: send `X-OLP-API-Key` (or an ordinary bearer key that is not a
SigV4 header), and CORS preflights must allow `X-OLP-API-Key`. The gateway
strips all inbound `Authorization` and `X-Amz-*` headers, rewrites only the
URL's route slug to the upstream model, and re-signs with the configured Bedrock
credential. Converse targets must hold a certified `bedrock`/unary-or-streaming
generation tuple. InvokeModel requires `bedrock_invoke` certification for Claude
generation, Titan embeddings, or Titan image generation. Certification refuses
unqualified Invoke models with `capability_unavailable`; unary responses are
also validated by model family. Event-stream frames are CRC- and size-validated
and re-encoded. Malformed or oversized frames terminate the stream.

Route `/bedrock` to the gateway service at the edge; the bundled Helm Ingress
and Vite proxy omit it. Local SDK clients can use the Go public listener
directly.

## Authentication

| Mode | Credentials |
| --- | --- |
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, optional `session_token` |

Discovery requires `bedrock:ListFoundationModels`; inference uses
`bedrock:InvokeModel`, `bedrock:InvokeModelWithResponseStream`, and
`bedrock:CountTokens` when requested. Scope permissions to configured resources
where supported.

OLP owns retry/failover policy; the connector does not create an SDK inference
client with a separate retry loop. Canonical connector calls enforce attempt,
overall, and streaming-idle deadlines. Native Bedrock ingress pins one target
and credential for the route deadline without cross-target failover. Response
and event limits apply; malformed responses fail explicitly.

## Testing

Run focused protocol/connector tests or the complete local suite:

```sh
make test-go GO_TEST_PACKAGES='./internal/connectors ./internal/protocols' GO_TEST_ARGS='-run Bedrock'
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
Deterministic protocol, routing and SigV4 checks remain in ordinary
qualification.
