# IBM watsonx.ai connector

The `watsonx` provider kind serves chat generation from IBM watsonx.ai
foundation models through its
[chat API](https://dataplatform.cloud.ibm.com/docs/content/wsj/analyze-data/fm-api-chat.html?context=wx).
The chat API is a variant of OpenAI Chat Completions, so OLP adapts each call
and serves watsonx on transformed routes:

| Chat Completions | watsonx chat API |
| --- | --- |
| `model` | `model_id` |
| — | `project_id`, from the provider's project |
| `stream: true` | `POST /ml/v1/text/chat_stream` instead of `/ml/v1/text/chat` |
| `tool_choice: "auto"` and the other modes | `tool_choice_option` |
| `[DONE]` | the usage chunk that ends every stream |

Results and stream chunks are returned as Chat Completions objects, without
the `model_id`, `created_at` and `system` notices watsonx adds. A stream that
ends before its usage chunk is reported as truncated, and every choice must
still finish. Requests that set a Chat Completions field watsonx does not
document, such as `logit_bias`, `user` or `parallel_tool_calls`, are refused
before any upstream call. OpenAI, Anthropic and Gemini clients reach watsonx
through translation; it has no native surface or strict profile.

## Configuration

| Field | Meaning |
| --- | --- |
| `cloud_region` | A watsonx.ai region, such as `us-south`, `eu-de`, `eu-gb`, `jp-tok`, `au-syd` or `ca-tor`. |
| `cloud_project` | The ID of the watsonx.ai project calls run and bill in. |
| `endpoint` | Defaults to `https://{region}.ml.cloud.ibm.com`. Set an origin without a path for a private endpoint. |
| `api_version` | The `version` date every call sends; defaults to `2025-10-25`. |

Model discovery lists the project's chat-capable foundation models through
`GET /ml/v1/foundation_model_specs?filters=function_text_chat`.

## Authentication

The `ibm_iam` mode stores an IBM Cloud API key. OLP exchanges it at
`https://iam.cloud.ibm.com/identity/token` for an IAM access token, which it
reuses until four fifths of its lifetime have passed, as IBM's SDKs do. IAM's
refusal of a key rejects the credential; an IAM outage fails the attempt
without rejecting it. Access tokens are redacted wherever upstream text is
recorded. watsonx.ai software on Cloud Pak for Data authenticates differently
and is not supported.

## Errors

watsonx reports failures as a list of errors, of which the first classifies
the attempt, both as an HTTP response and as an `error` event in a stream.

## Testing

Opt-in paid live qualification needs an API key and project:

```sh
OLP_LIVE_PROVIDER=watsonx \
OLP_WATSONX_LIVE_API_KEY=... \
OLP_WATSONX_LIVE_REGION=us-south \
OLP_WATSONX_LIVE_PROJECT=... \
go test -tags=liveproviders -count=1 -timeout=2m ./internal/connectors \
  -run TestLiveProviderNativeGeneration
```
