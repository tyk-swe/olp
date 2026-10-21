# Amazon SageMaker AI connector

The `sagemaker` provider kind serves chat generation from SageMaker AI
real-time endpoints through their
[OpenAI-compatible path](https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html).
The endpoint's container must serve `/v1/chat/completions` and stream it as
server-sent events, as the SageMaker vLLM and SGLang containers and any custom
container implementing that path and `/ping` do.

## Models

A SageMaker provider's models are its endpoints. Name a single-model endpoint
by its name, and a model an inference component hosts on a shared endpoint as
`endpoint/component`:

| Model | Request |
| --- | --- |
| `qwen3-4b` | `POST /endpoints/qwen3-4b/openai/v1/chat/completions` |
| `shared/qwen-ic` | `POST /endpoints/shared/inference-components/qwen-ic/openai/v1/chat/completions` |

SageMaker has no model-list API for these endpoints, so declare each model and
the connection test certifies it. The URL selects the model; OLP sends an empty
`model` in the body, as AWS's own example does, because containers such as vLLM
refuse a name they do not serve.

Generation is served on the OpenAI surface, unary and streaming. Like any
generic OpenAI-compatible server, an endpoint gains no Anthropic, Gemini or
Bedrock surface, and no embeddings, token counting or media. The
`sagemaker-openai-chat` profile serves the same address as a strict native
Chat Completions profile.

## Region and endpoint

Set `cloud_region`. The endpoint defaults to the region's SageMaker AI Runtime
origin, such as `https://runtime.sagemaker.us-west-2.amazonaws.com` or
`https://runtime.sagemaker.cn-north-1.amazonaws.com.cn`. A custom endpoint,
such as an interface VPC endpoint, must be an origin without a path.

## Authentication

| Mode | Credentials |
| --- | --- |
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, optional `session_token` |

SageMaker's OpenAI-compatible path takes a bearer token rather than a signed
request. OLP signs a fresh token for every request from the provider's AWS
credentials, locally and without a network call, exactly as the SageMaker
Python SDK's token generator does: a SigV4 presigned `CallWithBearerToken`
request that expires after 15 minutes. The identity needs
`sagemaker:InvokeEndpoint` on the endpoints it serves and
`sagemaker:CallWithBearerToken`, which AWS grants only on `"*"`. A token
carries the identity's authority, so scope `sagemaker:InvokeEndpoint` to the
endpoint ARNs and keep the identity free of other permissions. Tokens are
redacted wherever upstream text is recorded.

## Errors

A container's rejection that SageMaker reports as a `ModelError` (HTTP 424)
classifies by the container's own status and error body, so a container's 400
is a client error and its 503 a server error that may fail over.

## Testing

Opt-in paid live qualification uses the default AWS credential chain:

```sh
OLP_LIVE_PROVIDER=sagemaker \
OLP_SAGEMAKER_LIVE_REGION=us-west-2 \
OLP_SAGEMAKER_LIVE_MODEL=my-endpoint \
go test -tags=liveproviders -count=1 -timeout=2m ./internal/connectors \
  -run TestLiveProviderNativeGeneration
```

The bearer token is checked in ordinary tests against tokens botocore produces
for the same credentials, region and signing time.
