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

| | OLP today | LiteLLM reference |
| --- | --- | --- |
| Principals | Members, management tokens, API keys, bootstrap token ([security](../security.md#principals)) | Keys, users, teams, organizations, [customers](https://docs.litellm.ai/docs/proxy/customers) |
| Limits | Key requests and tokens per minute, concurrency, daily and monthly cost; shared [budget groups](../access.md#projects-and-budget-groups) | Key, user, team, organization, customer, tag and per-model budgets with arbitrary durations ([budgets](https://docs.litellm.ai/docs/proxy/users)) |
| Identity | OIDC, push provisioning, local passwords ([access](../access.md)) | SSO, [SAML](https://docs.litellm.ai/docs/proxy/saml_sso), [SCIM](https://docs.litellm.ai/docs/proxy/identity_provisioning), [JWT auth](https://docs.litellm.ai/docs/proxy/token_auth) |
| Network controls | Trusted-proxy address resolution | [IP allowlists](https://docs.litellm.ai/docs/proxy/ip_address) |

## Scope

### M4.1 End users

**Implemented.** Key-controlled identification, project-scoped digests, authorized
lookup and the `end_user` reporting dimension are available in the API and console.
Key and project policies enforce per-digest RPM, TPM, concurrency and daily/monthly
cost budgets, complete overrides and blocking. Configuration promotion carries
project defaults, preserves local digest controls and requires `keys` when changing
policy. Delegated project managers can edit policies; other members have read-only
controls. The persistence inventory is in [access control](../access.md#end-user-identity).

Race-enabled service tests exercise each limit under concurrent load across two
gateways, exact settlement and reconciliation, project spend shared across keys,
refunds and policy changes. The accounting pipeline test inspects PostgreSQL,
Valkey, captured logs and exported request/attempt traces for raw identifiers.
Boundary coverage includes strict generation, image/speech/transcription, all four
Bedrock surfaces, durable video, Gemini interactions across restart, subscription
attempts/refusals, and background Responses attributed to their creator. Established
Realtime and Gemini Live sessions close after a block reaches current authority.
Strict retained Responses store serving bindings without source requests; native
identification preserves wire bytes and content encoding. Subscription admission
retains its existing dollar-budget exemption and enforces the other dimensions.
Chromium/axe covers policy editing, delegated access and digest diagnostics.

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

**Progress:** Implemented. Key and project attribution requirements and pinned defaults
are implemented, including promotion and resolved accounting. Installation and
project aggregate day/week/month caps now use stable accounting owners, include
system-origin spend, reserve/refund independently, preserve history across policy
changes, expose current usage, and round-trip through configuration promotion.
Focused service checks cover historical initialization, shared-key spending,
project/installation enforcement, system accounting and promotion permissions.
Per-key `route_limits` now enforce independent route RPM/TPM/concurrency and
USD caps through the shared admission/accounting path, preserve history and
rotation identity, and have API/console controls. Focused checks cover two-gateway
rates, historical spend, key isolation and clearing/re-enabling caps.
Project limit templates now supply independent ceilings to keys, end-user
policies and budget groups, propagate through authority refresh, protect live
references, and round-trip through permission-checked promotion. Focused tests
cover shared policies with separate counters, group enforcement and promotion.
Weekly cost ceilings now propagate through caller policies, templates, budget
groups, aggregate budgets, route limits, reporting, threshold notifications and
console controls. Valkey enforces ISO-Monday boundaries and refuses incomplete
weekly evidence. Focused tests cover concurrent reservations, month/year overlap,
historical reconstruction before and after retention, and independent key/group
ceilings. Calendar arithmetic has focused DST, offset and skipped-date tests;
installation time-zone selection now schedules each period at its next boundary.
Valkey validates balance intervals with its own clock; reporting, notifications
and cost retention share the durable calendar. Focused tests cover DST,
repeated/skipped midnight, quarter-hour retained spend, promotion permissions
and protection against stale UTC snapshots. Reconstruction includes the full
union of all three periods when independent transitions leave a day starting
before both the current week and month.
Project attribution budgets now apply independent day/week/month caps to resolved
label/value pairs, share counters across keys/gateways, retain historical spend,
and include labeled system work. API/console controls, per-pair reporting and
permission-checked promotion are implemented. Focused tests cover all windows,
multiple matching caps, refunds, project isolation, clearing/re-enabling,
retention and promotion. Unpinned labels remain allocation controls, with
required/pinned attribution available to constrain callers.

Temporary increases now add exact amounts to one caller/aggregate cost ceiling,
require a reason and management authorization, cap expiry at the original
calendar boundary, and enforce time/window identity in Valkey without waiting
for authority refresh. API/console creation, replay recovery, revocation, audit
and effective key/group reporting are implemented. Focused checks cover all
three periods, target validation, conditional scopes, bounds, precise addition,
expiry, revocation and preserved permanent policies. Organization caps now use
stable owners and the same admission chain, sum current projects and system work,
and preserve spend through retention, disabling and promotion. Focused service
checks exercise organization exhaustion, earlier-reservation refunds, cross-project
history, inherited/direct membership, scope revocation and both management sweeps.
The final hierarchy audit also verifies response refusal levels and bounded
`budget_boundary` metadata through persistence and Request Explorer, without
retaining identifiers or changing native failure codes.

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

**Implemented.** API-key `allowed_cidrs` is implemented in the contract, gateway
and console. Unit/service tests cover IPv4/IPv6, trusted-proxy spoofing, public
scopes, clearing restrictions, subscription policy refresh and established
Realtime/Gemini Live sessions. Deployment management CIDRs cover the console,
public authentication and secured management operations before admission. Real
process tests verify trusted-proxy resolution, continued authentication and
unaffected inference/private probes; Helm rendering and Chromium/axe cover the
deployment setting and console notice. Required attribution and pinned defaults
are implemented across key/project policies, accounting and the delegated
console. Service tests cover both boundaries, authority refresh, current-policy
subscription admission, promotion permissions and deferred creator accounting.
Chromium/axe verifies delegated editing, read-only access and diagnostics.
Published ordinary and subscription routes now carry `max_body_bytes`, with
encoded/decoded HTTP, multipart epilogue and client WebSocket enforcement.
Configuration promotion and both route editors preserve the bound. Plain/gzip,
native identity, fallback, multipart epilogue, retained-response, realtime and
subscription refusal tests pass, alongside draft/revision/promotion checks and
Chromium/axe. The unconfigured path retains its zero-allocation gate.
Project-scoped route groups now bind key allowlists through cached authority,
including subscription admission and model discovery. Membership updates,
missing-group refusal (including video listings/cursors), simulation parity,
realtime closure, promotion permissions and authorization isolation have service
coverage; delegated console management and key references
are included. Explicit idempotent rotation supports a bounded overlap, sharing
current policy and rate/token/concurrency counters across every version. Declared
rotation intervals and key expiry produce metadata-only `key.expiring` reminders;
superseded schedules are cancelled and no secret is created by the worker.
Service tests cover replay, overlap bounds/expiry, revocation, two gateways,
in-flight leases, settled tokens, realtime closure, deduplicated reminders and
rule reassignment. Chromium verifies overlap selection, recovery of a lost
rotation response with the same request identity, rule creation and accessibility.

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

**Progress:** Implemented. Owner-managed issuer APIs and console, bounded
RS256/ES256/EdDSA verification, identity-egress key caching/rotation, explicit claim
mappings, digest-only stable principals, shared template/group admission and
accounting, and project-name-based configuration promotion are wired end to end.
Focused tests cover signature/security claims, JWKS rotation and outages,
cross-gateway counters, offline disablement, secretless-principal protections,
raw-token/subject exclusion, permission-gated promotion with destination-local
identities, renewed-token retained Responses, native realtime and idle subscription
expiry, current subscription authority, and management authorization/isolation.
Replay-safe console submission and affected component/type checks pass. Long
release qualification remains a human review handoff.

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

**Progress:** Implemented. Organizations and delegated project management have
one-level ownership, inherited scope, unchanged installation roles, bounded token
authority, organization budgets/increases, promotion, console controls and audited
ETag writes. Authorization goldens and both sweeps include organizations; focused
checks cover delegated management, read-only controls, last-manager protection,
revocation, direct-grant preservation and cross-project accounting/retention.
SCIM is implemented with bearer-only Access authorization, discovery, bounded
filter/search/sort/projection, atomic PATCH, ETags, stable identity lifecycle,
additive group role/project grants, local takeover and usable-owner protection.
The owner console and desired-state mapping promotion preserve local identities
and memberships. Focused protocol/service tests, authorization/isolation sweeps,
session/owner lifecycle tests and packaged Chromium/axe pass. External directory
conformance qualification is handed to the human reviewer. Local MFA is now
implemented: proved TOTP/WebAuthn enrollment, replay-resistant counters,
one-use recovery codes, owner policy/bootstrap, session rotation, recent-auth
factor checks, explicit offline recovery and policy promotion. Focused service
tests cover cryptographic verification, revoked/expired proof, unchanged OIDC
flows, password-session transitions, owner-only policy and authorization/isolation.
Chromium with a virtual authenticator covers enrollment and password sign-in
remaining unauthenticated until WebAuthn succeeds, plus axe. SAML now provides
owner-managed metadata import, independently signed solicited assertions, exact
issuer/audience/request/recipient/time checks, sealed SP keys and browser-bound
POST receipt/GET completion. Profile linking and fresh proof preserve independent
identity ownership, MFA factor-management and last-owner protections. Promotion
remaps public trust while keeping signing material and identities local. Signed
service fixtures cover tampering, malformed structures, replay, wrong-browser
completion, role loss, unlink/recovery and permission sweeps. Packaged Chromium
imports metadata and completes a real cross-site signed login with no session
from the POST; configuration/profile axe checks pass. Production IdP and external
SCIM conformance qualification remain with the human reviewer.

- **SAML 2.0.** A service provider for console sign-in with signed assertions,
  metadata import, and the same role, project and owner-protection rules as
  OIDC. The service provider's signing key uses a new `saml_key` seal purpose.
- **SCIM 2.0.** `/scim/v2/Users` and `/scim/v2/Groups` (RFC 7643 and RFC 7644)
  built on the existing [push provisioning](../access.md#management-tokens-and-provisioning)
  semantics, authenticated by management tokens with the `access` scope.
  Groups map to roles and project memberships.
- **Multi-factor authentication.** TOTP (RFC 6238) and WebAuthn passkeys for
  local sign-in, with an owner-enforced policy and recovery codes. The
  deployment guide now documents policy, relying-party origins and recovery.
- **Organizations.** An organization contains projects. Organization managers
  administer the projects, memberships, keys, budgets and notification rules
  inside it; installation-wide roles are unchanged. The policy table in
  [security](../security.md#management-authorization), the authorization golden
  file and both sweeps gain the organization scope.

### M4.6 Caller-supplied provider credentials

**Progress:** Implemented. Published connection source, stateless built-in
credentials, isolated transports, target binding, error redaction and source-only
attempt metadata are wired through generation, native/realtime, retained-resource
and media paths. Operator probes remain separate. Caller-driven video lifecycle
and immediate cleanup never substitute stored credentials; autonomous maintenance
excludes these jobs. Caller-paid route policy preserves rates/concurrency and
priced usage while omitting eligible attempts from all USD budgets. The immutable
fact/rollup flag survives retention and deferred accounting. Source, exemption,
console controls and desired-state promotion are complete. Focused tests cover
refusal/redaction, PostgreSQL/Valkey/log/trace privacy, static-auth cache exclusion,
connection/step isolation, video, mixed paid/exempt retention, historical budget
reconstruction, route graph constraints and promotion. A packaged Chromium/axe
journey saves/publishes the exemption, serves caller traffic and proves rate
limits remain active. Production qualification is handed to the reviewer.

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
      handed to the human reviewer; this does not assert those external results.
- [x] The authorization and isolation sweeps cover organizations and every new
      operation.
- [x] Caller-supplied credentials never appear in any persisted record, log or
      error body in integration tests.
- [x] The [parity matrix](parity.md) identity and budget rows are `Parity` or
      better.
