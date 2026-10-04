# Access control

The control plane manages installation setup, identity, projects, API keys,
management tokens, provisioning, settings, and metadata-only audit records.
Administrators belong to one trusted installation. See
[deployment](deployment.md) for installation,
[configuration](configuration.md#file-based-secrets) for secret files, and
[operations](operations.md#master-key-rotation-and-recovery) for key rotation.

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
and route changes require `configure`. Requests carrying an `olpm_` bearer
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
push-based reconciliation contract, not a polling or SCIM implementation.

Lifecycle ownership transfers deliberately in one direction. When an owner
changes a provisioned member's role or active state through ordinary user
administration, the account becomes locally managed in the same transaction
while its provisioning mapping is retained as the ownership record; a later
external reconciliation for that identity is refused with
`provisioning_ownership_changed` rather than silently reclaiming the account.

## Account recovery

`olp account reset-password EMAIL PASSWORD_FILE [flags]` is an offline operator
command that resets the password of an existing active account. The password
file must be a regular file readable only by the operator (no group or other
permission bits), at most 4097 bytes, and valid UTF-8; one trailing line ending
is removed and the usual 12–1024 character policy applies. A successful recovery
invalidates all sessions and recent-authentication grants for the account,
leaves role, active state, lifecycle ownership, and OIDC authorization
untouched, and writes a system-attributed `user.password_recover` audit record.
The command prints only the account id, email, and a `password_recovered` flag;
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

### OIDC discovery binding

Saving enabled OIDC configuration binds its validated authorization, token and
JWKS endpoints and token authentication method. Sign-in rejects rediscovery
changes before reading or sending the client secret. An owner must review the
identity provider and save the configuration again to accept such a change.
After upgrading an installation without this binding, retain a local owner
sign-in or an existing owner session to perform that save before relying on
OIDC sign-in.
