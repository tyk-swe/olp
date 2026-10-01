# M4: Tenancy, identity and budgets

| Status | Depends on | Integrates with | Unlocks |
| --- | --- | --- | --- |
| Planned | None | [M3](m03-routing-resilience.md) (budget fallbacks), [M7](m07-guardrails.md) (organization-scoped policies) | [M5](m05-observability.md), [M6](m06-cost-management.md), [M10](m10-agent-gateway.md), [M11](m11-operator-ecosystem.md) |

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
  budget group, key and end user), with per-route and per-access-group limits,
  calendar windows in a configured time zone, reusable templates and audited
  temporary increases.
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
| Principals | Members, management tokens, API keys, bootstrap token ([security](../security.md#principals)) | Keys, users, teams, organizations, [projects](https://docs.litellm.ai/docs/proxy/project_management), [customers](https://docs.litellm.ai/docs/proxy/customers) |
| Limits | Key requests and tokens per minute, concurrency, daily and monthly cost; shared [budget groups](../access.md#projects-and-budget-groups) | Key, user, team, organization, customer, tag and per-model budgets with arbitrary durations ([budgets](https://docs.litellm.ai/docs/proxy/users)) |
| Identity | OIDC with claim-to-role mapping, push provisioning, local passwords; project membership is managed locally ([access](../access.md)) | SSO, [SAML](https://docs.litellm.ai/docs/proxy/saml_sso), [SCIM](https://docs.litellm.ai/docs/proxy/identity_provisioning), [JWT auth](https://docs.litellm.ai/docs/proxy/token_auth), [OAuth 2.0 introspection](https://docs.litellm.ai/docs/proxy/oauth2) |
| Key lifecycle | On-demand rotation that replaces the secret at once, with no overlap | Scheduled rotation with a grace period, delivered to a secret manager |
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
  with request, token and cost limits for that route alone. An exhausted
  per-route cost limit can start a `budget`
  [fallback](m03-routing-resilience.md#m31-fallback-routes) once M3.1 ships.
- **Access-group budgets.** A budget may attach to an access group (M4.3), so
  every key that reaches a route in the group draws from one pool.
- **Windows.** Budgets support `day`, `week` (ISO weeks starting Monday) and
  `month` windows in an installation time zone, `budgets.time_zone` (an IANA
  name, default `UTC`). Calendar windows stay calendar-aligned and are computed
  from Valkey server time, as today. Subject to decision 4, a budget may
  instead use a fixed duration: a whole number of hours or days, counted from
  the budget's creation, which is how LiteLLM's reset periods work.
- **Templates.** A limit template is a named, project-scoped set of limits and
  budgets. Keys, end-user policies and budget groups reference a template, and
  editing it updates every member at the next authority refresh.
- **Temporary increases.** An increase raises one budget by an amount until an
  expiry time, records a reason, and writes an audit record. It cannot outlive
  its window.
- **Attribution budgets.** Budgets keyed by an attribution label and value
  within a project, for example `team=core`, cap allocated spend. Because
  callers choose label values today, a key may pin labels server-side with a
  new `attribution_defaults` policy; pinned labels cannot be overridden, which
  makes label budgets enforceable. Unpinned label budgets are documented as
  allocation caps, not security boundaries.
- Budget semantics do not change: cost budgets fail closed, a budget refuses
  work once accrued spend reaches it, exhaustion returns
  `429 budget_exhausted`, and missing snapshots return 503 until
  reconciliation initializes them.

### M4.3 Access ergonomics

- **Access groups.** Named, project-scoped sets of routes that key allowlists
  may reference. Membership changes reach gateways through key authority.
  When [M10](m10-agent-gateway.md) ships, a group may also hold toolsets and
  agents, so one group grants models, tools and agents together.
- **Key lifecycle.** Rotation gains an overlap period during which the old
  secret stays valid. Keys may declare a rotation interval, and the worker
  flags the key before expiry or a due rotation. The `key.expiring`
  notification itself is delivered by
  [M5.4](m05-observability.md#m54-alert-channels-and-events). OLP
  never generates a secret that nobody receives: rotation stays an explicit,
  idempotent API call until
  [M11.3](m11-operator-ecosystem.md#m113-kms-and-external-secret-stores) lets
  a key name a secret store as its rotation destination.
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
endpoint fetched through [identity egress](../security.md#egress), accepted
audiences and algorithms (RS256, ES256 or EdDSA), and claim mappings to a
project, a limit template, access groups, scopes and an end-user claim. A
gateway then accepts `Authorization: Bearer <jwt>` from that issuer:

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
  metadata import, and the same role-mapping and owner-protection rules as
  OIDC. The service provider's signing key uses a new `saml_key` seal purpose.
- **Group-to-project mapping.** OIDC maps claims to installation roles only,
  and project membership is managed locally. OIDC, SAML and SCIM groups gain
  mappings to project and organization memberships, under the same
  owner-protection rules.
- **SCIM 2.0.** `/scim/v2/Users` and `/scim/v2/Groups` (RFC 7643 and RFC 7644)
  built on the existing [push provisioning](../access.md#management-tokens-and-provisioning)
  semantics, authenticated by management tokens with the `access` scope.
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

## Non-goals

- Storing raw end-user identifiers, or letting a caller-chosen label act as a
  security boundary without a server-side pin.
- Caller-chosen upstream destinations. Callers may bring a credential, never a
  base URL.
- Budget windows shorter than one hour. Rate limits cover short intervals.
- Replacing installation roles. Organizations add a scope; they do not add
  roles.

## Data and secrets

| Data | Where | Retention | Purpose |
| --- | --- | --- | --- |
| End-user digests, policies and counters | PostgreSQL; Valkey counters | Usage retention; the budget window | New digest purpose `end_user` |
| Workload principals (issuer and subject) | PostgreSQL usage records | Usage retention | New digest purpose `workload_principal` |
| Trusted issuers and cached JWKS | PostgreSQL; gateway memory | Until removed; the key-authority freshness bound | None; public keys |
| SAML service-provider signing key | PostgreSQL, sealed | Until rotated | New seal purpose `saml_key` |
| TOTP secrets and WebAuthn credentials | PostgreSQL, sealed; recovery codes as digests | Until the factor is removed | New seal purpose `mfa_secret` |
| Templates, access groups, organizations, temporary increases | PostgreSQL; key authority | Until deleted; increases until their expiry | None |
| Caller-supplied provider credentials | Request memory only | Never stored | None |

## Change map

| Change | Start here |
| --- | --- |
| End users, templates, access groups, key policies | `internal/access/`, `console/src/lib/features/access/` |
| Key authority and authentication | `internal/runtime/manager.go`, `internal/gateway/credentials.go` |
| Hierarchical budgets and windows | `internal/access/budgets.go`, `internal/limits/`, `internal/usage/` |
| JWT issuers and workload principals | `internal/access/`, `internal/runtime/manager.go` |
| SAML, SCIM, MFA, organizations | `internal/access/`, [`openapi/management.json`](../../openapi/management.json) |
| Caller-supplied credentials | `internal/runtime/credentials.go`, `internal/egress/` |
| Desired state for templates, groups and issuers | `internal/configuration/artifact.go` |

## Decisions to settle

1. Organization depth: one level above projects, or arbitrary nesting. LiteLLM
   documents four levels (organizations, teams, projects, keys); OLP reaches
   the same depth with organizations, projects, budget groups and keys
   (recommended: one organization level, which keeps authorization decidable
   by `Principal.Project` plus one ancestor).
2. Whether end-user digests are installation-wide or per project (recommended:
   per project, so the same identifier in two projects cannot be correlated).
3. Window migration: whether existing daily and monthly budgets adopt the
   installation time zone when it changes (recommended: changes apply from the
   next window boundary).
4. Whether to add fixed-duration windows such as `30d`, which LiteLLM offers
   (recommended: yes, in whole hours or days; calendar windows alone are not
   equivalent, so declining this means splitting the parity row and marking
   fixed durations `Excluded`).
5. Whether to accept opaque OAuth 2.0 tokens through introspection (RFC 7662)
   beside JWTs (recommended: yes, as an issuer type with a bounded positive
   cache; otherwise the parity row becomes `Excluded`).

## Exit criteria

- [ ] **M4.1** End-user limits and budgets hold across two gateways under
      concurrent load, and no raw end-user identifier appears in PostgreSQL,
      Valkey, logs or traces.
- [ ] **M4.2** A request is refused when any level of its budget hierarchy is
      exhausted, including a per-route limit and an access-group budget, and
      reports show which level refused it.
- [ ] **M4.2** Weekly windows and non-UTC time zones reset at the correct
      boundary, including across daylight-saving transitions, and a
      fixed-duration window resets exactly one duration after it began.
- [ ] **M4.2** Editing a template changes every member's limits within the
      authority freshness bound, a temporary increase ends at its expiry and is
      audited, and a pinned attribution label cannot be overridden by a caller.
- [ ] **M4.3** A key reaches exactly the routes of its access groups; during a
      rotation overlap both secrets authenticate and afterwards only the new
      one does; a request from outside `allowed_cidrs` is refused.
- [ ] **M4.4** Workload JWTs from a test issuer authenticate, survive
      signing-key rotation, and stop working within the authority freshness
      bound after the issuer is disabled.
- [ ] **M4.5** SAML sign-in, SCIM provisioning and MFA pass Chromium journeys;
      SCIM passes a protocol conformance suite; group mappings grant and revoke
      project membership.
- [ ] **M4.5** The authorization and isolation sweeps cover organizations and
      every new operation.
- [ ] **M4.6** Caller-supplied credentials never appear in any persisted
      record, log or error body in integration tests.
- [ ] **M4.2, M4.3, M4.4** Templates, access groups, end-user policies and
      issuers round-trip through configuration export, plan and apply.
- [ ] The [parity matrix](parity.md) identity and budget rows are `Parity` or
      better.
