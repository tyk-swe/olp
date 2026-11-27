# Access control

Access control covers the people, projects, keys and delegated identities that can use or administer one trusted OLP installation. This guide defines the management API and policy behavior for identity, projects, API keys, management tokens, provisioning, settings and metadata-only audit records.

Initial installation belongs in [Deployment](../operate/deployment.md). File-backed secrets are covered in [Configuration](../operate/configuration.md#file-based-secrets), and master-key rotation and recovery belong in [Operations](../operate/operations.md#master-key-rotation-and-recovery).

## API key scopes, policies and task capabilities

Consumer API keys (`olp_`) carry one or more explicit scopes from `inference`, `models_read`, `guardrails`, `tools`, `agents`, `traces_write` and `delegate`. Scopes never imply access to a resource. Inference and discovery are further bounded by `allowed_routes` (up to 100 unique route slugs) and `allowed_route_groups` (up to 64 route groups). Project-owned keys can also grant `allowed_toolsets` and `allowed_agents` (up to 100 unique canonical UUIDs each of active published toolsets or A2A agents in the key's project) for the `tools` and `agents` scopes respectively.

The `traces_write` scope admits OTLP protobuf trace ingestion at `POST /v1/traces` (`application/x-protobuf` with optional `gzip` encoding, up to 1 MiB and 256 spans per batch) for the key's owning project. Trace ingestion requires an owning project and enforces a 60 requests-per-minute per-key trace ceiling alongside the key's and budget group's request-per-minute and concurrency limits.

Each key policy supports these additional controls through `POST /api/v1/api-keys` and `PATCH /api/v1/api-keys/{api_key_id}`.

| Field | Behavior |
| --- | --- |
| `cost_basis` | Selects `cost` (the default, measuring provider cost) or `charge` (measuring consumer charge under the applicable rate card) for daily, weekly and monthly cost limits. |
| `regional_limits` | Maps up to 64 region slugs to per-region `requests_per_minute`, `tokens_per_minute` and `max_concurrency` overrides. Omitted dimensions inherit default limits, cost accounts stay global, and project limit templates still impose ceilings. |
| `priority` and `max_priority` | Sets default admission queue class (`critical`, `high`, `normal` or `low`, defaulting to `normal`) and the highest class a request may select by header (defaulting to `priority`). `priority` cannot exceed `max_priority`. |
| `forbid_response_cache` | Defaults to `false`. Disables response cache lookup and storage for the key. On delegated calls, response caching is forbidden when either the delegating key or the workload agent sets this flag. |
| `run_limits` | Sets optional workflow run ceilings on `max_iterations` (1 to 32), `max_tool_calls` (0 to 128), `max_duration_seconds` (1 to 600), `max_concurrent_runs` (1 to 2147483647) and non-negative decimal `max_run_cost`. |
| `allow_provider_state` | Defaults to `false`. Permits retained provider-state resources such as files, batches and continuations. |
| `response_metadata` | Defaults to `false`. Opts the key into gateway metadata response headers. |

During A2A agent task execution, OLP mints short-lived task capability tokens (`olpt_`) bound to the calling principal, optional delegation, verified end-user digest and the pinned route and toolset revisions declared on the agent publication. A task capability token authorizes only `inference` and `tools` callbacks against those pinned revisions, cannot be used on other scopes, and rechecks live principal, delegation, end-user, expiry, pause and revocation authority on every callback attempt. See [Agents](agents.md) for A2A tasks, runs, toolsets and capability lifecycle.

## Attribution requirements and pinned labels

Keys use `required_attribution_keys`, `attribution_defaults` and `allowed_attribution_keys`. Project policies use `GET` and `PUT /api/v1/projects/{project_id}/attribution-policy`. Writes require `keys`, project-manager authority, the project ETag and an explicit `policy` field (or `null` to clear).

Requirements accumulate across project and key policies. Pinned defaults fill omitted labels, and callers may repeat them but cannot replace them (`400 pinned_attribution_override`). Unpinned labels still need key allowlist permission. Missing requirements return `missing_attribution`, while conflicting pins or more than four resolved labels return `invalid_attribution`. Use bounded machine tokens rather than secrets or request content.

These controls cover inference, media, retained resources, Bedrock, realtime and code mode, while model discovery is exempt. Realtime sessions recheck their original labels. Code refusals use the `code_` namespace, and a concurrent admission-policy change returns `409 code_attribution_changed`.

Resolved labels follow request, fact and hourly retention plus the separate code ledger. Project policies promote, and changing them requires both `configure` and `keys`. Key policies remain local. To impose spend limits on labels, configure [attribution budgets](#project-attribution-budgets).

## API-key network restrictions

Set `allowed_cidrs` to at most 64 IPv4 or IPv6 networks. An omitted, null or empty list is unrestricted. Host bits are masked, and equivalent duplicates or IPv4-mapped IPv6 ranges are refused. Use IPv4 notation for mapped addresses.

Client addresses are resolved using `OLP_TRUSTED_PROXY_CIDRS`, so untrusted peers cannot grant access through `X-Forwarded-For`. Denied or unparseable clients receive `403 ip_not_allowed` before body parsing or dispatch, including on model discovery and realtime handshakes. Established sessions recheck every five seconds, and code generations recheck during durable admission (`code_ip_not_allowed`). Key policies remain installation-local, while management restrictions are configured separately below.

## Management network restrictions

`OLP_MANAGEMENT_ALLOWED_CIDRS` and `--management-allowed-cidrs` accept up to 64 comma-separated IPv4 or IPv6 networks, and Helm uses `config.managementAllowedCidrs` on control pods. An empty setting is unrestricted, and malformed or IPv4-mapped IPv6 ranges fail startup.

The trusted-proxy resolver applies before authentication, parsing or admission. Denied clients receive `403 management_ip_not_allowed` for the console, static assets, setup, sign-in, callbacks, invitations, SCIM, OpenAPI and management paths. Allowed clients still need ordinary authentication, scopes and CSRF proof.

Changes require deployment updates and restarted control processes. Include operator and provisioning-client networks when configuring the list. Recover a lockout through the host or orchestrator because there is no API bypass. This setting stays outside configuration promotion, and capabilities report `management_network_restricted`.

## Route groups

Projects replace their group map through `GET` and `PUT /api/v1/projects/{project_id}/route-groups` or the project's page under **Projects** in the console. Writes require `keys`, project-manager authority and the project ETag.

```json
{"groups":{"production":["chat","embeddings"],"suspended":[]}}
```

Limits are 64 groups and 100 unique slugs per group. Group names follow route-slug syntax. Unknown future routes are allowed, while known routes must belong to the same project.

Keys grant the union of `allowed_routes` and `allowed_route_groups`. Every project route is allowed only when both lists are empty. Missing or empty referenced groups grant nothing. Deleting a group preserves key references, and recreating the name restores its members. Edits refresh authority without route publication. Discovery, simulation, retained access and realtime use the same boundary, while code admission checks membership transactionally.

`projects[].route_groups` promotes with portable slugs. Omitting the field or sending `null` or an empty map clears groups, and changed groups require `keys` in addition to `configure`. Keys remain local.

## API-key rotation and reminders

`POST /api/v1/api-keys/{api_key_id}/rotate` requires key-edit authority, `If-Match` and `Idempotency-Key`. `overlap_seconds` accepts 0 to 86400 seconds, and omitting it or passing 0 ends old overlaps on authority refresh. Positive overlap keeps the previous secret until its deadline or key expiry. At most eight old secrets may overlap (`key_overlap_limit`). Replays recover the same secret and deadline, and later rotations never extend earlier overlap deadlines.

All versions share the key's current permissions, stable rate and concurrency counters, cost owner and retained-resource identity. Revocation denies all versions. Expiry is checked even without refresh, and realtime closes expired versions on its five-second check. Admitted HTTP work keeps its original accounting.

The key detail page and `GET /api/v1/api-keys/{api_key_id}` show the last observed use of each active overlap. This covers new HTTP generation requests on the regular inference gateway that pass key and route admission, including cache hits. It does not cover Code Mode, session reauthentication or retained-resource calls. Recording happens in the background, so evidence can lag or be incomplete. **No use observed** does not prove all clients have switched.

If recording fails, the detail view shows **Observation unavailable (recording failed)** or marks the previous timestamp **May be stale (recording failed)**. A recording failure does not interrupt inference. The warning stays with that overlap even if a later use is recorded.

`rotation_interval_days` (1 to 3650, or `null` to clear) schedules reminders from creation or last rotation without rotating or expiring the key. Reads expose `rotation_due_at` and public `active_overlaps`. Subscribe to `key.expiring` with subject kind `api_key` and matching project scope. Workers claim one reminder per rule, key, reason and date when due within 24 hours or overdue, cancel superseded pending reminders and recheck authority before sending. In-flight sends cannot be withdrawn. See [notifications](../operate/observability.md#notifications).

Old secrets persist only as purpose-separated HMACs with public lookup and deadline metadata, and replay replies are sealed for 24 hours. Keys and reminders stay local.

## Pausing an API key

`POST /api/v1/api-keys/{api_key_id}/pause` and `POST /api/v1/api-keys/{api_key_id}/resume` take the same key-edit authority, `If-Match` and `Idempotency-Key` as rotation, and both are audited. The pause sits on the logical key, so every current and overlap secret stops admitting new work (`403 api_key_paused`) as soon as a replica installs the authority generation returned by the pause response. A replica still on an older proven-fresh generation may admit until its next poll, and the 60-second authority freshness deadline then fails closed.

A saved pause therefore does not by itself prove fleet-wide enforcement. `GET /api/v1/api-keys/{api_key_id}/pause-rollout` reports the generation each serving replica that reported in the last 60 seconds has installed. It reports `enforced: true` only when the key is still paused, at least one serving replica has reported within the last 60 seconds, and every reported replica is at or past the pause generation. When zero serving replicas have reported in the last 60 seconds, `pause-rollout` reports `enforced: false`. The console shows the pause as enforced only when `enforced` is true. Requests already admitted finish, while established realtime sessions close at their next five-second recheck, as they do on revocation.

Pausing also applies to workload agents and delegated access. Pausing either a delegating API key or a workload agent principal refuses new delegation exchanges (`POST /v1/delegations`), member consent creation and exchange (`POST /api/v1/delegation-consents`), delegated gateway requests, new A2A agent admission and new `olpt_` task capability callbacks once the authority generation takes effect.

Rotation while paused follows the normal overlap rules and keeps the key paused, with overlap deadlines still running. Resume clears only the pause and admits secrets that are still valid under the usual expiry and revocation checks. Resuming an expired or revoked key is refused, and revocation wins over pause and resume. Pause is meant for stopping an integration temporarily, so rotate or revoke a key whose secret may be compromised instead.

## End-user identity

Keys select `end_user_source` as `"header"`, `"native"` or `null`. Header mode requires one `X-OLP-End-User` header. Native mode reads OpenAI `safety_identifier` (falling back to `user` only when `safety_identifier` is absent) or Anthropic `metadata.user_id`. Native mode requires JSON, so use the header for uploads, reads, realtime and other dialects. Model discovery is exempt.

Identifiers match `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`. Invalid, ambiguous or missing values refuse before dispatch without echoing them. The header stays local, while strict native fields preserve forwarding. Accounting stores only an installation- and project-separated `end_user` HMAC. Same-project keys correlate, and different projects do not. Because the caller supplies this identifier, treat it as attribution rather than authentication.

To find a known identifier, POST `{"identifier":"customer-123"}` to `/api/v1/api-keys/{api_key_id}/end-user` with read and project authority plus browser CSRF proof. Use `dimension=end_user` in reports, where absent identity appears as `unidentified`.

Key `end_user_policy` and project `GET` and `PUT /api/v1/projects/{project_id}/end-user-policy` support `defaults`, digest-keyed `overrides` and `blocked`. Project writes require `keys`, project-manager access and an ETag, and `{"policy":null}` clears the policy. A project policy requires every serving key in that project to select a source (`end_user_source_required`). Each boundary enforces RPM, TPM, concurrency and daily, weekly and monthly cost independently. Overrides replace all local defaults, so omitted dimensions are unlimited while template ceilings remain. Blocks win (`403 end_user_blocked`). Override and block lists each allow up to 256 lowercase SHA-256 digests. Realtime rechecks blocks and required identification.

Unknown cost balances fail closed until reconstruction. Dollar limits do not apply to subscriptions, although rate, token, concurrency and block policies still do. Code admission refuses changed identity with `409 code_end_user_changed`.

Only digests enter request, usage, hourly, pending-generation and code ledgers. Policy, account and window metadata follow their owning boundary while raw identifiers stay in memory. Project defaults promote, while digest overrides, blocks and key policies remain destination-local.

## Access and OIDC

Installation roles are `owner`, `operator`, `developer` and `viewer`. Role changes or member disablement revoke active sessions, and losing membership-management authority retires issued invitations. API keys require explicit revocation.

Locally managed accounts keep local roles when linking OIDC. OIDC-managed accounts retain external ownership through password changes or linking and synchronize roles on verified login or reauthentication. Verified loss of mappings deauthorizes even the last owner, revokes sessions, proofs and invitations, and blocks password bypass. A later mapped login restores external authorization unless the account was administratively disabled. Self-service unlink cannot remove a member's last usable authority source.

Local sign-in requires both `OLP_LOCAL_LOGIN_ENABLED` and `auth.local_login_enabled`. Password invitations are refused when local sign-in is unavailable, so use mapped OIDC provisioning instead. Email matching never implicitly links accounts. Owner configuration guards use privately retained verified claim inputs. Changing claim names needs another verified owner path or a usable local owner. Replacing or removing the client secret requires an active local-password owner and invalidates saved OIDC sign-in evidence, so keep local recovery active until a successful login with the new credential.

Configure the exact issuer, discovery endpoint, client ID, client secret and `<public origin>/api/v1/oidc/callback`. Token endpoint authentication supports advertised `client_secret_basic` and `client_secret_post` (defaulting to basic when omitted). Discovery uses independent identity egress with public HTTPS, no redirects, private addresses or URL credentials, and bounded bodies and waits. Provider egress exceptions cannot relax identity egress. Saving binds the issuer, endpoints and authentication method, so changed discovery metadata or legacy unbound configuration requires owner review and resave before secret exchange.

Single-use browser-bound flows pin configuration ETag, state, nonce, PKCE and the initiating session. ID tokens require verified email, signature, issuer, audience, expiry and nonce. Account collisions need explicit linking after recent authentication. Five-minute purpose- and resource-bound proofs are consumed once, and authentication-method changes rotate the session and revoke predecessors. Loopback HTTP issuers are never permitted, and OIDC endpoints must use HTTPS.

## Projects and budget groups

Members have installation roles plus either global or assigned-project scope. Assigned managers can write their projects and viewers can read them within their role permissions, while installation administration remains global. Keep at least one project manager. Organization memberships can add inherited access.

`/api/v1/projects` and `/api/v1/projects/{project_id}/members` manage project boundaries. Providers, routes and gateway keys share project ownership, and unassigned keys use only unassigned routes. Invisible resources return 404, while visible denied writes return 403. Empty-project deletion needs an ETag and `Idempotency-Key`, and retained foreign-key dependencies return `409 project_in_use`.

`/api/v1/budget-groups` manages shared limits within one project or the unassigned boundary. Creating or updating a budget group requires at least one limit (`requests_per_minute`, `tokens_per_minute`, `max_concurrency`, `daily_cost_limit`, `weekly_cost_limit` or `monthly_cost_limit`) or a `limit_template` reference. A positive cost limit is not required when a rate limit, concurrency limit or template is set. `cost_basis` (`cost` by default, or `charge`) selects whether cost limits enforce provider cost or consumer charge. Attach keys with `budget_group_id`, noting that membership changes do not reassign prior spend history. Group and individual key limits both apply, including [fail-closed initialization](../operate/operations.md#spend-budget-reconciliation). Writes require `keys` and project change authority.

Budget-group and notification-resource deletion requires an ETag and `Idempotency-Key`, and retained accounting or delivery dependencies return 409. Disable or detach resources when retained history prevents deletion. Successful deletion is audited and replayable.

## Key response metadata

The key policy `response_metadata` is off by default. Set it when creating a key, with `PATCH /api/v1/api-keys/{api_key_id}`, or in the console key form to opt the key into the `X-OLP-Attempts`, `X-OLP-Route-Revision`, `X-OLP-Provider` and `X-OLP-Cost` response headers described in [gateway](../develop/architecture.md#gateway-metadata). Callers address routes rather than upstreams, so the gateway names the provider that served a request only to keys that opt in. The gateway does not scrub what an upstream says about itself. Upstream rejection messages are relayed with credential values redacted regardless of policy and may still name the vendor or model. Rotation keeps the setting, and a change takes effect on each gateway's next authority refresh.

The remaining-allowance headers need no opt-in. A key with a requests-per-minute or tokens-per-minute limit receives the [rate-limit headers](../develop/architecture.md#rate-limit-headers) of its surface for the dimensions it limits, and a key with neither receives none.

## Management tokens and provisioning

Owner sessions create management tokens at `/api/v1/management-tokens`. Secrets (`olpm_`) are shown once and stored as HMACs with an expiry of at most 366 days. Delegable scopes are `read`, `access_read`, `access`, `settings`, `configure`, `keys`, `playground`, `usage`, `manage_projects` and `manage_organization`. Effective scopes intersect the creator's current role, active and external authorization, and project memberships. A creator who regains authority restores tokens that were not explicitly revoked.

Machine bearer requests omit Origin and CSRF checks, while browser traffic retains those checks. Omitted `project_ids` grants access to all projects allowed to the creator, and explicit lists narrow authority. Project-scoped tokens cannot perform installation administration or configuration promotion. `manage_projects` supports installation project automation under current global owner authority without granting token administration. Organization operations additionally require their documented manager or global-owner authority.

Certain operations are never delegable to management tokens and require a signed-in browser session. Owner-session-only operations are `manage_tokens` (management token administration), `manage_sessions` (listing and revoking other members' sessions), `local_login` (changing local password sign-in availability) and `manage_plugins` (installing, approving, permitting and uninstalling plugins, plus listing, reviewing, permitting and revoking unconfined MCP executables). Permitting unconfined plugins or unconfined MCP executables also requires recent `plugin_permit` proof. Member-session-only operations are `self` (managing one's own profile, password, sessions, linked identities and MFA factors) and `POST /api/v1/budget-increase-requests` (submitting a project budget increase request).

Revocation and expiry take effect immediately, and audit records attribute actions to the token. Configuration promotion needs `configure` plus permissions for changed pricing, settings, project key policies or installation identity policy. See [configuration](../operate/configuration.md#configuration-promotion-artifacts).

`PUT /api/v1/provisioning/{source}/users/{external_id}` accepts authoritative `email`, `display_name`, `role` and `active` fields under owner or installation `access` authority. It creates passwordless provisioned accounts or updates a mapping, revoking sessions on each update and retiring invitations on deactivation. Email collisions and removal of the last usable owner are refused. `DELETE` reconciles the account as inactive. The `scim` source name is reserved.

Ordinary owner edits transfer a provisioned account to local management while retaining its mapping. External reconciliation then refuses with `provisioning_ownership_changed` and cannot silently reclaim the account.

## SCIM provisioning

Use `/scim/v2` with an installation-wide `access` management bearer token whose creator is a currently authorized owner. Browser cookies and project-scoped tokens cannot administer SCIM. Management CIDRs and admission rules apply, and errors use RFC 7644 `application/scim+json`, including early refusals.

Users and Groups support `POST`, `GET`, `PUT`, `PATCH`, `DELETE`, `POST .search`, filters, sorting and attribute projection. Pagination is one-based with at most 200 items per page, and `count=0` returns totals only. Filter and path bounds are 4096 bytes and 16 nesting levels, requests are bounded to 128 KiB and 15 seconds, and `PATCH` permits up to 32 atomic ordered operations. Filter values bind as SQL parameters. Attribute names ignore case while identity values are exact. Resource weak ETags support optional weak or strong `If-Match`, returning 412 on stale writes. Discovery documents declare the supported surface, and bulk, nested groups and password provisioning are unavailable.

`userName` is a unique email address. New users default to active passwordless viewers with assigned scope, and email matching never adopts local accounts. User OLP extensions set base role and scope. `DELETE` deactivates the user, removes inherited memberships and revokes sessions while retaining identity and history. Local takeover prevents SCIM reclaim.

Groups hold at most 1000 existing SCIM-managed users. Their OLP extension maps role, scope and up to 100 project manager or viewer grants. The highest role, global grant and unioned project memberships apply, and removing a group removes only inherited grants. Reconciliation revokes affected sessions and protects the last usable owner.

The **Directory sync** tab under **Identity** in the console and `/api/v1/scim/groups/{scim_id}/mapping` edit group policy under an ETag. Directory `PUT` omission preserves mappings, while an explicit extension replaces them. Portable `scim_group_mappings` use group names and destination project IDs. Missing groups are created empty while memberships and IDs stay local. Omission preserves policy, and explicit empty grants clear it. Artifacts containing mappings need installation `access` for export, plan, apply and replay, and unchanged mappings do not revoke sessions.

Identity metadata, group metadata and ownership tombstones follow identity lifecycle, while audit records follow audit retention. No passwords, bearer tokens or directory request bodies persist. Fixtures cover the supported profile, and external directory conformance requires separate qualification.

## Local multi-factor authentication

Profile supports one TOTP app and up to ten total factors, including WebAuthn security keys. Enrollment proves possession. First enrollment requires recent password or linked OIDC or SAML authentication, while later changes need an enrolled factor or recovery code. Automation tokens cannot manage personal factors.

TOTP uses six digits, SHA-1, 30-second periods and plus-or-minus one period of skew. Accepted counters cannot replay across replicas. WebAuthn checks exact origin and RP ID, challenge, user verification and signature counters, refusing clone warnings without identifying attestation. Use HTTPS DNS origins or `http://localhost` because IP and insecure origins do not offer WebAuthn. Hostname changes may require re-enrollment.

Password login with MFA returns a five-minute HTTP 202 challenge completed at `POST /api/v1/auth/mfa/verify`. Challenges bind account, MFA revision and session, allow five failures plus source and account throttles, and recheck login availability. Recent-authentication proofs also wait for the factor, and browser CSRF still applies.

First enrollment returns ten one-use 128-bit recovery codes once, and replacement invalidates the old set. Lost enrollment replies are not replayable, so use an enrolled factor to replace recovery codes. Successful enrollment rotates session and CSRF tokens and revokes other sessions. Removal also revokes other sessions, and removing the last factor clears recovery codes and authentication strength. Changes use `If-Match`, fresh proof and metadata-only audit.

`auth.mfa_required=true` blocks unverified local sessions and gives unenrolled password users a restricted enrollment challenge. The last factor cannot be removed while MFA is required. OIDC and SAML sessions rely on their identity provider's policy. Portable `require_local_mfa` changes need `access` plus `configure`, while factors and proofs remain local.

Factor secrets use `mfa_totp` and `mfa_webauthn` seals, while recovery and control tokens use purpose-separated HMACs. Pending challenges and enrollment secrets expire within five minutes. Back up the database and master-key ring together, and use offline recovery below if locked out.

## Account recovery

`olp account reset-password EMAIL PASSWORD_FILE [--reset-mfa] [flags]` is an offline operator command that resets the password of an existing active account. The password file must be a regular file readable only by the operator (no group or other permission bits), at most 4097 bytes and valid UTF-8. One trailing line ending is removed and the usual 12 to 1024 character policy applies. A successful recovery invalidates all sessions and recent-authentication grants for the account, leaves role, active state, lifecycle ownership and OIDC authorization untouched, and writes a system-attributed `user.password_recover` audit record.

Ordinary recovery preserves enrolled MFA. The explicit `--reset-mfa` (or `-reset-mfa`) option removes factors, pending enrollment secrets and recovery codes, and records `user.mfa_recover`. It leaves `auth.mfa_required` intact, so the next local sign-in must bootstrap a new factor when MFA is required. Pass `--reset-mfa` anywhere among the configuration flags after `PASSWORD_FILE` and before `--`. The command prints only the account ID, email and `password_recovered` and `mfa_reset` flags without echoing the password. A missing or inactive account fails with a generic recovery error that does not confirm whether the address exists.

## Mutation and audit boundaries

The [management contract](../../management/management.json) declares each operation's authority. Protected writes reauthorize inside feature transactions, and access changes serialize ownership checks with the installation lock. Expensive hashing, discovery and verification run outside that lock.

Versioned updates use strong quoted `If-Match` headers and return 412 when stale. Replay-protected writes bind `Idempotency-Key` to actor, method, path, precondition and body, and reauthorize before returning a sealed 24-hour reply. Lists use validated cursors with at most 200 rows. Confirmed writes stay complete even when a subsequent refresh fails.

Public authentication uses fixed one-minute PostgreSQL admission windows allowing 10,000 requests per action globally, 60 per resolved source (30 for invitations), five per source and target pair, and 30 per known target. HMAC targets and sources plus capped counters prevent denied attempts from extending windows. Trust only actual proxies in `OLP_TRUSTED_PROXY_CIDRS` because forwarded chains are walked from the right.

Audit retains actor, resource, action, outcome, timestamp, peer IP and coarse user-agent family, never hashes, ciphertext, raw claims or full user agents. Passwords use Argon2id (64 MiB and three iterations), lookups use purpose-separated HMACs, and secrets and cached replies use record-bound AES-256-GCM. See [security](../develop/security.md).

## Console authentication

Set `OLP_PUBLIC_ORIGIN` to the exact HTTPS browser origin (loopback development may use HTTP). Unsafe browser requests need that `Origin`, and authenticated writes also need `X-CSRF-Token`. Session and recent-auth cookies are `HttpOnly`, `Secure`, `Path=/` and `SameSite=Lax`. APIs send `no-store`, and health and metrics endpoints stay private.

Session and capability verification has a ten-second body-inclusive deadline. Passive failures retain page content with a Retry action and block writes until verification succeeds. Rotation invalidates older replies and sibling tabs without sharing credentials. `csrf_invalid` triggers verification and explicit review or retry rather than automatic mutation replay. Other 403 responses do not log the member out.

OIDC failures redirect with allowlisted reasons, and retrying creates a fresh flow. Recent authentication can use a linked identity provider even after password enrollment. Session inventory's **Last session verification** updates at most once a minute, and coarse device labels are untrusted display hints.

## Project limit templates

Project policies replace up to 64 named templates through `GET` and `PUT /api/v1/projects/{project_id}/limit-templates`, requiring `keys`, project-manager authority and an ETag for writes. Templates cap RPM, TPM, concurrency and daily, weekly and monthly USD dimensions. Keys, budget groups and key or project end-user policies select `limit_template`, and `null` detaches it.

Each non-null template dimension acts as a ceiling that inline policy and digest overrides may tighten. Templates create no counters or aggregate owner, so existing member counters and history survive edits and detachment. Code mode keeps USD exemptions. Unknown or unassigned references are refused, and deleting a used template returns `409 template_in_use`.

`projects[].limit_templates` and project end-user template references promote. Omitting the map or sending `null` preserves existing templates, while an explicit empty map clears unused templates. Changed policy needs `keys` plus `configure`, and apply cannot strand local key or group references. Digest overrides, blocks, keys and budget groups remain local.

## Limits by route

Key `route_limits` maps up to 100 route slugs to RPM, TPM, concurrency and exact daily, weekly and monthly USD limits. Empty dimensions are unlimited, and `null` or an empty map clears the limits. This policy grants no route access and supplements every other boundary.

All gateways and secret versions share each key and route counter. Fallback and retry cost belongs to the original ingress route, while classifiers use their own route. Collections without a single route use other limits. Code mode keeps USD exemptions, and system shadow or probe work is exempt from caller key limits.

Refusals use existing rate and budget errors and include `param` set to `route_limits.<slug>` where supported. Oversized estimates return `request_exceeds_token_limit`, and unknown cost balances return 503. Enabling a cap reconstructs prior window spend, while clearing it preserves history. Policy stays local with stable owners and numeric windows in the aggregate budget ledger.

## Installation and project budgets

Aggregate USD budgets supplement organization, key, group, end-user, route, attribution and supply caps. Shadow and paid probes count, while code subscriptions do not.

`GET` and `PUT /api/v1/budgets/installation` need global `read` and `settings` respectively. `/api/v1/projects/{project_id}/budget` needs project visibility for reads or `keys` plus project-manager authority for writes. Writes use an ETag and an explicit `policy` object or `null`, with exact-decimal `daily_cost_limit`, `weekly_cost_limit`, `monthly_cost_limit` and `cost_basis`. The `cost_basis` field is `cost` (the default, what the provider bills) or `charge` (what the consumer owes under its rate card, as covered in [pricing](../operate/pricing.md#rate-cards-and-charge)). Reports show live and retained spend plus monthly unpriced attempts on the enforced basis. Supply-side caps, including spend caps, connection limits and slot limits, always measure provider cost and do not take a consumer charge basis.

Every boundary reserves before dispatch and refunds on later refusal. Exhaustion returns `429 budget_exhausted`, and unknown balances return 503 until reconstruction completes. Settlement replaces estimates with actual cost. Missing or stale pricing reserves nothing and unpriced attempts accrue zero USD, so these limits do not act as exact invoice caps. Calendar windows and stable ledger owners survive cap removal.

Portable project `budget` omission or `null` clears the budget. `installation_budget` omission or `null` preserves the existing budget, while an empty object clears it. Changed project caps need `keys`, and changed installation caps need `settings`, in addition to `configure`.

## Weekly budget windows

`weekly_cost_limit` applies independently to keys, groups, templates, aggregate, end-user, key-route and attribution budgets. ISO weeks start Monday at local midnight and may cross months or years. Supply connection, slot and route budgets retain day and month dimensions, and subscription USD exemptions remain.

Admission uses Valkey time, accrued spend and active reservations. Exhaustion returns 429, while unknown, stale or incomplete reconstruction returns 503. Incremental delivery cannot initialize an unknown week, and `weekly_complete` records reconstruction completion. Reports expose `weekly`, notifications accept `window_kind` set to `"week"`, and limiter metrics use `window="weekly"`. Unpriced counts remain monthly.

## Budget time zone

Set `budgets.time_zone` (default `UTC`) to an IANA zone in **Settings** or through `PUT /api/v1/settings/budgets.time_zone` with an ETag and `{"value":"America/New_York"}`. Day, week and month periods finish on their old schedule and switch independently at the next boundary. Initial new-zone periods may shorten to align civil time. Later edits replace pending transitions rather than active or historical windows.

Valkey time and one durable calendar govern admission, reconciliation, supply, reports and notifications, including DST and fractional-hour boundaries. Missing or expired evidence fails closed. `budget_calendar_changes` stores historical zones and effective instants, and hourly `budget_bucket` splits cost evidence when needed. Subscription provider and token windows remain separate.

Portable `budget_time_zone` omission or `null` preserves the existing zone, and changes need `settings` plus `configure`. Apply schedules against destination windows without copying source history or resetting spend.

### Project attribution budgets

Replace a project's complete budget map at `PUT /api/v1/projects/{project_id}/attribution-budgets` with `keys`, project-manager authority and the project ETag.

```json
{"budgets":{"team":{"core":{"weekly_cost_limit":"500"}}}}
```

A project can configure at most 64 exact label and value pairs, and each pair needs a positive day, week or month cap. `GET` needs `read` and project visibility. Resolved labels after pins and requirements select independent shared project counters. Caller-selected labels can be omitted or changed, so [require and pin](#attribution-requirements-and-pinned-labels) allocations when necessary. Other boundaries still apply. Refusals identify `attribution_budgets.<label>.<value>` where supported, and unknown balances return 503.

Matching caller, classifier and shadow usage counts, while unlabeled probes do not invent labels. Background usage keeps original labels, and code mode keeps USD exemptions. Stable pair owners and history survive cap removal. Portable `projects[].attribution_budgets` omission, `null` or empty maps clear budgets, and changes need `keys` plus `configure`.

Use the reserved `delegation_pair` budget key to set a shared limit for an agent and its delegating principal. The value is the `delegation_pair_digest` returned when a member consents or an agent obtains a delegation grant. For example, replace the complete budget map with the pair ID and the desired limit.

```json
{"budgets":{"delegation_pair":{"PAIR_BUDGET_ID":{"daily_cost_limit":"50"}}}}
```

The gateway derives the pair ID from the authenticated principals and rejects it in caller attribution. Pair limits apply alongside key, agent and other attribution limits. A member and agent keep the same pair ID across consent renewals and replacement keys. Removing a limit does not erase recorded spend.

## Temporary budget increases

`POST /api/v1/budget-increases` adds exact USD to an existing day, week or month cap at installation, organization, project, key, group, key-route, end-user or attribution boundaries. Target a member directly rather than its template, and manage supply caps separately.

Creation needs `Idempotency-Key`, a 1 to 256 character single-line reason and target authority (`settings` for installation, organization-manager authority for an organization, or `keys` and project-manager access). Expiry defaults and clips to the end of the window, and up to eight active increases add together per owner and window. Other budgets still apply and unknown balances fail closed. No spend or reservation resets occur.

Valkey checks the original window and expiry without waiting for authority refresh. Cap edits retain the exception, while cap removal makes it irrelevant. Key rotation preserves ownership. `GET` is scope-filtered, and `DELETE` needs an ETag and creation authority. Replays return the same increase without extending it.

Reports separate `effective_limit`, `temporary_increase` and `increase_expires_at`, while threshold notifications keep the permanent or template cap. Rows retain audited target, amount, reason and expiry metadata after revocation. Increases are destination-local and excluded from configuration promotion.

### Requesting an increase

A signed-in member with a browser session who can see a project can ask for one increase against that project budget with `POST /api/v1/budget-increase-requests`, giving the project, the day, week or month window, an exact amount and a reason. Management tokens cannot submit this request. The request binds to the project budget, its current window and its base cap at that moment, and asking never changes serving limits. Project members submit and review requests from **Budgets -> Increase requests** (`/budgets/increases`) in the console.

Approving or declining takes the `keys` operation and project-manager access, the same authority as granting the increase directly, through `POST /api/v1/budget-increase-requests/{request_id}/approve` and `POST /api/v1/budget-increase-requests/{request_id}/decline`. Nobody can approve their own request, installation admins included, while direct grants keep their existing permission and audit trail.

Approval records the decision and creates one normal increase in the same audited transaction, serialized per budget and window together with direct grants, after rechecking the base cap, the window, the eight active increase limit and the request state. Distinct approved requests add together, and a repeat approval returns the recorded decision without granting again. A changed base cap or a rolled window returns `budget_increase_request_stale` and needs a new request, while a full window returns the usual `budget_increase_limit` conflict with no grant and the request left pending. Pending requests read as expired once their window ends, and pending, declined and expired requests never change serving limits.

## Workload JWT identity

Owners register `/api/v1/workload-issuers` with installation `access` authority (up to 64 issuers per installation). Definitions declare an immutable `issuer` URL, a mutable `jwks_url`, audiences, `RS256`, `ES256` or `EdDSA` algorithms, a 60 to 86400 second maximum lifetime, disabled key IDs and ordered mappings (1 to 32 per issuer). Production trust uses independent public-HTTPS identity egress. Updating an issuer through `PUT /api/v1/workload-issuers/{issuer_id}` cannot change `issuer`, while updating `jwks_url` or enabling a disabled issuer revalidates the JWKS endpoint outside the write lock before saving.

Exact claim matches at JSON pointers select the first matching mapping, which pins a fixed project, limit template, consumer scopes and optional resource allowlists. Mappings support all seven consumer scopes (`inference`, `models_read`, `guardrails`, `tools`, `agents`, `traces_write` and `delegate`), plus `allowed_toolsets`, `allowed_agents` and `agent_principal`. `route_groups` is required only when `inference` or `models_read` is selected. An empty `match` map accepts otherwise valid subjects from the issuer. Provider-state permission defaults off, and optional `end_user_claim` values must use bounded identifier syntax. Claims cannot select arbitrary projects or management authority, and all attribution and budget policies apply.

Requests require a valid signature, exact issuer and audience, subject, issued-at and expiry timestamps, not-before when supplied, and a bounded token lifetime. Tokens are at most 32 KiB. Only public RSA 2048 to 8192 bit, P-256 and Ed25519 keys qualify, and ambiguous, private or symmetric keys and token-selected key URLs are refused. JWKS caches last one minute, unknown-key refreshes run at most once every 15 seconds, and last-good fallback lasts five minutes. Disablement follows authority freshness, and disabled definitions do not resolve unavailable templates, route groups or agent resources until enabled.

An installation-separated issuer and subject HMAC registers a digest-only principal with ordinary shared counters and resource ownership, with no static secret. Revocation and pausing use the API key endpoints, while mapping-owned policy cannot be edited or rotated there. Existing principals cannot move to another mapping or project. JWTs, raw subjects and claims stay in memory, while persisted records contain configuration, digests and UUIDs.

Portable issuer definitions need installation `access` for export, plan, apply and replay. Omitted or empty lists preserve existing issuers, so disable explicitly. Principals and signing keys remain local. Realtime checks expiry and authority every five seconds, and renewed same-subject tokens may finish retained work. Code mode also requires pool assignment and refuses concurrent issuer edits with `code_workload_changed`.

### Delegating an API key to a workload agent

To approve an agent, set `agent_principal: true` on its workload mapping and provide an exact `/sub` match together with an exact `/client_id`, `/azp` or `/appid` match. The principal is bound to the issuer, subject and approved client claims.

An agent and an API key in the same project can request a delegated credential with `POST /v1/delegations`. The `Authorization` header carries the agent's workload JWT and `X-OLP-Delegating-Key` carries the API key. The key needs the `delegate` scope, and both principals need each requested `inference`, `tools` or `agents` scope. The JSON body supplies `scopes` along with a nonempty `allowed_routes` list for `inference`, a nonempty `allowed_toolsets` list for `tools`, and a nonempty `allowed_agents` list for `agents`. Toolsets and agents use their published UUIDs. The optional `ttl_seconds` defaults to five minutes and cannot exceed fifteen minutes, and the credential also expires when either principal expires.

A project member with change access can instead approve an agent and key in the console or through `POST /api/v1/delegation-consents` after recent identity verification. The returned one-use consent (`olpc_`) can be exchanged at `POST /v1/delegations` in `X-OLP-Delegation-Consent` for no more than its approved scopes, routes, toolsets and agents. `GET /api/v1/delegation-options` lists eligible agents and their currently shared resources for a key. `GET /api/v1/agent-delegations` returns a project-filtered, cursor-paginated list of delegation records and pending member consents (with optional `project_id` and `delegation_id` filters), including expiry, consumption and revocation timestamps, member authorization status, and pair budget policy and live spend, without returning tokens, JWTs or keys.

The delegation response contains a bearer token (`olpd_`), `expires_at` and `delegation_pair_digest`. Send the token as the bearer credential and the current agent workload JWT in `X-OLP-Agent-Identity`. One token authorizes one inference request, one JSON tool call, one MCP session or one A2A send. A session reuses the same token and agent identity until expiry. A renewed delegation for the same agent and caller can read or cancel its A2A task. A project operator with change permission can also use `GET /api/v1/a2a-tasks/{task_id}/remote` and `POST /api/v1/a2a-tasks/{task_id}/cancel` to contact the pinned remote agent. Those actions use the original task credential and cannot use a revoked OAuth grant or send it to a replacement endpoint.

Delegated credentials cannot be used to delegate again. Both principals must continue to authorize the resources, and revoking or pausing either the delegating key or the workload agent stops new delegation exchanges, member consents, delegated calls and A2A agent admission. Caller, agent and applicable member and pair budgets apply together, and renewed credentials retain pair spend. The delegating key remains the accounting owner while each attempt records the agent and a stable pair digest, along with the member for member-consented calls. Grant records contain credential digests and bounds rather than the JWT, key or raw claims.

Only calls observed through OLP are counted. OLP cannot count or stop hidden iterations performed inside a remote agent, and an unavailable remote agent can leave cancellation outcomes unknown.

## Organizations

Organizations contain projects at one level. Manager and viewer memberships add inherited access while direct project grants and global access remain. Use assigned scope to restrict people. Installation roles and token scopes still bound operations, and project-limited tokens cannot administer parent organizations.

Global owners create organizations and become managers. Reads need `read`, while rename, membership, project administration and budgets need `manage_organization` and manager authority. A token invoking global-owner authority also needs `access`. Membership writes use the organization ETag and protect the last manager (`409 last_organization_manager`). New organization projects inherit managers without adding permanent creator project grants.

Organization budgets use daily, weekly and monthly caps, standard calendar, reservation and reconstruction rules, and `budget.organization` refusal evidence. All applicable hierarchy caps apply, including system work, while code mode remains USD-exempt. Organization temporary increases use the same manager authority.

Portable organization names, budgets and project assignments exclude memberships. Creation and first assignment need `access`, and changed budgets also need `manage_organization`. Omitted organizations and assignments preserve existing state. Once assigned, projects cannot move (`409 project_organization_immutable`), and first assignment includes existing spend. Identity metadata, membership metadata and aggregate ledger history follow owner lifetimes.

## SAML console sign-in

Owners configure `/api/v1/saml/configuration` with global `access` and an ETag, while public trust reads need `access_read`. Metadata imports use bounded public HTTPS identity egress without redirects or provider exceptions. Register the displayed SP entity ID, ACS URL and certificate with the IdP. Signing-key rotation invalidates pending flows and requires IdP certificate updates.

The supported profile covers SP-initiated RSA-SHA256 Redirect requests and POST responses with exactly one independently signed, unencrypted assertion. Responses require a stable NameID, exact issuer, audience, recipient, destination and request binding, a current certificate, bounded times and bearer confirmation. OLP refuses SHA-1, wrapper-only signatures, ambiguous XML IDs or entities, unsolicited login, artifacts, encrypted assertions and single logout. Metadata permits one IdP role, one Redirect endpoint and up to eight signing certificates, and refresh is explicit.

Choose email, groups and display-name claims plus role mappings. Only SAML-managed users follow mapped roles, while linked local, SCIM or OIDC ownership stays unchanged. Signed role loss revokes sessions even when login is denied. Email collisions require explicit fresh-proof linking, and owner edits or unlinking preserve recovery paths.

ACS POST creates no session. It validates the assertion, checks assertion replay, seals verified facts and redirects to same-site completion. Completion requires the initiating `Secure`, `HttpOnly`, `SameSite=Lax` cookie and one-use state. Five-minute flow and receipt limits and configuration ETags bind both stages, and return destinations stay local. Disabling SAML revokes its sessions. Sensitive-action proofs require fresh IdP authentication and exact user, session and subject matching with purpose- and resource-bound one-use limits. SAML can authorize first local-MFA enrollment but cannot replace enrolled factors.

Portable public trust and mappings need `access` for export, plan, apply and replay, and omitting them or passing `null` preserves existing state. Destination signing identity remains local. Keys and flows use `saml_key` and `saml_flow` seals plus replay, state and cookie HMACs, and raw assertions never persist. Fixtures and browser checks cover this profile, while production IdPs require separate qualification.

### Budget refusal evidence

Cost refusals keep `budget_exhausted` and expose their boundary in messages and supported parameters. Request history's `budget_boundary` is a fixed hierarchy label without raw identifiers or label values. Unknown balances remain availability failures, distinct from budget exhaustion and ordinary rate limits.

## Guardrail authority

The API-key `guardrails` scope admits `POST /v1/guardrails/apply` for a policy revision in that key's project, subject to its request, token and concurrency limits. It does not grant inference. Model evaluator calls also require the caller's inference authority for the evaluator route. Console evaluation requires a session, playground authority and project visibility. Definition and policy writes require configuration authority, while attachment writes additionally require the existing management permission for their scope (`settings` for `installation`, `manage_organization` for `organization`, `configure` for `project`, `route` or `toolset`, and `keys` for `api-key`). All narrower attachments are additive.

## Provider batch resources

Files, batches and cloud batch jobs are retained provider-state resources. Creation pins one published route target, provider revision, slot and credential version, and does not fail over or migrate later. Creation requires an allowed route and `X-OLP-Route` when the request does not carry a model selector. Follow-up access uses the original API-key and project owner plus the retained resource ID, then rechecks current route and credential authority. Foreign, expired, deleted or otherwise unavailable IDs are indistinguishable from missing ones. Strict routes refuse non-OpenAI and non-Azure files and batches with `422 target_capability`.
