# Security architecture

This guide describes the security architecture that protects an OpenLLMProxy
installation. It covers trust boundaries, principals, authorization, secret
handling, plugin confinement, database roles, network egress and the contract
for public responses. The sections name the module that owns each rule, and
tests connect the implemented rule to the behavior described here.

Report suspected vulnerabilities privately through the
[security policy](../../SECURITY.md).

## Trust boundaries

| Boundary | What crosses it | How it is protected |
| --- | --- | --- |
| Edge to public listener | Console, management API and inference traffic | The edge terminates TLS. OLP classifies each path's surface (`internal/domain/surface`), admits it from that surface's pool and sets the [perimeter headers](#public-perimeter). Forwarded client addresses are trusted only through `OLP_TRUSTED_PROXY_CIDRS`. `OLP_MANAGEMENT_ALLOWED_CIDRS` optionally restricts management and console clients before admission and authentication. |
| Private listener | Health probes and metrics | Unauthenticated by design. Bind it to a private interface and restrict it with the chart's NetworkPolicy. |
| OLP to PostgreSQL | All durable state | Bearer credentials are stored only as digests and other secrets only sealed by the master key ring. See [secrets](#secrets) and [database role separation](#database-role-separation). |
| OLP to Valkey | Limits, routing hints and accounting events | Carries metadata and never credentials or content. Installations sharing one Valkey stay isolated by namespace. |
| OLP to providers | Inference and management calls | [Provider egress](#egress) validates every destination and pins every dial, and provider errors are scrubbed of the credentials OLP sent. |
| OLP to identity provider | OIDC discovery, tokens and keys, plus declared SAML metadata | Identity egress applies the provider denylist with no operator exceptions. |
| Operator to CLI | [Remote administration](../operate/automation.md), recovery, key rotation and `doctor` | Remote commands use a mounted scoped management token and HTTPS. Local maintenance requires protected secret files and database access. |

## Principals

| Principal | Credential | Verified by | Reaches |
| --- | --- | --- | --- |
| Member | The `__Host-olp_session` cookie, with a CSRF proof on unsafe methods and an allowed `Origin` | `access.Server.Authenticate` | The management operations of its role within its [access scope](#project-boundaries) |
| Management token | `Authorization: Bearer olpm_...` | `access.Server.Authenticate` | Its delegated operations that its creator can still perform |
| API key | The location its SDK uses, described in [gateway keys](#gateway-keys) | `gateway/ingress/credentials.go` and `kernel.GeminiClientKey` | Inference and model reads on its permitted routes within one project boundary |
| Bootstrap token | `X-OLP-Setup-Token` | `accountService.setup` in `internal/identity/identity.go` | Creating the first owner, once |

## Management authorization

The [management contract](../../management/management.json) declares every
operation's security requirement, specifying the management operations a session
or management token needs and whether an installation-wide scope is required.
The `Route`, `Stream` and `Public` methods on `*access.Server` mount each route
with that requirement, so the caller is admitted before any handler code runs,
and a route missing from the contract cannot be mounted. Handlers may demand
more with `Principal.Authorize`, such as the operation a record's project
implies, never less. Mutation helpers automatically call the unexported
`(*access.Server).reauthorize` method inside their transaction after acquiring
the installation lock, so changes commit only under authority that is still
current.

`internal/access/authz/policy.go` decides who holds each operation.

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

`internal/access/testdata/authorization.golden.json` records which caller
archetypes the management authorization policy admits for every route, including
anonymous callers and callers with an inference key. In the unit tier,
`TestManagementAuthorizationGoldenMatrix` verifies that the matrix covers exactly
the routes in the contract and matches their admission rules. Against a real
process, `tests/e2e/sweep_test.go` runs the owner read sweep
(`TestManagementReadsAnswerWithoutServerErrors`) and the cross-project isolation
sweep (`TestManagementIsolationSweep`) across every secured `GET` route.

## Project boundaries

Providers, routes, API keys, budget groups, export sinks and notification
destinations and rules belong to a project or to the unassigned boundary.
Guardrails and upstream MCP registrations require a project. Media jobs and
retained resources inherit their API key's boundary. An installation-wide
principal reaches everything, an assigned member reaches its member projects,
and a management token reaches its listed projects that its creator also
reaches. `Principal.Project` is the only decision. A resource beyond the
caller's reach answers 404 exactly as if it did not exist, and a visible
resource the caller may not change answers 403. Lists include only reachable
resources, and only installation-wide principals place resources in the
unassigned boundary. `TestManagementIsolationSweep` reads every secured route as
an assigned member and a project-scoped token of another project and fails on
any disclosure of a canary project.

Organization membership adds project scope without adding installation
operations. One organization level contains projects, and manager and viewer
membership combines with direct project membership using manager precedence.
Explicitly project-scoped tokens do not inherit organization administration.
Nested operations recheck the organization and owning project, and writes use
the current principal inside the management transaction. See
[organizations](../use/access.md#organizations).

| Organization context | Read organization | Change organization or members | Project scope |
| --- | --- | --- | --- |
| Global owner | Yes | `manage_organization`, plus `access` for tokens relying on global authority | Existing global scope |
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
| Gemini Live | Exactly one of `Authorization: Bearer`, `X-Goog-Api-Key` or the `key` query parameter |
| Bedrock | `X-OLP-API-Key`, otherwise a non-SigV4 bearer token, because OLP never verifies AWS signatures |

`gateway/ingress/credentials.go` owns the OpenAI, Anthropic, Gemini HTTP and
Bedrock locations and their precedence. `kernel.GeminiClientKey` in
`internal/gateway/kernel/duplex.go` owns Gemini Live (and Gemini Interactions)
key validation, refusing duplicate headers, duplicate query parameters or
competing locations instead of silently selecting one.

A retained resource, such as a stored response, file, batch, continuation or
video job, is usable only by the key that created it, through a route that key
may still use. `gateway/kernel/retained.go` answers any other key with 404, and
answers 409 when the provider revision, slot or credential that serves the
resource is gone.

The `native-responses-v2` retained-response contract stores the serving receipt,
model binding and optional prior-response provider ID under
`provider_continuation`. It does not retain the caller's source request,
including native user identifiers. The earlier internal v1 storage format is no
longer readable, so callers must recreate those short-lived response handles
after upgrading. Other retained native contracts keep only the encrypted
dependencies their documented continuation protocol requires.

## Secrets

Batch tables (`batch_settlements`, `batch_settlement_results` and
`attempt_usage_facts`) contain lifecycle and accounting metadata only.
`custom_id_digest` is an HMAC rather than the custom ID or request and result
lines. Gemini resumable upload URLs are sealed under the upload-session resource
and are never returned from management metadata.

OLP holds two keys, both mounted from files that other users cannot read. The
authentication key computes HMAC-SHA256 digests, each bound to the installation
and to one digest purpose. Sessions, API keys, management tokens and invitations
are stored only as digests, and CSRF, recent-authentication and OIDC state
proofs are digests of their session or flow. The master key ring seals every
other secret with AES-256-GCM, binding the ciphertext to the installation, one
seal purpose and the record it belongs to. Startup and `olp doctor` open every
stored record before anything else runs, and
[rotation](../operate/operations.md#master-key-rotation-and-recovery) re-seals
records in bounded batches.

Wrapped data keys are unwrapped at startup through workload identity and the
provider egress policy. External provider versions contain immutable store
metadata rather than sealed copies, and the sole runtime credential source
bounds their cache and reports unavailable versions in route plans. Expired
values and sealed credentials cannot substitute for an unavailable pinned
reference. See [external secrets](../operate/external-secrets.md) for identity,
rotation and recovery.

`internal/platform/secrets/purpose.go` declares every purpose, and only that
package can construct one, so no digest or ciphertext names an ad hoc purpose.
Purpose names are bound into stored data and never change.

<!-- purposes -->
| Kind | Purposes |
| --- | --- |
| Digest | `cache_key`, `saml_state`, `saml_cookie`, `saml_assertion`, `mfa_challenge`, `mfa_recovery`, `api_key`, `workload_identity`, `workload_agent_identity`, `end_user`, `management_token`, `session`, `recent_auth`, `csrf`, `oidc_state`, `oidc_cookie`, `invitation`, `admission`, `mutation`, `installation`, `batch_submission`, `batch_line`, `agent_approval`, `agent_principal`, `delegation_token`, `delegation_consent`, `delegation_pair`, `a2a_task_capability`, `retained_submission`, `retained_list_cursor`, `tool_input`, `tool_call`, `mcp_enrollment` |
| Seal | `saml_key`, `saml_flow`, `mfa_totp`, `mfa_webauthn`, `provider_credential`, `provider_continuation`, `notification_secret`, `mutation_replay`, `oidc_client`, `oidc_flow`, `provider_grant_refresh`, `grant_enrollment`, `sink_credential`, `mcp_credential`, `mcp_grant_access`, `mcp_grant_refresh`, `mcp_grant_enrollment`, `mcp_entra_client`, `mcp_delegation_signing`, `billing_credential`, `guardrail_credential`, `response_cache`, `provider_upload_session`, `agent_credential`, `workflow_continuation` |
<!-- /purposes -->

Passwords are hashed with Argon2id, with at most four concurrent hashes per
process.

## Plugin confinement

Confined provider and guardrail plugins (`internal/plugins/runtime.go`) run as
WASI preview 1 WebAssembly reactors on wazero with WebAssembly reference types
and DWARF debug decoding disabled. Each module is bounded to 64 MiB of linear
memory, an 8 MiB call stack, four concurrent instances, 1 MiB ABI replies and a
10-second per-call timeout. Modules receive no preopened directories, arguments
or environment variables. Only clock, cryptographic randomness and redacted
logging are exposed by default, and HTTP fetch is granted only to grant
enrollment and refresh calls targeting owner-approved origins over provider
egress.

Unconfined plugins (`internal/plugins/unconfined.go`, `executable.go` and
`executable_linux.go`) are an experimental, Linux-only tier that stays disabled
unless the deployment sets `OLP_UNCONFINED_PLUGIN_DIR` to an absolute path.
Before launching a permitted executable, OLP copies it into an executable
memory file (`memfd_create`), applies write, shrink, grow and seal locks
(`F_ADD_SEALS`), verifies the SHA-256 digest of the copied bytes against the
approved digest, and executes `/proc/self/fd/3` in its own process group
(`Setpgid: true`) speaking the plugin ABI over standard input and output.
Because the subprocess still runs with the operating system privileges of the
`olp` process rather than a sandbox, only an owner can permit each digest.

## Database role separation

Production deployments separate schema migrations from runtime database access.
`scripts/grant-runtime-database-role.sql` revokes `CREATE` on schema `public`
and all privileges on schema `olp` from `PUBLIC`, then grants the runtime role
`USAGE` on `olp`, `SELECT`, `INSERT`, `UPDATE` and `DELETE` on all tables in
`olp`, and `USAGE` and `SELECT` on all sequences. The script then revokes write
access on `olp.migrations` and grants `SELECT` only so runtime processes can
verify schema readiness without altering tables or migration history.
`TestServiceRunsUnderRuntimeRole` in `tests/e2e/runtime_role_test.go` verifies
that a service operates under this restricted role.

## Egress

Provider egress (`internal/platform/egress`) requires HTTPS unless an operator
lists the host, refuses addresses outside public unicast unless an operator
lists the network, rejects a host when any DNS answer is unsafe, dials only the
validated addresses, refuses redirects and requires TLS 1.2 or newer. Processes
log a warning at startup whenever operator exceptions are configured.

Identity egress uses the same address rules with no exceptions, so provider
settings cannot weaken it.

For credential redaction, every credential OLP applies to a provider request is
recorded for that request, and every provider-derived error message, type and
code is scrubbed of those values, including their JSON-escaped forms, before a
client, log or request record sees it. This includes native failed Response and
Interaction documents and Bedrock error payloads. A native error whose object
member name contains an applied credential is refused before delivery.

Consumer catalog reads admit a `models_read` inference key only on the declared
`GET /api/v1/catalog` surface. The gateway's ordinary authority, revocation,
expiry and client-address checks remain in force. A consumer principal cannot
authorize any management mutation or other management read. Public project
catalogs require owner publication, and price publication and upstream-model
disclosure are independent explicit choices. See
[model catalog](../use/console.md#model-catalog).

## FinOps and billing data

FOCUS datasets and metering records are content-free like other exports, though
they carry provider, connection and project names, API key IDs, route slugs,
attribution labels and, in metering payloads, end-user digests. Object sinks and
endpoints are operator-declared and egress-validated, and sink signing or API
credentials are write-only `sink_credential` secrets validated against the sink
type. For example, Stripe meter keys must use an `sk_` or `rk_` prefix. Billing
credentials use the dedicated `billing_credential` seal purpose. The import
worker reads them to fetch invoice data. Configuration apply can decrypt a
stored credential to compare it internally with a supplied binding. Clients
cannot read billing credentials, and the credentials are not exported or used
for inference. Stripe meter posts and billing export reads use the provider
egress policy, so `OLP_PROVIDER_EGRESS_*` exceptions govern them like provider
traffic. Cloud authentication depends on the selected credential mode. See
[FinOps](../operate/finops.md) for source-specific credentials and egress.

## Public perimeter

`Perimeter` in `cmd/olp/perimeter.go` (`package main`) wraps the public listener
ahead of admission and routing so refusals and fallbacks carry the same headers
as handled responses.

| Surface | Headers |
| --- | --- |
| Every response | `X-Content-Type-Options: nosniff` and `Strict-Transport-Security` when `OLP_PUBLIC_ORIGIN` is HTTPS |
| Management API (`/api/`) | `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin`, `X-Frame-Options: DENY`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` |
| Console | Its script-hashed `Content-Security-Policy`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy` and `Cross-Origin-Resource-Policy: same-origin`, `X-Frame-Options: DENY`, and a `Permissions-Policy` denying device features |
| Inference | `Cache-Control: no-store`, with CORS only for `OLP_GATEWAY_CORS_ALLOWED_ORIGINS` |

Browser sessions use `__Host-` cookies that are `Secure` and `SameSite=Lax`.
Unsafe management requests need the configured `Origin` and a CSRF proof, and
cross-site fetches are refused except for the OIDC callback and the narrowly
scoped SAML POST receipt and completion. The SAML POST creates no session, and a
separately cookie-bound GET consumes verified sealed facts.

## Audit

Management mutations write metadata-only audit records attributed to the member
or management token that made them, or to the installation for system actions
such as OIDC role synchronization. Records hold the direct peer address and a
user-agent family, never request bodies, credentials or content. See
[routine checks](../operate/operations.md#routine-checks).

## Non-goals and residual risks

OLP serves plain HTTP, so operators must terminate TLS at the edge and route
only the prefixes listed in [edge routing](../operate/deployment.md#edge-routing).
The private listener is unauthenticated and must never be exposed publicly.
[`builtin.regex` guardrails](../use/guardrails.md) are bounded text filters
rather than a security control. Administrators of one installation are trusted,
while project boundaries separate members' access within the installation and
installation-wide principals see every project. Bedrock clients authenticate to
OLP with an API key rather than AWS signatures.

Notification webhooks refuse redirects like other egress clients, and the OTLP
exporter connects to its configured endpoint with its own client. Notification
bodies carry metadata only, including events, rule subjects, windows and
amounts, and never request content, credentials or secret material. Channel
credentials such as chat webhook URLs, PagerDuty routing keys, SMTP credentials
and signing secrets are write-only, sealed under `notification_secret`, and
never returned by any read.

Payload capture is opt-in, owner-enabled and memory-only. Empty redactor lists
capture unredacted content, while configured local redactors fail closed for
capture. See [payload capture](../operate/observability.md#payload-capture).
Captured content never enters the database, Valkey, logs or spans and is
delivered only to the operator-configured sink.

Provider authentication may reach cloud metadata endpoints to obtain workload
identity for AWS, Azure and Google providers. Google metadata requests go to
`169.254.169.254`. Inference honors a `GCE_METADATA_HOST` override only when the
egress policy permits that host, so a hostname such as `metadata.google.internal`
must be listed in `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS`. Billing imports reach
only the fixed address, so Google ADC fails there while the override names
another host.

Local password authentication with enrolled or required MFA cannot create a
session until its challenge is completed. Enrollment rotates the current
session, and factors and recovery codes are managed only with fresh proof.
WebAuthn binds the configured public origin and requires user verification,
while TOTP and recovery proofs are transactionally one-use. See
[local MFA](../use/access.md#local-multi-factor-authentication) for storage
purposes, bootstrap, offline recovery and the external-SSO boundary.

SAML uses independently verified assertion signatures, explicit issuer,
audience, request and recipient checks, and a browser-bound two-stage callback.
Only declared metadata URLs use identity egress, and assertion-controlled URLs
are never fetched. Signing material and short-lived verified flow facts use
distinct `saml_key` and `saml_flow` seal purposes. See
[SAML console sign-in](../use/access.md#saml-console-sign-in).

Historical credential resolution reads and authorizes an immutable reference in
a short primary-database transaction, releases that connection before remote
secret I/O, and rechecks live eligibility after resolution. Rotation does not
change the pinned reference, and revocation observed during the remote call
refuses its eventual secret. Static and grant serving bytes are immutable
borrowed buffers, so callers requiring mutable bytes must copy rather than clear
runtime-owned state.

## Response-cache content

The optional response cache holds AES-256-GCM sealed bodies and event sequences
in a separate evicting Valkey instance. Its HMAC keys bind project and key
scope, native request semantics and immutable serving and guardrail
configuration. Cache entries and masking maps never enter request-history
tables. Semantic caching requires explicit acknowledgement that the vector index
holds unencrypted content-derived vectors while responses remain sealed. See
[caching](../use/caching.md).
