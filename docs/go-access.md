# Go installation and access control

The Go control plane supports owner setup, local and OIDC sign-in, membership,
invitations, sessions, profiles, gateway-key policy, installation settings, and
metadata-only audit reads. Gateway inference, routing, distributed limits, and
retention workers belong to later milestones. The console reports these
capabilities and labels configured limits as saved policy.

## Local development

Run `make go-dev` with the prerequisites in [CONTRIBUTING](../CONTRIBUTING.md).
It provisions isolated PostgreSQL/Valkey services, private secrets under
`.local/go-secrets`, and the Go schema before starting the application. Existing
files and database contents are retained across restarts. Open
`http://127.0.0.1:5173` and use the token in
`.local/go-secrets/bootstrap.token` once to create the first owner.

The public backend listens on 8082, private probes on 9092, and Vite forwards
same-origin requests to the backend. Restart Go after backend edits.

## Deployment and database roles

Use a fresh, separate database. Go owns schema `olp_go` and its checksum history;
it rejects Rust schemas and preexisting public tables before writing. The
installation UUID survives repeated and concurrent migrations. Valkey names
are reserved under `olp:go:v1:<installation UUID>:`; M2 does not publish runtime
coordination keys yet. Never share a Rust installation's database or Valkey data.

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

Management modes require these private regular files, mode 0600 or 0640:

| Environment variable | File contents |
| --- | --- |
| `OLP_AUTH_HMAC_KEY_FILE` | 32 random bytes, encoded as hex or standard base64 |
| `OLP_MASTER_KEY_FILE` | JSON master-key ring shown below |
| `OLP_BOOTSTRAP_TOKEN_FILE` | A random 32–256-byte token, needed for initial setup |

Database and Valkey URLs also support their corresponding `_FILE` options.
Inline and file-based URL settings are mutually exclusive. The local/test
helper `source scripts/go-secrets.sh <private-directory>` creates missing secret
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
   key only after no stored rows use it and backup retention permits removal.
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
those keys explicitly when required. OIDC-only accounts follow current role
mappings on sign-in, revoke old sessions on a role change, and lose outstanding
invitations when owner authority is lost. Accounts with local passwords retain
their locally managed roles.

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
hours with a 64 KiB bound. Lists use validated cursors and limits up to 200.

Public authentication admission is shared through PostgreSQL: 10,000 requests
per action globally per minute, 60 per directly connected source (30 for
invitation acceptance), and five per source/target when applicable. Rejected
attempts commit their reached counters. Forwarded source headers are not trusted.

Normal reads, errors, and audit records never expose password/token hashes,
credential ciphertext, raw identity claims, or full user agents. Audit stores
explicit actor/resource/action/outcome, time, direct peer IP, and a coarse user
agent family. Automatic role synchronization uses no invented human actor.
