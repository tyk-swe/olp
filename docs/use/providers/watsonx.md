# IBM watsonx.ai connector

The `watsonx` provider kind serves chat generation from IBM watsonx.ai foundation models through its [chat API](https://dataplatform.cloud.ibm.com/docs/content/wsj/analyze-data/fm-api-chat.html?context=wx). Because the watsonx chat API is a variant of OpenAI Chat Completions, OLP adapts each request and response and serves watsonx on transformed routes.

| Chat Completions | watsonx chat API |
| --- | --- |
| `model` | `model_id` |
| No client field | `project_id`, taken from the provider's configured project |
| `stream: true` | `POST /ml/v1/text/chat_stream` instead of `/ml/v1/text/chat` |
| `tool_choice` modes such as `"auto"`, `"none"`, or `"required"` | `tool_choice_option` |
| `[DONE]` | The usage chunk that terminates every stream |

Results and stream chunks return as Chat Completions objects without the `model_id`, `created_at`, and `system` notices that watsonx adds. A stream that ends before its usage chunk is reported as truncated, and every choice must still finish cleanly. Requests that set a Chat Completions field watsonx does not document, such as `logit_bias`, `user`, or `parallel_tool_calls`, are refused before any upstream call.

Clients can reach watsonx through OpenAI, Anthropic, and Gemini ingress surfaces in both unary and streaming generation modes on transformed routes. OLP translates Anthropic Messages and Gemini GenerateContent requests to the adapted watsonx chat call and converts responses back to the caller's surface. watsonx has no Bedrock surface, native surface, or strict provider profile.

## Configuration

| Field | Meaning |
| --- | --- |
| `cloud_region` | A watsonx.ai region, such as `us-south`, `eu-de`, `eu-gb`, `jp-tok`, `au-syd`, or `ca-tor` |
| `cloud_project` | The ID of the watsonx.ai project where calls run and bill |
| `endpoint` | Defaults to `https://{region}.ml.cloud.ibm.com`. Set an origin without a path for a private endpoint |
| `api_version` | The `version` query date every call sends, defaulting to `2025-10-25` |

Model discovery lists the project's chat-capable foundation models through `GET /ml/v1/foundation_model_specs?version=<api_version>&filters=function_text_chat&limit=200`.

## Authentication

The `ibm_iam` authentication mode stores an IBM Cloud API key. OLP exchanges that key at `https://iam.cloud.ibm.com/identity/token` for an IAM access token and reuses the token until four fifths of its lifetime have passed, matching IBM's SDKs. If IAM refuses the key, OLP rejects the credential. If IAM experiences a transient outage, the attempt fails without rejecting the credential. Access tokens are redacted wherever upstream text is recorded. watsonx.ai software on Cloud Pak for Data authenticates differently and is not supported.

## Errors

watsonx reports failures as a list of errors. OLP uses the first error entry to classify the attempt, both for HTTP error responses and for `error` events inside a stream.

## Testing

watsonx endpoint addressing, IBM IAM token exchange and caching, multi-surface capability rules, and configuration validation are covered by unit tests in `internal/connectors/*_test.go`, which run in `make test-unit`. Run them focused with the following command.

```sh
go test -count=1 ./internal/connectors
```

To verify behavior against a live project, run the gateway against a stub or a real watsonx.ai project.
