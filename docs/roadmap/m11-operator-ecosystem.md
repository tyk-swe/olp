# M11: Operator ecosystem

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | [M4](m04-tenancy-identity.md) | [M1](m01-measured-advantage.md) (client environments), [M2](m02-provider-catalog.md) (catalog facts), [M5](m05-observability.md), [M7](m07-guardrails.md) and [M10](m10-agent-gateway.md) (Terraform resources, skill listings) | None |

OLP is operated through its console, its management API and configuration
promotion, with one PostgreSQL primary and file-mounted secrets. LiteLLM adds a
management CLI, secret managers, a model hub, multi-region topologies and
administration through agents. This milestone delivers those capabilities on
OLP's existing contracts: every tool is a client of the declared management
API, so authorization, ETags, idempotency and audit apply unchanged.

## Outcome

- Operators automate OLP from a CLI, Terraform and CI pipelines.
- Master keys and provider credentials can live in cloud KMS and secret stores,
  and API keys can rotate on a schedule into a secret store.
- Developers find routes, their facts and working code samples in a catalog.
- Gateway fleets run in several regions under one control plane, with defined
  consistency.
- Agents can administer OLP through a scoped MCP server.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| CLI | `olp` process modes, `migrate`, `doctor`, `master-key`, `account reset-password` and `health-probe` | [lite CLI](https://docs.litellm.ai/docs/proxy/management_cli) for models, credentials, keys, teams and users, with [SSO sign-in](https://docs.litellm.ai/docs/proxy/cli_sso) |
| Desired state | [Export, plan and apply](../configuration.md#configuration-promotion-artifacts) with canonical digests, for projects, providers, routes and pricing | [config.yaml](https://docs.litellm.ai/docs/proxy/configs) and database models |
| Secrets | Mounted master-key ring and HMAC key files; provider secrets sealed in PostgreSQL ([secrets](../security.md#secrets)) | [Secret managers](https://docs.litellm.ai/docs/secret_managers/overview) (Enterprise) |
| Topology | One region; `all`, `gateway`, `control` and `worker` modes ([deployment](../deployment.md)) | [Read replicas](https://docs.litellm.ai/docs/proxy/db_read_replica), [multi-region](https://docs.litellm.ai/docs/proxy/multi_region) and a [global control plane](https://docs.litellm.ai/docs/proxy/global_control_plane) (Enterprise) |
| Discovery | `GET /v1/models` lists key-visible routes | [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) |

## Scope

### M11.1 Management CLI

`olp` gains client subcommands that call the management API with a management
token read from `OLP_MANAGEMENT_TOKEN_FILE` and an endpoint from
`OLP_MANAGEMENT_URL`:

| Command group | Operations |
| --- | --- |
| `olp login` | Sign a member in through the installation's identity provider with the OAuth device authorization flow, and store a short-lived management token scoped to that member's role. Automation keeps using a token file |
| `olp config` | `export`, `plan`, `apply`, with a digest check that refuses to apply a plan computed against a different destination state |
| `olp keys`, `olp routes`, `olp providers` | List, read, create, update, revoke, rotate, simulate, certify and activate, mirroring the console |
| `olp usage` | Summaries, breakdowns and CSV export |
| `olp client-env` | Print the environment for a client from [M1.3](m01-measured-advantage.md#m13-client-and-agent-qualification), such as Claude Code or Codex, pointed at OLP with a given key file. Clients appear as M1.3 qualifies them |

Commands are generated from [`openapi/management.json`](../../openapi/management.json)
together with the existing contract types, so the CLI cannot drift from the
API. Writes send `If-Match` and `Idempotency-Key` as the API requires.

### M11.2 Infrastructure as code

- A Terraform and OpenTofu provider, `openllmproxy`, in its own repository,
  with resources for projects, providers, credential slots (secret values
  write-only), routes, routing policies, keys, budget groups, and notification
  destinations and rules. Resources for guardrails, sinks and MCP servers
  follow as [M7](m07-guardrails.md), [M5](m05-observability.md) and
  [M10](m10-agent-gateway.md) ship. Plans call the management API and respect
  ETags.
- A reusable GitHub Actions workflow that runs `olp config plan` on pull
  requests, comments the actions, conflicts and blockers, and runs `apply` on
  merge.

### M11.3 KMS and external secret stores

- **Master key ring.** `OLP_MASTER_KEY_FILE` may hold a ring whose data keys
  are wrapped by AWS KMS, Google Cloud KMS, Azure Key Vault or HashiCorp Vault
  Transit, unwrapped at startup with workload identity. Rotation keeps using
  `olp master-key`.
- **Credential references.** A credential version may reference a secret in AWS
  Secrets Manager, Google Secret Manager, Azure Key Vault or Vault KV instead of
  holding sealed bytes. [`internal/runtime/credentials.go`](../../internal/runtime/credentials.go),
  the one credential source, resolves it with a bounded cache, pinning the
  external version where the store has versions. Rotation remains a new
  credential version that is validated before activation.
- **Key delivery.** An API key may name a secret-store path as its rotation
  destination. The worker then rotates the key on its interval, writes the new
  secret to the store and keeps the
  [M4.3](m04-tenancy-identity.md#m43-access-ergonomics) overlap period, so
  scheduled rotation never produces a secret that nobody receives.
- Secret-store endpoints pass the provider egress policy, and resolution
  failures surface as credential ineligibility in plan decisions, never as
  silent fallbacks.
- Other stores LiteLLM lists, such as CyberArk, close under the
  [breadth rule](parity.md#how-to-read-the-matrix) through mounted files and
  Vault-compatible endpoints.

### M11.4 Developer catalog

- `GET /api/v1/catalog` and a console page list the routes a member or key may
  use, with operations, context length, modalities, per-million prices, privacy
  facts and code samples for the OpenAI, Anthropic and Gemini SDKs. Upstream
  vendors and models stay hidden unless the route opts into showing them.
- An optional public catalog page per project serves the same information
  without authentication, for internal developer portals, when an owner enables
  it.
- Once [M10](m10-agent-gateway.md) ships, the catalog also lists the toolsets,
  agents and skills the viewer may use.

### M11.5 Multi-region operation

- **Read replicas.** Gateways may read runtime releases and key authority from
  `OLP_DATABASE_READ_URL`. Replica lag counts toward authority age, so the
  existing 60-second staleness rule still refuses traffic on a stale replica.
- **Regional fleets.** Each region runs gateways and workers with its own
  Valkey; PostgreSQL has one primary. Rate and concurrency limits are enforced
  per region, with optional per-region values on keys. Cost budgets stay
  globally authoritative in PostgreSQL and are enforced regionally from
  snapshots; the documented overshoot bound is the number of regions times the
  in-flight cost admitted between reconciliations.
- **Locality.** Gateways know their region, and selection prefers same-region
  connections within a priority tier, after hard region constraints.
- **Several installations.** A console fleet view switches between independent
  installations using per-installation sessions, without sharing data, keys or
  databases between them.

### M11.6 Management MCP server

`/api/v1/mcp` exposes management operations as MCP tools to clients holding a
management token. Tools are generated from the management contract, read-only
unless the token holds write operations, and each call goes through the same
authorization, ETag, idempotency and audit path as the API. Tool results never
include secret values.

### M11.7 Console polish

- An installation name and logo within the console design system.
- A comparison view in the playground that sends one prompt to up to three
  routes the member may use and shows output, latency, tokens and cost side by
  side. Like the playground today, it stores nothing.
- Bulk member editing, and session-scoped saved filters for usage and request
  history.

## Non-goals

- Sharing data, keys or databases between installations. The fleet view only
  switches sessions.
- More than one PostgreSQL primary.
- Console extension plugins that embed third-party applications.
- Rotating an API key on a schedule when no secret store receives the result.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| KMS-wrapped data keys | The mounted master-key ring file | Until rotated | Existing master-key ring; no plaintext key at rest |
| Secret-store references | Credential versions in PostgreSQL, as a reference instead of sealed bytes | As credential versions today | None |
| Resolved external secrets | Gateway memory, in a bounded cache | The cache bound | None |
| Rotated API key secrets | The operator's secret store | Outside OLP | None; OLP keeps only the digest |
| CLI sign-in tokens | The operator's machine | The token lifetime | Existing management-token digests in PostgreSQL |
| Regional limit counters | Each region's Valkey | The limit window | None |
| Installation name and logo | PostgreSQL settings | Until changed | None |

## Change map

| Change | Start here |
| --- | --- |
| CLI | `cmd/olp/`, generated client from `openapi/` |
| KMS and secret references | `internal/secrets/`, `internal/runtime/credentials.go` |
| Catalog | `internal/routes/`, new `console/src/lib/features/catalog/` |
| Read replicas and regions | `internal/database/`, `internal/runtime/`, `internal/limits/` |
| Management MCP server | `internal/access/` (routing, authorization, ETags, idempotency and audit), `internal/management/` (the served contract) |
| Comparison playground | `console/src/lib/features/inference/playground/` |

## Decisions to settle

1. Whether regional rate limits divide a key's global limit automatically
   (recommended: no; regional values are explicit, and the default applies the
   full limit per region, which is documented).
2. The Terraform provider's model: per-resource management or a single
   configuration-artifact resource (recommended: per-resource, which matches
   Terraform practice, with configuration promotion kept for whole-installation
   moves).
3. Whether the public catalog may show prices (recommended: only when the
   owner opts in separately).

## Exit criteria

- [ ] **M11.1** The CLI covers every operation it lists, passes the
      authorization sweep as a management-token client, and is generated from
      the contract in CI.
- [ ] **M11.1** `olp login` yields a token limited to the member's role that
      stops working when the member is deactivated.
- [ ] **M11.2** The Terraform provider creates, updates, imports and destroys
      each resource against a disposable installation, and the reusable
      workflow comments a plan and applies it on merge in a test repository.
- [ ] **M11.3** A KMS-wrapped master-key ring starts OLP with workload
      identity, a referenced credential rotates through validation and
      activation, and a key with a rotation destination rotates on schedule
      with both secrets valid during the overlap.
- [ ] **M11.4** The catalog lists exactly the routes its viewer may use, hides
      upstream identity unless the route opts in, and its code samples run
      against the SDK suites.
- [ ] **M11.5** Gateways reading from a lagging replica refuse traffic once
      authority age exceeds 60 seconds.
- [ ] **M11.5** A two-region integration test enforces regional limits and
      reconciles global cost budgets within the documented overshoot bound.
- [ ] **M11.6** The management MCP server refuses every operation the token's
      scopes do not admit.
- [ ] **M11.7** The comparison view and branding pass Chromium journeys and the
      axe checks.
- [ ] The [parity matrix](parity.md) administration rows are `Parity` or
      better, with the secret-manager row closed under the
      [breadth rule](parity.md#how-to-read-the-matrix).
