# Security architecture

This page describes how OpenLLMProxy protects an installation: the boundaries
it defends, who can call it, how each call is authorized, how secrets are held,
where it may connect, and what every public response carries. Each rule lives
in one module, named in the sections below, and a test holds the rule and this
page together. Report suspected vulnerabilities as [SECURITY.md](../SECURITY.md)
describes.

## Trust boundaries

| Boundary | What crosses it | How it is protected |
| --- | --- | --- |
| Edge → public listener | Console, management API, and inference traffic | The edge terminates TLS. OLP classifies each path's surface (`internal/surface`), admits it from that surface's pool, and sets the [perimeter headers](#public-perimeter). Forwarded client addresses are trusted only through `OLP_TRUSTED_PROXY_CIDRS`. `OLP_MANAGEMENT_ALLOWED_CIDRS` optionally restricts management and console clients before admission and authentication. |
| Private listener | Health probes and metrics | Unauthenticated by design. Bind it to a private interface and restrict it with the chart's NetworkPolicy. |
| OLP → PostgreSQL | All durable state | Bearer credentials are stored only as digests and other secrets only sealed by the master key ring; see [secrets](#secrets). |
| OLP → Valkey | Limits, routing hints, accounting events | Carries metadata, never credentials or content. Installations sharing one Valkey stay isolated by namespace. |
| OLP → providers | Inference and management calls | [Provider egress](#egress) validates every destination and pins every dial; provider errors are scrubbed of the credentials OLP sent. |
| OLP → identity provider | OIDC discovery/tokens/keys; declared SAML metadata | Identity egress applies the provider denylist with no operator exceptions. |
| Operator → CLI | Password recovery, key rotation, `doctor` | Requires the secret files and database access; secret files must not be readable by others. |

## Principals

| Principal | Credential | Verified by | Reaches |
| --- | --- | --- | --- |
| Member | The `__Host-olp_session` cookie, with a CSRF proof on unsafe methods and an allowed `Origin` | `access.Server.Authenticate` | The management operations of its role, within its [access scope](#project-boundaries) |
| Management token | `Authorization: Bearer olpm_…` | `access.Server.Authenticate` | Its delegated operations that its creator can still perform |
| API key | The location its SDK uses; see [gateway keys](#gateway-keys) | `gateway/credentials.go` | Inference and model reads on its permitted routes within one project boundary |
| Bootstrap token | `X-OLP-Setup-Token` | `access` setup | Creating the first owner, once |

## Management authorization

The [management contract](../openapi/management.json) declares every
operation's security requirement: the management operations a session or a
management token needs, and whether it needs an installation-wide scope.
`access.Route`, `Stream`, and `Public` mount each route with that requirement,
so the caller is admitted before any handler code runs, and a route the
contract does not declare cannot be mounted. Handlers may demand more with
`Principal.Authorize`, such as the operation a record's project implies, never
less. Mutations call `Reauthorize` inside their transaction, after the
installation lock, so they commit only under authority that is still current.

`internal/access/policy.go` decides who holds each operation:

<!-- operations -->
| Operation | Owner | Operator | Developer | Viewer | Installation-wide | Management tokens |
| --- | --- | --- | --- | --- | --- | --- |
| `read` | yes | yes | yes | yes |  | yes |
| `usage` | yes | yes | yes | yes |  | yes |
| `keys` | yes | yes | yes |  |  | yes |
| `playground` | yes | yes | yes |  |  | yes |
| `configure` | yes | yes |  |  |  | yes |
| `settings` | yes | yes |  |  | yes | yes |
| `access_read` | yes | yes |  |  | yes | yes |
| `access` | yes |  |  |  | yes | yes |
| `self` | yes | yes | yes | yes |  |  |
| `manage_sessions` | yes |  |  |  | yes |  |
| `manage_tokens` | yes |  |  |  | yes |  |
| `manage_projects` | yes |  |  |  | yes | yes |
| `local_login` | yes |  |  |  | yes |  |
| `manage_plugins` | yes |  |  |  | yes |  |
| `manage_organization` | yes | yes | yes |  |  | yes |
<!-- /operations -->

`internal/access/testdata/authorization.golden.json` records which callers
every route admits. The authorization sweep calls every route as each of those
callers against a real process, and the console's authorization is tested
against the same file, so the console offers exactly what the server admits.

## Project boundaries

Providers, routes, API keys, budget groups, and notification destinations and
rules belong to a project or to the unassigned boundary, and media jobs and
retained resources to their API key's. An installation-wide
principal reaches everything; an assigned member reaches its member projects,
and a management token reaches its listed projects that its creator also
reaches. `Principal.Project` is the only decision: a resource beyond the
caller's reach answers 404 exactly as if it did not exist, and a visible
resource the caller may not change answers 403. Lists include only reachable
resources, and only installation-wide principals place resources in the
unassigned boundary. The isolation sweep reads every operation as a member of
another project and fails on any disclosure of a canary project.

Organization membership adds project scope without adding installation operations.
One organization level contains projects; manager/viewer membership combines with
direct project membership using manager precedence. Explicitly project-scoped
tokens do not inherit organization administration. Nested operations recheck the
organization and owning project; writes use the current principal inside the
management transaction. See [organizations](access.md#organizations).

| Organization context | Read organization | Change organization/members | Project scope |
| --- | --- | --- | --- |
| Global owner | Yes | `manage_organization`; tokens also need `access` when relying on global authority | Existing global scope |
| Organization manager | `read` | `manage_organization`, subject to installation role | Manager in contained projects |
| Organization viewer | `read` | No | Viewer, plus independent direct grants |
| Explicit project-scoped token | No parent grant | No | Its project list intersected with current creator authority |

## Gateway keys

An API key is stored only as a digest and resolved against the key authority
each gateway reloads within five seconds of any key change. A gateway that
cannot refresh its authority for a minute refuses all traffic rather than serve
revoked keys, and realtime sessions reauthorize every five seconds.

| Surface | Where the key is read |
| --- | --- |
| OpenAI and native routes | `Authorization: Bearer` |
| Anthropic | `Authorization: Bearer`, otherwise `X-Api-Key` |
| Gemini | `Authorization: Bearer`, otherwise `X-Goog-Api-Key`, otherwise the `key` query parameter |
| Gemini Live | Exactly one of those locations |
| Bedrock | `X-OLP-API-Key`, otherwise a non-SigV4 bearer token; OLP never verifies AWS signatures |

`gateway/credentials.go` owns these locations, and a characterization table
pins every location, precedence, and refusal.

A retained resource, such as a stored response, file, batch, continuation, or
video job, is usable only by the key that created it, through a route that key
may still use. `gateway/retained.go` answers any other key with 404, and
answers 409 when the provider revision, slot, or credential that serves the
resource is gone.

The `native-responses-v2` retained-response contract stores the serving receipt,
model binding and optional prior-response provider ID under `provider_continuation`.
It does not retain the caller's source request, including native user identifiers.
The earlier internal v1 storage format is no longer readable; recreate those
short-lived response handles after upgrading. Other retained native contracts keep
only the encrypted dependencies their documented continuation protocol requires.

## Secrets

OLP holds two keys, both mounted from files that others cannot read:

- The **authentication key** computes HMAC-SHA256 digests, each bound to the
  installation and to one digest purpose. Sessions, API keys, management
  tokens, and invitations are stored only as digests, and CSRF,
  recent-authentication, and OIDC state proofs are digests of their session or
  flow.
- The **master key ring** seals every other secret with AES-256-GCM, binding
  the ciphertext to the installation, one seal purpose, and the record it
  belongs to. Startup and `olp doctor` open every stored record before
  anything else runs, and [rotation](operations.md#master-key-rotation-and-recovery)
  re-seals records in bounded batches.

Wrapped data keys are unwrapped at startup through workload identity and the
provider egress policy. External provider versions contain immutable store
metadata rather than sealed copies; the sole runtime credential source bounds
their cache and reports unavailable versions in route plans. Expired values
and sealed credentials cannot substitute for an unavailable pinned reference.
See [external secrets](external-secrets.md) for identity, rotation and recovery.

`internal/secrets/purpose.go` declares every purpose, and only that package can
construct one, so no digest or ciphertext names an ad hoc purpose. Purpose
names are bound into stored data and never change:

<!-- purposes -->
| Kind | Purposes |
| --- | --- |
| Digest | `saml_state`, `saml_cookie`, `saml_assertion`, `mfa_challenge`, `mfa_recovery`, `api_key`, `workload_identity`, `end_user`, `management_token`, `session`, `recent_auth`, `csrf`, `oidc_state`, `oidc_cookie`, `invitation`, `admission`, `mutation`, `installation` |
| Seal | `saml_key`, `saml_flow`, `mfa_totp`, `mfa_webauthn`, `provider_credential`, `provider_continuation`, `notification_secret`, `mutation_replay`, `oidc_client`, `oidc_flow`, `media_job_source`, `provider_grant_refresh`, `grant_enrollment`, `sink_credential` |
<!-- /purposes -->

Passwords are hashed with Argon2id, with at most four concurrent hashes per
process.

## Egress

**Provider egress** (`internal/egress`) requires HTTPS unless an operator lists
the host, refuses addresses outside public unicast unless an operator lists the
network, rejects a host when any DNS answer is unsafe, dials only the validated
addresses, refuses redirects, and requires TLS 1.2 or newer. Processes log a
warning at startup whenever operator exceptions are configured.

**Identity egress** uses the same address rules with no exceptions, so provider
settings cannot weaken it.

**Credential redaction**: every credential OLP applies to a provider request is
recorded for that request, and every provider-derived error message, type, and
code is scrubbed of those values, including their JSON-escaped forms, before a
client, log, or request record sees it.

Consumer catalog reads admit a `models_read` inference key only on the declared
`GET /api/v1/catalog` surface. The gateway’s ordinary authority, revocation,
expiry and client-address checks remain in force. A consumer principal cannot
authorize any management mutation or other management read. Public project
catalogs require owner publication; price publication and upstream-model
disclosure are independent explicit choices. See [model catalog](model-catalog.md).

## Public perimeter

`process.Perimeter` wraps the public listener ahead of admission and routing,
so refusals and fallbacks carry the same headers as handled responses:

| Surface | Headers |
| --- | --- |
| Every response | `X-Content-Type-Options: nosniff`; `Strict-Transport-Security` when `OLP_PUBLIC_ORIGIN` is HTTPS |
| Management API (`/api/`) | `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin`, `X-Frame-Options: DENY`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` |
| Console | Its script-hashed `Content-Security-Policy`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy` and `Cross-Origin-Resource-Policy: same-origin`, `X-Frame-Options: DENY`, and a `Permissions-Policy` denying device features |
| Inference | `Cache-Control: no-store`; CORS only for `OLP_GATEWAY_CORS_ALLOWED_ORIGINS` |

Browser sessions use `__Host-` cookies that are `Secure` and `SameSite=Lax`.
Unsafe management requests need the configured `Origin` and a CSRF proof, and
cross-site fetches are refused except for the OIDC callback and the narrowly scoped SAML POST receipt/completion. The SAML POST creates no session; a separately cookie-bound GET consumes verified sealed facts.

## Audit

Management mutations write metadata-only audit records, attributed to the
member or management token that made them, or to the installation for system
actions such as OIDC role synchronization. Records hold the direct peer address and a
user-agent family, never request bodies, credentials, or content. See
[routine checks](operations.md#routine-checks).

## Non-goals and residual risks

- OLP serves plain HTTP. Terminate TLS at the edge, and route only the prefixes
  in [edge routing](deployment.md#edge-routing) to it.
- The private listener is unauthenticated. Never expose it publicly.
- [Content policy](gateway.md) shapes prompts and outputs; it is not a security
  control.
- Administrators of one installation are trusted. Project boundaries separate
  members' access within the installation; installation-wide principals see
  every project.
- Notification webhooks follow up to five redirects, validating each hop with
  the provider egress rules, and the OTLP exporter connects to its configured
  endpoint with its own client.
- Provider authentication may reach cloud metadata endpoints to obtain workload
  identity for AWS and Azure providers.
- Bedrock clients authenticate to OLP with an API key, never with AWS
  signatures.

Local password authentication with enrolled or required MFA cannot create a
session until its challenge is completed. Enrollment rotates the current
session; factors/recovery codes are managed only with fresh proof. WebAuthn
binds the configured public origin and requires user verification; TOTP and
recovery proofs are transactionally one-use. See [local MFA](access.md#local-multi-factor-authentication)
for storage purposes, bootstrap, offline recovery and the external-SSO boundary.

SAML uses independently verified assertion signatures, explicit issuer/audience/
request/recipient checks and a browser-bound two-stage callback. Only declared
metadata URLs use identity egress; assertion-controlled URLs are never fetched.
Signing material and short-lived verified flow facts use distinct `saml_key` and
`saml_flow` seal purposes. See [SAML console sign-in](access.md#saml-console-sign-in).
