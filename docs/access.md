# Access control

The control plane manages installation setup, identity, projects, API keys,
management tokens, provisioning, settings, and metadata-only audit records.
Administrators belong to one trusted installation. See
[deployment](deployment.md) for installation,
[configuration](configuration.md#file-based-secrets) for secret files, and
[operations](operations.md#master-key-rotation-and-recovery) for master-key rotation.

## Attribution requirements and pinned labels

Keys accept `required_attribution_keys` and `attribution_defaults`. Project
managers with `keys` permission can edit the same policy through
`GET`/`PUT /api/v1/projects/{project_id}/attribution-policy` or **Access → Project
policies**. Reads require project visibility; writes also require its current
quoted ETag in `If-Match`. The PUT body requires `policy`; explicit null removes
the project policy. Key PATCH accepts null or empty values to clear its controls.

Requirements accumulate across key and project policies. Defaults are pinned
operator values: they fill omitted labels, and a caller may repeat the same
value but cannot replace it. A conflicting caller value returns
`400 pinned_attribution_override`. Conflicting key/project pins or more than four
resolved labels fail with `400 invalid_attribution`. Missing required labels
return `400 missing_attribution` before dispatch. Unpinned caller labels still
need permission in the key's `allowed_attribution_keys`; requiring a label does
not expand that allowlist. Configure the allowlist or a matching pin when adding
requirements. Requirements and pins together may name at most four distinct
keys at each boundary; requests must also fit the four-label accounting limit.
Existing machine-token syntax and size bounds apply.

These controls apply to generation, media, retained-resource operations,
Bedrock, realtime and subscription code operations. Model discovery is exempt.
Established OpenAI Realtime/Gemini Live sessions close at the next authority
check if their original labels no longer satisfy policy. Subscription errors
use the existing `code_` prefix, for example `code_missing_attribution`; a policy
change during durable admission returns `409 code_attribution_changed` before
dispatch. Retry after the gateway refreshes authority.

Resolved labels travel through the existing request/attempt accounting,
background completion and hourly retention paths. Subscription attempts store
and display them in their separate diagnostics ledger. Labels are ordinary
content-free metadata, not authentication of a person or team. Use bounded
machine labels, never secrets or prompt content. Policies live as plaintext
JSON in key/project records. Inference labels follow request/fact/hourly
retention; subscription labels remain with the separate durable code ledger.
No new cryptographic purpose or secret storage is introduced.

Project policies round-trip through configuration export, plan and apply.
Changing them requires both `configure` and `keys`; an unchanged apply requires
only `configure`. API-key policies remain installation-local. Attribution spend
caps are separate M4.2 work; required labels and pins alone do not impose a
spend budget.

## API-key network restrictions

An API key may set `allowed_cidrs` to at most 64 IPv4 or IPv6 CIDR ranges.
Configure **Allowed client networks** in the key form, or supply an array such
as `["192.0.2.0/24", "2001:db8::/32"]` through the key API. Omitted, null or
empty means unrestricted. A prefix includes its entire network; host bits do
not narrow it. Equivalent duplicate ranges are refused. Express IPv4-mapped
IPv6 ranges as IPv4; mapped client addresses match the corresponding IPv4 range.

The gateway resolves the client using `OLP_TRUSTED_PROXY_CIDRS`. An untrusted
peer cannot use `X-Forwarded-For` to gain access. A disallowed or unparseable
client address receives `403 ip_not_allowed` before body parsing or provider
dispatch, including model listing, media, retained resources and realtime
handshakes. Policy changes follow key-authority freshness; established OpenAI
Realtime and Gemini Live sessions check their original client address every
five seconds. Subscription generations also check fresh ledger policy before
dispatch. Its refusal code is `code_ip_not_allowed`.

Network ranges are ordinary key policy data, with the existing key lifecycle
and authorization. API keys remain installation-local during configuration
promotion. Management-traffic CIDR restrictions are tracked separately in M4.3.

## Management network restrictions

Set `OLP_MANAGEMENT_ALLOWED_CIDRS` (or `--management-allowed-cidrs`) to a
comma-separated list of up to 64 IPv4 or IPv6 client networks. Helm exposes
`config.managementAllowedCidrs` on control pods. Empty permits any network;
malformed ranges, IPv4-mapped IPv6 ranges, and nonempty lists without a range
fail startup. Host bits are masked to the declared network. Use IPv4 syntax
for mapped addresses.

The public listener uses the same `OLP_TRUSTED_PROXY_CIDRS` resolver as API
keys. Forwarded addresses from untrusted peers cannot grant access. A denied
client receives `403 management_ip_not_allowed` before authentication, body
parsing or management admission. This covers the console and its assets,
setup, sign-in, invitations, identity callbacks, the published OpenAPI document,
all secured management operations and unknown management paths. Allowed clients
still need the usual sessions/tokens, scopes, project membership and CSRF checks.
Inference routes and the separate private health/metrics listener retain their
existing controls.

The policy is immutable for a running process. Update the deployment and roll
out control processes to change it; include the networks operators and identity
provisioning clients actually use. If a range locks out management, correct the
deployment configuration through the host or orchestrator and restart control
processes. No management API bypass is provided. These process settings stay
outside configuration export/plan/apply, alongside listener addresses and trusted
proxies. Settings shows an informational notice while restrictions are active,
and `/api/v1/auth/capabilities` reports `management_network_restricted`.

## Route groups

Projects can define named route sets through **Project policies → Route groups**
or `GET`/`PUT /api/v1/projects/{project_id}/route-groups`. Reads require `read`
and project visibility; replacement requires `keys`, project-manager authority,
and the quoted project ETag in `If-Match`. The body replaces the complete map:

```json
{"groups":{"production":["chat","embeddings"],"suspended":[]}}
```

Each project permits up to 64 groups, each containing up to 100 unique route
slugs. Names use the route-slug syntax: 1–100 lowercase letters, digits, dots,
underscores or hyphens, beginning with a letter or digit. The management body
size bound still applies. Ordinary and subscription routes are supported.
Unknown slugs may be declared ahead of route creation; known routes must belong
to the same project, and authority always enforces the project's boundary.

A project key's `allowed_route_groups` references existing group names. Its
allowlist grants the union of explicit `allowed_routes` and referenced group
members. Only when **both lists are empty** does the key allow every route in its
project. An empty or missing referenced group grants nothing. Removing a group
leaves key references intact and fails closed; recreating the same name restores
its members. Clearing the key's references is a separate, explicit key edit.

Group edits advance key authority without republishing routes or rotating keys.
Gateways resolve membership during the existing authority refresh; inference
and model discovery use the same immutable lookup. Video-job listings and cursors
also enforce it, and routing simulation resolves the same groups. Established realtime sessions
close when refreshed authority removes their route. Subscription admission also
checks current group membership in its durable transaction.

Group maps are ordinary project metadata in PostgreSQL, with audited changes;
no new secret or cryptographic purpose is introduced. Configuration export,
plan and apply include `projects[].route_groups`. Omitted, null or empty maps
remove destination groups. Changing groups through promotion requires `keys` in
addition to `configure`; unchanged groups do not add that requirement. API keys
and their group references remain installation-local.

## API-key rotation and reminders

Rotation is an explicit `POST /api/v1/api-keys/{id}/rotate`, authorized like a key
edit and requiring `If-Match` and `Idempotency-Key`. Its optional
`overlap_seconds` is an integer from 0 to 86400. Zero, including an omitted
value, ends all previous overlaps when gateways refresh. A positive value keeps
the immediately preceding secret valid until the earlier of that deadline and
key expiry. Up to eight previous secrets can overlap; another positive rotation
returns `409 key_overlap_limit` until one expires or a zero-overlap rotation ends
them. Later rotations and idempotent replays never extend an earlier deadline.
Replaying the same authorized request returns the same replacement secret and
`overlap_expires_at`, including after a lost response. The console reuses the
request identity when retrying a rotation.

Every version belongs to the same key. Current scopes, project/route groups,
network, attribution and end-user policy apply to all versions after authority
refresh. Rate/token counters and in-flight concurrency retain a stable identity
across rotation; cost accounting and retained resources continue to use the key
ID. Rotation cannot reset allowances. Revocation denies every version. Gateways
check an overlap's deadline on authentication even without another refresh;
OpenAI Realtime and Gemini Live close an expired version on their next existing
five-second authority check. Already admitted HTTP work keeps its original
accounting and lease until it finishes.

Key `rotation_interval_days` declares a reminder interval of 1–3650 days, each
24 hours, from creation or the last explicit rotation. Null clears the interval.
It does not change key expiry or generate secrets. Key reads expose
`rotation_due_at` and `active_overlaps` with public lookup IDs and deadlines.
API Keys lets operators set the interval and choose overlap when rotating.

Subscribe to `key.expiring` in Notifications using subject kind `api_key`, its
key ID, and a destination in the same project boundary. Installation-wide rules
require settings permission; project rules require keys permission and project
management. The worker evaluates once per minute, claiming one metadata-only
delivery per rule, key, reason (`expiry` or `rotation`), and date when that date is
within 24 hours or overdue. A new rotation or expiry date creates a new schedule.
Pending reminders superseded by an edit, rotation, rule reassignment or
revocation become `cancelled`; the worker checks current authority immediately
before claiming a send. A later change cannot withdraw an in-flight webhook.
Disabled destinations/rules do not send. Existing signing, retries and egress
validation apply; see [notification delivery](operations.md#notifications).
No reminder generates or transmits a secret.

Previous versions store only the existing purpose-separated `api_key` HMAC,
public lookup ID, key ownership and expiry in PostgreSQL. The notification worker
prunes expired/revoked overlaps; rotation also removes its expired versions.
Authentication enforces deadlines even if the worker is stopped. The stable
counter lookup lives with the key, and existing replay responses remain sealed
under `mutation_replay` for 24 hours. Reminder evidence contains IDs, display
names, reason and due date, retained with notification delivery history. Keys,
version digests and key-specific reminder rules remain installation-local;
rotation is never part of configuration promotion.

## End-user identity

API keys may require an end-user machine token with `end_user_source: "header"`
or `"native"`; `null` disables identification. Configure it in the API key
policy form or the key create/update API. Header mode reads exactly one
`X-OLP-End-User`. Native mode reads OpenAI `safety_identifier`, falling back to
`user` only when `safety_identifier` is absent, or Anthropic
`metadata.user_id`. Native mode requires a JSON request body; use the header
for uploads, resource reads, realtime and other dialects. Model listing does
not require end-user identification.

Identifiers must match `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`. Missing, ambiguous
or invalid identifiers are refused before provider dispatch without echoing
the value. Native fields retain their strict forwarding semantics; the header
is never forwarded. OLP records an HMAC-SHA256 digest under the `end_user`
purpose, separated by installation and project. The same identifier on two
keys in one project groups together, while different projects cannot correlate
it. Unassigned keys share the installation's unassigned boundary. The key holder
supplies the identity; the digest does not authenticate its subject. Key and
budget-group limits provide independent aggregate controls.

To find a known identifier in reports, POST `{"identifier":"customer-123"}` to
`/api/v1/api-keys/{api_key_id}/end-user`. The read-authorized endpoint checks
project visibility and returns `end_user_digest`; session callers supply CSRF
proof. Using a request body keeps the identifier out of URLs, and the lookup
creates no audit payload or mutation replay. Select **End user (digest)** in
Usage, or request `dimension=end_user` from the usage breakdown API. Requests
without an identity appear as `unidentified`.

The persistence inventory is the digest in request metadata events (Valkey's
existing stream retention), PostgreSQL `requests` (request retention, 30 days
by default), `attempt_usage_facts` (usage retention, 90 days by default), and
`attempt_usage_hourly` (retained aggregate history). Background Responses retain
the original caller's digest in their content-free pending accounting record until
settlement, even when another identified user polls for completion. Retained
strict-response contracts keep serving and prior-response bindings, without the
caller's source request or native user fields.
The lookup body and header remain in request memory. Digests require no seal
purpose; HMAC keys use the existing installation authentication key. API keys
and their policies remain installation-local, outside configuration promotion.

Keys accept an `end_user_policy`; projects expose it at
`GET`/`PUT /api/v1/projects/{project_id}/end-user-policy`. Project writes require
`keys` authorization, project-manager access, and the current project ETag.
Pass an explicit `{"policy":null}` to disable the project policy. The console
provides matching controls in key policies and on **Project policies**. The
project selector includes every visible project. Delegated project managers
with key-management permission can edit; other project members can inspect the
policy without editing. Installation owners can also use the selected project's
settings under Access.
Every inference key in a project with a policy must select an identifier source;
otherwise inference fails with `end_user_source_required`.

Policies contain `defaults`, digest-keyed `overrides`, and `blocked` digests.
Each limit set supports `requests_per_minute`, `tokens_per_minute`,
`max_concurrency`, `daily_cost_limit`, and `monthly_cost_limit`. An override
replaces all defaults at its boundary: omitted or null limits are unlimited.
Project and key policies apply independently, along with ordinary key and group
limits. A later refusal refunds earlier reservations. Blocks always win and
return `403 end_user_blocked`. OpenAI Realtime and Gemini Live recheck blocks
at their five-second authority check and close blocked sessions with WebSocket
policy-violation status. Enabling a policy closes previously unidentified sessions
at that same check; clients must reconnect with the required identity. Each override and block list allows up to 256
unique lowercase SHA-256 digests. Raw identifiers are never policy keys.

Rate and concurrency counters use the existing shared Valkey admission path.
Cost admission checks accrued spend and in-flight estimates; settlement and
reconciliation use the same attempt-level accounting as key budgets. Unknown
balances fail closed with 503. The first accounted request, including a refusal
before dispatch, reconstructs the identity's current-window spend from retained
usage and publishes the snapshot. Retrying succeeds only after that snapshot is
available and sufficient. Budget exhaustion returns `429 budget_exhausted`.
Day, ISO-Monday week and month windows use the [installation budget time zone](#budget-time-zone), defaulting to UTC. Existing unpriced
attempt and stale-price limitations apply equally to these cost budgets. Code
subscriptions use their existing allowance accounting: dollar budgets do not
apply, while end-user rate, token, concurrency and block policies still do.
Code attempt/refusal ledgers retain only the digest alongside their existing
metadata. Their management lists and console diagnostics filter by
`end_user_digest` (or `unidentified`); subscription usage remains separate from
inference USD reports. If an identity-source policy changes during admission and
would change the digest, the request fails with `409 code_end_user_changed`
before dispatch. Retry after the authority refresh with the current source.

The additional persistence inventory is digest-only policy JSON on keys/projects,
`end_user_accounts` for each key/project and digest, and current-window exact
cost totals in `end_user_cost_windows`. Accounts survive policy changes and are
removed with their owning boundary; reconciliation prunes old cost windows.
Admission counters use deterministic account IDs, never identifiers. Project
`end_user_defaults` participate in configuration export, plan and apply;
destination-local overrides and blocks are preserved and never exported.
API-key policies remain installation-local. Protocol/accounting boundary coverage
and the remaining tenancy requirements are tracked in [M4](roadmap/m04-tenancy-identity.md).

## Access and OIDC

Owners manage membership, OIDC, local-login availability, and other users'
sessions. Operators can read membership and manage keys/settings. Developers can
manage keys. Viewers can read configuration, key metadata, and usage, and with
an installation-wide scope also settings and audit records. Every user manages
their own profile and sessions. The
[operation table](security.md#management-authorization) lists exactly what each
role holds. The last usable owner cannot be
removed, disabled, or stranded by authentication configuration changes.

Disabling or changing a member's role revokes their sessions. Losing membership
management authority retires outstanding invitations they issued. Existing API
keys retain their issuer attribution, project, and policy; revoke them
explicitly when required.

Role ownership is explicit: setup and invitation accounts are locally managed;
OIDC-provisioned accounts remain OIDC-managed across password enrollment,
password changes, and identity linking. Verified login and reauthentication
synchronize OIDC-managed roles. Losing all mappings commits deauthorization,
revokes all sessions and recent-auth grants, retires outstanding invitations,
and advances authorization state before returning denial. This also applies to
the last owner: administrative recovery protection never overrides verified
external access loss. Password login cannot bypass established deauthorization.
A later verified mapped login restores external authorization (not an
administratively disabled account); old sessions and invitations stay revoked.
Locally managed accounts retain their locally assigned role on OIDC sign-in.
OIDC-managed accounts must keep a usable linked identity so self-service unlink
cannot sever their authorization source. Role ownership is internal and cannot
be changed by self-service credential operations.

Local sign-in is usable only when both `OLP_LOCAL_LOGIN_ENABLED` and
`auth.local_login_enabled` permit it. Capabilities, invitation issuance and
acceptance, identity unlink, and last-owner configuration guards apply the same
policy. Password invitations are unsupported while effective local sign-in is
disabled: issue/accept returns actionable guidance before account creation or
token consumption. Use mapped OIDC provisioning for SSO-only onboarding; email
matching never implicitly links an existing account.

Owner protection evaluates proposed mappings against the latest verified email
and group inputs stored privately for each identity. These inputs never appear
in identity responses or audit. Changing claim names requires another verified
owner path or an owner with enabled local password sign-in.

Replacing or removing the OIDC client secret requires an active owner with
enabled local password sign-in. A changed secret invalidates prior OIDC sign-in
evidence; keep local login enabled until an owner successfully signs in through
OIDC with the saved credential. Omitting the secret or submitting the same value
preserves the existing evidence.

OIDC uses maintained `coreos/go-oidc` verification and `x/oauth2` code exchange.
Configure discovery, the exact issuer, client ID, client secret when required,
and the callback `<public origin>/api/v1/oidc/callback`. The client selects
`client_secret_basic` or `client_secret_post` authentication as advertised;
omitting the supported-methods field defaults to `client_secret_basic`.
Unsupported token authentication methods are rejected during configuration.
Discovery and endpoints must use HTTPS and public addresses. The dedicated
client rejects redirects, credentials in URLs, private/reserved DNS answers,
oversized responses, and unbounded waits. It applies the provider egress
address denylist without any operator exceptions and is never built from the
provider policy, so provider egress settings cannot weaken identity egress.

Saving enabled OIDC configuration binds its validated issuer, authorization,
token and JWKS endpoints and selected token authentication method. Sign-in and
callbacks reject changed discovery metadata before reading or sending the
client secret. An owner must review the identity provider and save the
configuration again to accept a change. Existing configurations without a saved
binding also require this resave; keep a local owner sign-in or an existing
owner session available during the upgrade. The binding is private and does
not appear in management responses.

Authorization binds a single-use encrypted flow to the browser, configuration
ETag, state, nonce, PKCE verifier, and initiating session when applicable. ID
tokens must have a verified email and valid signature, issuer, audience, expiry,
and nonce. Email collisions require explicit linking after recent
authentication. Enrollment/link/unlink and plugin-permit proofs expire after
five minutes, are purpose/resource-bound, and are consumed once; an OIDC
reauthentication for `plugin_permit` returns to the Plugins page.
Authentication-method changes rotate the current session and revoke prior
sessions.

Only the explicit `oidctest` build tag permits a loopback HTTP issuer; no
environment variable relaxes OIDC transport checks. The integration runner
builds a separate test binary, and release images never enable that tag.

## Projects and budget groups

Users have a global or assigned-project access scope in addition to their
installation role. Global users can access all projects and unassigned resources
within that role's permissions. Assigned users see only their member projects;
project managers can write resources and project viewers can read them, subject
to installation-role permissions. Assigned users cannot administer installation
membership, other members' sessions, OIDC, or global settings. Keep at least one
manager per project.

Manage projects and membership through `/api/v1/projects` and
`/api/v1/projects/{id}/members`. Providers, routes, and gateway keys carry a
project boundary; a key can use only routes in its own project, including the
unassigned boundary. Management tokens can also be limited to named projects.
A project, or a resource in one, outside the caller's scope answers 404 exactly
as if it did not exist; a visible project the caller cannot change answers 403.

`GET/POST /api/v1/budget-groups` and `GET/PATCH /api/v1/budget-groups/{id}`
manage shared cost budgets, measured against accrued spend and the estimated cost
of the members' requests in flight. A group requires a positive daily or monthly
limit, uses the installation currency, and belongs to one project or the
unassigned boundary. Attach a key through `budget_group_id`; key and group must
share that boundary. Writes require key-management permission and project write
access. Group limits supplement individual key limits and share their
[initialization and recovery rules](operations.md#spend-budget-reconciliation).

## Key response metadata

The key policy `response_metadata` is off by default. Set it when creating a key,
with `PATCH /api/v1/api-keys/{id}`, or in the console key form to opt the key
into the `X-OLP-Attempts`, `X-OLP-Route-Revision`, `X-OLP-Provider` and
`X-OLP-Cost` response headers described in
[gateway](gateway.md#gateway-metadata). Callers address routes, not upstreams, so
the gateway names the provider that served a request only to keys that opt in. It
does not scrub what an upstream says of itself: the message of an upstream
rejection is relayed with credential values redacted, whatever the policy, and
may name the vendor or the model. Rotation keeps the setting, and a change takes
effect on each gateway's next authority refresh.

The remaining-allowance headers need no opt-in. A key with a requests-per-minute
or tokens-per-minute limit receives the
[rate-limit headers](gateway.md#rate-limit-headers) of its surface for the
dimensions it limits, and a key with neither receives none.

## Management tokens and provisioning

Owners create management tokens from the Access console tab or
`POST /api/v1/management-tokens`. A token carries 1–8 unique management
operation scopes (`read`, `access_read`, `access`, `settings`, `configure`,
`keys`, `playground`, `usage`), a name, and an expiry no more than 366 days
ahead. The `olpm_` secret is displayed once; only its HMAC digest is stored.
Bearer authentication authorizes exactly the operations in scope: a token with
`read` and `configure` can read state and manage providers and routes but cannot
mutate keys, members, settings, or pricing. Configuration imports that change
pricing also require `settings`; imports with unchanged pricing or only provider
and route changes require `configure`. Changing project limits, templates, route
groups, attribution policy or attribution budgets additionally requires `keys`;
unchanged policies do not. Requests carrying an `olpm_` bearer
credential are non-browser traffic: they do not send Origin or CSRF proofs and
are authenticated by token digest, expiry, and revocation. Every other bearer or
cookie request keeps the full browser defenses. A token acts within its
creator's current authority: it fails authentication while the creator is
inactive or OIDC-deauthorized, loses operations the creator's current role does
not hold, and reaches only the creator's projects when the creator has an
assigned access scope. Nothing is revoked, so a creator who regains authority
also restores their tokens.
Token administration itself — create, list, read, revoke — is always
session-owner-only; no management token can manage tokens. Installing,
approving, permitting and uninstalling [provider plugins](plugins.md) is
session-owner-only in the same way, while any role or token with `read` can list
plugins and their declarations. Permitting an unconfined plugin also takes a
recent authentication for `plugin_permit`. Revocation and expiry take effect
immediately and audit records attribute machine actions to the token rather than
to a member.

Omitting `project_ids` creates an all-projects token. An explicit list limits
access to those projects with manager-equivalent project access, still bounded
by operation scopes. Project-scoped tokens cannot perform installation-wide
access or settings operations or configuration promotion.

`PUT /api/v1/provisioning/{source}/users/{external_id}` reconciles a provisioned
identity without waiting for a sign-in. An owner session or a management token
with the `access` scope may call it. The body is the authoritative desired state
(`email`, `display_name`, `role`, `active`): a missing `(source, external_id)`
mapping creates a passwordless provisioned-managed user, and a mapped one is
updated in place. Every reconciliation invalidates the member's sessions
immediately, and deactivation also retires outstanding invitations they issued.
A provisioned identity cannot take an email owned by another account, and the
last usable owner cannot be deprovisioned. `DELETE` on the same path reconciles
to inactive and returns 404 only when the mapping does not exist. This is a
push-based reconciliation contract. The standards-based SCIM adapter is described
below; its reserved `scim` source cannot be changed through this generic API.

Lifecycle ownership transfers deliberately in one direction. When an owner
changes a provisioned member's role or active state through ordinary user
administration, the account becomes locally managed in the same transaction
while its provisioning mapping is retained as the ownership record; a later
external reconciliation for that identity is refused with
`provisioning_ownership_changed` rather than silently reclaiming the account.

## SCIM provisioning

Use the public origin plus `/scim/v2` as the directory provisioning base URL.
Every operation, including discovery, requires an unexpired installation-wide
management bearer token with the `access` scope and a currently authorized owner
creator. Console cookies and project-scoped tokens cannot administer the directory.
Management CIDRs, trusted-proxy resolution, admission bounds and security headers
also apply. SCIM failures, including early network/admission refusals, use the RFC
7644 error envelope and `application/scim+json`, not the management Problem format.
Missing/invalid credentials return 401 with a bearer challenge.

`Users` and `Groups` support POST, GET, PUT, PATCH and DELETE. Collections use
one-based `startIndex`, at most 200 results per page, `count=0` for counts, filters,
ascending/descending sorting, `attributes`/`excludedAttributes`, and POST `.search`.
Counts and page rows share one database snapshot. Filters support presence,
equality/ordering, substring/prefix/suffix, logical operators and multi-value
filters such as `emails[type eq "work" and value co "@example.com"]`. Attribute
names ignore case; duplicate names differing only by case are refused. Identity
attributes remain case-exact. Multi-valued sorting uses the primary value when
present, then the first value; subattribute projection preserves array structure.
Filter/path input is limited to 4096 bytes and 16 nesting levels. Values and paths
are bound SQL parameters, never interpolated caller expressions.

Requests are bounded to 128 KiB and 15 seconds. PATCH accepts up to 32 ordered
operations and applies them atomically, including filtered member removal and
multi-valued subattributes. Stable IDs and computed attributes cannot be patched.
Weak resource ETags are returned in headers and `meta.version`; optional `If-Match`
accepts weak or strong versions and rejects stale writes with 412. Group versions
also change when computed member displays or membership removal change the
representation. `ServiceProviderConfig`, `Schemas` and `ResourceTypes` declare the
supported surface. Bulk, nested groups and password provisioning are not supported.

A User's `userName` is its unique email address. Creation defaults to an active,
passwordless, provisioned-managed viewer with assigned scope. Sign-in enrollment
is separate; SCIM cannot submit passwords or adopt a locally managed account by
email. The optional `urn:openllmproxy:params:scim:schemas:extension:2.0:User`
extension supplies a base `role` and `accessScope`. Deletion deactivates the user,
removes SCIM memberships, revokes sessions and preserves the stable user/accounting
identity. Re-enrollment can restore that identity through its retained external
mapping. Local takeover through ordinary user administration prevents subsequent
SCIM writes or group reconciliation from reclaiming its authority.

Group members are existing SCIM-managed User IDs, at most 1000 per group. The
`urn:openllmproxy:params:scim:schemas:extension:2.0:Group` extension can add an
installation `role`, `accessScope`, and up to 100 `projects` entries containing
project UUID `value` and `manager` or `viewer` role. Effective installation role is
the highest base/group role; global scope is granted if any active grant supplies
it. Project grants union with direct and organization memberships, with manager
precedence. Removing a group or member removes only that inherited grant. Every
reconciliation revokes affected managed sessions; deactivation/loss of ownership
retires issued invitations. Writes preserve the last usable owner transactionally.

Owners can manage mappings through **Access → SCIM** at `/scim-provisioning`,
`GET /api/v1/scim/groups`, and ETag-guarded
`PUT /api/v1/scim/groups/{scim_id}/mapping`. This changes access policy without
changing directory membership. Ordinary directory PUT requests that omit the OLP
extension preserve its current mapping; an explicit extension replaces it. The
console's group editor and the protocol share the same reconciliation and owner
protection. Users' computed `roles` and `groups` remain read-only.

Configuration `scim_group_mappings` promotes group display names and role/scope/
project-name grants. Project UUIDs are remapped at the destination. Missing groups
are created empty; existing membership and directory identifiers remain local.
Omitted mappings preserve destination policy, and an explicit empty grant set
clears the named group's grants. Export containing mappings and plan/apply/replay
containing mappings additionally require installation `access`, including unchanged
applications. Identical mappings do not revoke sessions. Configure the directory
client to find an existing group by display name before creating it.

SCIM stores bounded identity metadata in `users`, `provisioned_users` and
`scim_users`, plus group metadata and membership/project edges in `scim_groups`,
`scim_group_members` and `scim_group_projects`. User tombstones remain for identity
and accounting continuity; deleting a group removes its edges. Local takeover
retains the provisioning ownership record. These records follow identity lifecycle,
not request-history retention. Mutations write actor/resource audit records under
the existing audit retention policy. No request/response content, directory bearer
tokens or passwords enter SCIM documents; no new seal purpose is needed. Tokens
use the existing management-token HMAC purpose. Full external-client conformance
and enterprise-directory qualification remain with the human PR reviewer.

## Local multi-factor authentication

Profile settings supports one authenticator app (TOTP) and up to ten total
factors, including WebAuthn security keys or platform passkeys. Enrollment must
prove possession before a factor becomes active. First enrollment requires
recent password or linked OIDC authentication; later enrollment, removal and
recovery-code replacement require an enrolled factor or one-use recovery code.
Management automation tokens cannot administer a person's factors.

TOTP uses six digits, SHA-1 and 30-second periods, with one period of clock skew
in either direction. Accepted counters are recorded transactionally: a code
cannot be reused through another challenge or gateway. Wait for a new code when
you just used the previous one. WebAuthn verifies the challenge, exact configured
public origin, relying-party ID, user verification and authenticator counter.
Counters/credential state are persisted after success; clone warnings are
refused. No identifying authenticator attestation is requested. WebAuthn needs
an HTTPS DNS public origin; `http://localhost` is allowed for development.
IP-based or insecure non-localhost origins advertise it as unavailable, while
TOTP and recovery codes remain usable. Changing the relying-party hostname may
require re-enrollment; save recovery codes outside the browser first.

Once enrolled, password sign-in returns `202` with a five-minute challenge,
not a session. Complete it through `POST /api/v1/auth/mfa/verify`. The console
keeps pending proof in memory, offers the available methods and refreshes
session authority only after successful verification. Challenges have a
five-failure limit plus source/account throttling and bind the current account
ETag, MFA revision and, for authenticated management, the current session.
Expired, consumed or superseded challenges fail closed. The server rechecks
local-login availability before completing pending password authentication.
Password reauthentication similarly waits for a second factor before issuing
its purpose-bound recent-authentication grant. Browser session-bound proofs
retain ordinary Origin/CSRF requirements.

First enrollment generates ten random, one-use 128-bit recovery codes. Store
them securely: the response is the only time their values are available. Codes
can complete sign-in or recent-factor confirmation and are atomically consumed
across gateways. Replacing codes invalidates the entire previous set. The
console waits for explicit acknowledgment before dismissing newly shown codes;
closing a lost enrollment response does not make its secret material replayable.
Sign in using the enrolled device and replace recovery codes if needed.

Successful enrollment rotates the current session and CSRF proof, invalidating
its previous cookie rather than upgrading a potentially copied password-only
cookie. Other sessions are revoked. Removing a factor revokes other sessions;
removing the last factor also clears recovery codes and local MFA strength.
Management mutations match the MFA revision through `If-Match`, consume fresh
proof and audit their action without recording seeds, proofs or recovery values.

An installation owner can set `auth.mfa_required=true` in Settings. Existing
unverified local sessions stop authorizing; a correct password without an
enrolled factor receives a restricted bootstrap challenge. Only completing
factor enrollment creates the session. The last factor cannot be removed while
the policy requires it. Turning the policy off does not disable voluntary MFA.
OIDC sessions retain their external authentication method; enforce IdP MFA
through that provider's policy. Management tokens and inference credentials are
unchanged. Configuration export includes `require_local_mfa`; omission preserves
it and a change requires installation `access` in addition to `configure`.
Enrolled factors, codes, challenges and session strength never cross promotion.

Persistence: factor metadata, public credential IDs, last-used timestamps and
accepted TOTP counters live in `mfa_factors`. TOTP seeds and complete WebAuthn
credential state are record-bound sealed rows under `mfa_totp` and
`mfa_webauthn`, participating in normal master-key re-encryption/retirement
checks. `mfa_recovery_codes` stores only user-bound `mfa_recovery` HMACs.
`mfa_challenges` retains `mfa_challenge` HMACs of control tokens, user/revision
bindings and public WebAuthn ceremony data for at most five minutes; pending
TOTP seeds are separately sealed and expire. Enrollment prunes excess/expired
flows and retention removes expired challenges/secrets. Sessions record local
or external method and MFA strength until normal revocation/expiry. Audit rows
retain only action/actor/resource metadata under audit retention. Back up the
database and master-key ring together. Offline recovery is described below.

## Account recovery

`olp account reset-password EMAIL PASSWORD_FILE [--reset-mfa] [flags]` is an offline operator
command that resets the password of an existing active account. The password
file must be a regular file readable only by the operator (no group or other
permission bits), at most 4097 bytes, and valid UTF-8; one trailing line ending
is removed and the usual 12–1024 character policy applies. A successful recovery
invalidates all sessions and recent-authentication grants for the account,
leaves role, active state, lifecycle ownership, and OIDC authorization
untouched, and writes a system-attributed `user.password_recover` audit record.
Ordinary recovery preserves enrolled MFA. The explicit `--reset-mfa` option
removes factors, pending enrollment secrets and recovery codes, and records
`user.mfa_recover`. It leaves `auth.mfa_required` intact, so the next local sign-in
must bootstrap a new factor if required. Place this option immediately after
the password-file argument. The command prints only the account id, email,
and `password_recovered` / `mfa_reset` flags;
it never echoes the password. A missing or inactive account fails with a generic
recovery error that does not confirm whether the address exists.

## Mutation and audit boundaries

Every management route is authorized before its handler runs, from the
security requirement the [management contract](../openapi/management.json)
declares for it.
The session response lists the operations the member may perform, and the
console offers an action only when the requirement of the call it leads to
admits the member, so an assigned member never sees installation pages it
cannot open.
Protected writes reauthorize inside their feature transaction. Access mutations
take the installation row lock so ownership checks and writes commit together;
password hashing, OIDC discovery, and token verification run outside that lock.
Versioned updates require a strong quoted `If-Match`; stale edits return 412.
The console retains the ETag from the edit baseline and offers an explicit
reload after a conflict.
Key creation/revocation/rotation and invitation creation/retirement require
`Idempotency-Key`. Replays bind actor, method, path, precondition, and request
body, are reauthorized before reading, and encrypt responses for 24 hours.
Feature request and collection limits bound response sizes; aggregate responses
such as credential pools are replayed in full even above 64 KiB. Lists use
validated cursors and limits up to 200.

Public authentication admission is shared through PostgreSQL: 10,000 requests
per action globally per minute, 60 per resolved source (30 for invitation
acceptance), five per source/target, and 30 per target across sources when a
target is known. Targets and sources are HMAC digests. Windows are fixed at one
minute, counts are capped, and denied attempts never extend a window or create a
permanent account lockout. Management uses the same `OLP_TRUSTED_PROXY_CIDRS`
resolver as inference: the direct peer is authoritative unless explicitly
trusted, then `X-Forwarded-For` is walked from the right through configured
trusted hops. Never configure untrusted clients as trusted proxies.

Normal reads, errors, and audit records never expose password/token hashes,
credential ciphertext, raw identity claims, or full user agents. Audit stores
explicit actor/resource/action/outcome, time, direct peer IP, and a coarse user
agent family. Automatic role synchronization uses no invented human actor.

Passwords use salted Argon2id with fixed 64 MiB/3-iteration parameters and
bounded local concurrency. Session, API-key, invitation, and admission lookups use
installation- and purpose-separated HMAC digests. OIDC client secrets, pending
flows, and one-time mutation replies use AES-256-GCM with installation, record,
and purpose authenticated as associated data.

## Console authentication

Set `OLP_PUBLIC_ORIGIN` to the exact browser origin. Unsafe browser management
requests require that Origin; authenticated browser writes also require
`X-CSRF-Token`. [Management tokens](#management-tokens-and-provisioning) use
separate bearer authentication. Use HTTPS outside local loopback development.
Cookies are Secure, Path=/, SameSite=Lax, with HttpOnly session and
recent-authentication cookies. Private health and metrics endpoints belong on a
private listener/network. API responses use `no-store`; public static assets
remain separate.

Session verification and sign-in capabilities requests have a 10-second console
deadline, including response bodies. Navigation cancellation stays separate from
timeout/service failure. A passive failure retains loaded content with a visible
Retry notice and requires successful verification before writes. Session
rotation invalidates older responses and sends a credential-free invalidation to
sibling tabs. A specific `csrf_invalid` rejection refreshes verification and
asks the user to review and retry; mutations are never replayed automatically.
Other forbidden operations do not trigger logout.

Browser OIDC callback failures redirect to local sign-in/profile pages with an
allowlisted reason; raw provider errors and tokens are never included. Retry
always starts a fresh flow. JSON clients retain problem responses. Profile
reauthentication offers usable linked OIDC even with an enrolled password;
existing session, purpose, resource, and exact identity bindings still apply.

Session inventory labels `last_seen_at` as **Last session verification**
(updated at most once a minute by session verification). A coarse browser/device
hint helps distinguish sessions without retaining raw user agents. It is
untrusted display metadata, not authentication evidence; an unrecognized user
agent shows Unknown browser.

## Project limit templates

A project can publish up to 64 named templates through **Project policies →
Limit templates**, or `GET`/`PUT /api/v1/projects/{project_id}/limit-templates`.
Each template contains RPM, TPM, concurrency, daily USD and monthly USD limits.
Names use the route-slug syntax. PUT replaces the complete `templates` map,
requires `keys`, project-manager authority and a quoted project `If-Match` ETag.
GET requires `read` and project visibility. Empty limit sets add no ceilings.

Keys, key/project end-user policies and budget groups select a project-local
`limit_template`; null detaches it. Every non-null template dimension is a
ceiling: a member’s inline limits can tighten it but cannot raise it. End-user
digest overrides may replace local defaults but still obey the template. Keys
and end-user policies keep independent counters; a budget group shares counters
among its member keys. Templates themselves never aggregate spending or create
a new budget owner. Budget groups also accept inline RPM, TPM and concurrency
limits. Their usage details and budget-threshold notifications report effective
cost ceilings.

Template edits advance key authority and apply at its next refresh, including
across gateways and overlapping secret versions. Subscription generation checks
current templates again during durable admission and retains its USD exemption.
Counter identities and accrued history do not change when a template is edited
or detached. Cost-only limits still fail closed until their existing accounting
snapshots initialize. Removing an in-use template returns `409 template_in_use`;
detach its keys, budget groups and end-user policies first. Unknown references
and references from unassigned keys/groups are rejected.

Template documents are ordinary project policy metadata in PostgreSQL, with no
new sealed material. Changes are audited. Configuration exports include
`projects[].limit_templates` and `end_user_limit_template`; changes require
`keys` in addition to `configure`. Omitted/null template maps preserve destination
templates; an explicit empty map clears unused ones. Keys and budget groups remain
installation-local, and promotion refuses to leave any of their references
unresolved. Project end-user defaults and their template reference are portable;
digest overrides and block lists remain destination-local.

## Limits by route

An API key may declare `route_limits`, a map of up to 100 route slugs to
`requests_per_minute`, `tokens_per_minute`, `max_concurrency`,
`daily_cost_limit`, `weekly_cost_limit` and `monthly_cost_limit`. Configure these in the key editor's
**Limits by route** controls or through key create/update. Omitted dimensions
are unlimited at this boundary; the key's ordinary limits and every other
applicable boundary still apply. Empty entries add no limit; `{}` or null clears
the map. This policy does not grant access to a route.

Counters are shared by all gateways and all secret versions of the key, while
different keys and routes have separate counters. Requests, tokens and concurrency
use the existing reservation and reconciliation path. Costs include every attempt
and retry for the original ingress route, including fallbacks. Classifier work
uses its classifier route. Collections without one owning route have only the
other applicable limits. Subscription routes enforce rate/token/concurrency limits
and retain their USD-budget exemption; shadow/probe work retains its exemption
from caller key limits.

A refusal returns the existing rate or budget error and, where the protocol
supports it, `param: route_limits.<slug>`. Oversized token estimates return
`400 request_exceeds_token_limit`; unknown cost balances fail closed with 503.
Enabling a USD cap reconstructs earlier current-window spend. Clearing or changing
it does not erase spend. The pricing, unpriced-usage and calendar-window semantics are
the same as other current key budgets.

The key policy remains installation-local and is excluded from configuration
promotion. Costed entries have stable key/route owners in
`aggregate_budget_accounts`; `aggregate_cost_windows` stores current day/week/month
balances and prunes expired windows through reconciliation. Owners survive policy
changes until their key is deleted. No prompt, output or credential is added to
these records.

## Installation and project budgets

Installation and project budget policies cap aggregate inference spend in USD.
A request must pass both applicable caps as well as its key, budget group,
end-user and supply limits. Shadow traffic and paid probes count against these
aggregate caps; subscription code traffic retains its separate token allowance
and does not accrue inference USD.

Use `GET` and `PUT /api/v1/budgets/installation` for the installation policy.
Reading requires installation-wide `read`; changes require installation-wide
`settings`. Project policies use `/api/v1/projects/{project_id}/budget`: readers
need project visibility and changes require `keys` plus project-manager access.
Writes require a quoted `If-Match` ETag and an explicit `policy` object or null.
The object accepts `daily_cost_limit`, `weekly_cost_limit` and `monthly_cost_limit`; null dimensions
are unlimited. Settings exposes the installation policy, and Project policies
exposes each visible project's policy with read-only controls where appropriate.
Responses include current live and retained spend and monthly unpriced attempts.

Every configured boundary reserves its estimate before dispatch. If a later
boundary refuses, earlier reservations are refunded. `429 budget_exhausted`
identifies the installation or project boundary in its message and, on surfaces
that support it, the `budget.installation` or `budget.project` parameter. Missing
or unusable balances fail closed with 503. The first accounted refusal or the
normal reconciliation worker reconstructs existing spend before admission can
continue. Settlement replaces estimated cost with actual cost; idempotent
accounting removes the hold. Existing pricing freshness and unpriced-attempt
semantics apply: without a usable price, no estimate is reserved and unpriced
attempts accrue zero USD.

These policies use installation calendar days, weeks and months. Removing a cap never
resets spend. Once created, an aggregate account continues collecting usage even
while its cap is disabled. `aggregate_budget_accounts` stores only stable owner
identifiers; `aggregate_cost_windows` stores current-window numeric totals and
unpriced counts. Reconciliation reconstructs totals from ordinary live facts and
hourly rollups and prunes obsolete windows. Project account deletion follows the
project's lifetime. No new cryptographic purpose or request content is stored.

Configuration export includes project `budget` and `installation_budget`.
Project omission/null clears the cap. For the installation, omission/null leaves
the destination policy alone; an empty object explicitly clears it, and exports
always include an object so a round trip preserves the intended state. Changing
a project cap during promotion also requires `keys`; changing an installation
cap requires `settings`. Unchanged policies need no additional permission.

## Weekly budget windows

`weekly_cost_limit` is an independent exact-decimal USD ceiling for keys, budget
groups, project/installation budgets, per-key route limits, end-user policies and
project limit templates. ISO weeks start Monday at local midnight in the installation budget time zone and can span month
or year boundaries. Day, week and month caps all apply; raising or clearing one
never resets another. Supply-side connection/slot/route budgets retain their
existing day/month dimensions. Subscription dollar-budget exemptions are unchanged.

Admission derives the current week from Valkey server time and checks accrued
spend plus outstanding reservations. An exhausted week returns `429
budget_exhausted`; unknown or stale evidence returns 503. Reconciliation must
reconstruct a complete current week from live facts and retained usage before
admission trusts its balance. Incremental event delivery alone cannot initialize
a partially known week. The migration records this distinction using
`weekly_complete` on cost-window rows; the existing reconciliation worker backfills
weekly balances without treating missing history as zero.

Management budget responses expose a `weekly` window. The console shows its
accrued amount, ceiling and reset time; threshold notifications accept
`window_kind: "week"`. The limiter rejection metric includes `window="weekly"`.
Unpriced-attempt counts remain month-scoped. All periods follow the installation
budget calendar below.


## Budget time zone

`budgets.time_zone` is an installation setting, default `UTC`, accepting an IANA
name such as `America/New_York`, `Australia/Lord_Howe` or `Asia/Kathmandu`. Update
it in Settings or with an ETag-guarded `PUT /api/v1/settings/budgets.time_zone` and
`{"value":"America/New_York"}`. The existing installation settings permission,
CSRF and audit rules apply. No inference-side parameter can change the calendar.

Every active day, ISO-Monday week and month finishes under its existing schedule.
Each period switches independently at its next boundary; it does not reset early.
The first new-zone period may be shorter so subsequent boundaries align with the
new civil calendar. Requesting another zone replaces only pending transitions;
active/history entries remain intact. Requesting the current zone cancels that
period's pending change. Settings responses and the console show current bounds
and pending zone/effective date separately from the requested setting value.

Accrued spend, reconciliation, reporting, supply caps and threshold notifications
use the same durable calendar. Civil days may contain 23, 24, 25 or fractional
hours; repeated midnight uses its first occurrence and skipped dates have no
invented interval. Valkey's clock, not the gateway's clock, determines whether a
balance's start/end evidence is current. Missing or expired evidence fails closed
until reconciliation installs the current balance. A late UTC snapshot cannot
replace an active non-UTC interval; a setting change adds no per-request database
query or calendar-service round trip. Subscription allowances keep their existing
provider/token windows and dollar-budget exemptions.

`budget_calendar_changes` retains only zone names, period kinds and effective
instants. It preserves historical rules for delayed accounting and retention.
`attempt_usage_hourly.budget_bucket` splits cost evidence inside an ordinary UTC
reporting hour when necessary, preserving exact half-/quarter-hour boundaries;
normal usage reports still aggregate by their existing hourly buckets. No prompt,
output, user identifier or credential is added. Cost hashes carry the current
period bounds and expire at their end.

Configuration export includes the requested `budget_time_zone`. Omission/null
preserves the destination setting; changing it during apply also requires
`settings`. Promotion schedules new transitions against the destination's current
windows, rather than copying another installation's history or resetting spend.

### Project attribution budgets

Project managers with `keys` permission can allocate cost budgets to exact
attribution label/value pairs through **Project policies → Attribution budgets**
or `PUT /api/v1/projects/{project_id}/attribution-budgets`. The write requires the
project's quoted `If-Match` ETag and a complete `{"budgets": {...}}` map. For
example, `{"budgets":{"team":{"core":{"weekly_cost_limit":"500"}}}}`
limits this project's `team=core` allocation. Each pair needs at least one positive
`daily_cost_limit`, `weekly_cost_limit` or `monthly_cost_limit`; at most 64 pairs
may be configured. `GET` requires project visibility and `read`, and returns the
map, ETag and current per-pair usage. An empty map clears enforcement without
resetting spending. All writes are audited and propagate through authority
refresh; the console supports read-only inspection by other project members.

Admission uses the resolved labels after required-label checks and operator pins.
Every matching pair has an independent ceiling, shared across the project's keys
and gateways. Other key, group, end-user, route, project and installation limits
still apply. A refusal refunds earlier reservations. Exhaustion returns
`429 budget_exhausted`, with `param` identifying
`attribution_budgets.<label>.<value>` where the native error surface supports it.
Unknown balances remain `503` until accounting or reconciliation reconstructs
current history. Price freshness, unpriced attempts and estimate limitations are
the same as other cost budgets; these are not guaranteed invoice caps.

Caller-selected labels are allocation controls, not an authenticated security
boundary: callers can otherwise omit a label or choose another value. Require
labels and pin values using [attribution policy](#attribution-requirements-and-pinned-labels)
when callers must use a particular allocation. Pair names and values use the
existing bounded machine-token syntax and match exactly, including case. A pair
in another project has separate spending.

Caller, classifier and shadow usage carrying matching labels contributes to the
allocation. Unlabeled probes do not acquire labels or charge a label allocation;
their installation/project budgets still apply. Subscription code traffic retains
its USD exemption while retaining attribution requirements and diagnostic labels.
Background generation keeps the creator's labels for eventual accounting.

The project policy is metadata in `projects.attribution_budgets`. Stable
project/label/value owners live in `aggregate_budget_accounts`, with exact
calendar balances in `aggregate_cost_windows`. Accounts survive clearing a cap
and are deleted with their project. Existing live facts and retained budget
sub-buckets reconstruct prior spend when a cap is introduced or restored;
retained aggregation preserves the original labels. No additional request content
or credentials are stored. Unpriced counts remain monthly, as in other reports.

Configuration exports include `projects[].attribution_budgets`. Changing these
caps requires `keys` in addition to `configure`; unchanged promotion does not.
Omitted, null or empty maps clear caps and preserve accounting. Time-zone changes
follow the shared [budget calendar](#budget-time-zone).

## Temporary budget increases

The **Budget increases** console page and `POST /api/v1/budget-increases` add
an exact USD amount to one existing day, week or month cap. Supported boundaries
are installation, project, API key, budget group, a key's route, a key/project
end-user digest, or a project attribution pair. Supply connection/slot/route
policies retain their separate controls. A template is not an increase target:
select the individual member whose effective ceiling needs an exception.

Creation requires an Idempotency-Key, a single-line reason (1–256 characters),
and either `keys` with project-manager authority or installation-wide `settings`
for an installation target. The optional expiry defaults to the current calendar
window's end and is clipped to that end when later. Up to eight active increases
can add together for each owner/window. The existing permanent cap must be
present. Other budgets, limits, and admission requirements still apply; unknown
cost balances continue to fail closed. An increase changes neither accrued spend
nor outstanding reservations and does not create a new accounting owner.

The explicit deadline and original window identity travel with refreshed
authority; Valkey checks both using server time on every cost admission. Expiry
therefore takes effect without waiting for another gateway refresh. Changing a
permanent cap keeps the same additive exception until it expires or is revoked;
clearing the cap makes the exception irrelevant. Key rotation does not change its
budget owner. Subscription code traffic remains exempt from USD caps and
increases, while matching aggregate increases also apply to system work.

`GET /api/v1/budget-increases` and `GET /api/v1/budget-increases/{increase_id}`
provide project-filtered records to readers. `DELETE` on the individual resource
requires its quoted If-Match ETag and the same management authority as creation.
Revocation propagates through ordinary key-authority freshness. A revoked or
expired row stays available for audit. Retrying creation with the same actor,
input and Idempotency-Key recovers the original result without adding another
increase or extending its deadline. The console keeps that request identity when
a response is lost.

Key/group budget reports expose `effective_limit`, `temporary_increase` and the
next `increase_expires_at` separately from their permanent editable limits.
Effective allowances include template ceilings. The console uses those
allowances for spend status; editing a key never copies a temporary increase into
its permanent policy. Threshold notifications continue to monitor the configured
permanent/template cap, so an exception does not erase an existing warning.

`budget_increases` retains the target metadata, exact amount, reason, original
calendar identity/end, expiry, creator/revoker, timestamps and ETag; it contains
no credentials or raw end-user identities. Project deletion cascades these rows.
Creation/revocation also write audit records. Active grant evidence is cached in
memory and supplied to the existing Valkey reservation script, without a new
per-request round trip or a separate spend counter. Existing encrypted mutation
replay retains its normal lifetime. These time-bound, audited exceptions are
installation-local and excluded from desired-state export/promotion; permanent
budget policies continue to promote normally.

## Workload JWT identity

Disabled issuers do not resolve template or route-group bindings during authority
refresh. Their unavailable references cannot block unrelated keys or configuration
round trips. Only enabled workload mappings protect template references; enabling
trust revalidates every referenced template and route group before it can serve.

Owners register issuers under **Access → Workload identity** or
`/api/v1/workload-issuers`. Every management operation requires installation-wide
`access`; writes retain session CSRF, quoted ETags and creation replay protection.
Definitions declare an immutable issuer URL, a JWKS URL, accepted audiences and
RS256/ES256/EdDSA algorithms, a maximum lifetime of 60–86,400 seconds, disabled key
IDs, and ordered claim mappings. Production endpoints must use HTTPS through the
independent identity egress policy. Provider network exceptions do not apply.

A mapping matches exact string claims at JSON pointers, such as
`{"/repository":"example/repository"}`. An explicit empty match object accepts
any otherwise valid subject from that issuer. The first matching rule selects a
fixed project, existing limit template, named route groups and `inference` and/or
`models_read` scopes. Tokens cannot supply arbitrary project IDs or gain management
permissions. Optional provider-state access defaults off. An optional end-user
claim must contain the same bounded machine identifier accepted by ordinary
end-user policies; its digest supplies identification independently of caller
headers. Required project attribution and every applicable budget still apply.

Each request verifies the signature and exact issuer/audience, requires a subject,
issued-at and expiry, checks not-before when present, refuses future issued-at or
expired tokens, and bounds expiry minus issued-at. Tokens are limited to 32 KiB.
Only public RSA keys of 2048–8192 bits, P-256 keys and Ed25519 keys are accepted;
ambiguous key IDs and private or symmetric keys are refused. Token-provided key
URLs and embedded keys never select trust or network destinations.

Gateways cache each issuer's validated keys for a minute and retry unknown-key
refreshes at most once per 15 seconds. Failed refreshes may use last-good keys for
at most five minutes; unavailable verification evidence fails closed. Issuer and
key-ID disablement follows the ordinary authority freshness bound. Disabling
an issuer or a key ID does not require its unchanged JWKS endpoint to be online.
Changing/enabling an endpoint validates its public keys before saving.

The installation-separated `workload_identity` HMAC over the issuer/subject pair
identifies one principal. First use registers a digest-only `api_keys` owner in a
bounded transaction; later requests use cached authority and signing keys. This
lets all gateways share ordinary counters, cost ledgers and retained-resource
ownership. No static API secret is generated: the stored zero digest is
explicitly ineligible for API-secret authentication. Principal revocation uses
the ordinary key API; policy edits and secret rotation are refused because the
issuer mapping owns permissions. A registered subject cannot be moved to another
mapping/project. Updating its mapping's permissions refreshes every principal.

The database retains issuer configuration, public URLs, mapping metadata, ETags,
creator/timestamps, principal issuer/mapping IDs and the HMAC digest. It does not
retain JWT bodies, signatures, raw subjects or end-user claim values. Request,
usage, budget and resource records use the existing principal UUID. JWKS public
keys and verified token claims stay in process memory; no seal purpose is added.
Owner-declared match values are configuration, not captured token claims.

Configuration artifacts carry `workload_issuers` keyed by the exact issuer URL.
Mapping references use project names and resolve to destination IDs; existing
principals, HMAC digests, creator IDs and signing keys are excluded. Omitted or
empty issuer lists preserve destination definitions; disable explicitly. Export
containing issuers, plan with issuers, and apply/replay with issuers additionally
require installation-wide `access`, including unchanged definitions. Existing
project-policy permissions still apply. New/enabled/changed endpoints are checked
before the mutation lock; a concurrent issuer edit produces a retryable
configuration conflict. Enrolled mapping/project ownership cannot be changed by
promotion. Project templates and issuer mappings may change atomically.

OpenAI Realtime, Gemini Live and subscription WebSockets recheck JWT expiry and
current authority on their five-second cadence. A changed verified end-user claim
closes the existing session rather than relabeling its accounting. Native retained
Responses belong to the subject principal, so a renewed token can complete work
created with an expired token; another subject cannot read it. Deferred usage
retains the creator's digest and is accounted once.

Subscription access still requires an explicit pool assignment for the workload
principal. Durable admission checks the current issuer revision, enabled state,
project/group/template policy and ordinary creator/pool authority. A concurrent
issuer edit produces `409 code_workload_changed` before dispatch; obtain current
credentials and retry. Verified end-user claims survive re-admission after limit
changes. USD exemptions remain unchanged. Idle JWT subscription sockets close
when credentials expire, even before another generation is attempted.

Focused implementation checks cover these boundaries; long release qualification
remains with the human reviewer.

## Organizations

An organization contains projects at one level. It has `manager` and `viewer`
memberships; the four installation roles are unchanged. Membership adds access:
it does not remove a direct project grant or narrow a user's existing global
access. Use assigned access scope when a person should be limited to their
organizations and explicitly assigned projects.

Organization managers inherit project-manager scope in every contained project;
viewers inherit project-viewer scope. The stronger of direct and inherited
membership wins. The member's installation role still determines available
operations: for example, a viewer installation role cannot manage keys even with
an organization-manager membership. Management tokens remain bounded by their
scopes and creator's current role/memberships. Tokens with an explicit project
list cannot administer the parent organization or gain additional projects.

Installation owners create organizations through `POST /api/v1/organizations`.
The creator becomes a manager. Read operations require `read`; organization
rename, membership changes, contained-project administration and organization
budgets require `manage_organization` plus manager authority. Global owners can
administer all organizations; a token using that installation authority also
needs `access`. Other project operations retain their existing permissions,
such as `keys` for keys and project budgets. Membership writes reauthorize in the
transaction and use the organization's quoted ETag. Removing or demoting the
last organization manager returns `409 last_organization_manager`.

The Organizations console lists visible organizations, supports create/rename,
manager/viewer assignment, contained-project creation/rename and direct project
membership, and exposes the organization's budget. New organization projects
inherit their managers without adding a permanent direct grant to their creator.
Removing an organization membership immediately changes management-session and
token scope on the next request; independent direct grants remain. The Project
policies, API keys and Notifications pages use the same effective project scope.
Nested project operations verify both organization authority and project ownership.

`GET`/`PUT /api/v1/organizations/{organization_id}/budget` expose day/week/month
USD caps and accrued costs, with the same ETag, null-clearing, fail-closed
initialization, calendar, reservation and reconciliation behavior as other
aggregate budgets. Every request satisfies applicable installation, organization,
project, attribution, end-user, route, key and group caps. Organization spend sums
contained projects across keys and gateways, including shadow/probe invoice cost;
subscription code retains its invoice-budget exemption. Exhaustion identifies
`budget.organization` in the compatible error response. An organization is also a
supported temporary-increase target, requiring `manage_organization` and manager
authority; project-limited tokens cannot create or read parent grants.

Configuration exports organization names/budgets and project organization names.
Organization memberships remain destination-local. Organization creation and a
project's first assignment through promotion require `access`; changed organization
budgets additionally require `manage_organization` and authority over that
organization. Unchanged data requires no extra mutation permission. Omitted
organizations are preserved, and an omitted/null project organization preserves
its destination assignment. Once assigned, a project cannot move to another
organization: plan/apply return `409 project_organization_immutable`. This keeps
historical ownership stable. A first assignment includes the project's existing
spend when reconstructing its organization's current balances.

`organizations`, `organization_members` and `projects.organization_id` retain
metadata until removed with their owners. `effective_project_members` is a view
combining the direct and inherited grants, not a second mutable grant store.
Organization cost owners and calendar balances use the shared aggregate ledger;
clearing a cap preserves history. Increases retain their separate organization
scope. Normal access audit/replay conventions apply; no provider credentials or
request content are added, and no new cryptographic purpose is needed.

## SAML console sign-in

Owners configure one SAML 2.0 identity provider in **Access → SAML**, or through
`GET/PUT /api/v1/saml/configuration`. Reading public trust requires
`access_read`; changing it or importing metadata requires installation-wide
`access`, current session CSRF where applicable, and a quoted ETag after first
creation. Metadata import fetches only the declared HTTPS URL through the
independent identity-egress client: bounded time/body size, checked addresses,
no redirects or provider-egress exceptions. Imported XML is public trust data,
not an identity-provider password. Register the displayed service-provider entity
ID, ACS URL and public signing certificate with the IdP. The metadata download
is an authenticated management operation. Explicit signing-key rotation replaces
the sealed key, publishes its new certificate and invalidates pending flows;
register the replacement certificate with the IdP before relying on it.

This implementation uses SP-initiated, RSA-SHA256-signed HTTP-Redirect requests
and HTTP-POST responses containing exactly one separately signed, unencrypted
assertion. It requires a stable nontransient NameID, exact IdP issuer, SP audience,
recipient/destination and solicited request binding, current signing certificate,
bounded assertion times and a bearer subject confirmation. Signed response
wrappers never substitute for the assertion signature. SHA-1 signatures,
unsigned/tampered assertions, ambiguous XML IDs, directives/entities, excessive
XML size/depth, unsolicited IdP-initiated login, artifact resolution, encrypted
assertions and single logout are unsupported and refused. Supported metadata
contains one IdP role, one unambiguous Redirect endpoint and at most eight public
signing certificates. Configuration changes are explicit; imported metadata is
not silently refreshed.

Choose the email, groups and optional display-name attribute names. Explicit
email mappings take precedence; matching group mappings choose the highest
configured installation role, followed by an optional default role. A new
identity is provisioned only after installation setup and a role match. Existing
local, SCIM and OIDC management ownership remains unchanged when linked; only
SAML-managed users follow SAML role mappings. Existing project/organization
memberships and assigned/global scope remain under their ordinary management
controls. Signed loss of a SAML-managed role revokes its sessions and commits
that deauthorization even when sign-in is denied. Owner-authored changes and
unlinking retain the usable-owner safeguard. A mapped email equal to an existing
account never adopts it: sign in through an existing method and explicitly link
SAML from **Profile** after fresh proof.

The cross-site ACS POST accepts no session cookie as login authority and creates
no session. It validates the signed response, marks its assertion ID used, seals
only the verified identity facts, and redirects to a same-site completion GET.
Completion must present the initiating flow's Secure, HttpOnly, SameSite-Lax
cookie and unconsumed state. This avoids relaxing all session cookies to
SameSite=None. State expires after five minutes and verified completion is also
bounded by the assertion and session-confirmation deadline. Config ETags bind
both stages; any update invalidates pending flows. Responses and return locations
are no-store and return destinations must be local. Disabling SAML clears its
sessions while preserving independent local/OIDC recovery methods.

Linking and sensitive-action reauthentication request fresh IdP authentication
and recheck the initiating session, user and exact linked subject. Recent proofs
are purpose/resource bound and one-use; unlinking needs `saml_unlink` proof and
another usable sign-in method. Fresh linked SAML, like OIDC, can authorize initial
local-MFA enrollment; it cannot replace an enrolled factor or recovery code for
later MFA management. External SAML sessions do not claim completion of OLP's
local second factor. Password enrollment, OIDC linking/unlinking and other
recent-authentication flows can select a usable linked SAML identity.

Configuration export/plan/apply containing `saml` additionally requires `access`,
including unchanged apply and replay. The portable definition contains only public
IdP metadata, enabled state, attribute names and role mappings. Omission/null
preserves destination trust; `enabled:false` explicitly disables it subject to
owner recovery. The destination creates or preserves its own signing key,
certificate and entity ID. Private material, linked users/subjects, browser state,
replay proofs and sessions never travel in promotion.

Persisted-data inventory: `saml_configuration` retains public metadata, mapping
policy, the public SP certificate, ETag and updater. Its private PKCS8 key is
record-bound encrypted under `saml_key`. `saml_identities` retains the deliberately
linked issuer/subject, email-at-link, timestamps and bounded verified authorization
facts; raw assertions are never stored. Pending request/verified-receipt facts
are encrypted under `saml_flow` for at most five minutes; the separate flow table
holds only random IDs, request ID, ETag, deadlines and `saml_state`/`saml_cookie`
HMACs. Assertion replay rows hold purpose-separated `saml_assertion` HMACs until
the assertion deadline. Completion consumes the flow, and ordinary retention
prunes expired flows, secrets and replay records. Audit stores action/resource
metadata, never raw assertions, signing keys or flow credentials. Focused signed
fixtures and Chromium cover this profile; production IdP qualification belongs
to the human reviewer.

### Budget refusal evidence

Cost-budget refusals retain the native `budget_exhausted` code and identify their
hierarchy level in the response message and, where supported, error parameter.
Request Explorer and the request-detail API also expose `budget_boundary`:
installation, organization, project, budget group, API key, key/project end user,
per-key route, or attribution. This fixed label is stored with ordinary request
metadata under its existing retention policy. It contains no end-user digest,
label value, route identifier, secret or upstream error text. Unknown balances
remain availability failures, and ordinary rate-limit errors are not mislabeled
as cost exhaustion.
