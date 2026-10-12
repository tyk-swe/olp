# M4: Tenancy, identity and budgets

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Implemented | None | [M5](m05-observability.md), [M6](m06-cost-management.md), [M10](m10-agent-gateway.md), [M11](m11-operator-ecosystem.md) |

OLP's access model is strict and well tested: projects, four installation
roles, digest-only API keys, scoped management tokens, OIDC and
contract-declared authorization. It is also narrow. The milestone baseline has no end-user policies or
project budgets, supports only daily and monthly UTC windows, and has no SAML,
SCIM or workload identity. This milestone completes tenancy and identity without
weakening the authorization contract, and ships every capability that LiteLLM
reserves for Enterprise in the core product.

## Outcome

- Applications identify their end users, and OLP enforces per-end-user limits
  and budgets without storing raw end-user identifiers.
- Budgets and limits form a hierarchy (installation, organization, project,
  budget group, key, end user, route) with calendar windows in a configured
  time zone, reusable templates and audited temporary increases.
- Workloads authenticate with their platform's identity tokens instead of
  static keys.
- Members sign in with OIDC or SAML, are provisioned by SCIM, and may be
  required to use multi-factor authentication.
- Organizations delegate administration of groups of projects.
- Callers may supply their own provider credential for an operator-declared
  connection.

## Baseline

Planning-time comparison; shipped behavior is authoritative in the
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

## Exit criteria

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
