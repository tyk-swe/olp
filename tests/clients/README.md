# Client qualification

Real, pinned client releases run headless against an OLP gateway whose upstream
is a deterministic scripted fixture. Every suite asserts two things: the
client's own outcome, and what the upstream actually received, from a recording
of its requests. A client that bypasses OLP, or a gateway that mangles a
request, fails. [Client compatibility](../../docs/clients.md) names the pinned
releases and the configuration each suite tested.

```sh
pnpm install --frozen-lockfile        # the npm clients, once
tests/clients/run.sh                  # every suite
CLIENTS=harness,ai-sdk tests/clients/run.sh
tests/clients/run.sh --list
```

`run.sh` needs Go, Node.js, `jq` and `curl`. It builds and starts the gateway
and upstream, runs each suite in table order, prints `passed`, `failed` or
`skipped` with the reason for each, and tears everything down. It exits nonzero
when a suite fails. `CLIENTS` names the suites to run; an unknown name is an
error. A suite that cannot run because a toolchain or package is missing fails
rather than skipping, and so does one that runs no tests, has a test file that
defines none, or skips or leaves a test or a group of tests to do. The only
skips are rows of the table in `run.sh` with a stated reason, which are visible
open items. A signal to `run.sh` stops the running suite, its clients and the
gateway at once. `scripts/integration.sh` runs it as part of `make integration`.

## Layout

| Path | Contents |
| --- | --- |
| `run.sh` | The suite table, harness start and teardown, per-suite isolation |
| `lib/harness.mjs` | The harness contract for JavaScript suites: environment, recording, directives |
| `lib/cli.mjs` | The coding-agent suites' helpers: the pinned binary, a clean environment, a bounded run that kills the whole process group, also when the suite itself is stopped, the bait and `assertHeldByTheTrap`, which prove the proxy trap holds, and `assertPassedThrough` |
| `lib/leaf-tests.mjs` | The `node:test` reporter that lists the tests that ran, by file, and the tests and groups skipped or left to do, which `node:test` does not otherwise report for a skipped group |
| `lib/check-ran.sh` | What `run.sh` asks of that list: a test in every test file, and nothing skipped or left to do |
| `lib/tap.mjs` | A recording proxy between a client and the gateway: what the client sent and what OLP answered |
| `lib/relay.mjs` | `assertRelayed`, which holds the tap's recording against the upstream's, and the dialect helpers it uses |
| `suites/<name>/*.test.mjs` | `node:test` suites; `harness` checks the harness itself, including the tap, the relay assertions, the helpers of `lib/cli.mjs`, the reporter and `lib/check-ran.sh`, the others are one client each |
| `gosdk/` | Go SDK tests with the `integration` build tag, in the Go module of this directory: `harness_test.go` is the harness contract, one file per SDK, and `errors_test.go` holds the typed-error contracts |
| `package.json`, `go.mod` | Exact pins of every client, as development dependencies |
| `../clientfixture/` | The gateway host and, in `scripted/`, the scripted upstream |

The gateway host and the upstream are Go packages of the repository's root
module, because they import the gateway's internal packages. The Go module in
this directory stays black-box: it imports only the official SDKs and talks to
the harness over HTTP, so SDK dependencies never enter the gateway's module or
its runtime inventory. Root `go vet ./...` does not reach it, so `run.sh` vets
it before running its tests. It downloads the module's dependencies before the
isolation below begins; inside it `GOPROXY=off` leaves nothing to fetch.

## The harness contract

`tests/clientfixture` hosts the gateway in-process with a static runtime and no
database, as `tests/sdkfixture` does, and writes the contract below as a JSON
object of environment variables. `run.sh` exports them to each suite.

| Variable | Value |
| --- | --- |
| `OLP_CLIENTS_ORIGIN` | Gateway origin, `http://127.0.0.1:<port>` |
| `OLP_CLIENTS_API_KEY` | A key with `inference` and `models_read` scope for every route; provider state is forbidden |
| `OLP_CLIENTS_STATE_API_KEY` | The same scopes with `allow_provider_state`; retention-policy qualification uses it to verify transparent opt-in semantics |
| `OLP_CLIENTS_RESTRICTED_API_KEY` | A key allowed on no published route: it receives `403 route_forbidden` |
| `OLP_CLIENTS_OPENAI_BASE_URL` | Origin plus `/v1` |
| `OLP_CLIENTS_ANTHROPIC_BASE_URL` | Origin plus `/anthropic`; Anthropic SDKs append `/v1` |
| `OLP_CLIENTS_GEMINI_BASE_URL` | Origin plus `/gemini`; Gemini SDKs append the API version |
| `OLP_CLIENTS_UPSTREAM_URL` | The scripted upstream, which serves the recording API |
| `OLP_CLIENTS_MODEL_OPENAI`, `_ANTHROPIC`, `_GEMINI` | Transformed route slugs, the model names a client sends |
| `OLP_CLIENTS_MODEL_OPENAI_STRICT`, `_ANTHROPIC_STRICT`, `_GEMINI_STRICT` | Strict route slugs |
| `OLP_CLIENTS_MODEL_GEMINI_EMBED_STRICT` | Strict route for native Gemini embedding endpoints |
| `OLP_CLIENTS_MODEL_ANTHROPIC_CLAUDE`, `_GEMINI_FLASH` | Transformed routes named after a vendor model (`claude-sonnet-4-5`, `gemini-2.5-flash`), for clients that decide from the model name what a model can do |
| `OLP_CLIENTS_UPSTREAM_MODEL_OPENAI`, `_ANTHROPIC`, `_GEMINI` | The model names the upstream sees after the gateway rewrites a slug |
| `OLP_CLIENTS_DEFAULT_REPLY` | The text of a reply with no directive |
| `OLP_CLIENTS_TOOL_RESULTS_PREFIX` | The text before the results in the final reply of a tool loop |
| `OLP_CLIENTS_SCRATCH` | A private directory for the suite, set by `run.sh` |

### Routes

A transformed route accepts any client dialect and translates it, so
`olp-openai` also serves an Anthropic or Gemini client. A strict route preserves
the native invocation and needs a provider per explicit versioned profile.

| Slug | Fidelity | Upstream | Serves |
| --- | --- | --- | --- |
| `olp-openai` | Transformed | OpenAI | Chat Completions, Responses, input token counts and embeddings; Messages and `generateContent` by translation |
| `olp-anthropic` | Transformed | Anthropic | Messages and `count_tokens`; Chat Completions, Responses and `generateContent` by translation |
| `olp-gemini` | Transformed | Gemini | `generateContent`, `streamGenerateContent` and `countTokens`; Chat Completions and Messages by translation; `/v1/embeddings` through `embedContent` |
| `olp-openai-strict` | Strict | OpenAI, profiles `openai-chat` and `openai-responses` | The same OpenAI endpoints, unchanged |
| `olp-anthropic-strict` | Strict | Anthropic, profile `anthropic-messages` | Messages and `count_tokens`, with `anthropic-beta` forwarded |
| `olp-gemini-strict` | Strict | Gemini, profile `gemini-generation` | Native Gemini generation and counting |
| `olp-gemini-embed-strict` | Strict | Gemini, profiles `gemini-generation` and `gemini-batch-embeddings` | Native `embedContent` and `batchEmbedContents` |
| `claude-sonnet-4-5` | Transformed | Anthropic | As `olp-anthropic`; named after the model it stands for |
| `gemini-2.5-flash` | Transformed | Gemini | As `olp-gemini`; named after the model it stands for |

Model metadata declares only the optional parameters the scripted models take,
so structured output is admitted; context and output limits stay unknown, so
admission never excludes a target on an estimate.

### Isolation

`run.sh` starts each suite with `env -i` and an allowlist, so nothing from the
developer's environment reaches a client: no API keys, endpoints or cloud
credentials. A suite gets a private `HOME`, `XDG_*` tree and `TMPDIR` under the
run's scratch directory, telemetry and update checks turned off, and a proxy
that refuses everything except loopback (`NODE_USE_ENV_PROXY=1`), so a client
that bypasses OLP fails instead of reaching the network. Client-specific
configuration, such as `CODEX_HOME`, belongs to the suite and goes under
`OLP_CLIENTS_SCRATCH`. Suites run one at a time because the recording is shared, and they share one
gateway and upstream for the whole run. Provider health therefore carries
across suites: an upstream `429` cools the credential slot for its `Retry-After`,
one second for the scripted upstream, and a route answers `503` until then, and
five counted upstream failures in a row, such as `[[olp:fail 500]]`, within
thirty seconds open the provider's circuit for thirty. A suite that scripts
failures should use few of them, and one that scripts a `429` should wait for
the route to serve again before it ends; `awaitServing` in the Go harness does.
JavaScript suites point each client at a tap on loopback, so one that ignores
its base URL reaches only the proxy trap, and `localFetch` in `lib/harness.mjs`,
which fails any request outside the gateway, serves the helpers that call the
gateway themselves. The Go harness does the same for each Go SDK:
through the HTTP client of the Anthropic and Google SDKs, and through
middleware for the OpenAI SDK, which sends loopback requests through a
transport of its own.

## The scripted upstream

`scripted/` serves three vendors under their own prefixes. Every reply is a pure
function of the request, so a client and the gateway in front of it can be held
to exactly what the upstream received. The upstream accepts only its own
credential and one model name per vendor, which makes an unrewritten model or a
leaked caller key a visible failure.

| Vendor | Prefix | Endpoints |
| --- | --- | --- |
| OpenAI | `/openai/v1` | `POST /chat/completions`, `POST /responses`, `POST /responses/input_tokens`, `GET` and `DELETE /responses/{id}`, `GET /responses/{id}/input_items`, `POST /embeddings` |
| Anthropic | `/anthropic/v1` | `POST /messages`, `POST /messages/count_tokens` |
| Gemini | `/gemini/v1beta` | `POST /models/{model}:` `generateContent`, `streamGenerateContent`, `countTokens`, `embedContent`, `batchEmbedContents` |

Each endpoint answers unary and, where the API has one, streaming, in the
vendor's own format and error envelope. Behaviors:

- **Tool loops.** A request that asks for a tool call gets one; the follow-up
  that carries the tool result gets final text, `The tools returned: <results>`.
  Parallel calls, several rounds and forced `tool_choice` are supported. A
  tool the request did not declare is never called.
- **Reasoning.** Responses return a `reasoning` item when the request has
  `reasoning`, with `encrypted_content` when `include` asks for it. Anthropic
  returns a signed `thinking` block when `thinking` is enabled, and rejects a
  history whose signature was altered, as the real API does. Gemini returns
  thought parts when `includeThoughts` is set.
- **Stored responses.** Responses are stored unless `store` is false; a request
  with `previous_response_id` sees the stored conversation, and an unknown
  identifier is a 404.
- **Prompt caching.** Anthropic accepts `cache_control` on tools, system and
  message blocks. The first request with a breakpoint reports
  `cache_creation_input_tokens`, a later request with the same prefix reports
  `cache_read_input_tokens`, and a second breakpoint reads the first prefix and
  writes the difference. `ttl: "1h"` is reported under `cache_creation`.
- **Structured output.** `response_format`, `text.format`, `output_config.format`,
  `output_format`, `responseSchema` and `responseJsonSchema` get a JSON instance
  of the schema. A forced tool call gets arguments built from the tool's schema.
- **Counting and embeddings.** Anthropic `count_tokens` equals the input usage
  the same prompt reports. Embeddings are deterministic unit vectors, returned
  as floats or base64, and equal for equal text across the OpenAI and Gemini
  vendors.
- **Contract checks.** The upstream rejects what the real APIs reject: a tool
  result that answers no call, a call left unanswered, an
  `anthropic-version`-less request, and a model it does not serve. Anthropic
  also refuses a request field whose beta header is missing, as the real API
  does and Anthropic's gateway guide documents: `context_management`,
  `safeguards`, `output_config.effort` and a `system` message between turns
  each pair with a header, matched by its name before the date. A gateway that
  forwards the body and drops the header answers `400`.
- **Thought signatures.** A Gemini step that reasoned signs its first function
  call with `thoughtSignature`, which a client has to return.

### Directives

A suite scripts the upstream by writing directives into the user prompt it sends
through the client. Each is `[[olp:KEYWORD ARGUMENT]]` with a JSON argument, and
`lib/harness.mjs` exports `script` builders for them.

| Directive | Effect |
| --- | --- |
| `[[olp:tool NAME {"a":1}]]` | Call tool `NAME` in the next round |
| `[[olp:also NAME {"a":1}]]` | Add a parallel call to that round |
| `[[olp:reply "text"]]` | Use this final text instead of the default |
| `[[olp:think "text"]]` | Include reasoning with this text |
| `[[olp:fail 500]]` | Answer with this upstream error status; a `429` carries `Retry-After: 1` |

The directives come from the latest user prompt, the last user message without
tool results, so a client that adds context around the prompt still works. Each
`tool` directive is a round: the upstream calls the round's tools until the
results after the prompt cover it, then moves on, and answers with final text
after the last round. A malformed directive is a `400` that names the mistake.
The grammar is documented in `../clientfixture/scripted/script.go`.

### Recording

`GET /__recorded` on the upstream lists every request in arrival order,
including rejected ones, as `{"requests": [...], "in_flight": 0}`. Filters narrow
the list: `dialect` and `path` match prefixes, `model` and `script` match
exactly. `DELETE /__recorded` forgets the recording, the stored responses and the
prompt cache, and restarts the identifier counters. `lib/harness.mjs` wraps both:
`recorded()` waits until no request is in flight, and `resetRecorded()` belongs
in `beforeEach`.

| Field | Meaning |
| --- | --- |
| `seq`, `dialect`, `method`, `path`, `query` | Which request: `openai.chat`, `openai.responses`, `openai.embeddings`, `anthropic.messages`, `anthropic.count_tokens`, `gemini.generate`, `gemini.stream`, `gemini.count_tokens`, `gemini.embed`, `gemini.batch_embed`, and the Responses retrieval dialects |
| `headers` | Lower-cased request headers; credential headers keep only `[present]` |
| `model` | The upstream model addressed, from the body or the Gemini path |
| `body`, `body_text` | The JSON body, or any other body as text |
| `authorized` | Whether the request carried the upstream credential |
| `status`, `complete` | The response status; `complete` is false while it is in flight |
| `script`, `stream` | The scripted behavior that answered, such as `tool_call`, `tool_result`, `text`, `structured`, `fail` or `error:<kind>`, and whether it streamed |
| `leaked_client_credential` | True when a caller key appeared anywhere in the request |

`assertClean` in `lib/harness.mjs` checks that every request was authorized, none
leaked a caller credential, and each had the expected status.

## Writing a suite

1. Add `suites/<name>/*.test.mjs`, or a file under `gosdk/` that starts with
   `//go:build integration`, and set the suite's row in the table in `run.sh` by
   removing its skip reason. A Go test run without the tag finds no tests, and
   one run without the harness variables fails.
2. Configure the client with the base URL, key and route slug from the
   environment. A client without a base-URL override, or one that cannot run
   headless, is an open item: leave the row skipped with that reason and record
   it in [Client compatibility](../../docs/clients.md). Do not patch the client.
3. Script the upstream with directives, run the client, then assert its outcome
   and the recording: the path, the rewritten model, that tool results and
   headers arrived, and `assertClean`.
4. Where the client takes a base URL, point it at `startTap()` in
   `lib/tap.mjs` and finish each test with `assertRelayed` from `lib/relay.mjs`.
   It holds the client's own requests against the upstream's one for one: the
   client used the documented path and the OLP key, the upstream received the
   rewritten model and its own credential, and on a route that keeps the
   client's dialect the body arrived unchanged apart from the model. On a route
   that translates, `tools`, `maxTokens`, `temperature` and `contains` check the
   semantics instead, and `toolDeclarations` and `images` read tools and images
   from either side in any dialect.
5. Put a gateway fix a client demands in the gateway, with a test beside the
   code, and list it in the pull request.

### Coding agents

The `claude-code`, `codex` and `gemini-cli` suites run the real binaries. They
differ from the library suites in three ways.

- `lib/cli.mjs` runs each agent with `clientEnvironment`, which passes only the
  private home and the proxy trap, never the harness contract, and gives it a
  working directory of its own. stdin is closed, and a run that outlives its
  deadline is killed together with every shell command it started.
- Each agent's configuration is the one `docs/clients.md` documents, written by
  the test: environment variables for Claude Code and Gemini CLI, a
  `config.toml` in a private `CODEX_HOME` for Codex. Codex is started with
  `--strict-config`, so a key a release no longer knows fails the run.
- Each suite ends with a bait test. The agent is pointed at a listener on an
  address the proxy trap does not exempt, and the test asserts the listener
  never saw a connection. `startBait` proves the listener reachable before it
  is used. `assertHeldByTheTrap` runs the agent with the proxy variables
  pointed at a stand-in that records the host and port each request or CONNECT
  asks for and refuses it, and asserts that the agent asked for the bait and
  never connected to it. An agent retries through the dead proxy until it is
  stopped, so the helper stops it five seconds after its first request; the
  run's own deadline, a minute, only bounds how long the agent may take to
  start, which on a hosted runner has taken Gemini CLI over ten seconds. An
  agent that fails for another reason, such as a flag it no longer knows, asks
  for nothing, and is not taken for one that was held; nor does any traffic
  leave the machine. The helper tests in `suites/harness` prove the bait counts
  connections that bypass the trap, that the helper refuses an agent that never
  asks or that goes round the proxy after it was refused, and that it stops a
  client soon after its first request rather than at its deadline.

A tap sits between each agent and the gateway, and `assertPassedThrough` holds
what the agent sent against what the upstream received, on every route the
agent runs through: the same JSON body but for the rewritten model, and the same
`anthropic-beta` and `anthropic-version`.
Test each agent against the routes it is documented for, with the tool its own
configuration offers: `Read` for Claude Code, `exec_command` for Codex and
`read_file` for Gemini CLI. Assert on the JSON the agent prints (`--output-format
json`, `exec --json`, `--output-format json`), not on its prose.

## Pins

Every client is an exact `devDependency`, so `pnpm audit --prod` and the
runtime inventory are unaffected. A pin is the newest release that cleared
pnpm's one-day minimum release age, which keeps the lockfile free of
release-age exemptions. `allowBuilds` in `pnpm-workspace.yaml` approves only the
Claude Code installer, which links the binary its optional package ships, and
declines the optional native modules of Gemini CLI. Dependabot advances the
npm pins through the root workspace and the Go pins through its own entry.
`scripts/check-clients-doc.test.mjs` keeps
[Client compatibility](../../docs/clients.md) equal to the manifests.
