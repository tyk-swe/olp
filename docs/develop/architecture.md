# Architecture and gateway execution

This guide maps OLP's feature ownership and explains how a request moves through
the gateway. Production Go code lives under `internal/`, public packages live
under capability-named top-level folders such as `plugin/`, `management/`,
`inference/`, and `guardrails/`, and `cmd/olp` starts the binary. Folders are
named for the module that owns the behavior rather than for a layer. The console
mirrors the same feature boundaries under `console/src/lib/features/`.
PostgreSQL migrations live under `internal/platform/database/migrations/`.

| Area | Backend and console owner |
| --- | --- |
| Connections, certification, catalog and grants | `internal/providers/`, `internal/connectors/`, `internal/grants/`, `internal/catalog/`, `internal/signing/`, and console `providers/` |
| Routes, publication, caching and selection | `internal/routes/`, `internal/routing/`, `internal/release/`, `internal/cache/`, and console `routes/` |
| Users, keys, identity and consumer auth | `internal/identity/` (sign-in, sessions, MFA, OIDC, SAML, SCIM, members and tokens), `internal/apikeys/` (keys, end users and workload issuers), `internal/workload/`, `internal/consumerauth/`, `internal/keyauthority/` (the key authority policy a release publishes), `internal/access/authz/` (callers, roles and operations), `internal/auditlog/`, and console `identity/` and `access/` |
| Management transport, console shell and CLI | `internal/access/` (authentication, route registration, transactions, replay and audit), `internal/access/httpkit/` (problems, replies, decoding and pagination), `internal/management/` (generated contract types, serving and overview), `internal/console/`, `internal/operatorcli/`, and console `overview/` |
| Projects, installation settings and policy | `internal/projects/`, `internal/settings/` (settings and branding), `internal/budgets/` (budget groups, increases and execution debits), `internal/limits/`, and console `access/`, `settings/` and `budgets/` |
| Notifications | `internal/notifications/` and console `notifications/` |
| Admission, execution, media and retained resources | `internal/gateway/` (assembly, execution kernel in `kernel/`, extracted surfaces in `surface/`, plus `anthropicbeta/`, `codeforward/`, `delegation/`, `duplexrelay/`, `events/`, `fault/`, `generationrefs/`, `guardstream/`, `health/`, `healthprobes/`, `ingress/`, `managedbatch/`, `models/`, `publishedprompt/`, `quota/`, `realtimeresponses/` and `traceingest/`), `internal/resources/` (with `manage/` and `retainedworker/`), `internal/retainedapi/`, `internal/batches/`, `internal/retrieval/`, `internal/media/` (with `jobs/` and `transport/`), and console `inference/`, `media/`, `resources/` and `health/` |
| Code mode | `internal/codemode/` (vocabulary, with `codeadapter/`, `codeclient/`, `codewire/`, `codeadmin/`, the plan profiles and plugins in `codeplans/`, and the subscription auth plugins in `claudeauth/`, `codexauth/` and `antigravityauth/`), `internal/codesessions/` (account selection and bindings, with `codeallowance/` and `codeaccounts/`), plus research and Codex wire handling under `internal/gateway/codeforward/` (`coderesearch/` and `codexwire/`), and console `code-mode/` |
| Source, operation, protocol and lifecycle contracts | `internal/domain/` (`oif/`, `operations/`, `operationregistry/`, `protocols/`, `geminilifecycle/`, `agentcontract/`, `grantconfig/`, `vendors/`, and other pure vocabulary packages) and `internal/contracts/` (the strict contract compilers `interaction/`, `unary/`, `duplex/` and `media/`) |
| Preparation and failure evidence | `internal/providerinvoke/` and `internal/upstream/` |
| Accounting, pricing, billing and exports | `internal/usage/` (event contract, with `completeness/`, `delivery/`, `ingest/`, `param/`, `pricebook/`, `rates/`, `reports/`, `retention/` and `routinginputs/`), `internal/billing/`, `internal/export/`, `internal/sinks/`, and console `usage/` and `exports/` |
| Plugins and guardrails | `internal/plugins/`, `plugin/`, `internal/guardrails/`, and console `plugins/` and `guardrails/` |
| Agents, MCP registration, tool execution and personal grants | `internal/agents/` (`a2a/`, `investigations/`, `publications/`, `runs/`, `tasks/`), `internal/mcp/`, `internal/grants/personal/`, and console `agents/` and `plugins/` |
| Promotion and process lifetime | `internal/promotion/` kernel, one `internal/promotion/<section>/` package per document section, `internal/promotion/serve/`, `cmd/olp/`, and `internal/platform/database/` |
| Egress, secrets and telemetry | `internal/platform/egress/`, `internal/platform/secrets/`, `internal/platform/staging/`, `internal/platform/telemetry/`, `internal/platform/workerhealth/`, `internal/observability/`, and console `health/` |

## Area guides

| Guide | Covers |
| --- | --- |
| [Code structure](code-structure.md) | [Structure rules](code-structure.md#structure-rules) and [table ownership](code-structure.md#table-ownership) |
| [Management features](management.md) | [Adding a management endpoint](management.md#adding-a-management-endpoint) and [contracts and feature ownership](management.md#contracts-and-feature-ownership) |
| [Gateway packages](gateway-packages.md) | The execution kernel, the surfaces and what each gateway package may import |
| [Requests and responses](request-path.md) | [Request path](request-path.md#request-path), [response headers](request-path.md#response-headers) and [diagnostics](request-path.md#diagnostics) |
| [Execution](execution.md) | [Ordinary](execution.md#ordinary-execution), [code-mode](execution.md#code-mode-forwarding-boundary), [media](execution.md#media-and-durable-video-jobs) and [agent](execution.md#agent-execution) execution |
| [Runtime](runtime.md) | [Immutable sources](runtime.md#runtime-and-immutable-sources), [process modes](runtime.md#process-modes) and [publication and authority](runtime.md#runtime-publication-and-authority) |
| [Limits and accounting](accounting.md) | [Limits and budgets](accounting.md#limits-and-budgets), [retained generation accounting](accounting.md#retained-generation-accounting) and [caller-supplied credentials](accounting.md#caller-supplied-provider-credentials) |

## Where new code goes

Start from the owner in the table at the top of this guide. Feature behavior
lives in the feature package with its SQL, transactions, validation and audit
provenance, and the console screen for it lives in the matching feature under
`console/src/lib/features/`. A new gateway surface is a package under
`internal/gateway/surface/`, work every surface shares belongs in the kernel,
and a gateway handler that can be built from its own collaborators belongs in
its own package under `internal/gateway/`. Each declares its allowed imports in
`scripts/check-lint.sh`. Process wiring belongs in `cmd/olp`, where each plane
is assembled by one constructor and `Run` owns mode selection, listeners and
shutdown ordering. A new small package is justified only when it owns behavior
of its own or breaks an import cycle with stable vocabulary, as
`internal/domain/routeidentity` and `internal/keyauthority` do. A wrapper whose
only job is forwarding to another package belongs in that package instead.
