# M11: Operator ecosystem

| Status | Depends on | Unlocks |
| --- | --- | --- |
| In progress | [M4](m04-tenancy-identity.md) | Automation-first and multi-region operation |

OLP is operated through its console, its management API and configuration
promotion, with one PostgreSQL primary and file-mounted secrets. LiteLLM adds a
management CLI, secret managers, a model hub, multi-region topologies and
administration through agents. This milestone delivers those capabilities on
OLP's existing contracts: every tool is a client of the declared management
API, so authorization, ETags, idempotency and audit apply unchanged.

## Outcome

- Operators automate OLP from a CLI, Terraform and CI pipelines.
- Master keys and provider credentials can live in cloud KMS and secret stores.
- Developers find routes, their facts and working code samples in a catalog.
- Gateway fleets run in several regions under one control plane, with defined
  consistency.
- Agents can administer OLP through a scoped MCP server.

## Baseline

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| CLI | [Contract-generated management commands](../operator-cli.md), saved configuration plans and qualified-client environment setup, alongside process and recovery commands | [lite CLI](https://docs.litellm.ai/docs/proxy/management_cli) for models, credentials, keys, teams and users |
| Desired state | [Export, plan and apply](../configuration.md#configuration-promotion-artifacts) with canonical digests | [config.yaml](https://docs.litellm.ai/docs/proxy/configs) and database models |
| Secrets | Workload-identity wrapped rings and immutable AWS/GCP/Azure/Vault credential references with validated rotation ([external secrets](../external-secrets.md)) | [Secret managers](https://docs.litellm.ai/docs/secret_managers/overview) (Enterprise) |
| Topology | [Regional fleets](../deployment.md#regional-fleets), explicit key overrides, global cost reconciliation and [replica-aware runtime authority](../deployment.md#regional-read-replicas) and the [independent-installation console switcher](../operator-console.md#independent-installation-bookmarks) | [Read replicas](https://docs.litellm.ai/docs/proxy/db_read_replica), [multi-region](https://docs.litellm.ai/docs/proxy/multi_region) and a [global control plane](https://docs.litellm.ai/docs/proxy/global_control_plane) (Enterprise) |
| Discovery | [Member/key-visible model catalog](../model-catalog.md), SDK examples, explicit upstream disclosure and independently priced owner-enabled public catalogs | [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) |

## Scope

### M11.1 Management CLI

`olp` gains client subcommands that call the management API with a management
token read from `OLP_MANAGEMENT_TOKEN_FILE` and an endpoint from
`OLP_MANAGEMENT_URL`:

| Command group | Operations |
| --- | --- |
| `olp config` | `export`, `plan`, `apply`, with a digest check that refuses to apply a plan computed against a different destination state |
| `olp keys`, `olp routes`, `olp providers` | List, read, create, update, revoke, rotate, simulate, certify and activate, mirroring the console |
| `olp usage` | Summaries, breakdowns and CSV export |
| `olp client-env` | Print the environment for a client from [M1.3](m01-measured-advantage.md#m13-client-and-agent-qualification), such as Claude Code or Codex, pointed at OLP with a given key file |

Commands are generated from [`openapi/management.json`](../../openapi/management.json)
together with the existing contract types, so the CLI cannot drift from the
API. Writes send `If-Match` and `Idempotency-Key` as the API requires.

### M11.2 Infrastructure as code

- A Terraform and OpenTofu provider, `openllmproxy`, in its own repository,
  with resources for projects, providers, credential slots (secret values
  write-only), routes, routing policies, keys, budget groups, notification
  destinations and rules, guardrails, sinks and MCP servers. Plans call the
  management API and respect ETags.
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
- Secret-store endpoints pass the provider egress policy, and resolution
  failures surface as credential ineligibility in plan decisions, never as
  silent fallbacks.

### M11.4 Developer catalog

- `GET /api/v1/catalog` and a console page list the routes a member or key may
  use, with operations, context length, modalities, per-million prices, privacy
  facts and code samples for the OpenAI, Anthropic and Gemini SDKs. Upstream
  vendors and models stay hidden unless the route opts into showing them.
- An optional public catalog page per project serves the same information
  without authentication, for internal developer portals, when an owner enables
  it.

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

An installation name and logo within the console design system, bulk member
editing, and session-scoped saved filters for usage and request history.

## Change map

| Change | Start here |
| --- | --- |
| CLI | `cmd/olp/`, generated client from `openapi/` |
| KMS and secret references | `internal/secrets/`, `internal/runtime/credentials.go` |
| Catalog | `internal/routes/`, new `console/src/lib/features/catalog/` |
| Read replicas and regions | `internal/database/`, `internal/runtime/`, `internal/limits/` |
| Management MCP server | `internal/management/` |

## Decisions

1. Regional limits are explicit. A region without an override applies the full
   key limit; limits are never divided automatically as regions join or leave.
2. Terraform manages individual resources, with ETags protecting updates.
   Configuration promotion remains the workflow for whole-installation moves.
3. Public catalogs hide prices by default. A project owner must enable public
   prices separately from enabling the public catalog.

## Exit criteria

- [x] The CLI covers every operation it lists, passes the authorization sweep as
      a management-token client, and is generated from the contract in CI.
- [ ] The Terraform provider creates, updates, imports and destroys each
      resource against a disposable installation.
- [x] A KMS-wrapped master-key ring starts OLP with workload identity, and a
      referenced credential rotates through validation and activation.
- [x] Gateways reading from a lagging replica refuse traffic once authority age
      exceeds 60 seconds.
- [x] A two-region integration test enforces regional limits and reconciles
      global cost budgets within the documented overshoot bound.
- [x] The management MCP server refuses every operation the token's scopes do
      not admit.
- [ ] The [parity matrix](parity.md) administration rows are `Parity` or better.

Replica qualification: `TestManagementReadReplicaLagConsumesTheRevocationDeadline`
uses a physical PostgreSQL standby, pauses replay after revocation, checks
gateway HTTP 503 at the authority deadline, resumes replay, and checks HTTP 401
for the revoked key. WAL checkpoint unit tests cover idle replicas, replay
regression and bounded monitoring state. Deployment settings are validated by
the Helm checks and the full local check.

Regional qualification:
`TestRegionalRateAndConcurrencyLimitsWithGlobalBudgetReconciliation` uses two
independent Valkey services, checks inherited and explicit limits, elects one
leader per region, observes $1.20 global spend against a $1 budget, and verifies
that both stores refuse further cost after reconciliation. It also verifies
that local readiness cannot borrow another region's worker success. Selection
tests preserve priority and hard region constraints while preferring local
connections. The regional changes pass the full local check, focused race
tests and Helm validation.

Console qualification: `control.spec.ts` covers ETag-protected name/logo edits,
bulk role changes, session-scoped saved usage views, and accessibility.
`fleet/installations.spec.ts` runs two live installations with separate
databases and independently generated keys, verifies project and saved-view
isolation, switches between retained sessions, and checks the menu at mobile
width. Packaged Chromium journeys, the full local check, backend race tests
and the management authorization sweep pass.

Generated-client qualification:
`TestGeneratedManagementClientsAndMCPFollowEveryMachineAuthorization` exercises
every generated operation through both the SDK and CLI against each live
management-token archetype in the authorization golden. It checks the MCP tool
list against the same matrix, calls every disallowed tool, and rejects
session-only, unknown and recursive MCP operations. The uncached race test
passes against a disposable installation. Contract generation and drift checks
include the shared CLI/MCP registry.

Catalog qualification: `TestModelCatalogVisibilityPublishingAndUpstreamDisclosure`
verifies project/key/token visibility, precise current pricing, default identity
privacy, independent public-price opt-in, ETags, withdrawal and key revocation.
`TestModelCatalogPromotionPreservesOwnerConsentAndStagesRouteDisclosure` checks
conditional owner authority and disclosure publication only after imported draft
activation. The packaged catalog browser journey covers discovery, disclosure,
public publication and accessibility. Projection tests cover conservative facts,
unknown/partial coverage, exact decimal ranges and native SDK examples. The full
local gate and ordinary/generated-client authorization sweeps pass.

Secret-store qualification: four-platform protocol tests verify identity,
version and integrity bindings; egress tests refuse private destinations and
redirects. `TestExternalCredentialAndWrappedMasterKeyRotationKeepPublishedVersionsPinned`
uses live PostgreSQL/Valkey and Vault JWT/Transit/KV protocols to validate,
activate and promote a new reference without copying values, retain old pinned
versions, rotate/verify sealed records and start the built gateway with a
wrapped ring. Cache races cover single-flight, bounded capacity and unavailable
plan decisions. The packaged external-credential console journey creates and
rotates versions and passes accessibility checks; local and Helm gates pass.

Terraform qualification in progress: the separate
[provider repository](https://github.com/tyk-swe/terraform-provider-openllmproxy)
currently qualifies projects, budget groups, notification destinations and
notification rules against fresh live installations with Terraform 1.16.5 and
OpenTofu 1.13.1. Both engines exercise plan/create/update/import/destroy,
concurrent-edit refusal, nullable budget-ceiling removal and ephemeral signing
values excluded from saved plans/state. CI builds OLP from the SDK's immutable
module version. The full M11 resource-list exit criterion remains pending. See
[provider operations](../terraform.md).

API-key resource qualification also runs both engines against the SDK-pinned
OLP binary. It covers create/update/import/no-change/destroy, exact budget-window
projection, owner-only one-time output without secret state, and retained
revocation records. A forced output error preserves the created UUID in state
and allows destroy, preventing orphaned active credentials. The full resource
list remains pending.

Provider and slot lifecycle prerequisites are qualified by
`TestProviderDeletionRequiresUnusedDraftAndCleansOwnedSeals` and
`TestCredentialSlotConditionsAreIndependentAndPreservePublishedVersions`.
Unused draft deletion is conditional, replayable and audited, cleans owned seals,
and preserves provider-specific price history and published dependencies.
Individual slot preconditions survive sibling edits; deletion retains published
credential versions and refuses the required default slot. The generated SDK,
CLI and MCP authorization sweep covers the new operations; focused races,
contract generation and the full local gate pass. Terraform adapters and the
complete resource-list qualification remain in progress.

Provider and credential-slot Terraform/OpenTofu qualification now covers a real
compatible upstream, simultaneous pool creation, independent conditional updates,
validated write-only credential rotation, normalized nested configuration, UUID
and composite imports, and destruction. Tests prove secrets stay out of saved
plans/state, retain committed configuration and the prior counter after failed
credential validation, and refuse an externally edited provider during a saved
destruction plan. Atomic parent transition metadata survives idempotent replay.
The seven-type acceptance suite passes against the SDK-pinned OLP binary; the
complete twelve-resource exit criterion remains pending.

Published-route automation prerequisites are qualified by
`TestPublishedRouteWritesAreConditionalAtomicAndPreserveIndependentDrafts` and
`TestPublishedRouteReplacementPreservesItsPolicy`: create/replace publish through
the console's validation and promotion in one transaction; missing/stale
preconditions refuse, failed writes leave no draft/revision/release change,
replays preserve identity and audit counts, independent drafts remain untouched,
and the serving snapshot preserves existing policy. Full local checks, focused
races and the complete management/generated-client/MCP authorization sweeps pass.
Terraform route qualification remains pending until both engine lifecycles pass.

Published routes now have live Terraform 1.16.5 and OpenTofu 1.13.1 qualification:
`TestPublishedRouteLifecycleWithTerraformAndOpenTofu` exercises create, atomic
revision updates, UUID import, empty plans, retirement with retained history,
stale saved-plan refusal and project-boundary replacement rejection before
retirement. Exact target projection and canonical imported references are covered
by focused tests. The eight-resource acceptance suite passes against the provider's
immutable SDK-matched OLP build; the complete twelve-resource criterion remains
pending for routing policies, guardrails, sinks and MCP servers.

Routing-policy lifecycle prerequisites are qualified by
`TestRoutingPolicyRemovalPreservesFreshPreconditionsAndParentProof`: installation,
API-key, and route-draft removal/recreation, stale and missing preconditions,
original parent transitions on replay, and exact successful audit counts.
Removal retains a fresh default-policy ETag so an earlier default observation
cannot become valid again. Full local checks, focused races, and the complete
management/SDK/CLI/MCP machine authorization sweeps pass. The Terraform resource
qualification remains pending.
