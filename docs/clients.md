# Client compatibility

OLP keeps compatibility with real clients as a tested property. The
[client qualification suite](../tests/clients/README.md) runs pinned releases of
coding agents, agent SDKs and frameworks headless against a gateway whose
upstream is a deterministic scripted fixture. Each suite asserts the client's
own outcome and what the upstream actually received, so a client that bypasses
OLP, or a gateway that mangles a request, fails.

This page names the pinned releases, what the suites assert, the configuration
each client was tested with, and what is not yet qualified. Pinned versions
advance through the [dependency policy](../CONTRIBUTING.md#dependency-policy),
and `make integration` runs the suites, so a client release that breaks
compatibility fails the pull request that proposes it. The
`tests/clients/package.json` and `go.mod` files are the source of truth; a
script test keeps the tables below equal to them. To run the suites yourself, see
[Running the suites](#running-the-suites).

## Pinned releases

Client versions are exact. Each is the newest release that cleared pnpm's
one-day minimum release age when the suite was written.

| Client | Package | Pinned version | Qualification |
| --- | --- | --- | --- |
| Claude Code | `@anthropic-ai/claude-code` | 2.1.286 | `claude -p` on a strict and on a transformed route: streaming, tool-use loops, parallel tool calls, prompt caching, `count_tokens` through `/context`, the paired `anthropic-beta` headers and request fields including the auto-mode `safeguards` field, session resume, API-key and bearer credentials, typed errors |
| Codex CLI | `@openai/codex` | 0.159.2 | `codex exec` on a transformed route with the stock tools and on a strict route with the hosted tools off: Responses streaming, reasoning items replayed with their encrypted content, shell tool calls (parallel too), `exec resume` history replay, typed errors |
| Gemini CLI | `@google/gemini-cli` | 0.62.0 | Headless `-p` on a transformed and on a strict route: `streamGenerateContent`, function calling with the thought signature returned, `countTokens` for an attachment, typed errors |
| OpenAI Agents SDK | `@openai/agents` | 0.18.0 | Responses with function tools, a handoff between two agents, structured output, streamed runs and reasoning items, through `OpenAIProvider`; the Chat Completions model; tool loops through Anthropic and Gemini routes |
| Vercel AI SDK | `ai` | 7.0.124 | `generateText` and `streamText` with sequential and parallel tool loops, structured output and images, and `embed` and `embedMany`, through each provider below, which also run against routes of the other vendors |
| Vercel AI SDK OpenAI provider | `@ai-sdk/openai` | 4.0.83 | Chat Completions, Responses and embeddings, including embeddings served by a Gemini route |
| Vercel AI SDK Anthropic provider | `@ai-sdk/anthropic` | 4.0.70 | Messages, prompt caching, extended thinking replayed with its signature |
| Vercel AI SDK Google provider | `@ai-sdk/google` | 4.0.87 | `generateContent`, `streamGenerateContent` and native embeddings on a strict route |
| LangChain | `langchain` | 1.5.14 | `createAgent` tool loops over each chat model below |
| LangChain core | `@langchain/core` | 1.2.13 | Messages, tools, tool-call chunk assembly and `withStructuredOutput` |
| LangChain OpenAI provider | `@langchain/openai` | 1.6.0 | `ChatOpenAI` over Chat Completions and Responses, and `OpenAIEmbeddings` |
| LangChain Anthropic provider | `@langchain/anthropic` | 1.5.11 | `ChatAnthropic`, prompt caching, extended thinking replayed with its signature |
| LangChain Google provider | `@langchain/google-genai` | 2.3.2 | `ChatGoogleGenerativeAI` and `GoogleGenerativeAIEmbeddings` on a strict route |
| LlamaIndex.TS | `llamaindex` | 0.12.1 | Tool definitions and message helpers |
| LlamaIndex.TS core | `@llamaindex/core` | 0.6.23 | Message and tool types, pinned to one copy by a pnpm override |
| LlamaIndex.TS environment | `@llamaindex/env` | 0.1.31 | Runtime support; sends no requests |
| LlamaIndex.TS OpenAI provider | `@llamaindex/openai` | 0.4.23 | `OpenAI` and `OpenAIEmbedding`; `OpenAIResponses` unary only |
| LlamaIndex.TS Anthropic provider | `@llamaindex/anthropic` | 0.3.27 | `Anthropic` through an explicit session |
| LlamaIndex.TS Google provider | `@llamaindex/google` | 0.4.1 | `Gemini` and `GeminiEmbedding` on a strict route |
| LlamaIndex.TS workflow | `@llamaindex/workflow` | 1.1.24 | The `agent` that runs tool loops |
| Zod, required by the AI SDK and the Agents SDK | `zod` | 4.6.5 | Not a client |

The official Go SDKs run in a separate Go module, so their dependencies stay out
of the gateway's module and runtime inventory.

| SDK | Module | Pinned version | Qualification |
| --- | --- | --- | --- |
| OpenAI Go | `github.com/openai/openai-go/v3` | v3.70.0 | Chat Completions and Responses, unary and streaming, tool loops, structured output, embeddings, model listing, typed errors |
| Anthropic Go | `github.com/anthropics/anthropic-sdk-go` | v1.78.0 | Messages, unary and streaming, tool use loops with signed thinking, `count_tokens`, prompt caching, beta features, model listing, typed errors |
| Google Gen AI Go | `google.golang.org/genai` | v1.72.0 | `generateContent`, unary and streaming, function calling, thoughts, structured output, `countTokens`, native batch embeddings, model listing, typed errors |

## What the suites assert

Every test holds two things at once.

- **The client's outcome.** For a coding agent, its exit status and the JSON it
  prints: the final reply, the tools it ran, the usage that came back through
  the gateway, the typed error of a key that may not use the route. For an SDK
  or framework, the values, streamed events and typed errors its own API
  returns, such as a completed tool loop or an error of the vendor's documented
  type.
- **What the upstream received.** The scripted upstream records every request,
  rejected ones included. A test checks the path, that the route slug the client
  sent reached the upstream as the upstream's own model name, that the upstream
  saw its own credential and never the caller's key, that tool results, signed
  thinking, reasoning items, `anthropic-beta` headers and cache markers arrived
  intact, and that every request got the status the test expected.

In the JavaScript suites a recording proxy sits between the client and the
gateway and captures what the client really sent. The framework suites and the
three coding agents hold that against the upstream's recording, request for
request. Where the route keeps the client's dialect, the body must reach the
upstream unchanged apart from the rewritten model, with one documented
exception: the `stream_options.include_usage` the gateway adds to a streamed
Chat Completions request. Where the route translates between dialects, the
request is held to the same tools, token limit, temperature, text and images
instead. The `harness` suite tests the proxy and that comparison without any
client, against recordings altered the way a regression would alter them. The Go
SDK suites have no proxy; they assert against the upstream's recording and the
typed values the SDK returns.

Each suite also runs from an empty environment with a private home, telemetry
and update checks off, and a proxy that refuses everything but loopback, so a
client that ignores its base URL reaches nothing and fails. The coding-agent
suites add a bait test: the agent is pointed at a listener the proxy does not
exempt and must never connect to it. The proxy it runs under is a stand-in that
records what the agent asks it for and refuses it, and the agent must have asked
for the listener, so an agent that fails to start for another reason is not
taken for one that was held. The Go suites guard each SDK's HTTP client,
or its middleware for `openai-go`, which fails any request outside the gateway.

A suite that runs no tests, a test file that defines none, or a test or group of
tests that is skipped or left to do, fails. A client that cannot run headless or
take a base URL is an [open item](#open-items), recorded here and in the suite
table of `tests/clients/run.sh`, never patched around.

## Tested configuration

Every client needs the same three settings: the base URL of the surface it
speaks, an OLP API key with `inference` scope, and a published route slug as the
model name. The base URL is the gateway origin plus `/v1` for OpenAI,
`/anthropic` for Anthropic and `/gemini` for Gemini, as in the
[compatibility matrix](compatibility.md). A route that translates between
dialects must be transformed; a strict route preserves the native invocation.

Code mode is a separate contract: a `/code/<slug>` base URL, native model names
and a subscription account behind the route. Codex, Claude Code and OpenCode
take the configuration its management API generates for routes over Codex,
OpenCode Go and GLM Coding Plan accounts, which one route may mix; see
[code mode](features/code-mode.md#client-configuration).
The pins and evidence for that are in the
[code-mode qualification](qualification/code-mode.md), not in the tables below.

### Claude Code

Claude Code treats `ANTHROPIC_BASE_URL` as the Anthropic API and sends the beta
headers and request fields it sends `api.anthropic.com`. The fields of a beta
travel with its header, and Anthropic's own
[gateway guide](https://code.claude.com/docs/en/llm-gateway-protocol#feature-pass-through)
says a gateway that drops one half produces `400` errors. OLP passes both
through unchanged on a strict route bound to the `anthropic-messages` profile and
on a transformed route to an automatic Anthropic provider. Name a published
route with `--model` or `ANTHROPIC_MODEL`. Claude Code may send background work
to a Haiku model, which the gateway does not publish, so give that a published
route too.

```sh
export ANTHROPIC_BASE_URL=https://olp.example.com/anthropic
export ANTHROPIC_API_KEY=olp_...              # or ANTHROPIC_AUTH_TOKEN
export ANTHROPIC_DEFAULT_HAIKU_MODEL=assistant ANTHROPIC_SMALL_FAST_MODEL=assistant
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
claude -p "Hello" --model assistant
```

The suite runs `claude -p` with `--output-format json` and
`--permission-mode default`, because this release's own default is auto mode,
whose classifier makes requests of its own that no test scripts.

### Codex CLI

Codex takes OLP as a custom model provider that speaks the Responses API, in
`config.toml` or the matching `-c` overrides. Codex keeps no state at the
provider: it sends `store: false` and replays the whole conversation each turn,
reasoning items included, so a resumed session needs no stored response. A
transformed route serves the stock tool set. A strict route refuses the
provider-hosted tools Codex adds by default, web search and the multi-agent
namespace, with `state_carrier`, so a strict route needs them off.

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

The suite writes the configuration above to a private `CODEX_HOME`, together
with `approval_policy = "never"`, `sandbox_mode = "read-only"` and the
`[analytics]` and `[feedback]` tables switched off. It starts `codex exec --json`
with `--strict-config`, so a key a release no longer knows fails the run.

### Gemini CLI

Gemini CLI reads `GOOGLE_GEMINI_BASE_URL` and `GEMINI_API_KEY`. A base URL alone
selects an authentication method that headless mode refuses with `Invalid auth
method selected`, so select the Gemini API key in `~/.gemini/settings.json`.
Headless mode also needs the workspace trusted, by `--skip-trust` or
`GEMINI_CLI_TRUST_WORKSPACE=true`. The CLI counts tokens only for a prompt with
an attachment, such as `@image.png`. The settings file, merged into any existing
one:

```json
{ "security": { "auth": { "selectedType": "gemini-api-key" } } }
```

```sh
export GOOGLE_GEMINI_BASE_URL=https://olp.example.com/gemini
export GEMINI_API_KEY=olp_...
gemini --skip-trust --model assistant -p "Hello"
```

The suite also turns off telemetry, usage statistics and update checks in the
same settings file.

### OpenAI Agents SDK

The OpenAI Agents SDK takes the gateway through its `OpenAIProvider`. It exports
traces to `api.openai.com` unless they are disabled, and a strict route needs
`store: false`, because a strict Responses route treats an omitted `store` as
`store: true`.

```javascript
import { Agent, OpenAIProvider, Runner } from '@openai/agents';

const runner = new Runner({
  modelProvider: new OpenAIProvider({ apiKey, baseURL: 'https://olp.example.com/v1', useResponses: true }),
  tracingDisabled: true
});
const agent = new Agent({ name: 'Assistant', model: 'assistant', modelSettings: { store: false } });
```

Set `useResponses: false` for Chat Completions. The SDK makes function tools
strict by default. OpenAI's strict mode cannot be preserved by another vendor,
so a route that translates to an Anthropic or Gemini upstream refuses a tool
with `strict: true` by name; declare such tools with `strict: false`. An
Anthropic upstream also needs a token limit, because the SDK sends none: set
`modelSettings.maxTokens`, or give the provider a `max_tokens` default.

### Vercel AI SDK

The Vercel AI SDK reaches Chat Completions through `chat()`; calling the
provider directly selects the Responses API.

```javascript
import { createOpenAI } from '@ai-sdk/openai';
import { generateText } from 'ai';

const olp = createOpenAI({ apiKey: process.env.OLP_API_KEY, baseURL: 'https://olp.example.com/v1' });
const { text } = await generateText({ model: olp.chat('assistant'), prompt: 'Hello' });
```

The Anthropic and Google providers take a base URL that already ends in the API
version, and the Anthropic provider sets a token limit of its own unless the call
gives one. Structured output with the Anthropic provider asks for JSON through a
forced tool call that disables parallel tool use, which a Gemini upstream cannot
preserve, so the gateway refuses it there by name.

```javascript
import { createAnthropic } from '@ai-sdk/anthropic';
import { createGoogleGenerativeAI } from '@ai-sdk/google';

const claude = createAnthropic({ apiKey, baseURL: 'https://olp.example.com/anthropic/v1' });
const gemini = createGoogleGenerativeAI({ apiKey, baseURL: 'https://olp.example.com/gemini/v1beta' });
```

### LangChain

LangChain takes each base URL in the option its provider names. LangChain
retries six times by default, so set `maxRetries` as the deployment wants.
`ChatOpenAI` speaks Chat Completions unless it is given `useResponsesApi: true`.

```javascript
import { ChatAnthropic } from '@langchain/anthropic';
import { ChatGoogleGenerativeAI, GoogleGenerativeAIEmbeddings } from '@langchain/google-genai';
import { ChatOpenAI, OpenAIEmbeddings } from '@langchain/openai';

new ChatOpenAI({ model: 'assistant', apiKey, configuration: { baseURL: 'https://olp.example.com/v1' } });
new ChatAnthropic({ model: 'assistant', apiKey, anthropicApiUrl: 'https://olp.example.com/anthropic' });
new ChatGoogleGenerativeAI({ model: 'gemini-2.5-flash', apiKey, baseUrl: 'https://olp.example.com/gemini' });
new OpenAIEmbeddings({ model: 'assistant', apiKey, configuration: { baseURL: 'https://olp.example.com/v1' } });
new GoogleGenerativeAIEmbeddings({ model: 'gemini-embeddings', apiKey, baseUrl: 'https://olp.example.com/gemini' });
```

`ChatGoogleGenerativeAI` accepts images only for a model name it recognizes as
multimodal, such as `gemini-2.5-flash`, so name the route after the Gemini model.

### LlamaIndex.TS

LlamaIndex.TS takes the base URL on the OpenAI and Gemini classes, and needs an
explicit session for Anthropic. It decides from the model name whether a model
can call tools, so the `agent` runs only on a route named for a model it
recognizes: any name for OpenAI, a name containing `-3` or `-4` for Anthropic,
and a listed Gemini name such as `gemini-2.5-flash`. LlamaIndex retries ten times
by default, so set `maxRetries`.

```javascript
import { Anthropic, AnthropicSession } from '@llamaindex/anthropic';
import { Gemini, GeminiEmbedding } from '@llamaindex/google';
import { OpenAI, OpenAIEmbedding } from '@llamaindex/openai';

new OpenAI({ model: 'assistant', apiKey, baseURL: 'https://olp.example.com/v1' });
new Anthropic({ model: 'claude-sonnet-4-5', session: new AnthropicSession({ apiKey, baseURL: 'https://olp.example.com/anthropic' }) });
new Gemini({ model: 'gemini-2.5-flash', apiKey, httpOptions: { baseUrl: 'https://olp.example.com/gemini' } });
new OpenAIEmbedding({ model: 'assistant', apiKey, baseURL: 'https://olp.example.com/v1' });
new GeminiEmbedding({ model: 'gemini-embeddings', apiKey, httpOptions: { baseUrl: 'https://olp.example.com/gemini' } });
```

### Embeddings through the frameworks

The Anthropic API has no embeddings endpoint, so no Anthropic provider of a
framework embeds. The OpenAI providers embed through `/v1/embeddings`, which any
route that has the `embeddings` capability serves, including a Gemini route by
translation. The Google providers use the native `embedContent` and
`batchEmbedContents` endpoints, which only a strict route serves.

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
the gateway as the [deployment guide](deployment.md) describes. A loopback
development gateway needs `option.WithUnsafeAllowHTTP()`.

Each Go SDK reports the gateway's errors in its own typed error with the status
the vendor documents: `*openai.Error` carries the `type`, `code` and `param` of
the OpenAI envelope, `*anthropic.Error` the `type` of the Anthropic envelope
(`authentication_error`, `permission_error`, `not_found_error`, `rate_limit_error`
and the rest), and `genai.APIError` the `status` string and the `ErrorInfo`
detail, whose `reason` is the gateway's code. The suites assert this for an
unknown key, a key not allowed on the route, an unknown route, a malformed
request, a request the upstream rejects, an upstream rate limit and an upstream
failure, unary and streaming. The OpenAI and Anthropic SDKs retry a `429` after
the `Retry-After` the gateway relays, which the suites assert too.

## Gateway behavior that shapes client configuration

The suite found these while it was built; each is how the gateway behaves today.

- A transformed route forwards a caller's `anthropic-beta` header to an
  Anthropic upstream when it hands the upstream the caller's own Messages
  request, because the fields of a beta travel with its header: Claude Code
  sends `context_management`, `output_config.effort` and mid-conversation
  `system` messages with theirs. A request translated from another dialect
  carries no Anthropic semantics, so it gets no header. A strict route bound to
  the `anthropic-messages` profile forwards the header too. The header goes
  only to a provider that takes it: an Anthropic provider, or one whose profile
  declares `Anthropic-Beta`, as the Vertex AI hosting of Claude does. Bedrock
  InvokeModel declares none. A provider that publishes betas of its own in its
  semantic headers is sent them and the caller's, each beta once, so the fields
  of the caller's request keep their headers; a strict route refuses a caller's
  list that differs from the published one with `semantic_header_conflict`. A
  route with such a target refuses a header it cannot forward, one longer than
  2048 bytes (alone or joined with a provider's published betas) or not a valid
  header value, before any provider is called.
- The Anthropic SDKs, and Claude Code with them, add `?beta=true` to `messages`
  and `count_tokens` calls. The query selects no behavior, so a strict route
  consumes it and the upstream never sees it; any other query setting still has
  to match a setting the profile declares. Messages requests may also carry
  `system` messages between turns, which Claude Code appends.
- A strict Responses route refuses the provider-hosted tools Codex sends by
  default, web search and the multi-agent namespace, with `state_carrier`,
  because they need a lifecycle contract the gateway has not qualified. Codex
  turns them off by configuration.
- A strict Responses route treats an omitted `store` as `store: true`, which
  needs the `allow_provider_state` key policy. A client that cannot send
  `store: false` needs that policy.
- A Responses request that carries `store`, even `store: false`, is refused when
  the route translates it to an Anthropic or Gemini upstream, because the
  parameter has no equivalent there.
- `previous_response_id` and stored responses need the key policy
  `allow_provider_state` and the gateway's database-backed resource store, which
  the suite's static gateway does not have.
- Native Gemini `embedContent` and `batchEmbedContents` are served only on strict
  routes with the `gemini-generation` and `gemini-batch-embeddings` profiles. A
  client that needs Gemini embeddings through a transformed route uses the
  OpenAI `/v1/embeddings` endpoint.
- An upstream `429` cools the credential slot for the upstream's `Retry-After`
  (ten seconds when absent), so a rate-limited route answers `503` until it
  expires.
- The Anthropic Go SDK sends each beta as a line of its own in the
  `Anthropic-Beta` header. The gateway reads the lines as the one
  comma-separated list HTTP defines them to be, so a strict route accepts
  several betas however a client frames them. A repeated `Anthropic-Version` has
  no such reading and is still refused.
- The native Gemini embedding methods `embedContent` and `batchEmbedContents`
  answer `404` for a route that does not exist, as every other Gemini method
  does, and `400` for a transformed route, naming the strict route they need.
  The Google Go SDK sends every `EmbedContent` as one `batchEmbedContents`
  request whose `requests` array holds one entry per content.
- The Anthropic SDKs read the request ID of an error from the `request-id`
  response header. The gateway sends it on the Anthropic surface beside
  `X-Request-Id`, with the same value, so `anthropic.Error.RequestID` is the ID a
  user quotes to support.
- A route whose upstream speaks another dialect translates the request and
  refuses, by name and before any provider is called, what the target cannot
  preserve: `strict: true` tools, a non-empty `include` or `safetySettings`,
  `store`, and `parallel_tool_calls` for Gemini. It ignores what a client only
  spells out as a default or a delivery hint: `strict: false`, an empty
  `include` or `safetySettings`, the `name` of a tool message, and
  `eager_input_streaming`. It reads the JSON Schema fields `parametersJsonSchema`
  and `responseJsonSchema` that current Google SDKs send, and gives other vendors
  the lower-case type names of JSON Schema where Gemini's own dialect spells them
  in capitals, with the schema's members in the order the client wrote them. In
  the other direction a schema an OpenAI or Anthropic client wrote (`$schema`,
  `additionalProperties`, `$ref` and the like) reaches a Gemini provider in
  `parametersJsonSchema` or `responseJsonSchema`, which take JSON Schema as it is,
  where Gemini's `parameters` and `responseSchema` refuse members outside their
  OpenAPI subset; a schema within the subset goes in those, as before.
- Anthropic requires a token limit that OpenAI and Gemini clients do not always
  send, such as the OpenAI Agents SDK. The gateway refuses to invent one, naming
  `max_output_tokens`, so such a client sets a limit itself or the Anthropic
  provider sets `parameter_defaults` with `max_tokens`.
- The gateway asks an OpenAI-dialect upstream to report usage on every streamed
  Chat Completions request by adding `stream_options.include_usage`, and
  relays the usage chunk that follows. Across the suites it is the only change
  to a request that keeps its dialect, apart from the rewritten model.
- A tool result a Gemini client sends is a `functionResponse` object, and another
  vendor receives that object's JSON as the text of the result. The AI SDK
  wraps the output as `{name, content}` and LangChain as `{result}`.
- `/v1/embeddings` answered by a Gemini upstream reports as its `usage` the
  `promptTokenCount` of the response's `usageMetadata`, which the AI SDK reports
  as its token count. A response without one has no `usage`.

## Open items

What the suites do not yet qualify, and the client defects they found:

- None of the three coding agents runs through a route that translates to
  another vendor, because the gateway refuses, by name and before any provider
  is called, what the target cannot preserve, and each agent sends such fields
  on every request. Claude Code sends `cache_control`, `thinking`,
  `context_management`, `output_config`, `metadata` and `safeguards` to an
  OpenAI or Gemini route. Codex sends `store`, `reasoning`, `include`,
  `prompt_cache_key` and `client_metadata` to an Anthropic or Gemini route.
  Gemini CLI sends `generationConfig.topK` and `thinkingConfig` to an OpenAI or
  Anthropic route. Serving them needs a way to drop what a target cannot
  preserve, as an operator's choice; it is not a change to this suite.
- Codex's stored-response continuation is its history replay, which the suite
  qualifies. Codex sends `previous_response_id` only over its Responses
  WebSocket transport (`supports_websockets`), which the gateway does not serve,
  and it defaults to HTTP.
- The scripted upstream does not simulate Anthropic's server-side auto-mode
  classifier, so Claude Code prints that the session is not eligible for
  no-charge classifier requests. The suite asserts the `safeguards` request
  field and its paired beta reach the upstream unchanged; the `safeguard_results`
  the real API returns is not simulated, so its relay is not asserted.
- Claude Code retries a `401` with backoff, so `claude -p` with an invalid key
  takes minutes to fail. The suite qualifies the `403` for a key not allowed on
  the route instead.
- The framework suites cover chat, tools, structured output, images and
  embeddings. They do not cover the OpenAI Agents SDK's voice, realtime and
  sandbox features, a stream that fails after its first event, or an image given
  by URL instead of inline data.
- LlamaIndex.TS `OpenAIResponses` is qualified for unary chat and tool loops
  only. Its streaming repeats the last text delta once for each event that
  follows it, and the `agent` streams every step, so both return text such as
  `fixture.fixture.fixture.fixture.fixture.`. The gateway relays the Responses
  stream event for event, as the upstream sent it, and the AI SDK, LangChain
  and the Agents SDK assemble the same stream correctly, so the defect is the
  client's. The class also sends `store: false`, `metadata: {}`, `user: ""` and
  `reasoning: {}` on every request, and a route that translates to another
  vendor refuses `store`; the suite pins that refusal.
- The LlamaIndex.TS `Gemini` class sends safety settings that switch the filters
  off. A route that translates to another vendor refuses them, so the class
  needs `safetySettings: []` there. The class and the `agent` also depend on
  model names, as the configuration above describes.
- The AI SDK Anthropic provider's structured output cannot reach a Gemini
  upstream: it forces a tool call with parallel tool use disabled, which Gemini
  cannot preserve. The gateway refuses it with `parallel_tool_calls`, and the
  suite pins the refusal.
- The `429` the Go SDK suites qualify is an upstream rate limit relayed by the
  gateway. The gateway's own limits on an API key (requests, tokens,
  concurrency and cost budgets) need the Valkey limiter, which the static
  gateway does not have: a key with such a limit answers `503`
  `distributed_limits_unavailable` there. The typed `429` for a key limit needs
  a Valkey-backed qualification lane.
- The scripted upstream fails a request before its first byte, so the suites
  cover errors that precede a stream. A stream that fails after its first event
  is not scripted.
- Stored-response continuation through the gateway needs a database-backed
  qualification lane. The scripted upstream implements `previous_response_id`,
  and its own tests cover it.
- The pinned LlamaIndex.TS packages, except the `llamaindex` umbrella and
  `@llamaindex/workflow`, are marked deprecated on the npm registry as no longer
  maintained, and the registry names no successor. The suite pins their final
  releases.
- Whether a translated Responses request should drop `store` is undecided. The
  Agents SDK sends no `store` unless asked, and Codex qualifies against OpenAI
  routes, where it is native.

## Running the suites

```sh
pnpm install --frozen-lockfile                       # the npm clients, once, from the repository root
tests/clients/run.sh                                 # every suite, in table order
CLIENTS=claude-code,codex tests/clients/run.sh       # only these
tests/clients/run.sh --list                          # the suite names
```

The suites are `harness` (the harness's own tests), `ai-sdk`, `go-sdks`,
`claude-code`, `codex`, `gemini-cli`, `openai-agents`, `langchain` and
`llamaindex`; `CLIENTS` takes a comma-separated list of them and rejects any
other name. The script builds and starts the gateway host and the scripted
upstream, runs the selected suites one at a time, prints `passed`, `failed` or
`skipped` for each, and exits nonzero when one fails. A suite whose client is an
open item is a row of the table in `tests/clients/run.sh` with a stated reason
and prints as `skipped`.

It needs Go, Node.js 26, `jq` and `curl`, and uses no containers, database or
Valkey. The npm clients come from `pnpm install`, and the Go SDK module
downloads its dependencies once before the suites run, so a run needs the network
only to fill those caches. Two environment needs follow from what the suites
prove:

- The egress tests bind a listener on an address the proxy trap does not exempt:
  `127.0.0.2` or, if that cannot be bound, a non-loopback IPv4 address of the
  host. A host with neither fails them.
- The Codex suite runs its shell tool in Codex's own read-only sandbox, so the
  environment must allow that sandbox. One that does not fails the suite
  instead of skipping it. CI's integration job uses Ubuntu 26.04 for its
  distribution-provided bubblewrap support; qualification does not disable the
  sandbox or change host security policy.

`OLP_CLIENTS_READY_TIMEOUT_SECONDS` (60), `OLP_CLIENTS_SUITE_TIMEOUT_SECONDS`
(900) and `OLP_CLIENTS_TEST_TIMEOUT_SECONDS` (180) bound the gateway start, a
suite and a test of a Node suite. The Go SDK suite is one package, which the Go
toolchain bounds as a whole, so the suite timeout bounds it, and each SDK call in
it has a deadline of its own, 30 seconds. A signal to the script, such as an
interrupt, stops the running suite and the clients it started at once. The full
run takes about two minutes. `make integration` runs
the same script after the SDK smoke suites, which is how CI runs it. The
[client qualification guide](../tests/clients/README.md) describes the harness
contract and how to add a client, and the
[testing guide](../tests/README.md#client-and-agent-qualification) places the
suites among the others.

To advance a pin, change it with the package manager in `tests/clients` (`pnpm`
for the npm clients, `go get` for the Go SDKs), update the table under
[Pinned releases](#pinned-releases), and run the suites. A release that breaks
compatibility shows up here as a failing suite, to be fixed in the gateway if
the gateway is wrong, or recorded as an open item if the client is.
