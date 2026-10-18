# M4: Tenancy, identity and budgets

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | None | [M5](m05-observability.md), [M6](m06-cost-management.md), [M10](m10-agent-gateway.md), [M11](m11-operator-ecosystem.md) |

OLP implements project and organization access, digest-only end-user identity,
hierarchical budgets, workload JWTs, SAML, SCIM and local MFA in the core product.
Four installation roles, scoped management tokens and contract-declared
authorization remain the authority boundary. `Implemented` records code and
focused verification; it does not claim production IdP/directory conformance.

## Outcome

- Applications identify their end users, and OLP enforces per-end-user limits
  and budgets without storing raw end-user identifiers.
- Budgets and limits form a hierarchy (installation, organization, project,
  budget group, key, end user, route) with calendar windows in a configured
  time zone, reusable templates and audited temporary increases.
- Workloads authenticate with tokens from explicitly configured issuers instead
  of static keys.
- Members sign in with OIDC or SAML, are provisioned by SCIM, and may be
  required to use multi-factor authentication.
- Organizations delegate administration of groups of projects.
- Callers may supply their own provider credential for an operator-declared
  connection.

## Baseline

Historical planning comparison, not a description of missing current code.
Implemented behavior is authoritative in the
[compatibility matrix](../compatibility.md) and [access guide](../access.md).

| | Planning baseline | LiteLLM reference |
| --- | --- | --- |
| Principals | Members, management tokens, API keys, bootstrap token ([security](../security.md#principals)) | Keys, users, teams, organizations, [customers](https://docs.litellm.ai/docs/proxy/customers) |
| Limits | Key requests and tokens per minute, concurrency, daily and monthly cost; shared [budget groups](../access.md#projects-and-budget-groups) | Key, user, team, organization, customer, tag and per-model budgets with arbitrary durations ([budgets](https://docs.litellm.ai/docs/proxy/users)) |
| Identity | OIDC, push provisioning, local passwords ([access](../access.md)) | SSO, [SAML](https://docs.litellm.ai/docs/proxy/saml_sso), [SCIM](https://docs.litellm.ai/docs/proxy/identity_provisioning), [JWT auth](https://docs.litellm.ai/docs/proxy/token_auth) |
| Network controls | Trusted-proxy address resolution | [IP allowlists](https://docs.litellm.ai/docs/proxy/ip_address) |

## Scope

### M4.1 End users

Implemented key-controlled native/header identity, project-HMAC digests,
authorized lookup and per-end-user limits/reporting. Raw identity is not durable;
see [end users](../access.md#end-user-identity).

### M4.2 Budget hierarchy, windows and templates

Installation/organization/project/group/key/end-user/route/attribution budgets,
ISO-local weeks, scheduled IANA-calendar transitions, template ceilings and
audited temporary additions ship. Independent boundaries must all pass;
unknown spend fails closed and no edit resets historical owners/windows.
See [budget controls](../access.md#installation-and-project-budgets).
Templates are project-scoped policy ceilings, not shared spend accounts or
automatic subscription tiers. They cap each selected member independently and
do not create counters. Inline policies may tighten but cannot relax a template.

### M4.3 Access ergonomics

Key and management CIDRs, project route groups, overlap rotation and expiry
reminders ship. Groups change authority without route publication; rotation
versions share the same principal/accounting. [Access](../access.md) owns
bounds, ETags, replay and reminder semantics.

### M4.4 Workload identity

Configured issuers/JWKS/exact mappings enroll digest-only machine principals,
with restricted fixed scopes/project/templates/groups. JWT expiry, live issuer
revocation and retained-owner renewal apply; no static key is exposed.
See [workload JWT](../access.md#workload-jwt-identity).

### M4.5 Enterprise identity

One-level organizations/additive grants, narrow signed SAML profile, bounded SCIM
and local TOTP/WebAuthn/recovery controls ship. External ownership survives
password/MFA operations; verified mapping loss deauthorizes. Production IdP/
directory conformance needs independent qualification.
See [organizations](../access.md#organizations), [SAML](../access.md#saml-console-sign-in),
[SCIM](../access.md#scim-provisioning) and [MFA](../access.md#local-multi-factor-authentication).

### M4.6 Caller-supplied provider credentials

Declared caller-auth connections use request-local secrets/isolated transports.
Caller-paid route policy exempts eligible USD spend only, not authorization/rate/
concurrency/security. Shadows/probes stay paid; immutable exemption survives late
accounting. See [caller credentials](../gateway.md#caller-supplied-provider-credentials).

## Change map

| Change | Start here |
| --- | --- |
| End users, templates, route groups, key policies | `internal/access/`, `console/src/lib/features/access/` |
| Hierarchical budgets and windows | `internal/limits/`, `internal/usage/` |
| JWT issuers and workload principals | `internal/access/`, `internal/gateway/credentials.go` |
| SAML, SCIM, MFA, organizations | `internal/access/`, [`openapi/management.json`](../../openapi/management.json) |
| Caller-supplied credentials | `internal/runtime/credentials.go`, `internal/egress/` |

## Decisions settled

1. Organization depth is one level above projects. Existing assignments are
   immutable; promotion may assign a previously unassigned project.
2. End-user digests are per project, with installation separation from the
   authentication key. Unassigned keys share one unassigned boundary. The same
   identifier in different projects cannot be correlated.
3. Calendar changes activate separately at the next existing day, week and month
   boundary. Pending changes can be replaced; active periods and historical
   calendar entries are preserved. The first new-zone period may be shorter.
4. End-user identity is caller-supplied attribution, not proof of a human's
   identity. Keys explicitly select header or native input. Invalid, ambiguous
   or required-but-missing values refuse before dispatch; strict native input
   retains native forwarding. Raw identifiers never enter durable OLP state.
5. All applicable budget boundaries independently admit a request; this is not
   a fallback hierarchy. Reserve before dispatch, refund on later admission
   refusal and reconcile actual cost. Unknown/stale/incomplete balances return
   503; exhausted known balances return 429 with a bounded refusal owner/window.
   Default consumer basis is provider `cost`; configured `charge` uses M6 rate
   cards. Unpriced work remains visible and is not an invoice spending guarantee.
6. Temporary increases add to one existing owner/window, expire no later than
   its end and never extend on replay. Policy removal, key rotation and template
   detachment retain historical accounting. Changes are conditional and audited;
   unrelated limits still apply. Templates and portable policy promote;
   identities, digest overrides, secrets and temporary increases remain local.
7. Workload trust is owner-configured: exact issuer/audience/signature checks
   and first matching fixed mapping determine project, scopes, groups and
   template. Claims cannot choose arbitrary authority. Persist only the
   purpose-separated subject digest and principal UUID. Live disablement and
   the existing 60-second authority-age refusal override request-pinned policy.
8. Organization grants are additive and installation roles/token scopes still
   bound them. SAML and SCIM retain explicit external ownership: password/MFA
   operations do not adopt external accounts or restore lost external grants.
   SCIM deletion deactivates rather than erases accounting/history. The signed
   SAML profile, bounded SCIM operations and MFA recovery rules in the access
   guide are the supported product contract, not unrestricted protocol support.
9. Caller credentials apply only to operator-declared caller-auth connections
   and use isolated request-local transport. Their USD exemption is pinned on
   the accounting event and survives delayed settlement. Authorization, rate,
   token and concurrency rules still apply; paid shadow/probe work retains its
   own cost. Never infer exemption merely from a missing price or credential.

## Delivery order and evidence

The implementation sequence below records dependency boundaries for maintenance
and requalification; these are completed slices, not a new implementation plan.

| Slice | Contract and focused evidence |
| --- | --- |
| 1. End-user attribution | `internal/access/end_users.go`, `internal/gateway/end_user_limits.go`; `TestEndUserConcurrentCostBudgetsKeepTelemetryPrivate` covers shared limits and private durable facts |
| 2. Budget owners and policy | `internal/limits/`, `internal/usage/`; `TestAggregateProjectAndInstallationCapsBothApply`, `TestBudgetCalendarSettingAndValkeyAdmission`, `TestLimitTemplatesPropagateToKeysUsersAndGroups` |
| 3. Access ergonomics | Key CIDR, route-group, overlap-rotation and reminder suites; existing authority owners compile policy without changing published routes |
| 4. Workload identity | `internal/access/workload_issuers.go`; `TestWorkloadIdentityMapsAndAccountsWithoutRetainingCredentials` and `TestWorkloadSubscriptionRebindsCurrentPolicyAndRetainsDigest` |
| 5. Enterprise identity | `internal/samlidentity/`, SCIM/MFA/organization access owners; `TestSAMLRefusesMalformedUnsignedAndMisboundAssertions`, `TestSCIMUsersGroupsAndInheritedGrants`, `TestMFAPolicyPromotionAndExplicitOfflineRecovery`, `TestOrganizationsDelegateProjectsWithoutChangingInstallationRoles` |
| 6. Caller credentials | `internal/runtime/credentials.go`, `internal/egress/`; `TestCallerCredentialsStayOutOfDurableStateAndTelemetry` plus caller-paid browser journey |

Test names identify repository evidence, not tests newly run for this roadmap
edit. The [M1–M4 implementation review](m01-m04-implementation-review.md) records
focused historical verification; the later
[architecture review](../architecture-review.md#recorded-qualification) records
the integrated source run and its limits.

## Exit criteria

These checked items describe the recorded implementation qualification.

- [x] End-user limits and budgets hold across two gateways under concurrent
      load, and no raw end-user identifier appears in PostgreSQL, Valkey, logs
      or traces.
- [x] A request is refused when any level of its budget hierarchy is exhausted,
      and reports show which level refused it.
- [x] Weekly windows and non-UTC time zones reset at the correct boundary,
      including across daylight-saving transitions.
- [x] Workload JWTs from a test issuer authenticate, survive signing-key
      rotation, and stop working within the authority freshness bound after the
      issuer is disabled.
- [x] SAML sign-in, SCIM provisioning and MFA pass focused Chromium journeys.
      External SCIM protocol conformance and production IdP qualification are
      separate qualification; this does not assert those external results.
- [x] The authorization and isolation sweeps cover organizations and every new
      operation.
- [x] Caller-supplied credentials never appear in any persisted record, log or
      error body in integration tests.
- [x] The [parity matrix](parity.md) identity and budget rows are `Parity` or
      better.

## Remaining release qualification

- [ ] Run the supported signed-SAML profile and SCIM Users/Groups operations
      against the target production IdP/directory. Cover certificate rollover,
      disabled users, removed group grants, replay and provisioning retries;
      record exact product/version and refused unsupported features.
- [ ] Qualify workload issuer/JWKS rollover, token expiry, audience mismatch and
      disablement against each deployment's configured issuer. Demonstrate
      refusal within the 60-second authority deadline during an issuer outage.
- [ ] Re-run access/isolation, budget concurrency, caller-credential privacy,
      browser MFA/recovery and applicable race/integration gates on the exact
      release candidate after later code changes. Record the candidate and
      evidence before marking `Shipped`.

These gates do not reopen implemented scope. No date, additional organization
nesting, arbitrary budget durations, SCIM bulk operations or unrestricted SAML
profiles are implied by this milestone.
