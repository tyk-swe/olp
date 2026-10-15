# Access control

The control plane manages installation setup, identity, projects, API keys,
management tokens, provisioning, settings, and metadata-only audit records.
Administrators belong to one trusted installation. See
[deployment](deployment.md) for installation,
[configuration](configuration.md#file-based-secrets) for secret files, and
[operations](operations.md#master-key-rotation-and-recovery) for master-key rotation.

## Attribution requirements and pinned labels

Keys use `required_attribution_keys`, `attribution_defaults` and
`allowed_attribution_keys`. Project policies use
`GET/PUT /api/v1/projects/{project_id}/attribution-policy`; writes require `keys`,
project-manager authority, the project ETag and an explicit `policy` (null clears).

Requirements accumulate. Pinned defaults fill omitted labels; callers may repeat
but cannot replace them (`400 pinned_attribution_override`). Unpinned labels
still need key allowlist permission. Missing requirements return
`missing_attribution`; conflicting pins or more than four resolved labels return
`invalid_attribution`. Use bounded machine tokens, never secrets or content.

These controls cover inference, media, retained resources, Bedrock, realtime and
code mode; model discovery is exempt. Realtime sessions recheck their original
labels. Code refusals use the `code_` namespace; a concurrent admission-policy
change returns `409 code_attribution_changed`.

Resolved labels follow request/fact/hourly retention and the separate code ledger.
Project policies promote; changing them requires `configure` and `keys`.
Key policies remain local. To impose spend limits, configure
[attribution budgets](#project-attribution-budgets).

## API-key network restrictions

Set `allowed_cidrs` to at most 64 IPv4/IPv6 networks. Omitted/null/empty is
unrestricted. Host bits are masked; equivalent duplicates and IPv4-mapped IPv6
ranges are refused. Use IPv4 notation for mapped addresses.

Client addresses use `OLP_TRUSTED_PROXY_CIDRS`; untrusted peers cannot grant
access through `X-Forwarded-For`. Denied or unparseable clients receive
`403 ip_not_allowed` before body parsing or dispatch, including model discovery
and realtime handshakes. Established sessions recheck every five seconds;
code generations recheck during durable admission (`code_ip_not_allowed`).
Key policies remain installation-local. Management restrictions are below.

## Management network restrictions

`OLP_MANAGEMENT_ALLOWED_CIDRS`/`--management-allowed-cidrs` accepts up to 64
comma-separated IPv4/IPv6 networks; Helm uses `config.managementAllowedCidrs` on
control pods. Empty is unrestricted; malformed or mapped-IPv6 ranges fail startup.

The trusted-proxy resolver applies before authentication, parsing or admission.
Denied clients receive `403 management_ip_not_allowed` for the console/assets,
setup, sign-in, callbacks, invitations, SCIM, OpenAPI and management paths.
Allowed clients still need ordinary authentication, scopes and CSRF proof.

Changes require deployment updates and restarted control processes. Include
operator and provisioning-client networks. Recover a lockout through the host or
orchestrator; there is no API bypass. This setting stays outside configuration
promotion. Capabilities reports `management_network_restricted`.

## Route groups

Projects replace their group map through
`GET/PUT /api/v1/projects/{project_id}/route-groups` or Project policies.
Writes require `keys`, project-manager authority and the project ETag:

```json
{"groups":{"production":["chat","embeddings"],"suspended":[]}}
```

Limits are 64 groups and 100 unique slugs per group. Names follow route-slug
syntax. Unknown future routes are allowed; known routes must share the project.

Keys grant the union of `allowed_routes` and `allowed_route_groups`. Only when
both lists are empty is every project route allowed. Missing/empty referenced
groups grant nothing; deletion preserves key references, and recreating the
name restores its members. Edits refresh authority without route publication.
Discovery, simulation, retained access and realtime use the same boundary;
code admission checks membership transactionally.

`projects[].route_groups` promotes with portable slugs. Omitted/null/empty clears
groups; changed groups require `keys` as well as `configure`. Keys remain local.

## API-key rotation and reminders

`POST /api/v1/api-keys/{id}/rotate` requires key-edit authority, `If-Match` and
`Idempotency-Key`. `overlap_seconds` is 0–86400; omitted/zero ends old overlaps
on authority refresh. Positive overlap keeps the previous secret until its
deadline or key expiry. At most eight old secrets may overlap (`key_overlap_limit`).
Replays recover the same secret/deadline; later rotations never extend them.

All versions share the key's current permissions, stable rate/concurrency
counters, cost owner and retained-resource identity. Revocation denies all.
Expiry is checked even without refresh; realtime closes expired versions on its
five-second check. Admitted HTTP work keeps its original accounting.

`rotation_interval_days` (1–3650, null clears) schedules reminders from creation
or last rotation; it neither rotates nor expires a key. Reads expose
`rotation_due_at` and public `active_overlaps`. Subscribe to `key.expiring` with
subject kind `api_key` and matching project scope. Workers claim one reminder
per rule/key/reason/date when due within 24 hours or overdue, cancel superseded
pending reminders and recheck authority before sending. In-flight sends cannot
be withdrawn. See [notifications](operations.md#notifications).

Old secrets persist only as purpose-separated HMACs and public lookup/deadline
metadata; replay replies are sealed for 24 hours. Keys and reminders stay local.

## End-user identity

Keys select `end_user_source: "header"`, `"native"` or null. Header mode requires
one `X-OLP-End-User`; native mode reads OpenAI `safety_identifier` (then `user`
only when absent) or Anthropic `metadata.user_id`. Native mode requires JSON;
use the header for uploads, reads, realtime and other dialects. Discovery is exempt.

Identifiers match `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`. Invalid/ambiguous/missing
values refuse before dispatch without echoing them. The header stays local;
strict native fields preserve forwarding. Accounting stores only an
installation/project-separated `end_user` HMAC. Same-project keys correlate;
different projects do not. The caller supplies identity; it is not authentication.

To find a known identifier, POST `{"identifier":"customer-123"}` to
`/api/v1/api-keys/{api_key_id}/end-user` with read/project authority and browser
CSRF proof. Use `dimension=end_user` in reports; absent identity is `unidentified`.

Key `end_user_policy` and project
`GET/PUT /api/v1/projects/{project_id}/end-user-policy` support `defaults`,
digest-keyed `overrides` and `blocked`. Project writes require `keys`, manager
access and ETag; `{"policy":null}` clears. Project policy requires every serving
key to select a source (`end_user_source_required`). Each boundary enforces
RPM/TPM/concurrency and day/week/month cost independently. Overrides replace
all local defaults; omitted dimensions are unlimited but template ceilings remain.
Blocks win (`403 end_user_blocked`). Override/block lists each allow 256 lowercase
SHA-256 digests. Realtime rechecks blocks and required identification.

Unknown cost balances fail closed until reconstruction; dollar limits do not
apply to subscriptions, whose rate/token/concurrency/block policies still do.
Code admission refuses changed identity with `409 code_end_user_changed`.

Only digests enter request, usage/hourly, pending-generation and code ledgers.
Policy/account/window metadata follows its owning boundary; raw identifiers stay
in memory. Project defaults promote, while digest overrides/blocks and key
policies remain destination-local.

## Access and OIDC

Installation roles are owner, operator, developer and viewer. Role changes or
member disablement revoke sessions; lost membership-management authority retires
issued invitations. API keys require explicit revocation.

Locally managed accounts keep local roles when linking OIDC. OIDC-managed accounts
retain external ownership through password changes/linking and synchronize roles
on verified login/reauthentication. Verified loss of mappings deauthorizes even the
last owner, revokes sessions/proofs/invitations and blocks password bypass.
A later mapped login restores external authorization, except administrative
disablement. Self-service unlink cannot remove its usable authority source.

Local sign-in requires both `OLP_LOCAL_LOGIN_ENABLED` and
`auth.local_login_enabled`. Password invitations are refused when unavailable;
use mapped OIDC provisioning. Email matching never implicitly links accounts.
Owner configuration guards use privately retained verified claim inputs.
Changing claim names needs another verified owner path or a usable local owner.
Replacing/removing the client secret requires an active local-password owner and
invalidates saved OIDC sign-in evidence; keep local recovery until a successful
login with the new credential.

Configure exact issuer, discovery, client ID/secret and
`<public origin>/api/v1/oidc/callback`. Authentication supports advertised
`client_secret_basic`/`client_secret_post` (basic when omitted). Discovery uses
independent identity egress: public HTTPS, no redirects/private addresses/URL
credentials, bounded bodies and waits. Provider exceptions cannot relax it.
Saving binds issuer/endpoints/authentication method; changed discovery or legacy
unbound configuration requires owner review and resave before secret exchange.

Single-use browser-bound flows pin configuration ETag, state, nonce, PKCE and
initiating session. ID tokens require verified email, signature, issuer, audience,
expiry and nonce. Collisions need explicit linking after recent authentication.
Five-minute purpose/resource proofs are consumed once; authentication-method
changes rotate the session and revoke predecessors. Only the `oidctest` build
tag permits loopback HTTP issuers; release images exclude it.

## Projects and budget groups

Members have installation roles plus global or assigned-project scope.
Assigned managers can write their projects and viewers can read, within their
role permissions; installation administration remains global. Keep a project
manager. Organization memberships can add inherited access.

`/api/v1/projects` and `/api/v1/projects/{id}/members` manage boundaries.
Providers, routes and gateway keys share project ownership; unassigned keys use
only unassigned routes. Invisible resources return 404; visible denied writes
return 403. Empty-project deletion needs ETag and Idempotency-Key; retained
foreign-key dependencies return `409 project_in_use`.

`/api/v1/budget-groups` manages shared limits in one project/unassigned boundary.
A group needs a positive cost limit and can also set RPM/TPM/concurrency, and
`cost_basis` (`cost`, the default, or `charge`) picks whether the budget
enforces provider cost or the consumer charge. Attach keys with
`budget_group_id`; membership changes do not reassign history.
Group and individual limits both apply, including
[fail-closed initialization](operations.md#spend-budget-reconciliation).
Writes require `keys` and project change authority.

Group and notification-resource deletion requires ETag and Idempotency-Key;
retained accounting/delivery dependencies return 409. Disable or detach resources
when history prevents deletion. Successful deletion is audited and replayable.

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

Owner sessions create tokens at `/api/v1/management-tokens`. Secrets (`olpm_`)
are shown once and stored as HMACs; expiry is at most 366 days. Delegable scopes
are `read`, `access_read`, `access`, `settings`, `configure`, `keys`, `playground`,
`usage`, `manage_projects` and `manage_organization`. Scopes intersect the
creator's current role, active/external authorization and project memberships.
A creator regaining authority restores tokens that were not explicitly revoked.

Machine bearer requests omit Origin/CSRF; other browser traffic retains those
checks. Omitted `project_ids` means all projects; explicit lists narrow authority.
Project-scoped tokens cannot perform installation administration or promotion.
`manage_projects` supports installation project automation by current global
owner authority without token administration. Organization operations additionally
require their documented manager/global-owner authority.

Token administration and plugin install/approve/permit/uninstall remain
session-owner-only. Unconfined permits also need recent `plugin_permit` proof.
Revocation/expiry takes effect immediately; audit attributes actions to the token.
Promotion needs `configure` plus permissions for changed pricing/settings, project
key policies or installation identity policy; see
[configuration](configuration.md#configuration-promotion-artifacts).

`PUT /api/v1/provisioning/{source}/users/{external_id}` accepts authoritative
`email`, `display_name`, `role`, `active` under owner or installation `access`
authority. It creates passwordless provisioned accounts or updates a mapping,
revoking sessions each time and invitations on deactivation. Email collisions
and removal of the last usable owner refuse. DELETE reconciles inactive.
The `scim` source is reserved.

Ordinary owner edits transfer a provisioned account to local management while
retaining its mapping. External reconciliation then refuses
`provisioning_ownership_changed`; it cannot silently reclaim the account.

## SCIM provisioning

Use `/scim/v2` with an installation-wide `access` management bearer token whose
creator is a currently authorized owner. Cookies/project tokens cannot administer
SCIM. Management CIDRs/admission apply; errors use RFC 7644
`application/scim+json`, including early refusals.

Users/Groups support POST/GET/PUT/PATCH/DELETE, POST `.search`, filters, sorting
and attribute projection. Pagination is one-based, at most 200; `count=0` returns
counts. Filter/path bounds are 4096 bytes/16 levels; requests 128 KiB/15 seconds;
PATCH permits 32 atomic ordered operations. Values are SQL parameters. Attribute
names ignore case but identity values are exact. Resource weak ETags support
optional weak/strong `If-Match` (stale writes: 412). Discovery documents declare
support; bulk, nested groups and password provisioning are unavailable.

`userName` is unique email. New users default to active passwordless viewers
with assigned scope; email does not adopt local accounts. User OLP extensions
set base role/scope. DELETE deactivates, removes inherited memberships and revokes
sessions while retaining identity/history. Local takeover prevents SCIM reclaim.

Groups hold at most 1000 existing SCIM-managed users. Their OLP extension maps
role/scope and up to 100 project manager/viewer grants. Highest role/global grant
and unioned project memberships apply; removing a group removes only inherited
grants. Reconciliation revokes affected sessions and protects the last usable owner.

Access → SCIM and `/api/v1/scim/groups/{scim_id}/mapping` edit policy under ETag.
Directory PUT omission preserves mappings; an explicit extension replaces them.
Portable `scim_group_mappings` use names and destination project IDs. Missing
groups are created empty; membership/IDs stay local. Omission preserves policy;
explicit empty grants clear it. Artifacts containing mappings need installation
`access` for export/plan/apply/replay; unchanged mappings do not revoke sessions.

Identity/group metadata and ownership tombstones follow identity lifecycle;
audit follows audit retention. No passwords, bearer tokens or directory request
bodies persist. Fixtures cover the supported profile; external directory
conformance requires separate qualification.

## Local multi-factor authentication

Profile supports one TOTP app and up to ten total factors, including WebAuthn.
Enrollment proves possession. First enrollment requires recent password or linked
OIDC/SAML authentication; later changes need an enrolled factor/recovery code.
Automation tokens cannot manage personal factors.

TOTP uses six digits, SHA-1, 30-second periods and ±1-period skew. Accepted
counters cannot replay across replicas. WebAuthn checks exact origin/RP ID,
challenge, user verification and counters, refusing clone warnings without
identifying attestation. Use HTTPS DNS origins or `http://localhost`; IP/insecure
origins do not offer WebAuthn. Hostname changes may require re-enrollment.

Password login with MFA returns a five-minute 202 challenge, completed at
`POST /api/v1/auth/mfa/verify`. Challenges bind account/MFA revision/session,
allow five failures plus source/account throttles and recheck login availability.
Recent-authentication proofs also wait for the factor. Browser CSRF still applies.

First enrollment returns ten one-use 128-bit recovery codes once; replacement
invalidates the old set. Lost enrollment replies are not replayable: use the
factor to replace codes. Successful enrollment rotates session/CSRF and revokes
other sessions. Removal revokes other sessions; removing the last factor clears
codes and strength. Changes use `If-Match`, fresh proof and metadata-only audit.

`auth.mfa_required=true` blocks unverified local sessions and gives unenrolled
password users a restricted enrollment challenge. The last factor cannot be
removed while required. OIDC/SAML sessions rely on their IdP policy. Portable
`require_local_mfa` changes need `access` plus `configure`; factors/proofs remain local.

Factor secrets use `mfa_totp`/`mfa_webauthn` seals; recovery/control tokens use
purpose-separated HMACs. Pending challenges/secrets expire within five minutes.
Back up the database and master-key ring together; offline recovery is below.

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

The [management contract](../openapi/management.json) declares each operation's
authority. Protected writes reauthorize inside feature transactions; access
changes serialize ownership checks with the installation lock. Expensive hashing,
discovery and verification run outside it.

Versioned updates use strong quoted `If-Match` (stale: 412). Replay-protected
writes bind Idempotency-Key to actor/method/path/precondition/body and reauthorize
before returning a sealed 24-hour reply. Lists use validated cursors, at most
200 rows. Confirmed writes stay complete when subsequent refresh fails.

Public authentication uses fixed one-minute PostgreSQL admission: 10,000 per
action globally, 60 per resolved source (30 for invitations), five per
source/target and 30 per known target. HMAC targets/sources and capped counters
prevent denied attempts from extending windows. Trust only actual proxies in
`OLP_TRUSTED_PROXY_CIDRS`; forwarded chains are walked from the right.

Audit retains actor/resource/action/outcome, time, peer IP and coarse user-agent
family, never hashes, ciphertext, raw claims or full user agents. Passwords use
Argon2id (64 MiB, three iterations); lookups use purpose-separated HMACs and
secrets/replies use record-bound AES-256-GCM. See [security](security.md).

## Console authentication

Set `OLP_PUBLIC_ORIGIN` to the exact HTTPS browser origin (loopback development
may use HTTP). Unsafe browser requests need that Origin; authenticated writes
also need `X-CSRF-Token`. Session/recent-auth cookies are HttpOnly, Secure,
Path=/ and SameSite=Lax. APIs use `no-store`; health/metrics stay private.

Session/capability verification has a ten-second body-inclusive deadline.
Passive failures retain content with Retry and block writes until verification.
Rotation invalidates older replies and sibling tabs without sharing credentials.
`csrf_invalid` triggers verification and explicit review/retry, never automatic
mutation replay. Other 403 responses do not log the member out.

OIDC failures redirect with allowlisted reasons; retry creates a fresh flow.
Recent authentication can use a linked IdP even after password enrollment.
Session inventory's **Last session verification** updates at most once a minute;
coarse device labels are untrusted display hints.

## Project limit templates

Project policies replace up to 64 named templates through
`GET/PUT /api/v1/projects/{project_id}/limit-templates`, with `keys`, manager
authority and ETag for writes. Templates cap RPM/TPM/concurrency and day/week/month
USD dimensions. Keys, groups and key/project end-user policies select
`limit_template`; null detaches it.

Each non-null template dimension is a ceiling: inline policy and digest overrides
may tighten it. Templates create no counters or aggregate owner; existing member
counters/history survive edits and detachment. Code mode keeps USD exemptions.
Unknown/unassigned references refuse; deleting a used template returns
`409 template_in_use`.

`projects[].limit_templates` and project end-user template references promote.
Omitted/null maps preserve; an explicit empty map clears unused templates.
Changed policy needs `keys` plus `configure`, and apply cannot strand local
key/group references. Digest overrides/blocks and keys/groups remain local.

## Limits by route

Key `route_limits` maps up to 100 slugs to RPM/TPM/concurrency and exact
day/week/month USD limits. Empty dimensions are unlimited; null/empty clears.
The policy grants no route access and supplements every other boundary.

All gateways/secret versions share each key/route counter. Fallback/retry cost
belongs to the original ingress route; classifiers use their own route.
Collections without one route use other limits. Code mode keeps USD exemptions;
system shadow/probe work is exempt from caller key limits.

Refusals use existing rate/budget errors and `param: route_limits.<slug>` where
supported. Oversized estimates return `request_exceeds_token_limit`; unknown
cost balances return 503. Enabling a cap reconstructs prior window spend;
clearing it preserves history. Policy stays local, with stable owners and
numeric windows in the aggregate budget ledger.

## Installation and project budgets

Aggregate USD budgets supplement organization, key, group, end-user, route,
attribution and supply caps. Shadow/paid probes count; code subscriptions do not.

`GET/PUT /api/v1/budgets/installation` needs global `read`/`settings` respectively.
`/api/v1/projects/{project_id}/budget` needs project visibility or `keys` plus
manager authority. Writes use ETag and explicit `policy`/null, with exact-decimal
`daily_cost_limit`, `weekly_cost_limit`, `monthly_cost_limit` and `cost_basis`,
which is `cost` (default, what the provider bills) or `charge` (what the
consumer owes under its rate card; see [pricing](pricing.md#rate-cards-and-charge)).
Reports show live/retained spend and monthly unpriced attempts on the enforced
basis. Supply-side caps — spend caps, connection limits and slot limits —
always measure provider cost and take no basis.

Every boundary reserves before dispatch and refunds on later refusal. Exhaustion
returns `429 budget_exhausted`; unknown balances return 503 until reconstruction.
Settlement replaces estimates with actual cost. Missing/stale pricing reserves
nothing; unpriced attempts accrue zero USD, so these are not invoice caps.
Calendar windows and stable ledger owners survive cap removal.

Portable project `budget` omission/null clears. `installation_budget` omission/
null preserves, while an empty object clears. Changed project caps need `keys`;
installation caps need `settings`, in addition to `configure`.

## Weekly budget windows

`weekly_cost_limit` applies independently to keys, groups, templates, aggregate,
end-user, key-route and attribution budgets. ISO weeks start Monday at local
midnight and may cross months/years. Supply connection/slot/route budgets retain
day/month dimensions; subscription USD exemptions remain.

Admission uses Valkey time, accrued spend and reservations. Exhaustion returns
429; unknown/stale/incomplete reconstruction returns 503. Incremental delivery
cannot initialize an unknown week; `weekly_complete` records reconstruction.
Reports expose `weekly`, notifications accept `window_kind: "week"` and limiter
metrics use `window="weekly"`. Unpriced counts remain monthly.

## Budget time zone

Set `budgets.time_zone` (default `UTC`) to an IANA zone in Settings or
`PUT /api/v1/settings/budgets.time_zone` with ETag and `{"value":"America/New_York"}`.
Day/week/month periods finish on their old schedule and switch independently at
the next boundary. Initial new-zone periods may shorten to align civil time.
Later edits replace pending transitions, not active/history windows.

Valkey time and one durable calendar govern admission, reconciliation, supply,
reports and notifications, including DST and fractional-hour boundaries.
Missing/expired evidence fails closed. `budget_calendar_changes` stores historical
zones/effective instants; hourly `budget_bucket` splits cost evidence when needed.
Subscription provider/token windows remain separate.

Portable `budget_time_zone` omission/null preserves; changes need `settings`
plus `configure`. Apply schedules against destination windows, never copies
source history or resets spend.

### Project attribution budgets

Replace a project's complete budget map at
`PUT /api/v1/projects/{project_id}/attribution-budgets` with `keys`, manager
authority and project ETag:

```json
{"budgets":{"team":{"core":{"weekly_cost_limit":"500"}}}}
```

At most 64 exact label/value pairs; each needs a positive day/week/month cap.
GET needs `read`/visibility. Resolved labels after pins/requirements select
independent shared project counters. Caller-selected labels can be omitted or
changed; [require and pin](#attribution-requirements-and-pinned-labels) allocations
when necessary. Other boundaries apply. Refusals identify
`attribution_budgets.<label>.<value>` where supported; unknown balances return 503.

Matching caller/classifier/shadow usage counts; unlabeled probes do not invent
labels. Background usage keeps original labels; code keeps USD exemptions.
Stable pair owners/history survive cap removal. Portable
`projects[].attribution_budgets` omission/null/empty clears; changes need `keys`
plus `configure`.

## Temporary budget increases

`POST /api/v1/budget-increases` adds exact USD to an existing day/week/month cap
at installation, organization, project, key, group, key-route, end-user or
attribution boundaries. Target a member, not its template; supply caps are separate.

Creation needs Idempotency-Key, a 1–256-character single-line reason and target
authority (`settings` for installation, organization manager authority for an
organization, or `keys`/project-manager access). Expiry defaults/clips to window
end; up to eight active increases add together per owner/window. Other budgets
still apply and unknown balances fail closed. No spend/reservation resets occur.

Valkey checks original window and expiry without waiting for authority refresh.
Cap edits retain the exception; cap removal makes it irrelevant. Key rotation
preserves ownership. GET is scope-filtered; DELETE needs ETag and creation
authority. Replays return the same increase without extending it.

Reports separate `effective_limit`, `temporary_increase` and
`increase_expires_at`; threshold notifications keep the permanent/template cap.
Rows retain audited target/amount/reason/expiry metadata after revocation.
Increases are destination-local and excluded from promotion.

## Workload JWT identity

Owners register `/api/v1/workload-issuers` with installation `access` authority.
Definitions declare immutable issuer/JWKS URLs, audiences, RS256/ES256/EdDSA,
60–86400-second maximum lifetime, disabled key IDs and ordered mappings.
Production trust uses independent public-HTTPS identity egress.

Exact claim matches at JSON pointers select the first matching fixed project,
limit template, route groups and `inference`/`models_read` scopes. Empty match
accepts otherwise valid issuer subjects. Provider-state permission defaults off;
optional end-user claims must use bounded identifier syntax. Claims cannot select
arbitrary projects or management authority. All attribution/budget policy applies.

Requests require valid signature, exact issuer/audience, subject, issued-at/expiry,
not-before when supplied and bounded lifetime. Tokens are at most 32 KiB.
Only public RSA 2048–8192-bit, P-256 and Ed25519 keys qualify; ambiguous/private/
symmetric keys and token-selected key URLs refuse. JWKS caches last one minute,
unknown-key refresh at most every 15 seconds and last-good fallback five minutes.
Disablement follows authority freshness; enabling/changing endpoints revalidates.
Disabled definitions do not resolve unavailable templates/groups until enabled.

An installation-separated issuer/subject HMAC registers a digest-only principal
with ordinary shared counters/resource ownership. No static secret exists.
Revocation uses the key API; mapping-owned policy cannot be edited/rotated there.
Existing principals cannot move mapping/project. JWTs, raw subjects and claims
stay in memory; persisted records contain configuration, digests and UUIDs.

Portable issuer definitions need installation `access` for export/plan/apply/
replay. Omitted/empty lists preserve; disable explicitly. Principals and signing
keys remain local. Realtime checks expiry/authority every five seconds;
renewed same-subject tokens may finish retained work. Code mode also requires
pool assignment and refuses concurrent issuer edits with `code_workload_changed`.

## Organizations

Organizations contain projects at one level. Manager/viewer memberships add
inherited access; direct grants/global access remain. Use assigned scope to
restrict people. Installation roles and token scopes still bound operations.
Project-limited tokens cannot administer parent organizations.

Global owners create organizations and become managers. Reads need `read`;
rename/membership/project administration/budgets need `manage_organization` and
manager authority. A token invoking global-owner authority also needs `access`.
Membership writes use organization ETag and protect the last manager
(`409 last_organization_manager`). New organization projects inherit managers
without adding permanent creator project grants.

Organization budgets use day/week/month caps, calendar/reservation/reconstruction
rules and `budget.organization` refusal evidence. All applicable hierarchy caps
apply, including system work; code remains USD-exempt. Organization temporary
increases use the same manager authority.

Portable names/budgets/project assignments exclude memberships. Creation/first
assignment needs `access`; changed budgets also need `manage_organization`.
Omitted organizations/assignments preserve. Once assigned, projects cannot move
(`409 project_organization_immutable`); first assignment includes existing spend.
Identity/membership metadata and aggregate ledger history follow owner lifetimes.

## SAML console sign-in

Owners configure `/api/v1/saml/configuration` with global `access` and ETag;
public trust reads need `access_read`. Metadata imports use bounded public HTTPS
identity egress without redirects or provider exceptions. Register displayed
SP entity ID, ACS and certificate with the IdP. Signing-key rotation invalidates
pending flows and requires IdP certificate updates.

Supported profile: SP-initiated RSA-SHA256 Redirect requests and POST responses
with exactly one independently signed, unencrypted assertion. Require stable
NameID, exact issuer/audience/recipient/destination/request binding, current
certificate, bounded times and bearer confirmation. Refuse SHA-1, wrapper-only
signatures, ambiguous XML IDs/entities, unsolicited login, artifacts, encrypted
assertions and single logout. Metadata permits one IdP role, one Redirect endpoint
and up to eight signing certificates. Refresh is explicit.

Choose email/groups/display-name claims and role mappings. Only SAML-managed
users follow mapped roles; linked local/SCIM/OIDC ownership stays unchanged.
Signed role loss revokes sessions even when login is denied. Email collisions
require explicit fresh-proof linking. Owner edits/unlink preserve recovery paths.

ACS POST creates no session. It validates/assertion-replay-checks, seals verified
facts and redirects to same-site completion. Completion requires the initiating
Secure/HttpOnly/SameSite-Lax cookie and one-use state. Five-minute flow/receipt
limits and configuration ETags bind both stages; return destinations stay local.
Disabling SAML revokes its sessions. Sensitive-action proofs require fresh IdP
authentication and exact user/session/subject, with purpose/resource one-use bounds.
SAML can authorize first local-MFA enrollment but cannot replace enrolled factors.

Portable public trust/mappings need `access` for export/plan/apply/replay;
omission/null preserves. Destination signing identity remains local. Keys/flows
use `saml_key`/`saml_flow` seals and replay/state/cookie HMACs. Raw assertions
never persist. Fixtures/browser checks cover this profile; production IdPs
require separate qualification.

### Budget refusal evidence

Cost refusals keep `budget_exhausted` and expose their boundary in messages and
supported parameters. Request history's `budget_boundary` is a fixed hierarchy
label without raw identifiers/label values. Unknown balances remain availability
failures, distinct from budget exhaustion and ordinary rate limits.

## Guardrail authority

The API-key `guardrails` scope admits `POST /v1/guardrails/apply` for a policy
revision in that key's project, subject to its request/token/concurrency limits.
It does not grant inference. Model evaluator calls also require the caller's
inference authority for the evaluator route. Console evaluation requires a
session, playground authority and project visibility. Definition and policy
writes require configuration authority; attachment writes additionally require
the existing management permission for their installation, organization,
project/route or key scope. All narrower attachments are additive.
