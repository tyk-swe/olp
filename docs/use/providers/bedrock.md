# Amazon Bedrock connector

The Bedrock connector reaches Amazon Bedrock through AWS SDK for Go v2 credential providers and SigV4 signing. Endpoint resolution is partition-aware, streaming responses use bounded event-stream decoding, and native `Converse`, `ConverseStream`, `CountTokens`, and foundation-model discovery use OLP's bounded HTTP transport. Model IDs and supported ARNs pass through unchanged.

Two InvokeModel families also qualify by model prefix. Models matching `amazon.titan-embed-text-*` serve single-input embeddings, and models matching `amazon.titan-image-generator-*` serve `image_generation` with the `TEXT_IMAGE` task. Stability text-to-image models matching `stability.sd3-5-large-*`, `stability.stable-image-core-*`, or `stability.stable-image-ultra-*` also serve `image_generation`. They return one image per request, converting `1024x1024`, `1536x1024`, and `1024x1536` sizes to aspect ratios and accepting `png`, `jpeg`, or `webp` as `output_format`. A request refused by a Stability filter fails with `content_filter`. Both families normalize to the OpenAI surface described in the [compatibility matrix](../compatibility.md).

Rerank models, such as `amazon.rerank-v1:0` and `cohere.rerank-v3-5:0`, serve `/v1/rerank` through the Bedrock Agent Runtime [Rerank API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_agent-runtime_Rerank.html) at `bedrock-agent-runtime.{region}`, which OLP reaches only from the region's default Bedrock Runtime endpoint. The request names the model by its regional foundation-model ARN, refuses `truncation`, and returns documents from the caller's request when `return_documents` is set. AWS bills one query for every hundred documents, which is the usage OLP records. Inference needs `bedrock:Rerank` and `bedrock:InvokeModel` on the model.

## Speech, retrieval, and realtime

A Bedrock connection also supports speech synthesis through Amazon Polly, operator-managed retrieval through Bedrock Knowledge Bases, and bidirectional audio streaming through Bedrock Nova Sonic.

For speech synthesis on transformed routes, OLP routes `/v1/audio/speech` requests to Amazon Polly in the provider's configured AWS region at `polly.{region}` and signs them for the `polly` service. The AWS identity needs `polly:SynthesizeSpeech`. The model names a Polly engine (`standard`, `neural`, `long-form`, or `generative`) and `voice` names a Polly voice ID such as `Joanna`. See [vendor media](media.md#amazon-polly) for supported formats, billing headers, and certification behavior.

For retrieval, choose the `bedrock-knowledge-bases` profile on a `bedrock` connection. OLP defaults its endpoint to `bedrock-agent-runtime.{region}`, certifies metadata against `bedrock-agent.{region}/knowledgebases/{id}` on the default regional endpoint, and queries `/knowledgebases/{id}/retrieve`. See [retrieval](../retrieval.md) for Knowledge Bases setup and query contracts.

For bidirectional realtime sessions with Nova Sonic, choose the `bedrock-realtime` profile (`bedrock-bidi` transport). Clients open native duplex sessions on strict routes at `GET /native/bedrock-realtime/sessions/{route}` for models certified with `realtime`/`native`/`realtime`. OLP opens AWS `InvokeModelWithBidirectionalStream` on the configured Bedrock Runtime endpoint using the provider's AWS credentials and forwards bounded bidirectional frames without modifying opaque session payloads.

## Bedrock SDK ingress

AWS SDK clients can call the gateway's Bedrock surface directly under `/bedrock/model/{route}/converse`, `/bedrock/model/{route}/converse-stream`, `/bedrock/model/{route}/invoke`, and `/bedrock/model/{route}/invoke-with-response-stream`, where `{route}` is an OLP route slug rather than a provider model ID. Incoming SigV4 signatures are never trusted as gateway authentication. Clients must send `X-OLP-API-Key` or an ordinary bearer API key that is not a SigV4 header, and CORS preflights must allow `X-OLP-API-Key`.

The gateway strips all inbound `Authorization` and `X-Amz-*` headers, rewrites only the route slug in the URL to the upstream model, and re-signs the request with the configured Bedrock credential. Converse targets must hold a certified `generation`/`bedrock`/`unary` or `streaming` tuple. InvokeModel requires `bedrock_invoke` certification for Claude generation, Titan embeddings, or Titan image generation. Certification refuses unqualified Invoke models with `capability_unavailable`, and unary responses are validated by model family. Event-stream frames are CRC-validated, size-validated, and re-encoded, so malformed or oversized frames terminate the stream.

Both the bundled Helm Ingress and the console Vite dev proxy route `/bedrock` to the gateway service. When running behind a custom reverse proxy or ingress controller, route `/bedrock` to the gateway service as well.

## Authentication

The `bedrock-batch-inference` profile is batch-only and leaves Converse generation behavior unchanged. Configure nonsecret `batch_storage` prefixes (`s3://.../`) and the required IAM `role_arn`, granting the role permission to read inputs and write outputs. OLP signs control-plane calls for `bedrock` and manifest downloads for `s3`, validates output scope, and settles the manifest as one aggregate usage result.

| Mode | Credentials |
| --- | --- |
| `default_chain` | AWS environment, profile, web identity, ECS, or EC2 providers |
| `static` | JSON `access_key_id`, `secret_access_key`, and optional `session_token` |

Discovery requires `bedrock:ListFoundationModels`. Inference uses `bedrock:InvokeModel`, `bedrock:InvokeModelWithResponseStream`, and `bedrock:CountTokens` when requested, plus `bedrock:InvokeModelWithBidirectionalStream` for `bedrock-realtime` and `polly:SynthesizeSpeech` for Amazon Polly speech. Scope IAM permissions to configured resources wherever AWS supports resource-level constraints.

OLP owns retry and failover policy rather than creating an SDK inference client with a separate retry loop. Canonical connector calls enforce attempt, overall, and streaming-idle deadlines. Native Bedrock ingress pins one target and credential for the route deadline without cross-target failover. Response and event limits apply, and malformed responses fail explicitly.

## Testing

Bedrock connector addressing, SigV4 signing, profile validation, and protocol translation run in `make test-unit` through `internal/connectors/*_test.go` and `internal/domain/protocols/bedrock*_test.go`. Run them focused with the following commands.

```sh
go test -count=1 ./internal/connectors
go test -count=1 ./internal/domain/protocols -run Bedrock
```

To verify behavior against live AWS APIs, run the gateway against a stub or a real AWS account.
