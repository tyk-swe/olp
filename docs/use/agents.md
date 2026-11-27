# Agents, toolsets, prompts, and workflows

OLP governs agentic traffic through project-scoped publications, gateway-executed tool loops with human approval checkpoints, Agent-to-Agent (A2A) proxying with scoped callback capability tokens, short-lived delegation and personal grants, and durable execution accounting. Each layer enforces live authority freshness and keeps provisional financial exposure separate from observed spend.

See [upstream MCP servers](mcp-servers.md) for registering the tool servers behind toolsets, [access control](access.md) for API key scopes and workload identities, the [compatibility matrix](compatibility.md) for model surfaces, the [console guide](console.md) for operator navigation, and [observability](../operate/observability.md) for traces, logs, and telemetry exports.

## Toolsets, agents, and prompts

Toolsets, agents, and prompts share a draft-and-publish lifecycle under `/api/v1/toolsets`, `/api/v1/agents`, and `/api/v1/prompts`. Every publication belongs to one project, and its project ownership cannot change after creation. A project can hold up to 256 active (non-retired) publications across all three kinds, and each publication can retain up to 32 immutable published revisions. Publishing a draft whose canonical SHA-256 digest matches an existing revision reuses that revision without consuming an additional slot.

Reads require management `read` authority and project visibility. Writes require `configure` authority and permission to change the owning project, with an extra rule for agents described below. All mutations require `Idempotency-Key`, and updates, publishes, retirements, and prompt label updates require `If-Match` carrying the observed `ETag`. Draft names must be 1 to 100 characters and unique within their project and kind, failing with HTTP 409 `publication_exists` on collision. Retired publications cannot be changed or republished (HTTP 409 `publication_retired`), while their historical revisions remain readable for audit and run inspection. Publishing, retiring, or relabeling advances gateway authority and publishes a runtime release.

| Resource | Endpoints | Notes |
| --- | --- | --- |
| Toolsets | `GET`, `POST /api/v1/toolsets`, `GET`, `PUT`, `DELETE /api/v1/toolsets/{publication_id}`, `POST .../publish`, `GET .../revisions/{revision_id}` | Pins up to 128 certified MCP tools and optional execution classifications. |
| Agents | `GET`, `POST /api/v1/agents`, `GET`, `PUT`, `DELETE /api/v1/agents/{publication_id}`, `POST .../publish`, `GET .../revisions/{revision_id}` | Pins A2A protocol, card digest, endpoint, credentials, callback pins, and cost/task bounds. |
| Prompts | `GET`, `POST /api/v1/prompts`, `GET`, `PUT`, `DELETE /api/v1/prompts/{publication_id}`, `POST .../publish`, `GET .../revisions/{revision_id}`, `PUT .../labels/{label}` | Pins template messages, string variable declarations, default parameters, and `staging` or `production` labels. |

### Toolsets

A toolset draft lists up to 128 tools in `toolset.tools`. Each tool entry pins `server_id`, `revision_id`, `catalog_digest`, `name`, its deterministic `alias`, and an optional operator `classification` set to `"read_only"` or `"approval_required"`. When publishing a toolset, OLP rechecks inside the database transaction that every referenced MCP server belongs to the same project, is enabled and active, still points at that latest certified revision, and matches `catalog_digest`, without contacting the upstream MCP server.

### Agents

An agent draft defines a remote A2A peer under `agent` with the following pinned controls.

| Field | Constraints and behavior |
| --- | --- |
| `protocol` | Required. Either `"a2a-0.3.0"` or `"a2a-1.0.0"`. |
| `endpoint` | Required. Canonical URL permitted by the installation's provider egress policy. |
| `card_digest` | Required. Lowercase 64-character SHA-256 hex digest of the upstream `/.well-known/agent-card.json` body. |
| `toolset_revisions` | Up to 100 distinct published toolset revision UUIDs in the same project for callback calls. |
| `route_revisions` | Up to 100 distinct published route revision UUIDs in the same project for callback calls. |
| `pricing_revision_id` | Required. Installed immutable pricing revision UUID. |
| `max_cost` | Required. Finite positive decimal cost bound. |
| `message_price` | Optional nonnegative decimal fee per sent message, at most `max_cost`. |
| `remote_work_bound_seconds` | Optional integer from 1 to 3600. Required whenever a budgeted caller invokes the agent. |
| `max_task_continuations` | Optional integer from 1 to 128, defaulting to 32. |
| `task_deadline_seconds` | Optional integer from 1 to 3600, defaulting to 600. |
| `credential` / `oauth` / `entra_client_secret` | Write-only static bearer `credential`, personal `oauth` (browser or device), or `oauth.entra_obo` with write-only `entra_client_secret` (1 to 16,384 bytes). Static bearer and OAuth modes are mutually exclusive. |

Changing an agent's trust boundary (`endpoint`, `protocol`, `card_digest`, `credential_id`, or `oauth`) requires installation `access` authority in addition to project change rights, even when a project manager edits policy bounds. Static credentials and Entra client secrets are sealed under `agent_credential` in the installation key ring and never returned in reads. Changing `endpoint` requires replacing or removing the static credential, or removing OAuth before re-enrolling a new grant.

When publishing an agent, OLP fetches `/.well-known/agent-card.json` from the endpoint origin (up to 1 MiB, 30-second timeout, egress-validated) and verifies that its SHA-256 matches `card_digest` and that its `supportedInterfaces` declares the configured JSON-RPC `endpoint` and `protocol`.

### Prompts and labels

A prompt draft stores bounded template text (at most 64 KiB total across all messages) with `{{variable_name}}` placeholders where each variable name matches `^[A-Za-z_][A-Za-z0-9_]{0,63}$` and at most 256 variables are declared. Every declared variable must appear at least once in the prompt, and every placeholder in the prompt must be declared.

Prompts use one of two shapes. The single-message shape sets `template` and `variables` (rendered as one `user` message). The structured multi-message shape sets `messages` (up to 256 items with roles `"system"`, `"user"`, or `"assistant"`, where `"system"` may appear only as the first message) together with a strict `variable_schema` (`type: "object"`, `additionalProperties: false`, every property `type: "string"`, and every property listed in `required`) while leaving `template` and `variables` empty.

Optional `default_parameters` sets `temperature` (0 to 2), `top_p` (0 to 1), and `max_output_tokens` (1 to 2147483647). After publishing revisions, operators point the `"staging"` or `"production"` label at any published revision of an active prompt through `PUT /api/v1/prompts/{publication_id}/labels/{label}` with `{"revision_id": "<UUID>"}`.

Toolsets, agents, and prompts (including `staging` and `production` prompt labels) also participate in declarative configuration promotion (up to 512 publications per document), matched by logical project, kind, and name without exporting raw credentials.

## Expanding published prompts on inference routes

Callers can render a published prompt directly on transformed inference routes instead of sending inline messages. On OpenAI Chat Completions, Anthropic Messages, and Gemini `generateContent` or `streamGenerateContent`, include a top-level `olp_prompt` object with `id` (the prompt publication UUID), `variables` (a JSON object mapping every declared variable to a string value), and exactly one of `label` (`"staging"` or `"production"`) or `revision_id`. On OpenAI Responses, pass the native top-level `prompt` object with `id` prefixed by `olpp_` (for example `"olpp_019a0000-0000-7000-8000-000000000001"`), plus the same `variables` and `label` or `revision_id` fields.

The following example expands the `production` revision of a prompt on Chat Completions.

```json
{
  "model": "chat-support",
  "olp_prompt": {
    "id": "019a0000-0000-7000-8000-000000000001",
    "label": "production",
    "variables": {
      "customer_tier": "standard",
      "question": "How do I reset my password?"
    }
  }
}
```

Prompt expansion enforces strict rules and returns HTTP 400 `invalid_prompt` on violations. Published prompts require a `transformed` route in the same project as the prompt publication. Strict routes refuse prompt references. Callers cannot combine a published prompt with inline input or system fields (`messages`, `input`, `instructions`, `system`, `contents`, `systemInstruction`, or `system_instruction`), and unknown fields inside the prompt selector object are rejected. Every rendered message set must fit within both the route's `max_body_bytes` and the installation body limit, returning HTTP 413 if exceeded.

During expansion, OLP removes the prompt selector and inserts the rendered conversation into `messages`, `input`, or `contents` (mapping `assistant` to `model` for Gemini). On Anthropic Messages and Gemini, a leading `system` message is lifted automatically into `/system` or `/systemInstruction`. Any configured `default_parameters` apply only when the caller omitted the corresponding parameter, mapping `max_output_tokens` to `max_tokens` on Chat Completions (unless `max_completion_tokens` is present) and Anthropic Messages, to `max_output_tokens` on Responses, and into `generationConfig.maxOutputTokens` and `generationConfig.topP` on Gemini.

## Gateway-executed tool workflows and human approval

A transformed route can execute published MCP toolsets inside the gateway by setting `tool_execution: "gateway"` and listing distinct toolset UUIDs in `toolsets` (with optional `run_limits` and optional `responses_mcp_items: true` for Responses MCP items). Strict routes refuse gateway tool execution with HTTP 400 `tool_execution_strict_route`. Gateway workflows run on Chat Completions, Responses, Anthropic Messages, and Gemini generation (other surfaces refuse with `tool_execution_unsupported`), and require a single choice (`n` or `candidateCount` equal to 1, otherwise `tool_execution_choices_unsupported`).

Callers can inspect effective workflow capabilities on `GET /v1/models` (in each route's `gateway_execution` object) and through route explain. On individual requests, callers can narrow the route's toolset list with `olp_toolsets` (an array of distinct UUIDs that must be a subset of the route's toolsets, otherwise `toolset_not_permitted`) and tighten execution bounds with `olp_run_limits` (strictly decoded, otherwise `invalid_run_limits`).

Effective workflow bounds always take the tightest (minimum) value across any prior paused run state, the route's `run_limits`, the API key's `run_limits`, and the caller's `olp_run_limits`.

| Field | Default | Allowed range | Error on exhaustion |
| --- | --- | --- | --- |
| `max_iterations` | `8` | `1` to `32` | HTTP 400 `tool_iteration_limit` |
| `max_tool_calls` | `32` | `0` to `128` | HTTP 400 `tool_call_limit` |
| `max_duration_seconds` | `120` | `1` to `600` | Run deadline cancellation |
| `max_concurrent_runs` | Unbounded unless set | `1` to `2147483647` | Admission refusal |
| `max_run_cost` | Unbounded unless set | Exact nonnegative decimal | Refuses if model or tool cost cannot be bounded or exceeds cap |

During execution, OLP advertises the resolved tool schemas to the model, records an agent run and ordered step debits, and rechecks live key, route, toolset, delegation, end-user, attribution, and client-IP authority on every model turn and every 4 seconds during active tool calls. If authority is revoked or paused mid-workflow, the gateway cancels the in-flight call.

### Human approval pause and resume (`olp_approval`)

Within a gateway-executed tool loop, tools marked `classification: "read_only"` in their published toolset run automatically. Whenever the model requests one or more tool calls whose classification is `"approval_required"` or unset (up to 32 calls per turn), OLP pauses the workflow before executing those calls.

OLP writes unconsumed approval rows into `olp.agent_approvals`, captures the workflow state, and seals a continuation token with the installation key ring under the `workflow_continuation` purpose. Tokens have the form `olpa_<key_version>_<approval_id>_<base64url>` and expire after at most 5 minutes, clamped to the remaining workflow duration deadline.

On non-streaming requests, a paused workflow returns HTTP 200 with a JSON approval pause object.

```json
{
  "type": "tool_approval_required",
  "continuation_token": "olpa_1_019a0000-0000-7000-8000-000000000099_...",
  "calls": [
    {
      "server_label": "019a0000-0000-7000-8000-000000000050",
      "id": "call_abc123",
      "name": "srv_create_ticket",
      "arguments": {
        "title": "Rotate expired TLS certificate"
      }
    }
  ]
}
```

When `responses_mcp_items` is enabled on a Responses route, the same HTTP 200 payload also includes Responses metadata (`id`, `created_at`, `model`, `object: "response"`, `status: "incomplete"`) and an `output` array of `mcp_approval_request` items. On streaming requests, OLP returns HTTP 200 `text/event-stream` carrying `olp_approval` on the surface's terminal pause frame (a Chat Completions chunk with `finish_reason: "stop"` followed by `[DONE]`, an Anthropic `message_start`/`message_delta`/`message_stop` sequence, a Responses `response.incomplete` event, or a Gemini candidate event with `finishReason: "STOP"`), or `response.output_item.added`, `response.output_item.done`, and `response.incomplete` frames when `responses_mcp_items` is active.

To approve and resume, resend the exact same original request transcript with a top-level `olp_approval` object containing `approved: true`, the returned `continuation_token`, and an `arguments` map keyed by each paused `call.id` with the exact reviewed arguments.

```json
{
  "model": "ops-assistant",
  "messages": [
    { "role": "user", "content": "Open a ticket to rotate the expired TLS certificate." }
  ],
  "olp_approval": {
    "approved": true,
    "continuation_token": "olpa_1_019a0000-0000-7000-8000-000000000099_...",
    "arguments": {
      "call_abc123": {
        "title": "Rotate expired TLS certificate"
      }
    }
  }
}
```

On resume, OLP unseals the `olpa_` token and verifies the expiry timestamp, exact principal digest, exact transcript digest, exact argument digest for every paused call ID, unchanged active route revision, unchanged active toolset revisions, and unconsumed approval rows. Any mismatch, replay, expiry, or revoked authority fails with HTTP 403 `tool_approval_invalid`.

## Agent-to-agent (A2A) proxying and tasks

Published agents are exposed to callers holding the `agents` scope with the agent publication UUID in their allowlist inside the owning project.

`GET /a2a/{agent}/.well-known/agent-card.json` verifies the upstream peer's card against the pinned `card_digest`, rewrites `supportedInterfaces` to `{public_origin}/a2a/{agent}` with Bearer authentication, and serves the card as `application/a2a+json` with `Cache-Control: no-store` formatted for either A2A `0.3.0` or A2A `1.0.0` to match the published agent protocol.

`POST /a2a/{agent}` serves JSON-RPC 2.0 (`Content-Type: application/json`) with a 1 MiB limit on request bodies, unary responses, and individual SSE stream events. Callers select the A2A protocol version with the `A2A-Version` HTTP header or query parameter (`0.3` or `0.3.0` selects `a2a-0.3.0` and is the default when omitted, while `1.0` or `1.0.0` selects `a2a-1.0.0`). Conflicting header and query values or unsupported versions return HTTP 400.

| Action | A2A `0.3.0` method | A2A `1.0.0` method | Transport |
| --- | --- | --- | --- |
| Send message | `message/send` | `SendMessage` | JSON-RPC unary response |
| Stream message | `message/stream` | `SendStreamingMessage` | JSON-RPC over SSE (`text/event-stream`) |
| Get task | `tasks/get` | `GetTask` | JSON-RPC unary response |
| Cancel task | `tasks/cancel` | `CancelTask` | JSON-RPC unary response |
| Resubscribe to task | `tasks/resubscribe` | `SubscribeToTask` | JSON-RPC over SSE (`text/event-stream`) |

Send requests must supply a user message with a nonempty message ID and at least one part, and must omit `pushNotificationConfig`, `referenceTasks`, and `tenant` (otherwise JSON-RPC error `-32602`).

OLP enforces layered admission controls before dispatching to a remote agent. Every calling key has a built-in A2A send limit of 60 sends per minute in addition to the key's, delegated agent's, and budget group's request and concurrency limits, and each key may hold at most 4 active or `outcome_unknown` tasks (including pending task admissions) at once. Exceeding task or rate limits returns JSON-RPC error `-32002`. Budgeted callers also require `remote_work_bound_seconds` on the agent publication, returning `-32002` otherwise. Each send reserves `message_price` as a durable `agent` execution debit (with a 90-second prepared window) and creates an agent run linked to the task.

### Callback capability tokens (`olpt_`)

When OLP dispatches `SendMessage` or `SendStreamingMessage` to a remote agent, it mints a short-lived task capability token prefixed with `olpt_` and sends it to the remote peer in the `X-OLP-Task-Capability` HTTP header. Only the keyed digest of the token is stored in `olp.a2a_task_capabilities`. Token expiry is bounded by the earliest of `task_deadline_seconds` (default 600 seconds), the task's `deadline_at`, and the caller's credential expiry.

The remote agent uses that `olpt_` token as a Bearer credential when calling back into OLP for `inference` or `tools`. OLP permits callbacks only against the exact `route_revisions` and `toolset_revisions` pinned on the agent publication, rejects all other scopes, rechecks live principal, delegation, end-user, expiry, pause, and revocation authority on every callback attempt, counts each callback attempt against `max_task_continuations` (default 32), and links every callback inference request and tool call to the parent task in `olp.a2a_task_callback_accounting`.

### Task monitoring, operator inspection, and cancellation

A background `TaskMonitor` scans active tasks every 5 seconds with up to 16 concurrent cancellation workers. If a task passes its `deadline_at` or loses caller or delegation authority, the monitor claims the task and sends a remote cancel request (10-second timeout). If the remote peer cannot confirm cancellation, the task transitions to `outcome_unknown`.

Operators inspect and cancel tasks through four management endpoints.

| Method and path | Required authority | Behavior |
| --- | --- | --- |
| `GET /api/v1/a2a-tasks` | `read` | Cursor-paginated list of tasks across authorized projects, with optional `project_id` filter. |
| `GET /api/v1/a2a-tasks/{task_id}` | `read` and project view | Returns task metadata, state, admitted callback count, remote message fee debits, callback model accounting, and callback tool accounting. |
| `GET /api/v1/a2a-tasks/{task_id}/remote` | `configure` and project change | Queries the remote A2A peer for live task state (35-second deadline). Returns HTTP 502 `agent_unavailable` if unreachable. |
| `POST /api/v1/a2a-tasks/{task_id}/cancel` | `configure` and project change | Audits a cancel attempt, requests remote cancellation (35-second deadline), and audits completion. Returns HTTP 409 `a2a_task_changed` if the task binding changed concurrently. |

## Agent delegation, personal grants, and pausing

Remote agents and workload-authenticated agent principals support personal user grants, short-lived delegated authority, and rapid pause controls.

### Personal grants for remote agents

When a published agent uses browser or device `oauth`, every caller needs an active personal grant matching that agent's exact endpoint, client, audience, scopes, project, and user identity. Static credentials are never used as a fallback.

A signed-in project member with `keys` authority and project change permission can start key-bound enrollment through `POST /api/v1/agents/{agent_id}/grant-enrollments` with `{"api_key_id": "<UUID>"}`. For project end users calling through a verified workload identity, the workload calls `POST /a2a/{agent}/grant-enrollments` on the consumer surface (requiring a verified workload end-user claim, returning HTTP 403 otherwise and HTTP 400 if the agent uses Entra OBO or has no OAuth configured). Both paths return a one-use handoff claimed and completed through the same `/api/v1/mcp-grant-enrollments/*` and `/api/v1/mcp-user-grants*` lifecycle documented in [upstream MCP servers](mcp-servers.md). Agents configured with `oauth.entra_obo` and `entra_client_secret` exchange the incoming verified user assertion directly without browser or device enrollment.

### Short-lived delegation (`olpd_` and `olpc_`)

An agent workload identity configured with `agent_principal: true` can request a short-lived delegated token (`olpd_`) through `POST /v1/delegations`. The agent sends its own workload assertion in `Authorization: Bearer` and pairs it with either a delegating API key in `X-OLP-Delegating-Key` (the key must carry the `delegate` scope in the same project) or a one-use member consent code (`olpc_`) in `X-OLP-Delegation-Consent`. Sending both headers is refused.

Signed-in members with `keys` authority create member consent codes through the management API. `GET /api/v1/delegation-options?api_key_id=<UUID>` lists eligible agent principals and the intersection of routes, toolsets, and agents both the key and agent may use. `POST /api/v1/delegation-consents` (requiring a browser session, CSRF protection, and recent authentication for `agent_delegation`) mints a one-use `olpc_` consent code.

Delegation requests specify `scopes` (1 to 3 distinct values from `"inference"`, `"tools"`, and `"agents"`), matching allowlists (`allowed_routes`, `allowed_toolsets`, `allowed_agents`, each up to 100 entries and nonempty if and only if its scope is selected), and optional `ttl_seconds` (default 300 seconds, maximum 900 seconds, always clamped to the agent's and delegating key's expiry). Delegated tokens are single-use on initial consumption (`consumed_at` is recorded on first use) and track combined pair spend against per-delegation-pair attribution budgets. Operators inspect issued, pending, consumed, expired, and revoked delegations plus pair budget usage through `GET /api/v1/agent-delegations` (requiring `read` authority, with optional `project_id` and `delegation_id` filters).

### Pausing keys and agent principals

Operators can pause an API key or agent workload principal without permanently revoking it by calling `POST /api/v1/api-keys/{api_key_id}/pause` (with `If-Match` and `Idempotency-Key`), check enforcement across live gateway replicas with `GET /api/v1/api-keys/{api_key_id}/pause-rollout` (which inspects replica heartbeats from the last 60 seconds against `paused_authority_sequence`), and clear the pause with `POST /api/v1/api-keys/{api_key_id}/resume`.

Pausing blocks all new work immediately once authority refreshes. That includes new inference, tool, and A2A sends, new delegations where the agent principal is paused, and new `olpt_` callback attempts on active tasks. Already-admitted A2A tasks remain monitorable and readable until they complete or reach their deadline because task monitoring checks admitted-work authority without rejecting paused principals.

## Runs, spans, and investigations

Every gateway-executed tool workflow and A2A send creates a run record in `olp.agent_runs` linked to ordered execution steps in `olp.agent_steps`, approval records in `olp.agent_approvals`, and correlated trace spans in `olp.trace_spans`.

| Method and path | Required authority | Behavior |
| --- | --- | --- |
| `GET /api/v1/agent-runs` | `read` | Cursor-paginated list of runs in authorized projects, with optional `project_id` filter. |
| `GET /api/v1/agent-runs/{run_id}` | `read` and project view | Returns run metadata, ordered steps (with debit kind, state, currency, bounds, observed and remaining amounts, `bound_violation`, and tool alias/status/pricing state), and approval rows. |
| `GET /api/v1/agent-runs/{run_id}/spans` | `read` and project view | Lists correlated spans ordered newest first (`limit` 1 to 200 default 50, opaque `cursor`, optional `span_id` UUID filter). |

Operators can record structured incident notes and evidence links as investigations through three management endpoints.

| Method and path | Required authority | Behavior |
| --- | --- | --- |
| `GET /api/v1/investigations` | `read` | Cursor-paginated list of investigations in authorized projects, with optional `project_id` filter. |
| `POST /api/v1/investigations` | `configure` and project change | Saves a project-scoped investigation (max body 32 KiB, `Idempotency-Key` required). Returns HTTP 201 and `Location`. |
| `GET /api/v1/investigations/{investigation_id}` | `read` and project view | Returns the saved investigation and resolves `run_id` onto `step`, `approval`, and `span` evidence references. |

Each investigation requires `project_id`, `filters` (optional RFC 3339 `from` and `to` timestamps, `run_id`, non-negative `min_duration_ms` and `max_duration_ms`, `error` boolean, and `tool` name up to 256 characters), `evidence` (0 to 100 items, each with a `kind` among `"run"`, `"step"`, `"approval"`, `"span"`, `"task"`, or `"delegation"` and a canonical UUID verified to exist in that project), and a nonempty `summary` up to 4096 characters. Investigations persist only filters, validated ID references, and operator summaries rather than raw prompts or tool payloads. Referenced runs, spans, tasks, and delegations are read through their own endpoints and expire independently under their own retention policies.

## Provisional exposure and debit reconciliation

Before dispatching bounded agentic or tool work, OLP reserves provisional financial exposure in `olp.execution_debits` across every applicable budget owner (on both `cost` and `charge` bases). Execution debits cover four kinds (`api`, `model`, `tool`, and `agent`) and transition through durable states.

| State | Meaning |
| --- | --- |
| `prepared` | Debit intent persisted in PostgreSQL before Valkey budget verification, bounded by `prepared_until`. |
| `ready` | Owner budgets verified and synchronized, awaiting upstream dispatch. |
| `dispatched` | Request sent upstream while the full bound remains reserved against owner budgets. |
| `outcome_unknown` | Upstream outcome could not be confirmed, so exposure stays held across restarts until resolved. |
| `settled` | Final observed cost and charge recorded and closed. |
| `released` | Undispatched reservation released back to available budget. |
| `not_accepted` | Closed with audited non-acceptance proof (for example transport-not-sent or operator resolution with SHA-256 evidence digest). |
| `conflict` | Recorded evidence conflicted with bounds or identity, blocking further admissions for affected owners until reconciled. |

`GET /api/v1/execution-debits` (requiring `read` authority, with optional `api_key_id` filter and cursor pagination) reports each debit's `cost_bound`, `charge_bound`, `observed_cost`, `observed_charge`, `remaining_cost`, `remaining_charge`, `currency`, `state`, and `bound_violation`. Remaining amounts equal `GREATEST(0, bound - observed)` while open and `0` once closed. Provisional exposure protects spending limits during in-flight or uncertain work and is kept strictly separate from settled invoice usage. Any debit flagged with `bound_violation: true` or in `conflict` blocks further budgeted admissions for its owners until reconciled.

Two background workers keep durable exposure synchronized. The debit reconciliation worker runs every minute (with a 40-second pass timeout, checkpointed under worker health task `execution_debits`) to release expired `prepared` and `ready` debits (`prepared_until < now`, up to 256 per pass), sync open exposure and spend snapshots for up to 128 owners per pass into Valkey, recover orphaned MCP tool dispatches, and drain expired 24-hour MCP call-identity bindings (up to 256 per pass). A separate workflow model recovery worker runs every 5 seconds (4-second timeout) to recover orphaned workflow model debits (up to 256 per pass). An `outcome_unknown` debit never auto-expires, and operators resolve held MCP tool debits explicitly through `POST /api/v1/mcp-tool-calls/{call_id}/resolve` with the SHA-256 digest of reviewed evidence.

## Console routes

All agent authoring, execution monitoring, investigation, and exposure views are exposed in the console under the **Agents** and **Spend** navigation sections.

| Console route | Navigation path | Backing API |
| --- | --- | --- |
| `/agents/toolsets` | **Agents** -> **Agents and tools** -> **Toolsets** | `/api/v1/toolsets*` |
| `/agents/definitions` | **Agents** -> **Agents and tools** -> **Agents** | `/api/v1/agents*` |
| `/agents/prompts` | **Agents** -> **Agents and tools** -> **Prompts** | `/api/v1/prompts*` |
| `/mcp-servers` | **Agents** -> **MCP servers** | `/api/v1/mcp-servers*` |
| `/agents/grants` | **Agents** -> **My connections** | `/api/v1/mcp-user-grants*`, `/api/v1/agents/{agent_id}/grant-enrollments` |
| `/activity/runs` | **Agents** -> **Activity** -> **Runs** | `/api/v1/agent-runs*` |
| `/activity/tool-calls` | **Agents** -> **Activity** -> **Tool calls** | `/api/v1/mcp-tool-calls*` |
| `/activity/tasks` | **Agents** -> **Activity** -> **Agent-to-agent tasks** | `/api/v1/a2a-tasks*` |
| `/activity/delegations` | **Agents** -> **Activity** -> **Delegations** | `/api/v1/agent-delegations`, `/api/v1/delegation-*` |
| `/investigations` | **Agents** -> **Investigations** | `/api/v1/investigations*` |
| `/budgets/exposure` | **Spend** -> **Budgets** -> **Pending agent spend** | `/api/v1/execution-debits` |
