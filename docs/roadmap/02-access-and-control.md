# M2: Installation, identity, and management

[Roadmap](README.md) | [Previous: foundation](01-foundation.md) |
[Next: core gateway](03-core-gateway.md)

**Status:** Implemented and qualified on Linux amd64. **Prerequisites:** M1 implementation is present; its native arm64 qualification remains open.

[M2 evidence and screenshots](evidence/access-and-control.md) | [Operations guide](../go-access.md)

All M2 tickets and exit scenarios pass. Milestone closure still inherits the
open M1 native arm64 gate; this implementation does not claim that qualification.

Make the Go installation manageable through the reused console. Complete the
identity and management boundaries before routing requests or storing provider
credentials.

## Backlog

### M2-01

[Implementation and qualification](evidence/access-and-control.md#m2-01).

- [x] **Implement the fresh Go database and migration runner.**

**Depends on:** Milestone prerequisites.

**Deliver:** Define a separate Go schema baseline, installation identity, and
forward-only migration history. Implement pgx pools, explicit transactions,
bounded queries, migration locking/checksums, and separate migration/runtime
privileges. Preserve historical Rust migrations while the reference is active.

**Accept:** Empty-database installation and repeated migration runs succeed.
Concurrent migrators serialize; failed migrations have a tested recovery path.
Existing Rust installations are detected and rejected before writes. The Go
database and Valkey namespace cannot be confused with the reference installation.

**References:** [Database](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/database.rs), [migrations](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/migrations),
[migrate command](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/process/cli/migrate.rs),
[runtime grants](../../scripts/grant-runtime-database-role.sql).

### M2-02

[Implementation and qualification](evidence/access-and-control.md#m2-02).

- [x] **Restore secret storage and key lifecycle.**

**Depends on:** [M2-01](#m2-01).

**Deliver:** Implement file-based secret loading, password hashing, API/session
secret derivation, authenticated credential encryption bound to installation
and record identity, key-ring loading, and master-key rotation/reencryption.
Use maintained cryptographic implementations and preserve write-only semantics.

**Accept:** Tampered or misbound ciphertext is rejected. Rotation preserves
decryptability and can resume after interruption. Invalid secret permissions
fail startup; secrets never enter normal reads, logs, errors, or audit payloads.
The maintenance CLI exercises the same key lifecycle as the application.

**References:** [Crypto](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/crypto),
[master-key command](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/process/cli/master_key.rs),
[reencryption tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/master_key_reencryption_postgres.rs).

### M2-03

[Implementation and qualification](evidence/access-and-control.md#m2-03).

- [x] **Implement common management authorization and mutation rules.**

**Depends on:** [M2-01](#m2-01), [M2-02](#m2-02).

**Deliver:** Implement principal/permission checks, typed problem responses,
validated pagination, ETag/If-Match preconditions, idempotent mutation replay,
explicit audit provenance, and same-origin response policies. Keep shared HTTP
rules small; feature handlers retain transaction and business-rule ownership.

**Accept:** Unauthorized operations cannot mutate state. Stale writes conflict,
replays do not repeat side effects, and different requests cannot reuse a replay
as authorization. One-time secrets in replays remain encrypted and bounded.
APIs use no-store and audit records expose only permitted metadata.

**References:** [Management HTTP](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/http/control),
[permissions](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/permissions.rs),
[audit](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/audit.rs),
[idempotency tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/management_idempotency_replay_postgres.rs).

### M2-04

[Implementation and qualification](evidence/access-and-control.md#m2-04).

- [x] **Restore bootstrap, membership, and invitations.**

**Depends on:** [M2-03](#m2-03).

**Deliver:** Implement bootstrap-token first-owner setup, local accounts and
passwords, roles, member updates/disabling, and invitation creation, retirement,
and acceptance. Preserve installation ownership invariants and public
authentication admission limits.

**Accept:** Concurrent setup produces one initial owner. Expired, retired, or
already-used invitations cannot grant access. Role/member changes cannot
remove the last usable owner, and disabled accounts lose new access.
Mutation results and audit provenance agree under concurrent requests.

**References:** [Identity workflows](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/identity),
[public auth admission](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/identity/auth_admission.rs),
[identity persistence](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/identity_postgres.rs),
[invitation retirement](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/invitation_retirement_postgres.rs).

### M2-05

[Implementation and qualification](evidence/access-and-control.md#m2-05).

- [x] **Restore sessions and profile management.**

**Depends on:** [M2-04](#m2-04).

**Deliver:** Implement login/logout, expiry and revocation, secure cookie/origin
handling, active-session management, profile updates, local password changes,
and recent-authentication checks for sensitive operations.

**Accept:** Revoked/expired sessions cannot be reused. Cross-origin mutation
attempts and unsafe return URLs fail. Sensitive profile changes require the
proper recent proof, and changing identity or signing out clears the console's
identity-partitioned query state.

**References:** [Authentication](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/authentication),
[session HTTP](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/http/sessions.rs),
[profile HTTP](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/http/profile.rs),
[console session lifecycle](../../console/src/lib/features/access/session/).

### M2-06

[Implementation and qualification](evidence/access-and-control.md#m2-06).

- [x] **Restore gateway-key management and authority records.**

**Depends on:** [M2-04](#m2-04), [M2-05](#m2-05).

**Deliver:** Implement key issuance, one-time secret display, hashed lookup,
scopes, expiry, route allowlists, issuer lifecycle, revocation, and policy/limit
configuration storage. Expose durable authorization state for M3 runtime refresh;
distributed limit enforcement arrives in M4.

**Accept:** `inference` and `models_read` remain independent positive scopes.
Key secrets cannot be recovered through list/detail endpoints. Ownership and
issuer transitions follow existing behavior, and revocation is durable before
it is acknowledged. Unimplemented enforcement is not presented as active.

**References:** [API keys](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/api_keys),
[key policies](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/api_keys/http/policy.rs),
[issuer tests](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/configuration_postgres/api_key_issuers.rs).

### M2-07

[Implementation and qualification](evidence/access-and-control.md#m2-07).

- [x] **Restore OIDC and linked identities.**

**Depends on:** [M2-02](#m2-02), [M2-04](#m2-04), [M2-05](#m2-05).

**Deliver:** Implement OIDC configuration/secrets, discovery and callback
validation, state/nonce/PKCE handling, one-time flow consumption, claim and role
mapping, linked-identity management, and local-login settings. Use maintained
OIDC/OAuth verification components with a bounded HTTP client.

**Accept:** Issuer/audience/nonce mismatches, callback replay, stale flows, and
identity-link races are rejected. Provider egress exceptions do not authorize
OIDC destinations. Production refuses test-only insecure configuration, while
the local mock issuer supports repeatable browser and persistence tests.

**References:** [OIDC](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/access/oidc),
[OIDC persistence](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/persistence/oidc_flow_postgres.rs),
[OIDC system suite](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/system/oidc_http_postgres.rs),
[mock issuer](../../console/tests/journeys/mock-oidc.mjs).

### M2-08

[Implementation and qualification](evidence/access-and-control.md#m2-08).

- [x] **Connect settings and access pages to the Go contracts.**

**Depends on:** [M2-03](#m2-03), [M2-05](#m2-05), [M2-06](#m2-06), [M2-07](#m2-07).

**Deliver:** Implement installation settings and audit reads, then adapt setup,
login, access, invitations, keys, profile, settings, and audit pages. Reuse
existing validation/components and regenerate API types for contract changes.

**Accept:** All implemented access workflows work through the Go origin and
Vite proxy. Role-aware navigation, loading/errors, one-time-secret handling,
conflict messages, and session expiry behave correctly. No page relies on a
Rust API or on a hand-edited generated contract.

**References:** [Settings](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/settings),
[access console](../../console/src/lib/features/access/),
[settings console](../../console/src/lib/features/settings/),
[browser journeys](../../console/tests/journeys/rust-hosted-console.spec.ts).

### M2-09

[Implementation and qualification](evidence/access-and-control.md#m2-09).

- [x] **Qualify the identity and management boundary.**

**Depends on:** [M2-01](#m2-01), [M2-02](#m2-02), [M2-08](#m2-08).

**Deliver:** Port the relevant management contract, identity, idempotency, and
data-safety scenarios to the Go harness. Add evidence for concurrent setup,
invitation acceptance, owner changes, callback consumption, and secret rotation.

**Accept:** Service and browser checks cover successful workflows and denied
mutations, typed errors, stale ETags, session expiry, and secret redaction.
Schema migrations and key rotation preserve populated Go data. Attach
screenshots for visible console changes and update the capability map.

**References:** [Management contracts](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract/management_contract.rs),
[management failures](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract/management_failures.rs),
[data safety](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/contract/data_safety.rs),
[identity HTTP](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/tests/system/identity_http_postgres.rs).

## Exit scenarios

- Install an empty Go database, create the owner, invite a user, and manage roles.
- Complete local/OIDC login, profile changes, session revocation, and logout.
- Exercise conflicting mutations, replayed callbacks, and concurrent invitations.
- Verify encrypted secret rotation, one-time key display, and metadata-only audit.
- Run Go service suites, console checks, and access journeys at both browser origins.
