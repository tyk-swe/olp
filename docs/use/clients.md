# Client compatibility

OLP keeps compatibility with real clients as a tested property. The
[client qualification suite](../../tests/clients/README.md) runs coding agents,
agent SDKs and frameworks headless against a gateway with a deterministic
scripted upstream. Each suite checks the client's outcome and what the upstream
received, so bypasses and damaged requests fail qualification.

Codex, Claude Code and OpenCode resolve the publisher's npm `latest` channel
anew for every CI job. Code-mode qualification and ordinary-route client suites
share the resolved binaries. Metadata failures and compatibility regressions fail
the job. Package URLs, SHA-512 integrity and exact binary versions are retained
in the `coding-client-releases` artifact, including failed runs. Historical
fixture references do not select releases. See
[code-mode qualification](../develop/qualification.md#latest-official-clients).

SDKs, frameworks and Gemini CLI retain exact pins under the
[dependency policy](../../CONTRIBUTING.md#dependency-policy). Their source of truth
is `tests/clients/package.json` and `go.mod`. To run the suites, see
[Running the suites](#running-the-suites).

## Client releases

The three coding clients use `latest`; other versions below remain exact pins.

| Client | Package | Pinned version | Qualification |
| --- | --- | --- | --- |
| Claude Code | `@anthropic-ai/claude-code` | latest | `claude -p` on a strict and on a transformed route: streaming, tool-use loops, parallel tool calls, prompt caching, `count_tokens` through `/context`, the paired `anthropic-beta` headers and request fields including the auto-mode `safeguards` field, session resume, API-key and bearer credentials, typed errors |
| Codex CLI | `@openai/codex` | latest | `codex exec` on a transformed route with the stock tools and on a strict route with the hosted tools off: Responses streaming, reasoning items replayed with their encrypted content, shell tool calls (parallel too), `exec resume` history replay, typed errors |
| OpenCode | `opencode-ai` | latest | Code-mode generated configuration and controlled coding-plan journeys; see [qualification](../develop/qualification.md) |
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
| OpenAI Go | `github.com/openai/openai-go/v3` | v3.74.0 | Chat Completions and Responses, unary and streaming, tool loops, structured output, embeddings, model listing, typed errors |
| Anthropic Go | `github.com/anthropics/anthropic-sdk-go` | v1.79.1 | Messages, unary and streaming, tool use loops with signed thinking, `count_tokens`, prompt caching, beta features, model listing, typed errors |
| Google Gen AI Go | `google.golang.org/genai` | v1.73.0 | `generateContent`, unary and streaming, function calling, thoughts, structured output, `countTokens`, native batch embeddings, model listing, typed errors |

## Gemini managed-agent resources

The pinned `google.golang.org/genai@v1.75.0` contract exposes agents under its
nested interactions client. The gateway supports create/list/get/delete at
`/gemini/v1beta/agents`; PATCH is excluded. Use a published `agent_resource`
route and an inference key with provider-state permission. SDK retry attempts
must reuse the same creation identity; disable automatic retries when in doubt.

For example, with `OLP_BASE_URL`, `OLP_API_KEY` and a stable
`OLP_SUBMISSION_KEY` set for one logical creation:

```sh
curl "$OLP_BASE_URL/gemini/v1beta/agents" \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H "Idempotency-Key: $OLP_SUBMISSION_KEY" \
  -H 'X-OLP-Route: managed-agents' \
  -H 'Content-Type: application/json' \
  --data '{"description":"Support agent","system_instruction":"Give concise answers."}'
```

Use the returned `agent_resource_…` ID for reads/deletes. `base_agent` accepts
an owned, compatible local agent ID; raw provider agent/environment references
are not accepted. Listing returns only resources created by this key. See the
[operator recovery notes](../operate/operations.md#managed-gemini-resources)
before retrying an ambiguous creation. This API manages resources; it does not
execute agents.

## What the suites assert

Every suite checks the client's result **and** recorded upstream requests:
path/model rewrite, provider credential (never caller key), tools/schemas/reasoning,
usage and typed errors. Clients run with empty environment/private home, telemetry
off and loopback-only egress; a bypass fails.

Fixtures model native operations deterministically, not model quality, actual
account permission or every upstream rule. Configuration and limitations below
remain part of qualification. See the
[harness contract](../../tests/clients/README.md#the-harness-contract).

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
OpenCode Go and coding-plan accounts, which one route may mix; see
[code mode](code-mode.md#client-configuration).
The shared resolution policy and evidence for that are in the
[code-mode qualification](../develop/qualification.md), not in the tables below.

Generate mounted-key setup with [`olp client-env`](../operate/automation.md#configure-qualified-clients).
It supports Codex, Claude Code and Gemini CLI, all four framework families and explicit
JavaScript, Python and Go constructors for the official SDKs. Generated
configuration runs against the same pinned libraries in client qualification.

Anthropic's Python SDK 2.x and JavaScript SDK 1.x default to an API revision
that OLP has not qualified. Set `anthropic-version` to `2023-06-01` in the
client's default headers. Use `default_headers={"anthropic-version": "2023-06-01"}`
in Python or `defaultHeaders: { "anthropic-version": "2023-06-01" }` in
JavaScript. The generated SDK constructors set this header for you.

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
the gateway as the [deployment guide](../operate/deployment.md) describes. A loopback
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

- Model names are published route slugs; base URLs select native ingress.
- `allow_provider_state` gates retained state. Translated Responses refuses `store`,
  including false; static client fixtures have no database-backed resource store.
- Native Gemini embeddings require strict native profiles; transformed clients use
  `/v1/embeddings`. Missing route is 404; wrong transformed surface is 400.
- Upstream 429 cools the slot for bounded Retry-After (default ten seconds); without
  another eligible slot, subsequent requests receive 503 during cooldown.
- Repeated Anthropic-Beta lines form one list; repeated Anthropic-Version refuses.
  SDK errors expose `request-id`, matching `X-Request-Id`.
- Translation refuses unpreservable strict tools, nonempty include/safety settings,
  store and Gemini parallel-tool controls. Empty/default hints may be ignored.
  Gemini JSON-schema/type spelling maps only where representable. Late system
  messages cannot be hoisted into Anthropic/Bedrock's initial system field.
- Anthropic requires caller output-token bound or provider `max_tokens` default;
  OLP never invents one. Defaults cannot override explicit caller values.
- Native OpenAI Chat streams request/relay usage. Provider-native consumption
  remains authoritative even if projection/delivery fails.
- Gemini functionResponse objects become JSON-text tool results across dialects;
  framework wrappers remain. Translated embeddings report observed input usage
  only, never invented counts.

See [compatibility](compatibility.md) for supported/refused mappings and
[profiles](provider-profiles.md) for native/default rules.

## Open items

- Coding agents qualify against their own dialects, not cross-vendor routes.
  Claude cache/thinking/context/output/safeguards, Codex retained/reasoning/include
  and Gemini topK/thinking controls can refuse translation. Semantic dropping is
  a product decision, not a suite workaround.
- Ordinary-route Codex continuation is history replay. Database-backed retained
  continuation/key-limit typed 429 need service lanes; static fixtures qualify
  upstream 429. [Code mode](code-mode.md) separately qualifies pinned
  native Codex HTTP/WebSocket/compact continuation against controlled fixtures.
- Anthropic auto-mode classifier results are not simulated: safeguards/beta request
  relay is asserted, server safeguard-results relay is not. Slow Claude 401 retries
  are outside this lane; route-denied 403 is the qualified refusal.
- Framework lanes omit voice/realtime/sandbox, post-first-event stream failures and
  image URLs. Pre-first-byte failures are covered.
- LlamaIndex OpenAIResponses is unary-only: its streaming repeats text deltas.
  Unconditional store fields refuse translation. Gemini needs `safetySettings: []`
  for translated routes and retains the example's model-naming constraints.
- AI SDK Anthropic structured-output forcing with parallel use disabled cannot map
  to Gemini. Deprecated LlamaIndex leaf packages retain final registry releases
  (no named successor); umbrella/workflow are separate.

Fixture success is not live-provider or subscription qualification.

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
[client qualification guide](../../tests/clients/README.md) describes the harness
contract and how to add a client, and the
[testing guide](../../tests/README.md#client-and-agent-qualification) places the
suites among the others.

To advance a pin, change it with the package manager in `tests/clients` (`pnpm`
for the npm clients, `go get` for the Go SDKs), update the table under
[Pinned releases](#client-releases), and run the suites. A release that breaks
compatibility shows up here as a failing suite, to be fixed in the gateway if
the gateway is wrong, or recorded as an open item if the client is.
