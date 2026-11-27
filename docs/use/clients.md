# Client compatibility

The configurations on this page were written against the client releases
listed below and were formerly qualified by headless suites that ran each
client against a gateway backed by a scripted upstream. Those suites were
subtracted with the test system, so compatibility is no longer continuously
qualified. Treat the release tables as the record of what was covered when
last qualified, and verify a client by running it against your gateway
before relying on it. See
[code-mode qualification](../develop/qualification.md#latest-official-clients)
for the current status.

## Client releases

The three coding clients tracked `latest` when last qualified. Other
versions below are the exact pins from that record.

| Client | Package | Pinned version | Coverage when last qualified |
| --- | --- | --- | --- |
| Claude Code | `@anthropic-ai/claude-code` | latest | `claude -p` on strict and transformed routes covering streaming, tool-use loops, parallel tool calls, prompt caching, `count_tokens` through `/context`, paired `anthropic-beta` headers and request fields including auto-mode `safeguards`, session resume, API-key and bearer credentials, and typed errors |
| Codex CLI | `@openai/codex` | latest | `codex exec` on a transformed route with stock tools and on a strict route with hosted tools off covering Responses streaming, reasoning items replayed with encrypted content, sequential and parallel shell tool calls, `exec resume` history replay, and typed errors |
| OpenCode | `opencode-ai` | latest | Code-mode generated configuration and controlled coding-plan journeys, see [qualification](../develop/qualification.md) |
| Gemini CLI | `@google/gemini-cli` | 0.62.0 | Headless `-p` on transformed and strict routes covering `streamGenerateContent`, function calling with thought signatures returned, `countTokens` for an attachment, and typed errors |
| OpenAI Agents SDK | `@openai/agents` | 0.18.0 | Responses with function tools, handoff between two agents, structured output, streamed runs and reasoning items through `OpenAIProvider`, the Chat Completions model, and tool loops through Anthropic and Gemini routes |
| Vercel AI SDK | `ai` | 7.0.124 | `generateText` and `streamText` with sequential and parallel tool loops, structured output and images, and `embed` and `embedMany` through each provider below, including cross-vendor routes |
| Vercel AI SDK OpenAI provider | `@ai-sdk/openai` | 4.0.83 | Chat Completions, Responses and embeddings, including embeddings served by a Gemini route |
| Vercel AI SDK Anthropic provider | `@ai-sdk/anthropic` | 4.0.70 | Messages, prompt caching, and extended thinking replayed with its signature |
| Vercel AI SDK Google provider | `@ai-sdk/google` | 4.0.87 | `generateContent`, `streamGenerateContent` and native embeddings on a strict route |
| LangChain | `langchain` | 1.5.14 | `createAgent` tool loops over each chat model below |
| LangChain core | `@langchain/core` | 1.2.20 | Messages, tools, tool-call chunk assembly and `withStructuredOutput` |
| LangChain OpenAI provider | `@langchain/openai` | 1.6.3 | `ChatOpenAI` over Chat Completions and Responses, and `OpenAIEmbeddings` |
| LangChain Anthropic provider | `@langchain/anthropic` | 1.5.13 | `ChatAnthropic`, prompt caching, and extended thinking replayed with its signature |
| LangChain Google provider | `@langchain/google-genai` | 2.3.4 | `ChatGoogleGenerativeAI` and `GoogleGenerativeAIEmbeddings` on a strict route |
| LlamaIndex.TS | `llamaindex` | 0.12.1 | Tool definitions and message helpers |
| LlamaIndex.TS core | `@llamaindex/core` | 0.6.23 | Message and tool types, pinned to one copy by a pnpm override |
| LlamaIndex.TS environment | `@llamaindex/env` | 0.1.31 | Runtime support with no outbound requests |
| LlamaIndex.TS OpenAI provider | `@llamaindex/openai` | 0.4.23 | `OpenAI`, `OpenAIEmbedding`, and unary-only `OpenAIResponses` |
| LlamaIndex.TS Anthropic provider | `@llamaindex/anthropic` | 0.3.27 | `Anthropic` through an explicit session |
| LlamaIndex.TS Google provider | `@llamaindex/google` | 0.4.1 | `Gemini` and `GeminiEmbedding` on a strict route |
| LlamaIndex.TS workflow | `@llamaindex/workflow` | 1.1.24 | The `agent` that runs tool loops |
| Zod, required by the AI SDK and the Agents SDK | `zod` | 4.6.5 | Dependency only, not a client |

The official Go SDKs were qualified from a separate Go module, so their
dependencies stayed out of the gateway module and runtime inventory. That
module was subtracted with the suites.

| SDK | Module | Pinned version | Coverage when last qualified |
| --- | --- | --- | --- |
| OpenAI Go | `github.com/openai/openai-go/v3` | v3.86.0 | Chat Completions and Responses, unary and streaming, tool loops, structured output, embeddings, model listing, typed errors |
| Anthropic Go | `github.com/anthropics/anthropic-sdk-go` | v1.84.0 | Messages, unary and streaming, tool use loops with signed thinking, `count_tokens`, prompt caching, beta features, model listing, typed errors |
| Google Gen AI Go | `google.golang.org/genai` | v1.76.0 | `generateContent`, unary and streaming, function calling, thoughts, structured output, `countTokens`, native batch embeddings, model listing, typed errors |

## Gemini managed-agent resources

The pinned `google.golang.org/genai@v1.76.0` contract exposes agents under its
nested interactions client. The gateway supports create, list, get, and delete
at `/gemini/v1beta/agents`, while PATCH is excluded. Creation requires a
published **strict** route bound to the `gemini-agents` profile with the
`agent_resource` operation (transformed routes are refused with
`strict_route_required`), an inference key with `allow_provider_state`
permission, and an `Idempotency-Key` header of at most 128 bytes. SDK retry
attempts must reuse the same `Idempotency-Key` value, so disable automatic
retries if your client generates fresh keys per attempt.

For example, with `OLP_BASE_URL`, `OLP_API_KEY`, and a stable
`OLP_SUBMISSION_KEY` set for one logical creation:

```sh
curl "$OLP_BASE_URL/gemini/v1beta/agents" \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H "Idempotency-Key: $OLP_SUBMISSION_KEY" \
  -H 'X-OLP-Route: managed-agents' \
  -H 'Content-Type: application/json' \
  --data '{"description":"Support agent","system_instruction":"Give concise answers."}'
```

Use the returned `agent_resource_...` ID for reads and deletes. The
`base_agent` field accepts an owned, compatible local agent ID, while raw
provider agent or environment references are refused. Listing returns only
resources created by the calling key. See the
[operator recovery notes](../operate/operations.md#managed-gemini-resources)
before retrying an ambiguous creation. This API manages agent resource
definitions and does not execute agents.

## Compatibility status

No client suite runs today. When they ran, the suites checked the client
result and the recorded upstream requests, covering path and model rewrite,
provider credentials, tools, reasoning, usage, and typed errors against
deterministic fixtures. Fixture success never established model quality,
account permission, or live provider acceptance. To check a client now,
run it against a local gateway with a stub or real upstream and inspect
both the client outcome and the request the upstream received.

## Tested configuration

Every client needs the same three settings, namely the base URL of the surface
it speaks, an OLP API key with `inference` scope, and a published route slug as
the model name. The base URL is the gateway origin plus `/v1` for OpenAI,
`/anthropic` for Anthropic, `/gemini` for Gemini, and `/bedrock` for AWS Bedrock
SDK ingress, as listed in the [compatibility matrix](compatibility.md). A route
that translates between dialects must be transformed, while a strict route
preserves the native invocation.

Code mode is a separate contract that uses a `/code/<slug>` base URL, native
model names, and a subscription account behind the route. Codex, Claude Code,
and OpenCode take the configuration that the code-mode management API generates
for routes over Codex, OpenCode Go, and coding-plan accounts, which one route
may mix. See [code mode](code-mode.md#client-configuration). The former shared
resolution policy and its evidence are summarized in
[code-mode qualification](../develop/qualification.md) rather than in the tables
on this page.

Generate mounted-key setup with
[`olp client-env`](../operate/automation.md#configure-qualified-clients). It
supports Codex, Claude Code, and Gemini CLI, all four framework families, and
explicit JavaScript, Python, and Go constructors for the official SDKs.

### Claude Code

Claude Code treats `ANTHROPIC_BASE_URL` as the Anthropic API and sends the beta
headers and request fields that it sends to `api.anthropic.com`. The fields of a
beta travel with its header, and Anthropic's own
[gateway guide](https://code.claude.com/docs/en/llm-gateway-protocol#feature-pass-through)
notes that a gateway dropping one half produces `400` errors. OLP forwards
`Anthropic-Beta` and its paired request fields unchanged on a strict route bound
to the `anthropic-messages` profile, and on a transformed route to an automatic
Anthropic provider or any profiled provider whose profile declares
`Anthropic-Beta` as a semantic header (such as `anthropic-messages` or
`vertex-anthropic`). Providers whose profiles do not declare `Anthropic-Beta`
(such as `bedrock-anthropic-invoke`) and translated non-Anthropic targets do not
receive the header. Name a published route with `--model` or `ANTHROPIC_MODEL`.
Claude Code may send background work to a Haiku model that the gateway does not
publish, so point those background model settings at a published route too.

```sh
export ANTHROPIC_BASE_URL=https://olp.example.com/anthropic
export ANTHROPIC_API_KEY=olp_...              # or ANTHROPIC_AUTH_TOKEN
export ANTHROPIC_DEFAULT_HAIKU_MODEL=assistant ANTHROPIC_SMALL_FAST_MODEL=assistant
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
claude -p "Hello" --model assistant
```

For scripted checks, run `claude -p` with `--output-format json` and
`--permission-mode default`, because this release defaults to auto mode,
whose classifier makes requests of its own.

### Codex CLI

Codex takes OLP as a custom model provider that speaks the Responses API, in
`config.toml` or matching `-c` overrides. Codex keeps no state at the provider
because it sends `store: false` and replays the full conversation on each turn,
reasoning items included, so a resumed session needs no stored response. A
transformed route serves the stock tool set. A strict route refuses the
provider-hosted tools that Codex adds by default, web search and the multi-agent
namespace, with `state_carrier`, so disable them on strict routes.

```toml
# $CODEX_HOME/config.toml, which is ~/.codex/config.toml by default
model = "assistant"
model_provider = "olp"
web_search = "disabled"          # strict route only
check_for_update_on_startup = false

[features]
multi_agent = false              # strict route only

[model_providers.olp]
name = "OLP"
base_url = "https://olp.example.com/v1"
env_key = "OLP_API_KEY"
wire_api = "responses"
```

```sh
export OLP_API_KEY=olp_...
codex exec "Hello"
```

For isolated checks, write the configuration above to a private `CODEX_HOME`,
together with `approval_policy = "never"`, `sandbox_mode = "read-only"`, and
the `[analytics]` and `[feedback]` tables switched off. Start `codex exec --json`
with `--strict-config` so any key that a newer release no longer recognizes
fails fast.

### Gemini CLI

Gemini CLI reads `GOOGLE_GEMINI_BASE_URL` and `GEMINI_API_KEY`. Setting a base
URL alone selects an authentication method that headless mode refuses with
`Invalid auth method selected`, so select the Gemini API key method in
`~/.gemini/settings.json`. Headless mode also needs the workspace trusted via
`--skip-trust` or `GEMINI_CLI_TRUST_WORKSPACE=true`. The CLI counts tokens only
for a prompt with an attachment, such as `@image.png`. Merge this authentication
entry into `~/.gemini/settings.json`:

```json
{ "security": { "auth": { "selectedType": "gemini-api-key" } } }
```

```sh
export GOOGLE_GEMINI_BASE_URL=https://olp.example.com/gemini
export GEMINI_API_KEY=olp_...
gemini --skip-trust --model assistant -p "Hello"
```

Turn off telemetry, usage statistics, and update checks in the same settings
file for reproducible checks.

### OpenAI Agents SDK

The OpenAI Agents SDK reaches the gateway through `OpenAIProvider`. It exports
traces to `api.openai.com` unless tracing is disabled, and a strict route needs
`store: false` because a strict Responses route treats an omitted `store` as
`store: true`.

```javascript
import { Agent, OpenAIProvider, Runner } from '@openai/agents';

const runner = new Runner({
  modelProvider: new OpenAIProvider({ apiKey, baseURL: 'https://olp.example.com/v1', useResponses: true }),
  tracingDisabled: true
});
const agent = new Agent({ name: 'Assistant', model: 'assistant', modelSettings: { store: false } });
```

Set `useResponses: false` for Chat Completions. The SDK marks function tools as
strict by default. OpenAI strict tool mode cannot be preserved by another
vendor, so a route that translates to an Anthropic or Gemini upstream refuses a
tool with `strict: true` by name. Declare those tools with `strict: false`. An
Anthropic upstream also needs a token limit because the SDK sends none, so set
`modelSettings.maxTokens` or give the provider a `max_tokens` default.

### Vercel AI SDK

The Vercel AI SDK reaches Chat Completions through `chat()`, while calling the
provider directly selects the Responses API.

```javascript
import { createOpenAI } from '@ai-sdk/openai';
import { generateText } from 'ai';

const olp = createOpenAI({ apiKey: process.env.OLP_API_KEY, baseURL: 'https://olp.example.com/v1' });
const { text } = await generateText({ model: olp.chat('assistant'), prompt: 'Hello' });
```

The Anthropic and Google providers take a base URL that already ends in the API
version, and the Anthropic provider sets a token limit of its own unless the
call supplies one. Structured output with the Anthropic provider requests JSON
through a forced tool call that disables parallel tool use, which a Gemini
upstream cannot preserve, so the gateway refuses it there by name.

```javascript
import { createAnthropic } from '@ai-sdk/anthropic';
import { createGoogleGenerativeAI } from '@ai-sdk/google';

const claude = createAnthropic({ apiKey, baseURL: 'https://olp.example.com/anthropic/v1' });
const gemini = createGoogleGenerativeAI({ apiKey, baseURL: 'https://olp.example.com/gemini/v1beta' });
```

### LangChain

LangChain takes each base URL in the option its provider names. LangChain
retries six times by default, so set `maxRetries` as your deployment requires.
`ChatOpenAI` speaks Chat Completions unless it receives `useResponsesApi: true`.

```javascript
import { ChatAnthropic } from '@langchain/anthropic';
import { ChatGoogleGenerativeAI, GoogleGenerativeAIEmbeddings } from '@langchain/google-genai';
import { ChatOpenAI, OpenAIEmbeddings } from '@langchain/openai';

new ChatOpenAI({ model: 'assistant', apiKey, configuration: { baseURL: 'https://olp.example.com/v1' } });
new ChatAnthropic({ model: 'assistant', apiKey, anthropicApiUrl: 'https://olp.example.com/anthropic' });
new ChatGoogleGenerativeAI({ model: 'gemini-3.8-flash', apiKey, baseUrl: 'https://olp.example.com/gemini' });
new OpenAIEmbeddings({ model: 'assistant', apiKey, configuration: { baseURL: 'https://olp.example.com/v1' } });
new GoogleGenerativeAIEmbeddings({ model: 'gemini-embeddings', apiKey, baseUrl: 'https://olp.example.com/gemini' });
```

`ChatGoogleGenerativeAI` inspects the `model` string client-side before sending
images to decide whether the model supports multimodal input. Name the OLP route
after a Gemini model that your installed `@langchain/google-genai` version
recognizes as multimodal, such as `gemini-3.8-flash` on current releases. If you
are pinned to the historical `@langchain/google-genai@2.3.4` release listed in
the table above, its client-side allowlist predates newer models and expects an
older listed name such as `gemini-2.5-flash`.

### LlamaIndex.TS

LlamaIndex.TS takes the base URL on the OpenAI and Gemini classes, and requires
an explicit `AnthropicSession` for Anthropic. LlamaIndex retries ten times by
default, so set `maxRetries` explicitly.

```javascript
import { Anthropic, AnthropicSession } from '@llamaindex/anthropic';
import { Gemini, GeminiEmbedding } from '@llamaindex/google';
import { OpenAI, OpenAIEmbedding } from '@llamaindex/openai';

new OpenAI({ model: 'assistant', apiKey, baseURL: 'https://olp.example.com/v1' });
new Anthropic({ model: 'claude-sonnet-5-5', session: new AnthropicSession({ apiKey, baseURL: 'https://olp.example.com/anthropic' }) });
new Gemini({ model: 'gemini-3.8-flash', apiKey, httpOptions: { baseUrl: 'https://olp.example.com/gemini' } });
new OpenAIEmbedding({ model: 'assistant', apiKey, baseURL: 'https://olp.example.com/v1' });
new GeminiEmbedding({ model: 'gemini-embeddings', apiKey, httpOptions: { baseUrl: 'https://olp.example.com/gemini' } });
```

Older LlamaIndex.TS packages inspect the `model` string client-side to decide
whether a model can call tools in `agent` workflows. OpenAI accepts any route
slug, while the pinned historical `@llamaindex/anthropic@0.3.27` package checks
that the model name contains `-3` or `-4` (such as `claude-sonnet-4-5`) and
`@llamaindex/google@0.4.1` checks a built-in list of legacy Gemini names (such
as `gemini-2.5-flash`). Use current models like `claude-sonnet-5-5` and
`gemini-3.8-flash` on up-to-date clients, and be aware of that client-side
allowlist check if you remain pinned to those historical packages.

### Embeddings through the frameworks

The Anthropic API has no embeddings endpoint, so no Anthropic framework
provider embeds. The OpenAI providers embed through `/v1/embeddings`, which any
route with the `embeddings` capability serves, including a Gemini route by
translation. The Google providers use the native `embedContent` and
`batchEmbedContents` endpoints, which only a strict Gemini route serves.

### Official Go SDKs

The official Go SDKs take the same three settings. The Anthropic SDK appends
`/v1` to its base URL, and the Google SDK appends the API version, so both take
the bare surface prefix.

```go
import (
	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"
)

openAI := openai.NewClient(option.WithAPIKey(key), option.WithBaseURL("https://olp.example.com/v1"))
claude := anthropic.NewClient(anthropicoption.WithAPIKey(key), anthropicoption.WithBaseURL("https://olp.example.com/anthropic"))
gemini, err := genai.NewClient(ctx, &genai.ClientConfig{
	APIKey: key, Backend: genai.BackendGeminiAPI,
	HTTPOptions: genai.HTTPOptions{BaseURL: "https://olp.example.com/gemini", APIVersion: "v1beta"},
})
```

The OpenAI Go SDK sends credentials only over HTTPS. Terminate TLS in front of
the gateway as the [deployment guide](../operate/deployment.md) describes. A
loopback development gateway needs `option.WithUnsafeAllowHTTP()`.

Each Go SDK reports the gateway's errors in its own typed error with the status
that the vendor documents. `*openai.Error` carries the `type`, `code`, and
`param` of the OpenAI envelope. `*anthropic.Error` carries the `type` of the
Anthropic envelope (`authentication_error`, `permission_error`,
`not_found_error`, `rate_limit_error`, and the rest). `genai.APIError` carries
the `status` string and the `ErrorInfo` detail whose `reason` is the gateway's
code. Expect the same mapping for an unknown key, a key not allowed on the
route, an unknown route, a malformed request, a request the upstream rejects,
an upstream rate limit, and an upstream failure, in both unary and streaming
modes. The OpenAI and Anthropic SDKs retry a `429` after the `Retry-After`
duration that the gateway relays.

### AWS Bedrock SDK

AWS Bedrock Runtime SDK clients can call the gateway's native `/bedrock` surface
directly for `/bedrock/model/{route}/converse`, `converse-stream`, `invoke`, and
`invoke-with-response-stream`. Pass the published OLP route slug as `ModelId`
instead of an upstream Bedrock model ID or ARN.

Incoming AWS SigV4 signatures are never accepted as gateway authentication.
Attach an OLP inference API key in the `X-OLP-API-Key` header (or a standard
non-SigV4 `Authorization: Bearer` header). The gateway strips all inbound
`Authorization` and `X-Amz-*` headers, rewrites the route slug to the configured
Bedrock model, and re-signs the outbound request with the provider's AWS
credentials. Bedrock surface errors set the `X-Amzn-Errortype` response header
to the gateway error code.

```go
import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

client := bedrockruntime.New(bedrockruntime.Options{
	Region:       "us-east-1",
	BaseEndpoint: aws.String("https://olp.example.com/bedrock"),
	Credentials:  aws.AnonymousCredentials{},
	APIOptions: []func(*middleware.Stack) error{
		smithyhttp.AddHeaderValue("X-OLP-API-Key", olpKey),
	},
})
```

Remember to route `/bedrock` to the gateway service at your edge proxy or
ingress, because the bundled Helm Ingress omits `/bedrock` by default. See the
[Amazon Bedrock connector guide](providers/bedrock.md) for qualified Converse
and InvokeModel families, event-stream validation, and authentication details.

## Gateway behavior that shapes client configuration

Model names in client requests are always published route slugs, and base URLs
select native surface ingress. The `allow_provider_state` key policy gates
retained provider state, and translated Responses routes refuse `store`,
including `false`, while static client fixtures have no database-backed resource
store. Native Gemini embeddings require strict native profiles, whereas
transformed clients should use `/v1/embeddings`. A missing route returns `404`,
and an incompatible transformed surface returns `400`.

An upstream `429` cools down the credential slot for the bounded `Retry-After`
period, which defaults to ten seconds. When no other eligible slot exists,
subsequent requests receive `503` during cooldown. Repeated `Anthropic-Beta`
header lines are folded into a single comma-separated list, while repeated
`Anthropic-Version` headers are refused. The gateway sets `X-Request-Id` on
every response, and sets `Request-Id` with the matching value only on the
Anthropic surface so Anthropic SDK errors expose the gateway request ID.

Cross-protocol translation refuses unpreservable strict tools, nonempty
`include` or safety settings, `store`, and Gemini parallel-tool controls, while
empty or default hints may be ignored. Gemini JSON Schema and type spellings map
only where representable, and late system messages cannot be hoisted into
Anthropic's or Bedrock's initial system field. Anthropic requires a caller
output-token bound or a provider `max_tokens` default because OLP never invents
one, and configured defaults never override explicit caller values.

Native OpenAI Chat streams request and relay usage, and provider-native
consumption remains authoritative even if response projection or client delivery
fails. Gemini `functionResponse` objects become JSON-text tool results across
dialects, while framework wrappers remain intact. Translated embeddings report
observed input usage only and never invent counts. See
[compatibility](compatibility.md) for supported and refused mappings and
[provider profiles](provider-profiles.md) for native and default rules.

## Negotiated tool continuation

The public `chat-anthropic-tools-v1` carrier lets an explicit strict OpenAI
Chat route reach a qualified direct Anthropic Messages profile with native
reasoning and tool defaults. Native Anthropic SDK requests send complete
history through the native API and need no carrier.

Each request carries one timestamp and UUID submission identity. A streaming
response contains ordinary Chat chunks plus ordered `olp.observation`
extensions, and the standard assistant message and opaque handle are usable
only after a terminal `olp.ready:true` delivery. The terminal extension also
returns validated provider-native usage categories in `olp.native_usage`.
Cache-read tokens appear in standard Chat `prompt_tokens_details`, while
cache-write and TTL details stay in that versioned extension. An unknown
native category makes the delivery incomplete. The next turn sends the
complete assistant message plus one ordered result per tool call, with the
previous handle and a fresh submission identity. Keep the submission identity
unchanged when retrying the same request after a client or network failure.
A retry of a committed result returns the exact recorded delivery and
performs no inference.

Each Chat Completions request in the workflow carries the contract in headers.
The submission identity is the Unix time in milliseconds, a dot, and a random
UUID, and the gateway refuses one older than fifteen minutes or more than five
minutes ahead of its clock with `invalid_submission_identity`. The first turn
sends no handle, and every later turn sends the `olp.handle` value from the
previous ready delivery.

```http
X-OLP-Continuation: chat-anthropic-tools-v1
X-OLP-Submission-ID: <unix-milliseconds>.<uuid>
X-OLP-Continuation-Handle: <olp.handle from the previous ready delivery>
```

The route's API key needs `inference`, route access, and
`allow_provider_state:true`. The handle references encrypted native reasoning
and complete provider dependencies in the existing resource authority for up
to 24 hours. It is scoped to that key, route, provider revision, serving
identity, parent branch, and live credential permission. Recovery requires the
same key and contract header.

```http
GET /v1/continuation-submissions/{timestamp.UUID}
GET /v1/continuations/{continuation_<uuid>}
X-OLP-Continuation: chat-anthropic-tools-v1
Authorization: Bearer <api-key>
```

These read-only endpoints recover a lost response. A ready recovery contains
the standard assistant message and the original recorded unary body or
streaming chunks, with no opaque native signature. An accepted request that
never committed a ready delivery returns `continuation_outcome_unknown` and
must not be resent under a new submission identity as if it were known unused,
because the provider may have accepted work before the gateway saw a terminal
result. OLP does not promise exactly-once provider work or tool execution. A
changed history, missing tool result, changed control, expired or tampered
handle, or revoked key or credential fails closed. Ordinary text observations
may arrive before the encrypted ready commit, and ordinary actionable tool
chunks arrive only after it. A bare official SDK without the explicit carrier
gets a pre-dispatch `state_carrier` error on this translated workflow.

## Open items

Coding agents were qualified against their own dialects rather than cross-vendor
routes. Claude cache, thinking, context, output, and safeguards controls, Codex
retained, reasoning, and `include` controls, and Gemini `topK` and thinking
controls can refuse cross-dialect translation because semantic dropping is a
product decision rather than a client workaround.

Ordinary-route Codex continuation relies on history replay. Database-backed
retained continuation and key-limit typed `429` behavior were covered only by
the deleted service suites. [Code mode](code-mode.md) continuation was qualified
against controlled fixtures rather than live subscriptions.

Anthropic auto-mode classifier results were not simulated. Safeguards and beta
request relay were covered, while server safeguard-results relay was not. Slow
Claude `401` retries were out of scope, and route-denied `403` was the covered
refusal.

Framework coverage omitted voice, realtime, sandbox, post-first-event stream
failures, and image URLs, while pre-first-byte failures were covered. LlamaIndex
`OpenAIResponses` is unary-only because its streaming implementation repeats
text deltas, and unconditional `store` fields refuse translation. Gemini needs
`safetySettings: []` on translated routes and retains the model-naming
allowlist constraints noted above. The Vercel AI SDK Anthropic structured-output
forcing with parallel tool use disabled cannot map to Gemini. Deprecated
LlamaIndex leaf packages retain their final registry releases with no named
successor, while the umbrella and workflow packages remain separate.

Fixture success was never live-provider or subscription qualification.

## Verifying a client by hand

The suites were subtracted with the test system in commit `dbce24318` and
remain in git history at commit `99234cb0` (just before `dbce24318`). To verify
a client today, install it outside the repository, point it at a local gateway
with the configuration above, and run a unary call, a streaming call, a tool
loop where the client supports one, and one refused request. Inspect both the
client result and the request the upstream received. Record the client version,
platform, route, and outcome with the change that relies on the result.

To advance a pin in the tables above, verify the new release by hand as
described, then update the version and coverage text to match what ran.
Fix incompatibility in the gateway when the gateway is wrong, and record it as
an open item when the client is.
