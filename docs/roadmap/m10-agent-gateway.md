# M10: Agent gateway

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | [M4](m04-tenancy-identity.md), [M7](m07-guardrails.md) | One control plane for models, tools and agents |

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
| MCP | None | [MCP gateway](https://docs.litellm.ai/docs/mcp) over Streamable HTTP, SSE and stdio, with [OAuth](https://docs.litellm.ai/docs/mcp_oauth), [per-user auth](https://docs.litellm.ai/docs/mcp_per_user_auth), [toolsets](https://docs.litellm.ai/docs/mcp_toolsets) and [cost tracking](https://docs.litellm.ai/docs/mcp_cost) |
| Agents | None | [A2A gateway](https://docs.litellm.ai/docs/a2a) with [iteration budgets](https://docs.litellm.ai/docs/a2a_iteration_budgets) and a [kill switch](https://docs.litellm.ai/docs/a2a_kill_switch) |
| Prompts | None | [Prompt management](https://docs.litellm.ai/docs/proxy/prompt_management) |
| Reusable building blocks | Grant enrollment and refresh ([plugins](../plugins.md#grant-enrollment)), egress policy, revisions, guardrails ([M7](m07-guardrails.md)) | |

## Scope

### M10.1 MCP gateway

**Registration.** An MCP server is a project-scoped resource with a transport,
an endpoint and authentication:

| Transport | Support |
| --- | --- |
| Streamable HTTP | Supported, at the MCP protocol versions pinned in the compatibility guide, negotiated during `initialize` |
| HTTP with SSE | Supported for servers that have not moved to Streamable HTTP, which replaced it in the MCP specification |
| stdio | Only as executables in the image's unconfined plugin directory, under the experimental [unconfined tier](../plugins.md#unconfined-plugins-experimental) |

Authentication is one of: none, static headers sealed under a new
`mcp_credential` purpose, or an OAuth 2.1 grant enrolled and refreshed through
the existing [grant](../plugins.md#grant-enrollment) machinery (authorization
code with PKCE, or device authorization). Per-user grants bind a grant to an
API key or an end user, so a tool call acts with that user's upstream
authority.

**Certification and pinning.** Certifying a server runs `initialize` and
`tools/list` (and `prompts/list` and `resources/list` when declared) within
bounded time, and records each tool's name, description and input schema with
a digest. Publishing a server revision pins those digests. When the server later
reports a changed tool, the change is quarantined: the pinned definition keeps
serving, the console shows the diff, and the new definition serves only after
an operator approves a new revision. This defeats silent tool redefinition.

**Toolsets.** A toolset is a published, revisioned selection of tools across
servers. Keys reach toolsets through their allowlist, or through route groups
from [M4.3](m04-tenancy-identity.md#m43-access-ergonomics).

**Client endpoint.** `/mcp` is a Streamable HTTP MCP server that exposes the
caller's permitted toolsets. It authenticates API keys with a new `tools` scope.
Tool names are prefixed with their server name in a format that satisfies the
MCP tool-name rules. `tools/call` is proxied with egress policy, per-key and
per-server limits, a deadline and bounded results. A JSON facade,
`GET /v1/mcp/tools` and `POST /v1/mcp/tools/call`, serves clients that call
tools without an MCP session.

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
- Strict routes forward the native `mcp` tool type unchanged to providers that
  host MCP themselves, and never run the loop in the gateway.

### M10.3 A2A agent gateway

- An agent is a project-scoped resource registered from its agent card URL. OLP
  fetches the card through egress policy, pins it by digest, and supports A2A
  protocol versions 0.3 and 1.0.
- `/a2a/{agent}` serves the protocol's JSON-RPC methods for sending and
  streaming messages and for reading and cancelling tasks (in version 0.3,
  `message/send`, `message/stream`, `tasks/get` and `tasks/cancel`), and OLP
  serves a rewritten agent card that advertises its own endpoint and
  authentication.
- Keys reach agents through allowlists with a new `agents` scope. Limits cover
  messages per minute, concurrent tasks and an iteration budget per task.
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

## Change map

| Change | Start here |
| --- | --- |
| MCP servers, toolsets, pinning | new `internal/mcp/`, `internal/grants/` |
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

## Exit criteria

- [ ] MCP servers on each supported transport pass an MCP conformance suite
      through `/mcp`, including OAuth grant refresh and per-user grants.
- [ ] A changed upstream tool definition never reaches a client before an
      operator approves it.
- [ ] Gateway-executed tools complete multi-step tasks for OpenAI, Anthropic and
      Gemini targets, respect every bound, and appear as separate attempts.
- [ ] A2A messages and streams pass against reference agents for both protocol
      versions, and disabling an agent stops it within the authority freshness
      bound.
- [ ] Prompt revisions render identically from the API, the playground and
      configuration import.
- [ ] Tool calls, agent messages and prompt usage appear in usage reports and
      exports.
- [ ] The [parity matrix](parity.md) agent rows are `Parity` or better.
