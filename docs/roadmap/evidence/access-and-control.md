# M2 access and control qualification

Recorded on 2026-09-13 on native Linux amd64 with Go 1.27.1, Node 26.8.2,
pnpm 11.24.0, PostgreSQL 18, Valkey 9, and Chromium.

The Go installation supports setup, local and OIDC identity, membership,
sessions, profiles, client keys, settings, and audit through the existing
console. The operations guide is [Go access and control](../../go-access.md).
All M2 tickets and exit scenarios are qualified on this platform. M1's native
arm64 qualification remains open and still prevents closing the inherited
platform gate. Rust storage and migration history remain untouched.

## M2-01

[Storage and migration runner](../../../internal/database/) use an isolated
schema, stable installation identity, transaction advisory locking, checksums,
query deadlines, and an existing separately privileged runtime role. Startup
validates migration history without writing it.

[Migration recovery tests](../../../tests/integration/migration_recovery_test.go)
inject a failure after DDL starts, verify complete rollback, recover, and apply
the next migration to populated storage without changing sessions, key IDs, or
encrypted replay. [Access tests](../../../tests/integration/access_test.go) and
[race tests](../../../tests/integration/access_races_test.go) cover repeated and
concurrent migration, lock cancellation, checksum rejection, reference-database
rejection before schema writes, installation isolation, and runtime-role denial
of migration-history writes.

## M2-02

[Cryptographic storage](../../../internal/secrets/) implements private mounted
files, Argon2id, installation-separated HMAC derivation, authenticated AES-GCM
envelopes, and versioned key rings. [Maintenance commands](../../../internal/process/maintenance.go)
use the same implementation as request handlers.

Tests reject bad file modes, malformed rings, changed installation/purpose/record
bindings, and tampering. An interrupted rotation commits 100 records, encounters
an injected failure on record 101, rejects a stale writer, then resumes with one
remaining record. CLI reencryption is repeatable; populated replay and browser
sessions survive. Rotation writes metadata-only audit provenance. Responses,
storage dumps, audit, and invalid-configuration diagnostics are checked for
plaintext credential leakage.

## M2-03

[HTTP policy](../../../internal/access/http.go) and
[encrypted replay](../../../internal/access/replay.go) share fixed permissions,
typed problems, validated cursors, strong preconditions, origin/CSRF/no-store
rules, bounded bodies, and explicit audit metadata. Feature transactions
reauthorize before consulting replay and serialize authority changes through the
installation row. Replays bind actor, method, path, precondition, and body, and
expire after 24 hours with a 64 KiB encrypted-response bound.

[Contract validation](../../../tests/integration/contracts_test.go) checks
declared JSON responses against the checked-in OpenAPI schemas while reporting
only structural error locations. Access tests exercise denied reads/mutations,
stale and missing preconditions, conflicting and matching replay, revoked
actors, pagination, CSRF, origin checks, and single committed audit events.

## M2-04

[Identity workflows](../../../internal/access/identity.go) implement atomic
bootstrap, local membership, fixed roles, disabling, invitation issuance and
retirement, one-time acceptance, and last-usable-owner protection. Public-auth
admission uses independently committed PostgreSQL counters, including rejected
attempts, with bounded cleanup and HMAC-derived bucket identities.

Concurrent setup and invitation acceptance each produce exactly one winner.
Competing owners cannot both remove usable authority. Expired, retired, used,
and authority-revoked invitations are rejected.
[Issuer lifecycle tests](../../../tests/integration/authority_lifecycle_test.go)
verify that disabling or demoting an invitation issuer retires outstanding grants
and revokes sessions in the same authority transaction.

## M2-05

[Session and profile workflows](../../../internal/access/sessions.go) provide
local login/logout, fixed expiry, active session controls, profile updates,
password change/enrollment, and five-minute purpose/resource/session-bound recent
proofs. Authentication-method changes revoke previous sessions and rotate the
current browser's session and CSRF material.

Service tests check expiry/revocation, secure cookie attributes, conflicting
cookies, recent-proof requirements, password transitions, and admission denial.
Browser tests verify profile changes reach the account menu and that logout and
identity changes use the existing partitioned query lifecycle without retaining
another user's visible data.

## M2-06

[Gateway-key authority](../../../internal/access/keys.go) stores only hashed
lookup material, returns secrets once or through authenticated encrypted replay,
preserves issuer attribution, and persists independent positive scopes, route
allowlists, expiry, revocation, rotation, and limit policy. Restricted keys require
an allowed route; omission cannot bypass the allowlist. Existing keys retain
installation-scoped authority when their issuer's account changes.

Policy and service tests cover scope independence, route restrictions, nullable
patch semantics, old-secret invalidation, rotation/revocation replay, and durable
authority generations. Distributed enforcement remains M4 work. Capability and
budget responses explicitly mark it inactive; console copy and controls show saved
policy and omit live accounting, routing publication, and unavailable inference
connection tests.

## M2-07

[OIDC workflows](../../../internal/access/oidc.go) use maintained verification
and OAuth libraries, an independent bounded identity HTTP client, exact issuer
validation, nonce, PKCE, one-use state, browser/configuration/session binding,
verified claims, role mapping, and explicit identity linking. OIDC-only roles
synchronize on subsequent login; role loss retires sessions and outstanding
invitations with an explicitly absent human actor. Local-password accounts retain
locally managed roles.

[Real signed-token tests](../../../tests/integration/oidc_test.go) reject issuer,
audience, nonce, expiry, email verification, browser binding, stale configuration,
and callback replay failures. [OIDC races](../../../tests/integration/oidc_races_test.go)
qualify competing callback consumers and identity links, role synchronization,
and session/grant retirement. Link/unlink tests check recent proofs, retained
usable sign-in methods, and revocation of other sessions. Default login redirects
have a destination. Production builds reject insecure OIDC environment settings;
only an explicitly tagged `oidctest` binary permits the loopback mock issuer.

## M2-08

The reused [access](../../../console/src/lib/features/access/) and
[settings](../../../console/src/lib/features/settings/) features consume
regenerated Go contracts. Service capabilities gate navigation and later gateway
features. Profile refresh, one-time secret handling, owner-only login settings,
readable setting labels, edit-baseline ETags, and explicit conflict reload are
covered by console checks and browser flows. Optional capabilities preserve the
reference console's existing behavior when omitted.

Both browser origins use independent databases and correctly configured backend
origins: packaged assets at port 4182 and Vite at port 4183 proxying a second Go
process. The real [browser mock issuer](../../../console/tests/access/mock-oidc.mjs)
validates PKCE and exchanges one-time codes for signed ID tokens.

## M2-09

The [service suite](../../../tests/integration/) and
[browser journey](../../../console/tests/access/control.spec.ts) qualify the
complete installation-to-member flow, successful and denied access, competing
mutations, callback replay, secret rotation, populated migration, profile/session
changes, role-aware controls, and audit. The
[Go integration driver](../../../scripts/integration.sh) provisions disposable
TLS services, private secrets, separate browser databases, the production binary,
and the explicitly tagged identity-test binary without compiling Rust.

## Validation

- `go test ./...` and `go vet ./...` passed; affected packages were checked again
  after later changes, including the restricted-key missing-route regression.
- Console verification passed all 460 Vitest tests across 51 files, formatting,
  ESLint, and Svelte/type checks. After the profile refresh change, type/lint checks
  and all 26 draft-saving tests passed again.
- `scripts/go-integration.sh` passed: all race-enabled service tests in 35.584 s,
  SDK contract metadata, static asset build/manifest checks, and four Chromium
  cases across packaged and Vite origins. The final browser run and screenshot
  capture passed in 28.1 s. Targeted race tests subsequently verified expired
  sessions, expired OIDC flows/proofs, and cookie visibility in 7.398 s.
- The native amd64 candidate built using `deploy/go.Dockerfile` and passed
  `scripts/go-image-smoke.sh` in all, gateway, control, and worker modes: migration,
  private readiness, listener ownership, static console, dependency connections,
  unprivileged/read-only execution, and bounded clean shutdown. The measured image
  was 93,278,120 bytes. Native arm64 remains the existing M1 qualification gap.
- OpenAPI and TypeScript generation run independently of Rust and services. The
  source and generated Go types include optional availability/enforcement fields.

Reproduce the full checks with `make go-check` and `make go-integration` using
the pinned toolchains and Docker, then build and smoke-test the native image.
The local qualification environment required Node 26 on `PATH` and an ignored
Docker wrapper forwarding the disposable Compose variables through `sudo`.

## Review regressions

All six review findings have dedicated regression coverage:

- [Owner mapping tests](../../../tests/integration/oidc_owner_protection_test.go)
  reject removal or downgrade of the sole default-, email-, or group-mapped
  owner, preserve configuration/secret/audit state on rejection, and verify
  fresh owner login after existing sessions expire. Migration 0003 stores only
  private verified authorization inputs; populated migration tests preserve
  existing identities and require fresh verification of previously absent facts.
- [Return-path tests](../../../internal/access/policy_test.go) reject control
  characters and network-path references. Both browser projects verify that
  Chromium normalizes tab/newline attacks across origins while the Go GET and
  POST login endpoints reject them.
- [Token authentication tests](../../../tests/integration/oidc_test.go) complete
  real exchanges using discovery-selected Basic or form credentials, including
  omitted-method defaults, and reject unsupported methods before configuration
  is saved.
- [Expired-key replay tests](../../../tests/integration/key_replay_expiry_test.go)
  preserve the original body, ETag, and Location after actual expiry, retain
  expired runtime authority, reject changed requests, and record only one create
  audit event.
- [Browser journeys](../../../console/tests/access/control.spec.ts) force stale
  profile, OIDC, and settings writes, check HTTP 412, preserve unsaved drafts,
  reload remote values, and successfully save again. Go emits the canonical
  `etag_mismatch` problem recognized by all three editors.

Post-review verification passed `go test ./...`, `go vet ./...`, full console
verification with 460 tests, and `scripts/go-integration.sh`: race-enabled
services in 49.401 seconds and all four Chromium cases in 33.4 seconds, including
SDK metadata and static-console build/manifest checks. The image measurements
above describe the earlier native-image qualification.

## Dependencies

[Recorded module graphs](access-dependencies.json) distinguish nine direct
library requirements, 14 executable modules, and 134 resolved external modules
including test/tool dependencies. Eight direct requirements support production:

| Dependency | Purpose and impact |
| --- | --- |
| `pgx/v5` | PostgreSQL pools, direct SQL, transactions and TLS; existing foundation dependency. |
| `valkey-glide/go/v2` | Existing coordination client and native archive boundary; no new native subsystem. |
| `oapi-codegen/nullable`, `oapi-codegen/runtime` | Generated management contract representation; retained generator/runtime contract boundary. |
| `google/uuid` | Durable installation identity and ordered UUIDv7 feature records; promoted from indirect to direct use. |
| `coreos/go-oidc/v3` | Discovery metadata and maintained signed-token verification, with `go-jose/v4` transitively. |
| `x/oauth2` | Bounded authorization-code exchange and PKCE using the dedicated identity HTTP client. |
| `x/crypto` | Fixed-parameter Argon2id password hashing; standard-library AES-GCM and HMAC cover other secret operations. |

`jsonschema/v6` is the direct test-only requirement for validating service
responses against OpenAPI schemas; it is absent from the executable graph.
`oapi-codegen/v2` remains a pinned development tool. New identity dependencies are
pure Go. GLIDE's prebuilt Rust archive, CGO/C toolchain, glibc runtime, license
inventory, and native-platform requirement remain the documented M1 boundary.

## Screenshots

Screenshots contain disposable test identities and key lookup IDs, captured after
one-time secret dialogs were dismissed.

| Flow | Packaged origin | Vite origin |
| --- | --- | --- |
| Access overview | [Screenshot](access-screenshots/packaged-overview.png) | [Screenshot](access-screenshots/vite-overview.png) |
| Key policy inventory | [Screenshot](access-screenshots/packaged-keys.png) | [Screenshot](access-screenshots/vite-keys.png) |
| Installation settings | [Screenshot](access-screenshots/packaged-settings.png) | [Screenshot](access-screenshots/vite-settings.png) |
| Audit | [Screenshot](access-screenshots/packaged-audit.png) | [Screenshot](access-screenshots/vite-audit.png) |
| OIDC-only profile | [Screenshot](access-screenshots/packaged-oidc-profile.png) | [Screenshot](access-screenshots/vite-oidc-profile.png) |
