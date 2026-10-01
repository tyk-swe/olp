# M10: Agent gateway

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M4](m04-tenancy-identity.md), [M7](m07-guardrails.md) | [M5](m05-observability.md) (usage export), [M9](m09-api-surface.md) (search routes as a tool), [M11](m11-operator-ecosystem.md) (Terraform resources, catalog listings) | None |

LiteLLM positions itself as one gateway for models, MCP tools and A2A agents,
with per-key access, cost tracking and OAuth. OLP governs models only. This
milestone extends OLP's control plane to tools, agents and prompts with the same
discipline it applies to providers: registration, certification, immutable
revisions, pinned definitions, scoped access, egress policy, guardrails and
attempt-level accounting.

## Outcome

- Operators register MCP servers, certify and pin their tool definitions, and
  publish curated toolsets that keys reach through one MCP endpoint.
- Any model on a transformed route can use MCP tools that the gateway executes.
- A2A agents are registered, access-controlled, budgeted and accounted.
- Prompts are versioned, published and referenced by applications.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| MCP | None | [MCP gateway](https://docs.litellm.ai/docs/mcp) over Streamable HTTP, SSE and stdio, with [OAuth](https://docs.litellm.ai/docs/mcp_oauth), [per-user auth](https://docs.litellm.ai/docs/mcp_per_user_auth), [token exchange](https://docs.litellm.ai/docs/mcp_obo_auth), [SigV4](https://docs.litellm.ai/docs/mcp_aws_sigv4), [OpenAPI-backed servers](https://docs.litellm.ai/docs/mcp_openapi), [toolsets](https://docs.litellm.ai/docs/mcp_toolsets), [tool search](https://docs.litellm.ai/docs/mcp_tool_search) and [cost tracking](https://docs.litellm.ai/docs/mcp_cost) |
| Agents | None | [A2A gateway](https://docs.litellm.ai/docs/a2a) with [iteration budgets](https://docs.litellm.ai/docs/a2a_iteration_budgets) and a [kill switch](https://docs.litellm.ai/docs/a2a_kill_switch) |
| Prompts and skills | None | [Prompt management](https://docs.litellm.ai/docs/proxy/prompt_management) and a [skills registry](https://docs.litellm.ai/docs/skills_gateway) |
| Reusable building blocks | Plugin-mediated grant enrollment and refresh ([plugins](../plugins.md#grant-enrollment)), SigV4 signing, egress policy, immutable revisions and route content policy | |

## Scope

### M10.1 MCP gateway

**Registration.** An MCP server is a project-scoped resource with a transport,
an endpoint and authentication:

| Transport | Support |
| --- | --- |
| Streamable HTTP | Supported, at the MCP protocol versions pinned in the compatibility guide, negotiated during `initialize` |
| HTTP with SSE | Supported for servers that have not moved to Streamable HTTP, which replaced it in the MCP specification |
| stdio | Only as executables in the image's unconfined plugin directory, under the experimental [unconfined tier](../plugins.md#unconfined-plugins-experimental) |

A server may also be defined from an OpenAPI document. Each selected operation
becomes a tool that the gateway calls over HTTP with the server's
authentication, certified and pinned like any other tool.

**Authentication.** A server uses one of:

| Mode | Contract |
| --- | --- |
| None | For servers on a trusted network |
| Static headers | Sealed under a new `mcp_credential` purpose |
| OAuth 2.1 grant | Authorization code with PKCE, or device authorization, with refresh. Today's [grant](../plugins.md#grant-enrollment) flow is driven by a provider plugin and returns by paste-back, so this adds a native OAuth client and callback to the grant store |
| Per-user grant | A grant bound to a server-verified workload issuer and subject (M4.4), or explicitly to one API key representing that user; caller-supplied end-user labels never select grants |
| Token exchange | The caller's workload JWT ([M4.4](m04-tenancy-identity.md#m44-workload-identity)) is exchanged for a token scoped to the server, through RFC 8693 or an identity-assertion grant, so the caller's own token is never forwarded |
| AWS SigV4 | Reuses the Bedrock request signer and its credential modes |
| Gateway assertion | OLP signs each outbound call with a short-lived JWT, and publishes its verification keys, so a server can refuse calls that did not come through the gateway |

Per-user grant enrollment binds the authenticated identity, project and MCP
server to the grant. Every tool invocation revalidates that binding; an
`X-OLP-End-User` value, dialect attribution field or claimed session identity
cannot switch upstream users. A shared API key without a separately verified
subject can use only a grant explicitly bound to that key.

**Certification and pinning.** Certifying a server runs `initialize` and
`tools/list` (and `prompts/list` and `resources/list` when declared) within
bounded time, and records each tool's name, description and input schema with
a digest. Publishing a server revision pins those digests. When the server later
reports a changed tool, the change is quarantined: the pinned definition keeps
serving, the console shows the diff, and the new definition serves only after
an operator approves a new revision. This defeats silent tool redefinition.

**Toolsets.** A toolset is a published, revisioned selection of tools across
servers. Keys reach toolsets through their allowlist, or through the
[access groups](m04-tenancy-identity.md#m43-access-ergonomics) of M4.3, which
this milestone extends to hold toolsets and agents.

**Client endpoint.** `/mcp` is a Streamable HTTP MCP server that exposes the
caller's permitted toolsets. It authenticates API keys and verified M4.4
workload principals with a new `tools` scope.
Tool names are prefixed with their server name in a format that satisfies the
MCP tool-name rules. `tools/call` is proxied with egress policy, per-key and
per-server limits, a deadline and bounded results. A JSON facade,
`GET /v1/mcp/tools` and `POST /v1/mcp/tools/call`, serves clients that call
tools without an MCP session; its path follows the roadmap's
[endpoint decision](README.md#cross-milestone-decisions).

**Governance and accounting.** Every tool call is a request record with
operation `tool_call`, an attempt per upstream call, latency, status, and a
price from the `unit_price` of a pricing entry scoped to the server. The
[tool guardrail](m07-guardrails.md#m76-tool-governance) and any other attached
guardrails inspect arguments and results.

### M10.2 Gateway-executed tools

On transformed routes, a route policy `tool_execution: gateway` lets any model
use MCP tools:

- Requests reference permitted toolsets. The gateway advertises their tools to
  the model in its dialect, executes returned tool calls through M10.1, appends
  the results and calls the model again, until the model finishes or a bound is
  reached: iterations, wall time and an optional cost ceiling.
- Every model call and tool call is its own attempt with its own record.
- On the Responses surface, intermediate steps appear as the `mcp_list_tools`,
  `mcp_call` and `mcp_approval_request` output items that the OpenAI SDKs
  already understand. Approval-required tools pause the loop and return an
  approval request.
- **Pending approvals.** A paused call must resume exactly as it was proposed.
  The gateway keeps the tool, its arguments, the pinned revisions and the
  remaining bounds in a sealed continuation, as it keeps stored-response
  continuations today, for at most 24 hours. An approval is accepted only from
  the key that received the request, and only for that call.
- **Tool search.** When the permitted tools exceed a configured count, the
  gateway advertises a search tool and a call tool instead of every
  definition. Search ranks tools by keyword, or by similarity through an OLP
  embeddings route when one is configured, so large catalogs do not fill the
  model's context.
- **Web search.** A built-in `web_search` tool executes through a
  [search route](m09-api-surface.md#m97-ocr-and-search), giving every model
  server-side search. It ships when both this workstream and M9.7 have.
- Strict routes forward the native `mcp` tool type unchanged to providers that
  host MCP themselves, and never run the loop in the gateway.

### M10.3 A2A agent gateway

- An agent is a project-scoped resource registered from its agent card URL. OLP
  fetches the card through egress policy, pins it by digest, and supports the
  A2A protocol versions pinned in the compatibility guide (0.3 and 1.0 when
  this was written).
- `/a2a/{agent}` serves the protocol's JSON-RPC methods for sending and
  streaming messages and for reading and canceling tasks (in version 0.3,
  `message/send`, `message/stream`, `tasks/get` and `tasks/cancel`), and OLP
  serves a rewritten agent card that advertises its own endpoint and
  authentication.
- Keys reach agents through allowlists or access groups, with a new `agents`
  scope. Limits cover messages per minute, concurrent tasks and an iteration
  budget per task.
- Disabling an agent stops new messages and cancels running tasks at the next
  authority refresh, which serves as the kill switch.
- Each message and task is accounted. Model calls the agent makes back through
  OLP carry the task identifier as attribution, so agent spend is attributable.

### M10.4 Prompt registry

- A prompt is a project-scoped resource with drafts, immutable revisions and
  movable labels such as `production` and `staging`. A revision holds a message
  template in a logic-less syntax (variable substitution only), a JSON Schema
  for its variables, and optional default parameters.
- On the Responses surface, a `prompt` whose `id` carries the OLP prefix
  `olpp_` is rendered by the gateway on transformed routes; any other `prompt`
  passes through natively to the provider's own prompt feature. Chat
  Completions, Anthropic and Gemini requests reference prompts through a
  gateway-owned `olp_prompt` member on transformed routes, which is consumed
  before dispatch.
- Variables are validated against the schema before dispatch, and rendered
  content goes through input guardrails like any other input.
- Usage reports group by prompt and revision. Prompt templates are operator
  configuration, stored in PostgreSQL and exported by configuration promotion;
  rendered prompts and variable values are request content and are never
  stored.
- **Skills.** Subject to decision 4, the registry also lists agent skills: a
  project-scoped name, a source repository and a pinned immutable revision.
  Operators create and promote listings through contract-declared management
  operations; a key-visible registry API exposes only permitted listings.
  Once M11.4 ships, developers and agents also discover them through the
  [developer catalog](m11-operator-ecosystem.md#m114-developer-catalog).
  Before then the registry API provides discovery. Listings round-trip
  through configuration promotion. OLP indexes skills; it does not execute them.

## Non-goals

- Hosting MCP servers, agent runtimes or code-execution sandboxes. OLP governs
  calls to them; a sandbox is reached as an MCP server.
- Running stdio servers in the confined tier.
- Template logic. Prompts substitute variables and nothing else.
- Storing tool arguments, tool results, rendered prompts or variable values,
  apart from the sealed arguments of a call awaiting approval.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| MCP servers, pinned tool definitions, toolsets, agents and agent cards | Immutable revisions in PostgreSQL; the runtime snapshot | As route revisions today | None |
| Static MCP headers | PostgreSQL, sealed | Until rotated | New seal purpose `mcp_credential` |
| OAuth grants and refresh tokens for MCP servers | The grant store in PostgreSQL, sealed | Until revoked or lapsed | New seal purpose `mcp_grant` |
| Gateway assertion signing keys | PostgreSQL, sealed; public keys served | Until rotated | New seal purpose `mcp_assertion_key` |
| Prompt templates and skill listings | Revisions in PostgreSQL; configuration export | Until deleted | None |
| Tool-call, agent-message and prompt usage | Request and attempt records | Request retention | None; metadata only |
| Pending tool approvals, including the call's arguments | PostgreSQL, sealed | Until approved, denied or 24 hours | New seal purpose `tool_approval` |
| Other tool arguments and results, rendered prompts | Request memory | Never stored | None |

## Change map

| Change | Start here |
| --- | --- |
| MCP servers, toolsets, pinning | new `internal/mcp/` |
| Native OAuth client and per-user grants | `internal/grants/`, which serves only plugin-driven grants today |
| Gateway-executed tools | `internal/gateway/`, `internal/protocols/` |
| A2A agents | new `internal/agents/` |
| Prompt registry | new `internal/prompts/`, `internal/providerinvoke/` |
| Console | new `console/src/lib/features/agents/` |

## Decisions to settle

1. Whether tool quarantine blocks the changed tool or keeps serving the pinned
   definition (recommended: keep serving the pinned definition, which some
   servers will reject; the operator then decides).
2. The template syntax (recommended: a minimal Mustache subset without sections
   or partials, so templates cannot execute logic).
3. Whether gateway-executed tools may stream intermediate steps on Chat
   Completions, which has no native item types for them (recommended: no;
   stream only the final answer there).
4. Whether to ship a skills registry (recommended: yes, as an index only;
   otherwise the parity row becomes `Excluded`).

## Exit criteria

- [ ] **M10.1** MCP servers on each supported transport pass an MCP conformance
      suite through `/mcp`, including OAuth grant refresh and per-user grants.
- [ ] **M10.1** Each authentication mode reaches a local fake that verifies it:
      a SigV4 signature, an exchanged token that is not the caller's, and a
      gateway assertion that verifies against the published keys.
- [ ] **M10.1** A shared-key caller cannot select another user's grant by
      changing end-user or session fields; grants are isolated by verified
      subject or explicit key binding, project and MCP server.
- [ ] **M10.1** A changed upstream tool definition never reaches a client
      before an operator approves it, for MCP servers and OpenAPI-backed
      servers alike.
- [ ] **M10.2** Gateway-executed tools complete multi-step tasks for OpenAI,
      Anthropic and Gemini targets, respect every bound, and appear as separate
      attempts.
- [ ] **M10.2** An approved call runs with exactly the arguments that were
      proposed; an approval from another key, for another call, or after
      expiry is refused.
- [ ] **M10.2** Above the tool-count threshold a model sees only the search and
      call tools and can still reach every permitted tool, and none it is not
      permitted.
- [ ] **M10.3** A2A messages and streams pass against reference agents for both
      protocol versions, and disabling an agent stops it within the authority
      freshness bound.
- [ ] **M10.4** Prompt revisions render identically from the API, the
      playground and configuration import.
- [ ] **M10.4** If decision 4 includes skills, a listing can be created,
      pinned to an immutable source revision and promoted; configuration
      export and import preserve it, and the registry API returns only
      listings the key may access. Once M11.4 ships, catalog discovery passes
      the same access-isolation test.
- [ ] **M10.1–M10.4** Tool calls, agent messages and prompt usage appear in
      usage reports. Once M5.1 ships, the same metadata-only facts also appear
      in durable exports; before then reports provide access to them.
- [ ] The [parity matrix](parity.md) agent rows are `Parity` or better.
