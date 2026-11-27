# Amazon SageMaker AI connector

The `sagemaker` provider kind serves chat generation from a SageMaker AI real-time endpoint through its [OpenAI-compatible path](https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html). The endpoint container must serve `/v1/chat/completions`, stream responses as server-sent events, and answer `/ping`. SageMaker's vLLM and SGLang containers meet that contract, as can a custom container that implements the same paths.

## Models

A SageMaker provider's models are its endpoints. Name a single-model endpoint by its endpoint name, and name a model hosted by an inference component on a shared endpoint as `endpoint/component`.

| Model | Request |
| --- | --- |
| `qwen3-4b` | `POST /endpoints/qwen3-4b/openai/v1/chat/completions` |
| `shared/qwen-ic` | `POST /endpoints/shared/inference-components/qwen-ic/openai/v1/chat/completions` |

SageMaker has no model-list API for these endpoints, so declare each model and let the connection test certify it. The URL selects the model, and OLP sends an empty `model` field in the request body, following AWS's own example, because containers such as vLLM refuse a model name they do not serve.

Generation is served on the OpenAI surface in unary and streaming modes. Like any generic OpenAI-compatible server, a SageMaker endpoint does not gain an Anthropic, Gemini, or Bedrock surface, and it does not serve embeddings, token counting, or media. The `sagemaker-openai-chat` profile serves the same address as a strict native Chat Completions profile.

## Region and endpoint

Set `cloud_region` to the AWS region that hosts the endpoint. The endpoint defaults to the region's SageMaker AI Runtime origin, such as `https://runtime.sagemaker.us-west-2.amazonaws.com` or `https://runtime.sagemaker.cn-north-1.amazonaws.com.cn`. A custom endpoint, such as an interface VPC endpoint, must be an origin without a path.

## Authentication

| Mode | Credentials |
| --- | --- |
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, and optional `session_token` |

SageMaker's OpenAI-compatible path takes a bearer token rather than a SigV4-signed inference request. OLP signs a fresh token locally for every request from the provider's AWS credentials without a network call, matching the SageMaker Python SDK's token generator by presigning a SigV4 `CallWithBearerToken` request that expires after 15 minutes. The AWS identity needs `sagemaker:InvokeEndpoint` on the endpoints it serves and `sagemaker:CallWithBearerToken`, which AWS grants only on `"*"`. Because each token carries the identity's authority, scope `sagemaker:InvokeEndpoint` to the specific endpoint ARNs and keep the identity free of unrelated permissions. Tokens are redacted wherever upstream text is recorded.

## Errors

When SageMaker reports a container rejection as a `ModelError` with HTTP 424, OLP unwraps and classifies the response by the container's own `OriginalStatusCode` and error body. A container 400 therefore classifies as a client error, while a container 503 classifies as a server error that may fail over.

## Testing

SageMaker endpoint addressing, model validation, presigned bearer token authentication, and `sagemaker-openai-chat` profile rules are covered by unit tests in `internal/connectors/*_test.go`, which run in `make test-unit`. Run them focused with the following command.

```sh
go test -count=1 ./internal/connectors
```

To verify behavior against a live runtime, run the gateway against a stub or a real SageMaker endpoint.
