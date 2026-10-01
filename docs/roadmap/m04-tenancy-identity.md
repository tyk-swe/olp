# M4: Tenancy, identity and budgets

| Status | Depends on | Unlocks |
| --- | --- | --- |
| Planned | None | [M5](m05-observability.md), [M6](m06-cost-management.md), [M10](m10-agent-gateway.md), [M11](m11-operator-ecosystem.md) |

OLP's access model is strict and well tested: projects, four installation
roles, digest-only API keys, scoped management tokens, OIDC and
contract-declared authorization. It is also narrow. It has no end users, no
project budgets, only daily and monthly UTC windows, and no SAML, SCIM or
workload identity. This milestone completes tenancy and identity without
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

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Principals | Members, management tokens, API keys, bootstrap token ([security](../security.md#principals)) | Keys, users, teams, organizations, [customers](https://docs.litellm.ai/docs/proxy/customers) |
| Limits | Key requests and tokens per minute, concurrency, daily and monthly cost; shared [budget groups](../access.md#projects-and-budget-groups) | Key, user, team, organization, customer, tag and per-model budgets with arbitrary durations ([budgets](https://docs.litellm.ai/docs/proxy/users)) |
| Identity | OIDC, push provisioning, local passwords ([access](../access.md)) | SSO, [SAML](https://docs.litellm.ai/docs/proxy/saml_sso), [SCIM](https://docs.litellm.ai/docs/proxy/identity_provisioning), [JWT auth](https://docs.litellm.ai/docs/proxy/token_auth) |
| Network controls | Trusted-proxy address resolution | [IP allowlists](https://docs.litellm.ai/docs/proxy/ip_address) |

## Scope

### M4.1 End users

- **Identification.** A key policy, `end_user_source`, names where the end-user
  identifier comes from: the `X-OLP-End-User` header, or the dialect's native
  field (OpenAI `safety_identifier` or `user`, Anthropic `metadata.user_id`).
  The value must be a bounded machine token. Native fields are forwarded
  unchanged on strict routes; the header is never forwarded.
- **Storage.** OLP stores only an HMAC digest of the identifier under a new
  `end_user` digest purpose. Reports group by digest, and an operator who knows
  an identifier can look it up by computing the same digest through the API.
  Raw identifiers never reach PostgreSQL, Valkey, logs or traces.
- **Policy.** End-user policies attach to a project or key. They set default
  limits for every distinct end user (requests and tokens per minute,
  concurrency, daily and monthly cost), overrides for specific end users, and
  a block list. Counters live in Valkey under the end user's digest and
  reconcile like key budgets.
- **Reporting.** Usage reports gain an `end_user` dimension.

### M4.2 Budget hierarchy, windows and templates

- **Levels.** Organization (M4.5), project and installation budgets join key,
  budget-group and end-user budgets. A request must satisfy every level on its
  path, which is how LiteLLM documents its hierarchy too.
- **Per-route limits.** A key may carry `route_limits` keyed by route slug,
  with request, token and cost limits for that route alone.
- **Windows.** Budgets support `day`, `week` (ISO weeks starting Monday) and
  `month` windows in an installation time zone, `budgets.time_zone` (an IANA
  name, default `UTC`). Windows remain calendar-aligned and are computed from
  Valkey server time, as today.
- **Templates.** A limit template is a named, project-scoped set of limits and
  budgets. Keys, end-user policies and budget groups reference a template, and
  editing it updates every member at the next authority refresh.
- **Temporary increases.** An increase raises one budget by an amount until an
  expiry time, records a reason, and writes an audit record. It cannot outlive
  its window.
- **Attribution budgets.** Budgets keyed by an attribution label and value
  within a project, for example `team=core`, cap allocated spend. Because
  callers choose labels, a key may pin labels server-side with
  `attribution_defaults`; pinned labels cannot be overridden, which makes label
  budgets enforceable. Unpinned label budgets are documented as allocation caps,
  not security boundaries.
- Budget semantics do not change: cost budgets fail closed, exhaustion returns
  `429 budget_exhausted`, and missing snapshots return 503 until reconciliation
  initializes them.

### M4.3 Access ergonomics

- **Route groups.** Named, project-scoped sets of routes that key allowlists
  may reference. Membership changes reach gateways through key authority.
- **Key lifecycle.** Rotation gains an overlap period during which the old
  secret stays valid. Keys may declare a rotation interval, and the worker
  raises `key.expiring` notifications ([M5.4](m05-observability.md#m54-alert-channels-and-events))
  before expiry or a due rotation. OLP never generates or transmits a secret
  without a caller present: rotation stays an explicit, idempotent API call.
- **Network allowlists.** Keys may carry `allowed_cidrs`, and the installation
  may restrict management traffic to configured CIDRs. Client addresses come
  from the existing `OLP_TRUSTED_PROXY_CIDRS` resolver.
- **Required attribution.** Key and project policies may require attribution
  labels; requests without them fail with `400 missing_attribution` before
  dispatch.
- **Request size.** Routes may lower the installation body limits for their own
  requests.

### M4.4 Workload identity

Installation owners register trusted JWT issuers: an issuer URL, its JWKS
endpoint fetched through identity egress, accepted audiences and algorithms
(RS256, ES256 or EdDSA), and claim mappings to a project, a limit template,
route groups, scopes and an end-user claim. A gateway then accepts
`Authorization: Bearer <jwt>` from that issuer:

- Signatures, `iss`, `aud`, `exp` and `nbf` are verified on every request
  against cached keys, with a maximum token lifetime.
- Each distinct issuer and subject becomes a principal whose limits come from
  its template, accounted under a digest of the pair.
- Disabling an issuer or a key ID reaches gateways through key authority, with
  the same freshness rules as key revocation.

This covers Kubernetes service account tokens, GitHub Actions and other CI
OIDC tokens, and cloud workload identity federation without distributing static
keys. Issuer administration requires the `access` operation.

### M4.5 Enterprise identity

- **SAML 2.0.** A service provider for console sign-in with signed assertions,
  metadata import, and the same role, project and owner-protection rules as
  OIDC. The service provider's signing key uses a new `saml_key` seal purpose.
- **SCIM 2.0.** `/scim/v2/Users` and `/scim/v2/Groups` (RFC 7643 and RFC 7644)
  built on the existing [push provisioning](../access.md#management-tokens-and-provisioning)
  semantics, authenticated by management tokens with the `access` scope.
  Groups map to roles and project memberships.
- **Multi-factor authentication.** TOTP (RFC 6238) and WebAuthn passkeys for
  local sign-in, with an owner-enforced policy and recovery codes. The
  deployment guide currently defers MFA; this closes it.
- **Organizations.** An organization contains projects. Organization managers
  administer the projects, memberships, keys, budgets and notification rules
  inside it; installation-wide roles are unchanged. The policy table in
  [security](../security.md#management-authorization), the authorization golden
  file and both sweeps gain the organization scope.

### M4.6 Caller-supplied provider credentials

A connection may declare `credential_source: caller`. Requests on routes
targeting it supply the provider credential in `X-OLP-Provider-Credential`:

- The endpoint, profile and certification come from the operator. Only the
  secret comes from the caller, so no caller can choose a destination.
- The credential lives only in request memory. It is redacted from every
  upstream error path, never logged, persisted or forwarded to another target,
  and never cached in a connection pool shared with other credentials.
- Certification and slot validation use an operator-held probe credential.
- Attempts record `credential_source: caller`. Usage is accounted as usual,
  and the route can opt out of cost budgets because the caller pays the
  provider.

## Change map

| Change | Start here |
| --- | --- |
| End users, templates, route groups, key policies | `internal/access/`, `console/src/lib/features/access/` |
| Hierarchical budgets and windows | `internal/limits/`, `internal/usage/` |
| JWT issuers and workload principals | `internal/access/`, `internal/gateway/credentials.go` |
| SAML, SCIM, MFA, organizations | `internal/access/`, [`openapi/management.json`](../../openapi/management.json) |
| Caller-supplied credentials | `internal/runtime/credentials.go`, `internal/egress/` |

## Decisions to settle

1. Organization depth: one level above projects, or arbitrary nesting
   (recommended: one level, which matches LiteLLM's model and keeps
   authorization decidable by `Principal.Project` plus one ancestor).
2. Whether end-user digests are installation-wide or per project (recommended:
   per project, so the same identifier in two projects cannot be correlated).
3. Window migration: whether existing daily and monthly budgets adopt the
   installation time zone when it changes (recommended: changes apply from the
   next window boundary).

## Exit criteria

- [ ] End-user limits and budgets hold across two gateways under concurrent
      load, and no raw end-user identifier appears in PostgreSQL, Valkey, logs
      or traces.
- [ ] A request is refused when any level of its budget hierarchy is exhausted,
      and reports show which level refused it.
- [ ] Weekly windows and non-UTC time zones reset at the correct boundary,
      including across daylight-saving transitions.
- [ ] Workload JWTs from a test issuer authenticate, survive signing-key
      rotation, and stop working within the authority freshness bound after the
      issuer is disabled.
- [ ] SAML sign-in, SCIM provisioning and MFA pass Chromium journeys; SCIM
      passes a protocol conformance suite.
- [ ] The authorization and isolation sweeps cover organizations and every new
      operation.
- [ ] Caller-supplied credentials never appear in any persisted record, log or
      error body in integration tests.
- [ ] The [parity matrix](parity.md) identity and budget rows are `Parity` or
      better.
