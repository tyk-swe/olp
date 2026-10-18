# M11: Operator ecosystem

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | [M4](m04-tenancy-identity.md) | Automation-first and multi-region operation |

OLP operators use the console, a generated CLI, Terraform/OpenTofu and scoped
management MCP tools against the same declared management API. Authorization,
ETags, idempotency and audit protect each client. Regional fleets, cloud-backed
secrets and a caller-visible catalog extend that boundary without changing
installation ownership or request-pinned serving configuration.
`Implemented` records the code and historical qualification below; release
qualification must still name the candidate and target environment.

## Outcome

- Operators automate OLP from a CLI, Terraform and CI pipelines.
- Master keys and provider credentials can live in cloud KMS and secret stores.
- Developers find routes, their facts and working code samples in a catalog.
- Gateway fleets run in several regions under one control plane, with defined
  consistency.
- Agents can administer OLP through a scoped MCP server.

## Baseline

The table describes the implemented boundary. The linked runbooks,
[compatibility matrix](../compatibility.md) and [access guide](../access.md)
own the detailed operating contracts.

| | Implemented baseline | LiteLLM reference |
| --- | --- | --- |
| CLI | [Contract-generated management commands](../operator-cli.md), saved configuration plans and qualified-client environment setup, alongside process and recovery commands | [lite CLI](https://docs.litellm.ai/docs/proxy/management_cli) for models, credentials, keys, teams and users |
| Desired state | [Export, plan and apply](../configuration.md#configuration-promotion-artifacts) with canonical digests and [twelve conditional Terraform/OpenTofu resources](../terraform.md) | [config.yaml](https://docs.litellm.ai/docs/proxy/configs) and database models |
| Secrets | Workload-identity wrapped rings and immutable AWS/GCP/Azure/Vault credential references with validated rotation ([external secrets](../external-secrets.md)) | [Secret managers](https://docs.litellm.ai/docs/secret_managers/overview) (Enterprise) |
| Topology | [Regional fleets](../deployment.md#regional-fleets), explicit key overrides, global cost reconciliation and [replica-aware runtime authority](../deployment.md#regional-read-replicas) and the [independent-installation console switcher](../operator-console.md#independent-installations) | [Read replicas](https://docs.litellm.ai/docs/proxy/db_read_replica), [multi-region](https://docs.litellm.ai/docs/proxy/multi_region) and a [global control plane](https://docs.litellm.ai/docs/proxy/global_control_plane) (Enterprise) |
| Discovery | [Member/key-visible model catalog](../model-catalog.md), SDK examples, explicit upstream disclosure and independently priced owner-enabled public catalogs | [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub) |

## Scope

### M11.1 Management CLI

`olp` provides client subcommands that call the management API with a management
token read from `OLP_MANAGEMENT_TOKEN_FILE` and an endpoint from
`OLP_MANAGEMENT_URL`:

| Command group | Operations |
| --- | --- |
| `olp config` | `export`, `plan`, `apply`, with a digest check that refuses to apply a plan computed against a different destination state |
| `olp keys`, `olp routes`, `olp providers` | List, read, create, update, revoke, rotate, simulate, certify and activate, mirroring the console |
| `olp usage` | Summaries, breakdowns and CSV export |
| `olp client-env` | Print the environment for a client from [M1.3](m01-measured-advantage.md#m13-client-and-agent-qualification), such as Claude Code or Codex, pointed at OLP with a given key file |

Commands and the shared MCP registry are generated from
[`openapi/management.json`](../../openapi/management.json), with CI drift checks.
The supported machine-command set is the contract's JSON/empty-response
operations, not session-only, raw upload/download, live SSE or XML operations.
Writes require the observed `If-Match` and explicit `Idempotency-Key` where the
API declares them; clients never fetch a newer ETag to force a stale write.
After an uncertain response, retry the exact intent and original identity.
Completed replay still requires current authority. One-time CLI secrets go only
to the caller's output or an owner-only file, never generated source or CI logs.

### M11.2 Infrastructure as code

- The [Terraform/OpenTofu provider](../terraform.md), `openllmproxy`, lives in
  its own repository. Its twelve resources are projects, providers, credential
  slots, routes, routing policies, keys, budget groups, notification destinations,
  notification rules, guardrails, export sinks and upstream MCP servers.
  Resource plans retain observed ETags; apply refuses foreign edits. Secrets
  use ephemeral/write-only fields and never enter saved plans/state. Deletion
  is conditional and may retire/revoke when accounting or published history
  must survive. MCP registration does not implement the M10 tool gateway.
- The [promotion workflow](../configuration-actions.md) plans pull requests
  and comments only actions/conflicts/blockers. Apply on default-branch push
  requires a committed reviewed plan whose destination and intent still match.
  It never silently replans a stale review. Whole-installation promotion and
  per-resource Terraform ownership must not concurrently manage the same intent.

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
- References name immutable versions; `latest` aliases are refused. The sole
  runtime credential source caches at most 128 values for one minute, coalesces
  fetches and never serves expired values after a failed refresh. Creating a
  reference also requires installation `settings` authority. Cloud protocol
  fixtures and live Vault qualification do not imply all four hosted services
  have been exercised with production identities.

### M11.4 Developer catalog

- `GET /api/v1/catalog` and a console page list the routes a member or key may
  use, with operations, context length, modalities, per-million prices, privacy
  facts and code samples for the OpenAI, Anthropic and Gemini SDKs. Upstream
  model names stay hidden unless the route opts into showing them.
- Consumer keys require `models_read`; members and management tokens retain
  project visibility. The catalog reports unknown facts and partial pricing
  coverage explicitly. Examples are emitted only for certified eligible unary
  surfaces and never contain credentials.
- An optional public catalog page and `GET /api/v1/catalog/public/{project_id}`
  serve a project's published routes when an installation owner enables them.
  Public prices are a separate opt-in. Upstream model disclosure is separately
  controlled per route; provider identities/endpoints remain private.

### M11.5 Multi-region operation

- **Read replicas.** Gateways may read runtime releases and key authority from
  `OLP_DATABASE_READ_URL`. Replica lag counts toward authority age, so the
  existing 60-second staleness rule still refuses traffic on a stale replica.
  Freshness is proved from primary WAL progress and replica replay, not the
  replica's reachability or wall clock. Writes, registration, accounting and
  historical credential reads remain on the primary. Both pools must identify
  the same installation/schema; no replica query runs in ordinary admission.
- **Regional fleets.** Each region runs gateways and workers with its own
  Valkey; PostgreSQL has one primary. Rate and concurrency limits are enforced
  per region, with optional per-region values on keys. Cost budgets stay
  globally authoritative in PostgreSQL and are enforced regionally from
  snapshots refreshed once a minute by one leader per region. Exposure includes
  **all** spend admitted before fresh global evidence, including completed work,
  not merely in-flight cost. With bounded estimates and timely accounting the
  exposure is the sum of each region's admissions against stale snapshots;
  outages enlarge it. This is eventual global enforcement, not a fixed dollar
  or invoice cap. Unknown reconstruction fails closed and each region must
  prove its own worker readiness.
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
It is a stateless POST-only Streamable HTTP endpoint at MCP versions
`2025-03-26`, `2025-06-18` and `2025-11-25`, not the planned consumer `/mcp`.
The tool list is authority-filtered and direct calls repeat authorization.
Recursive MCP/session-only operations are excluded. Secret redaction includes
one-time generated keys, so create-through-MCP does not deliver usable secret
material; use the CLI/console when capture is required.

### M11.7 Console polish

An installation name and logo within the console design system, bulk member
editing, and session-scoped saved filters for usage and request history.

## Change map

| Change | Start here |
| --- | --- |
| CLI | `cmd/olp/`, `internal/operatorcli/`, generated `sdk/management/` from `openapi/` |
| KMS and secret references | `internal/secretstore/`, `internal/secrets/`, `internal/runtime/credentials.go` |
| Catalog | `internal/modelcatalog/`, `console/src/lib/features/model-catalog/` |
| Read replicas and regions | `internal/database/`, `internal/runtime/`, `internal/limits/` |
| Management MCP server | `internal/management/` |

## Decisions

1. Regional limits are explicit. A region without an override applies the full
   key limit; limits are never divided automatically as regions join or leave.
2. Terraform manages individual resources, with ETags protecting updates.
   Configuration promotion remains the workflow for whole-installation moves.
3. Public catalogs hide prices by default. An installation owner must enable
   public prices separately from enabling the public catalog. Route disclosure
   is an independent conditional change and does not disclose provider identity.
4. Resource clients use the same API handlers and declared authority as the
   console. No client-specific admin bypass, automatic mutation identity or
   hidden stale-plan retry is permitted. Replayed writes retain original parent
   transition metadata and still check current caller authority.
5. Secret references select immutable external versions; configuration promotion
   binds destination secrets separately. Wrapped master-key rotation retains
   old keys until stored records and restored backups no longer need them.
6. Regional fleets share one installation UUID, HMAC/master ring and primary;
   independent installations share none of that state. The console switcher
   selects isolated sessions and is not a federated identity or global database.
7. Scope is the documented twelve infrastructure resources and generated JSON
   management operations. M10 owns consumer MCP execution, OAuth tool grants,
   toolsets and agents; registering a server here does not expose its tools.

## Delivery order and dependency boundaries

The code is implemented. These completed slices define the order for maintaining
or requalifying the ecosystem against a new release.

| Slice | Prerequisite | Verification handoff |
| --- | --- | --- |
| 1. Shared management contract | M4 roles, scopes and conditional mutations | Generated SDK/CLI/MCP registry, machine authorization sweep and exact replay semantics |
| 2. Promotion and resource automation | Slice 1 | Saved-plan stale-destination refusal, two-engine CRUD/import/destroy and write-only secret inspection |
| 3. Secret integrations | Credential lifecycle and egress policy | Version/integrity binding, failed refresh, rotation, retained old versions and wrapped-ring restart |
| 4. Catalog and operator console | Published routes plus M4 project/key visibility | Disclosure/price consent, revocation, promotion, independent sessions and accessibility |
| 5. Regional serving | Runtime snapshots, global accounting and shared authority | Physical standby lag, two independent Valkeys, regional leadership/readiness and budget reconciliation |
| 6. Release qualification | All slices and the exact release candidate | Integrated gates, target-cloud identity, target topology and backup/restore rehearsal |

## Exit criteria

Checked items record historical implementation evidence, not fresh results for
this documentation change.

- [x] The CLI covers every operation it lists, passes the authorization sweep as
      a management-token client, and is generated from the contract in CI.
- [x] The Terraform provider creates, updates, imports and destroys each
      resource against a disposable installation.
- [x] A KMS-wrapped master-key ring starts OLP with workload identity, and a
      referenced credential rotates through validation and activation.
- [x] Gateways reading from a lagging replica refuse traffic once authority age
      exceeds 60 seconds.
- [x] A two-region integration test enforces regional limits and reconciles
      global cost budgets within the documented overshoot bound.
- [x] The management MCP server refuses every operation the token's scopes do
      not admit.
- [x] The [parity matrix](parity.md) administration rows are `Parity` or better.

## Recorded implementation qualification

The detailed evidence below is retained as historical context. Named tests and
recorded timings do not imply that this documentation edit reran them.

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

Terraform qualification covers all twelve resource types in the separate
[provider repository](https://github.com/tyk-swe/terraform-provider-openllmproxy):
projects, providers, credential slots, routes, routing policies, guardrails,
export sinks, keys, budget groups, notification destinations, notification rules and upstream MCP servers. Terraform 1.16.5 and OpenTofu 1.13.1 run plan/create/update/import/no-change/
destroy against fresh live installations built from the SDK's immutable module
version. The full twelve-type suite passed under race in 140.698 seconds.
See [provider operations](../terraform.md).

Budget/notification tests cover concurrent-edit refusal, explicit nullable-ceiling
removal and ephemeral signing values excluded from saved plans/state. API-key
qualification covers exact budget-window projection, owner-only one-time output,
retained revocation, and recovery from forced output failure: state retains the
created UUID so destroy cannot leave an orphaned active credential.

Provider/slot backend qualification includes
`TestProviderDeletionRequiresUnusedDraftAndCleansOwnedSeals` and
`TestCredentialSlotConditionsAreIndependentAndPreservePublishedVersions`.
Unused draft deletion is conditional, replayable and audited, cleans owned seals,
and preserves scoped prices and published dependencies. Individual slot
preconditions survive sibling edits; deletion retains published credentials and
refuses the default slot. Both infrastructure engines exercise simultaneous pool
creation, validated write-only rotation, normalized configuration and composite
imports. Failed validation retains committed configuration and the prior secret
counter. Saved destruction plans refuse external parent edits. Atomic parent
transition metadata survives idempotent replay and advances only contiguous own
writes.

`TestPublishedRouteWritesAreConditionalAtomicAndPreserveIndependentDrafts` and
`TestPublishedRouteReplacementPreservesItsPolicy` prove atomic shared validation
and promotion, conditional refusal, failure rollback, replayed identity/audit,
independent draft preservation and pinned serving policy. The infrastructure
route test covers revision updates, canonical target import, empty plans,
retirement/history, stale saved-plan refusal and project-boundary replacement
rejection before retirement.

`TestRoutingPolicyRemovalPreservesFreshPreconditionsAndParentProof` covers all
three scopes, removal/recreation, missing/stale conditions, original replay
proofs and exact audit counts. Removal retains a fresh default-policy ETag.
`TestRoutingPolicyLifecyclesWithTerraformAndOpenTofu` covers composite imports,
key/policy graph mutations, omitted-constraint clearing and foreign saved-plan
refusal without overwriting the external policy.

`TestNamedGuardrailRevisionsRemainScopedAndPublishedCopiesStayPinned` and
`TestNamedGuardrailConfigurationPromotionIsPortableAndIdempotent` cover project
boundaries, immutable revisions, conditional replay/audit, actual input refusal,
pinned published copies and portable reuse. The infrastructure guardrail/route
graph qualifies reviewed policy composition, old-revision reads, foreign
saved-plan refusal and retained retirement.

Content-free sink qualification covers atomic scoped source queuing, replay
suppression, stable signed retries, usage/pricing corrections, expired-gap counts,
audit-field privacy, retirement cleanup and destination-bound promotion with
unchanged ETag reuse. The three-test service suite passed under race in 13.759
seconds. Ordinary management, isolation and every generated SDK/CLI/MCP machine
operation passed in 188.450 seconds; focused races and the full local gate pass.
`TestSignedExportSinkLifecyclesWithTerraformAndOpenTofu` additionally verifies
actual signed delivery by the built installation worker, both scoped lifecycles,
write-only values absent from saved-plan JSON and state, UUID imports, empty
plans, foreign saved-plan refusal and signing-material cleanup. Both engines are
included in the complete twelve-type suite.

Upstream MCP registration now certifies bounded Streamable HTTP handshakes and
schemas into project-owned immutable catalogs. The two-test live service suite
passed under race in 21.659 seconds: project/revision isolation, replay without
network rediscovery, concurrent retirement refusal, certification without held
mutation locks, sealed-material cleanup, and destination promotion matching a
reviewed digest with pinned reuse. JSON/SSE session handling, schema bounds,
remote-loader refusal, reflected-credential rejection and exact numeric bounds
have focused race coverage. Ordinary authorization, isolation and every generated
SDK/CLI/MCP machine operation passed in 245.075 seconds; the full local gate and
focused races pass.
`TestCertifiedMCPServerLifecyclesWithTerraformAndOpenTofu` qualifies create, update, static bearer rotation, UUID import, no-change plans and retirement with
both engines. It checks immutable old catalogs, no-network pinned reads after
upstream drift, secret exclusion from saved-plan JSON/state and foreign
saved-plan refusal. The complete twelve-type suite uses the same immutable SDK
and built installation version. Export checkpoint health is separately tested
for installation and regional summaries.

Client setup qualification executes generated configuration with every named
M1.3 family. The `operator-env` client suite completes 13 authenticated
JavaScript/framework cases, checking the client's result and upstream request
rewrite. `TestOperatorGeneratedGoClients` compiles and executes generated
constructors for all three official Go SDKs. Generated Python constructors
complete all three native requests through the smoke fixture, and
`TestOperatorGeneratedPythonClients` runs that proof in the integration gate.
Mounted credentials remain outside generated source. Path quoting, unsupported
format/surface refusal, Go syntax, focused races and operator tests pass.

The default merge-base benchmark gate passes ten paired samples across all seven
shared packages and 93 benchmarks, with no additions or removals and no
statistically significant regression above the 10% budget. The first full
comparison exposed unnecessary synchronization in empty external-credential
caches; the eligibility fast path was corrected and the entire gate rerun.
External-reference freshness, failure and concurrent-fetch behavior retain
focused race coverage.

## Recorded integrated qualification

The October 10 implementation passed the default integration flow, internal/
extension/SDK/client suites, packaged/Vite browsers and fresh/restored journeys.
Local race/check, contract/version/build/Helm gates also passed. Later integrated
repair evidence is recorded in [architecture review](../architecture-review.md).
These historical results are not new validation for later changes.

The [Terraform/OpenTofu provider](https://github.com/tyk-swe/terraform-provider-openllmproxy/pull/1)
has separate two-engine acceptance evidence. Runbooks/screenshots cover
[operators](../operator-console.md), [catalog](../model-catalog.md) and
[external secrets](../external-secrets.md).

All fourteen administration/deployment parity rows closed; LiteLLM documentation
was reviewed 2026-10-10. M5 observability is now implemented; broader guardrail
execution and agent-gateway work remain M7/M10.

## Remaining release qualification

- [ ] Run generation/drift, management authorization/isolation, the complete
      twelve-resource Terraform/OpenTofu suite, packaged browser journeys and
      normal check/integration/build/Helm gates against the same candidate.
      Record OLP/provider commits and both engine versions.
- [ ] For each cloud service claimed by a deployment, qualify workload identity,
      exact version/integrity binding, denied access, rotation and startup after
      outage using that service. Recorded protocol fixtures and live Vault tests
      are retained evidence, not a substitute for target-cloud execution.
- [ ] Rehearse primary/replica interruption, regional worker/Valkey loss and
      recovery at the target admission rate. Record observed stale-snapshot spend
      exposure, the 60-second authority refusal and local readiness behavior.
- [ ] Restore the candidate's backup with its required old master-key versions,
      validate secret resolution and independent-console isolation, and record
      the deployment's measured recovery objectives before marking `Shipped`.
