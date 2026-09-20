# Installation and access control

The control plane supports owner setup, local and OIDC sign-in, membership,
invitations, sessions, profiles, gateway-key policy, installation settings, and
metadata-only audit reads. Provider connections, routes, inference, media,
distributed limits, and the retention workers are described in the
[gateway guide](gateway.md). The console reports these
capabilities and labels configured limits as saved policy.

## Local development

Follow [CONTRIBUTING](../CONTRIBUTING.md#local-development) for local services,
Vite proxying, private secret files, and the one-time owner bootstrap token.

## Deployment and database roles

Use a fresh, separate database. Go owns schema `olp_go` and its checksum history;
it rejects Rust schemas and preexisting public tables before writing. The
installation UUID survives repeated and concurrent migrations. Valkey names
are reserved under `olp:go:v1:<installation UUID>:`, the namespace the limits,
cooldown, and request-metadata keys documented in the
[operations runbook](operations.md#shared-state-in-valkey) live under. Never share a
Rust installation's database or Valkey data.

Provision a database owner for migration and a separate, existing login role for
runtime use. Using the migration connection, run:

```sh
olp migrate --runtime-role olp_runtime
```

The command grants schema usage and feature-table DML to that role, with
read-only migration history. It neither creates login roles nor grants DDL
privileges. Supply the runtime connection to `olp all` or `olp control` afterward.
Startup requires the complete, unchanged migration history and never migrates
implicitly. Other process modes also require an initialized installation.
Queries have statement/lock deadlines; access mutations take the installation
row lock so ownership checks and resulting writes commit together. Password
hashing, OIDC discovery, and token verification run outside that lock.

Set `OLP_PUBLIC_ORIGIN` to the exact browser origin. Unsafe management requests
require that Origin and authenticated requests also require `X-CSRF-Token`.
Use HTTPS outside local loopback development. Cookies are Secure, Path=/,
SameSite=Lax, with HttpOnly session and recent-authentication cookies. Private
health and metrics endpoints belong on a private listener/network.

## Mounted secrets

Use private regular files with mode 0400, 0440, 0600, or 0640; group-write and
world permissions are rejected. [Configuration](configuration.md#file-based-secrets)
identifies which modes require each file and the mounted-connector exception:

| Environment variable | File contents |
| --- | --- |
| `OLP_AUTH_HMAC_KEY_FILE` | 32 random bytes, encoded as hex or standard base64 |
| `OLP_MASTER_KEY_FILE` | JSON master-key ring shown below |
| `OLP_BOOTSTRAP_TOKEN_FILE` | A random 32–256-byte token, needed for initial setup |

Database and Valkey URLs also support their corresponding `_FILE` options.
Inline and file-based URL settings are mutually exclusive. The local/test
helper `source scripts/secrets.sh <private-directory>` creates missing secret
files without printing their contents; production secret distribution remains
the operator's responsibility.

```json
{
  "active_version": 1,
  "keys": [{ "version": 1, "key": "<32 random bytes encoded as hex or base64>" }]
}
```

A ring holds 1–32 distinct positive versions. Keep the authentication HMAC key
stable: its installation fingerprint prevents accidental replacement. Passwords
use salted Argon2id with fixed 64 MiB/3-iteration parameters and bounded local
concurrency. Session, API-key, invitation, and admission lookups use
installation- and purpose-separated HMAC digests. OIDC client secrets, pending
flows, and one-time mutation replies use AES-256-GCM with installation, record,
and purpose authenticated as associated data.

## Master-key rotation and recovery

1. Retain every key version still in use, add a higher version, and select it as
   `active_version` in the private ring file. Back up the database and required
   key material together.
2. Stop management writers for a controlled maintenance window, mount the new
   ring, and run `olp master-key reencrypt` using the same database and auth key.
3. Rerun the command after an interruption. It commits batches of 100 records;
   authenticated old records remain readable with the retained keys. A stale
   process cannot write ciphertext using the retired active version. Keep the
   same key material for each version: every run authenticates existing
   destination-version records before committing any batches.
4. Run `olp master-key status` and `olp doctor`. They authenticate stored
   ciphertext and emit only installation/version/count metadata. Remove an old
   key only after `olp master-key verify-retirement VERSION` succeeds and backup
   retention permits removal. `olp master-key reencrypt --dry-run` authenticates
   records without changing them.
5. Restart management processes with the new ring. Existing sessions and API
   credentials retain their HMAC identity; encrypted replayed credentials retain
   their original response. Rotation records a metadata-only audit event.

## Access and OIDC

Owners manage membership, OIDC, local-login availability, and other users'
sessions. Operators can read membership and manage keys/settings. Developers
can manage keys. Viewers can read key metadata, settings, and audit records.
Every user manages their own profile and sessions. The last usable owner cannot
be removed, disabled, or stranded by authentication configuration changes.

Disabling or changing a member's role revokes their sessions. Losing membership
management authority retires outstanding invitations they issued. Existing API
keys retain their issuer attribution and installation-scoped policy; revoke
those keys explicitly when required.

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
cannot sever their authorization source.

Local sign-in is usable only when both `OLP_LOCAL_LOGIN_ENABLED` and
`auth.local_login_enabled` permit it. Capabilities, invitation issuance and
acceptance, identity unlink, and last-owner configuration guards apply the same
policy. Password invitations are unsupported while effective local sign-in is
disabled: issue/accept returns actionable guidance before account creation or
token consumption. Use mapped OIDC provisioning for SSO-only onboarding; email
matching never implicitly links an existing account.

Migration `0010_authentication_ownership.sql` classifies legacy accounts using
provisioning evidence. Setup/accepted-invitation audit events or accepted
invitation records preserve local ownership; remaining accounts with linked
OIDC identities become OIDC-managed, including ambiguous mixed-method accounts
whose original provisioning evidence has expired. Review these ambiguous
accounts and ensure appropriate provider mappings before upgrading. The
migration cannot infer ownership from a password added through enrollment.
Accounts without OIDC identities remain locally managed. Role ownership is
internal and cannot be changed by self-service credential operations.

Owner protection evaluates proposed mappings against the latest verified email
and group inputs stored privately for each identity. These inputs never appear
in identity responses or audit. Run `olp migrate` before starting an updated
binary; migration `0003_oidc_role_claims.sql` preserves existing identities
without inventing verified inputs. Complete an OIDC sign-in before relying on
an upgraded identity as the only owner sign-in method. Changing claim names
requires another verified owner path or an owner with enabled local password
sign-in.

Replacing or removing the OIDC client secret requires an active owner with
enabled local password sign-in. A changed secret invalidates prior OIDC
sign-in evidence; keep local login enabled until an owner successfully signs
in through OIDC with the saved credential. Omitting the secret or submitting
the same value preserves the existing evidence.

OIDC uses maintained `coreos/go-oidc` verification and `x/oauth2` code exchange.
Configure discovery, the exact issuer, client ID, client secret when required,
and the callback `<public origin>/api/v3/oidc/callback`. The client selects
`client_secret_basic` or `client_secret_post` authentication as advertised;
omitting the supported-methods field defaults to `client_secret_basic`.
Unsupported token authentication methods are rejected during configuration.
Discovery and endpoints must use HTTPS and public addresses. The dedicated
client rejects redirects, credentials in URLs, private/reserved DNS answers,
oversized responses, and unbounded waits. Provider egress settings cannot weaken
identity egress.

Authorization binds a single-use encrypted flow to the browser, configuration
ETag, state, nonce, PKCE verifier, and initiating session when applicable. ID
tokens must have a verified email and valid signature, issuer, audience, expiry,
and nonce. Email collisions require explicit linking after recent
authentication. Enrollment/link/unlink proofs expire after five minutes, are
purpose/resource-bound, and are consumed once. Authentication-method changes
rotate the current session and revoke prior sessions.

Production builds reject insecure OIDC environment switches. Only the explicit
`oidctest` build tag permits a loopback HTTP issuer, and the integration runner
builds a separate test binary; release images never enable that tag.

## Mutation and audit boundaries

Protected writes reauthorize inside their feature transaction. Versioned
updates require a strong quoted `If-Match`; stale edits return 412. The console
retains the ETag from the edit baseline and offers an explicit reload after a
conflict. Key creation/revocation/rotation and invitation creation/retirement
require `Idempotency-Key`. Replays bind actor, method, path, precondition, and
request body, are reauthorized before reading, and encrypt responses for 24
hours. Feature request and collection limits bound response sizes; aggregate
responses such as credential pools are replayed in full even above 64 KiB.
Lists use validated cursors and limits up to 200.

Public authentication admission is shared through PostgreSQL: 10,000 requests
per action globally per minute, 60 per resolved source (30 for invitation
acceptance), five per source/target, and 30 per target across sources when a
target is known. Targets and sources are HMAC digests. Windows are fixed at one
minute, counts are capped, and denied attempts never extend a window or create
a permanent account lockout. Management uses the same `OLP_TRUSTED_PROXY_CIDRS`
resolver as inference: the direct peer is authoritative unless explicitly
trusted, then `X-Forwarded-For` is walked from the right through configured
trusted hops. Never configure untrusted clients as trusted proxies.

Normal reads, errors, and audit records never expose password/token hashes,
credential ciphertext, raw identity claims, or full user agents. Audit stores
explicit actor/resource/action/outcome, time, direct peer IP, and a coarse user
agent family. Automatic role synchronization uses no invented human actor.

## Console authentication

Session verification and sign-in capabilities requests have a 10-second console
deadline, including response bodies. Navigation cancellation stays separate
from timeout/service failure. A passive failure retains loaded content with a
visible Retry notice and requires successful verification before writes.
Session rotation invalidates older responses and sends a credential-free
invalidation to sibling tabs. A specific `csrf_invalid` rejection refreshes
verification and asks the user to review and retry; mutations are never replayed
automatically. Other forbidden operations do not trigger logout.

Browser OIDC callback failures redirect to local sign-in/profile pages with an
allowlisted reason; raw provider errors and tokens are never included. Retry
always starts a fresh flow. JSON clients retain problem responses. Profile
reauthentication offers usable linked OIDC even with an enrolled password;
existing session, purpose, resource, and exact identity bindings still apply.

Session inventory labels `last_seen_at` as **Last session verification** (updated
at most once a minute by session verification). A coarse browser/device hint
helps distinguish sessions without retaining raw user agents. It is untrusted
display metadata, not authentication evidence; legacy sessions show Unknown
browser.
