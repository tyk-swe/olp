# ADR 0004: Freeze compatibility and migration contracts

- **Status:** Accepted target (approval pending)
- **Decision issue:** XOD-86
- **Date:** 2026-07-19
- **Owners:** Storage/API architecture

## Context

The enterprise hierarchy will add scope to installation-global records and
public management operations. That work cannot safely begin until an old and a
new process have an unambiguous database, wire, and rollback contract.

The repository already contains useful point protections. Migrations 0022-0024
fence unsafe legacy runtime, usage-rollup, and OIDC writes; the runtime snapshot
uses defaults for selected additive fields; SQLx verifies applied migration
checksums; and the upgrade rehearsal restores and migrates a previous schema.
Those protections do not by themselves define which mixed binaries are
supported, prevent a future destructive migration, or preserve a released API
contract. In particular, a generated OpenAPI file compared only with the same
revision of its generator is a drift check, not a compatibility check.

This ADR freezes the contract that M1 and later milestones must follow. Its
machine-readable counterpart is
[`../contracts/compatibility.json`](../contracts/compatibility.json).

## Decision

### Supported release and schema window

OLP supports application release `N` and `N-1` during the expand and migrate
phases. The database moves forward only. The migration process is the sole new
binary allowed to run against an `N-1` schema; an `N` gateway, control, or
worker on that schema is unsupported and must fail startup or readiness.

The default-scope behavior of every legacy v1 operation remains available in
every supported `N`/`N-1` combination. A new-only feature stays disabled while
any `N-1` component is present.

| Database phase | Gateway | Control | Worker | Contract |
| --- | --- | --- | --- | --- |
| Expand or migrate, all `N` | `N` | `N` | `N` | Supported, including new behavior once its gate is enabled |
| Expand or migrate, any mix of `N` and `N-1` | `N` or `N-1` | `N` or `N-1` | `N` or `N-1` | Supported for default-scope reads and writes; new-only behavior is disabled |
| Contract | `N` | `N` | `N` | Supported |
| Contract, any `N-1` role | `N-1` in any cell | any | any | Unsupported; startup/readiness or the affected write fails closed before state changes |
| Old schema | any new server role | any | any | Unsupported; only `N` migration mode may run |

This matrix covers all eight choices of old/new gateway, control, and worker,
not only a homogeneous old or new deployment. Conformance must prove the
crossed data paths: runtime compiled by each control version is readable by
each gateway version, and usage emitted by each gateway version is readable by
each worker version. A fail-closed combination is successful only when it
rejects before a partial write and returns the documented failure; an
unexplained crash or corrupt state is not a compatibility result.

This is the contract a release pair must qualify, not a claim that the current
repository has already qualified one. There is presently no immutable previous
release version plus binary or OCI digest in release metadata. Until that
identity, its representative fixture, and all eight live combinations pass,
mixed-version rolling operation is **unqualified and unsupported**; operators
must use the maintenance-window procedure in `docs/operations.md`. A schema
number alone does not identify an N-1 application.

The pre-M0 migrations through 0027 are grandfathered, not exemplars for future
migrations. Their existing fail-closed behavior and the operations runbook
remain authoritative. They are immutable, and changing one is not an escape
from this policy.

### Expand, migrate, contract

Every migration after 0027 has a machine-checked sidecar under
`tests/migration-fixtures` and belongs to exactly one phase.

1. **Expand** adds a representation that all legacy readers can ignore and all
   legacy writers can omit. Columns are nullable, new constraints are not yet
   enforced against legacy writes, and no old object or value is removed or
   renamed. The feature gate is off.
2. **Migrate** makes `N` dual-read and dual-write while retaining legacy data as
   an authority for `N-1`. Backfill is a bounded, restartable, idempotent job
   with a persisted cursor, progress/error telemetry, and a verification query.
   An unbounded data rewrite inside a schema migration is prohibited by
   default. Re-running after interruption must converge without duplicating an
   event, audit record, or side effect.
3. **Contract** is a separate later release. It may run only after the minimum
   supported version no longer reads or writes the legacy representation, the
   feature has been enabled successfully, backfill verification reports zero
   legacy-only rows, queued events and stored replays have aged out or been
   converted, and every old workload is drained. The contract sidecar names
   the earlier expansion migrations. A contract schema treats every `N-1`
   component as unsupported and fail-closed.

The checker proves that every migrate sidecar has an earlier expand sidecar
with the same feature gate. A contract sidecar may reference only earlier,
post-M0 expand/migrate sidecars with that same gate, must include at least one
expand phase, and records zero legacy-only rows, workloads, queued events, and
idempotency replays plus the evidence that verified those zeros. This makes the
phase chain and removal preconditions machine-enforced rather than prose-only.

Adding a non-null column with a default, renaming in place, immediately
validating a large constraint, changing an enum because PostgreSQL permits it,
or installing a trigger that silently changes old writes does not bypass these
phases. The checker flags those constructs. A genuine bounded exception needs
one rule-specific rationale and an approving `XOD-*` issue in the migration
sidecar; unused or blanket exceptions fail validation.

`scripts/check-migration-contract.sh` freezes the cutoff at 0027, validates the
sidecar and role matrix for every later migration, and rejects destructive,
blocking, unbounded, or behavior-changing SQL outside the approved phase. Its
`--self-test-rules` mode exercises both unsafe examples and safe expansion
controls so weakening a rule fails CI. `scripts/test-migration-contract.sh`
also builds an isolated three-phase fixture, proves a valid chain passes, and
proves destructive expansion and an unlinked contract phase fail.

### Feature gates and dual formats

A feature gate identifies one durable capability, defaults off, and is safe to
evaluate on every replica. Enabling it does not change the database schema.
While `N-1` exists, an `N` producer emits the oldest common event and runtime
format and writes both old and new database representations. An `N` consumer
reads both formats. The gate may become on-by-default only after the mixed
matrix and backfill verification pass. Removing the gate is contract work.

Dual write is one transaction when both representations are in PostgreSQL. If
one side is a queue or external system, the transactional outbox carries a
stable event identity and retry is idempotent. A mismatch is observable and
fails the mutation; it is never repaired by silently preferring the new copy.

### Rollout failure decision

Before rollout, operators create the quiesced PostgreSQL backup and snapshot
the mounted keyring described by the operations runbook. At the first failed
health or contract check, freeze management mutation and inference admission,
record the last successful migration, current phase, gate state, whether a
new-only write occurred, and which old workloads remain.

| Failure point | Decision |
| --- | --- |
| Before any database change | Roll back the application |
| After expand/migrate, before a new-only write | Application rollback is allowed; forward-fix is preferred when quick and lower-risk |
| After a new-only write or any contract migration | Do not run old binaries. Forward-fix on the upgraded database |
| Forward-fix cannot meet the recovery objective | Restore the final pre-upgrade database and matching keys into a replacement cluster with fresh Valkey, verify it with the old release, then redirect traffic |

Restore is never performed in place over the failed cluster. The release gate
must inject a post-migration failure and prove both decision branches: a
corrected candidate can forward-fix the upgraded database, and the pinned
previous release can start and serve the frozen default-scope contract on the
restored pre-upgrade fixture.

`scripts/decide-upgrade-recovery.sh` makes the decision table executable from
an immutable evidence record and fails closed when restore is required without
the pre-upgrade backup and matching key snapshot. Its table-driven regression
suite covers pre-change rollback, safe expand rollback, forward-fix after new
writes, and replacement-cluster restore; it does not claim that a live N-1
artifact has been qualified.

### Frozen public and stored contracts

The four contract families use semantic versions independently of the OLP
product version.

| Family | Frozen baseline | Major change | Minor change | Patch change |
| --- | --- | --- | --- | --- |
| Management OpenAPI | immutable `1.0.0` baseline, `/api/v1`, and baseline hash in the machine contract | Remove/rename a path or field, add a required input, narrow a type, or change auth, scope, error, idempotency, cursor, ordering, or side-effect semantics | Backward-readable optional endpoint/field; new behavior remains gated for `N-1` | Description or behavior fix with no observable contract change |
| Declarative documents | implicit `1.0.0` until an explicit envelope lands | A prior document cannot be read with identical meaning | Optional/defaulted data that old readers ignore and new readers interpret only behind a gate | Canonicalization fix with byte and meaning compatibility |
| Connector configuration/capability | implicit `1.0.0` | A deployed configuration or capability tuple stops loading or changes meaning | Optional capability/configuration that is not emitted to an old strict reader | Validation/message correction without accepted-input change |
| Usage, runtime-hint, and outbox events | implicit `1.0.0` | A producer/consumer cannot decode, retry, deduplicate, or preserve meaning | Optional/defaulted field or event; producers emit the oldest common version during `N/N-1` | Meaning-preserving codec correction |

The v1.0.0 management document is copied to an immutable, version-named
baseline with a pinned byte SHA-256. CI compares the current document to that
separate baseline: it permits new optional surface while preserving every
frozen operation, security declaration, required input, response, enum, and
schema constraint. The baseline is never replaced to make a change pass. An
additive release consciously bumps the API minor version and retains every
earlier v1 baseline. A breaking change requires `/api/v2`; changing only
`info.version` is insufficient.

Strict readers make an otherwise “optional” JSON key breaking. The current
mounted connector configuration rejects unknown fields, and current usage,
runtime, and outbox payloads do not carry an explicit schema version. Until
versioned envelopes and golden cross-version fixtures land, they are treated
as implicit v1 and may not gain an unconditional field or closed-enum value.

### Default-scope legacy behavior

When M1 creates organization, project, and environment rows, every existing
unscoped v1 request resolves to the installation's deterministic default
organization, project, and environment. No existing resource UUID, revision,
API-key lookup ID, usage row, audit row, or runtime selection changes. V1 gains
no required scope header, query parameter, or body field.

Compatibility tests use frozen black-box HTTP requests and responses rather
than current Rust request types or a regenerated client. That prevents a source
change from silently updating both the implementation and its supposed legacy
test. The same corpus runs against fresh and upgraded default-scope fixtures
and every supported mixed-version combination.

### Idempotency and cursors

For an unscoped v1 mutation, an idempotency key is scoped by the authenticated
actor, stable operation ID, and `Idempotency-Key`, matching the current durable
contract. An explicitly scoped operation additionally includes the effective
environment ID. Route aliases and later minor API versions reuse the same
stable operation ID. The retention floor is 24 hours. The same key and request
replays the original status, selected headers, and body; the same key with a
different request fingerprint conflicts. Scope or operation identifiers used
as encryption AAD are never renamed in place.

Cursors are opaque. A new cursor envelope binds the API major, stable operation
ID, effective scope, normalized filters, and ordering. It is valid across `N`
and `N-1` for the same context and rejected with the stable v1 400 problem in a
different context. Existing unversioned v1 cursors remain accepted for the full
deprecation window; clients never decode or synthesize either form. Pagination
goldens prove no duplicate or omitted item across a version handoff.

### Deprecation and removal

A public element is deprecated for at least two minor releases and 180 days,
whichever is later. Its contract marks it deprecated, names a replacement,
includes a release note, and supplies content-free usage telemetry. Public v1
removal occurs only in the next API major.

A stored document, connector format, event, cursor, or replay reader remains
for both the `N/N-1` window and its maximum persisted or queued retention
window, whichever is longer. Contract migration requires evidence that the old
population is zero. A deprecation clock alone never authorizes data removal.

## Required release evidence

The compatibility gate is release-blocking and consists of:

1. the migration checker on every pull request;
2. an immutable, sanitized fixture made by the pinned previous release, with
   migration checksums and representative identity, configuration, runtime,
   idempotency, OIDC, media, usage, audit, and outbox states;
3. generated-to-checked-in OpenAPI drift plus frozen-baseline semantic diff;
4. golden v1 declarative, connector, event, idempotency, and cursor decode tests
   in both directions;
5. the eight-combination gateway/control/worker rehearsal; and
6. injected forward-fix and restore decision tests.

Fixtures contain only generated test credentials and content-free metadata.
The previous binary or image is pinned by immutable digest; constructing an
old schema with the current binary is useful migration testing but is not
previous-release compatibility evidence.

### Evidence present at M0

M0 has useful provisional evidence, but it must not be relabelled as live N-1
qualification:

| Evidence | What it proves | What it does not prove |
| --- | --- | --- |
| `crates/storage/tests/upgrade_0021_postgres.rs` | Current migrations preserve representative schema-0021 route, runtime, OIDC, and usage state and exercise selected database fences | Behavior of an actual released 2.x binary or every old control write |
| `crates/storage/tests/usage_surface_upgrade_postgres.rs` | Pre-0010 usage surfaces retain attribution and completeness through upgrade | The current N/N-1 event codec in both directions |
| `apps/olp/tests/identity_http_postgres.rs` | Current default installation identity/session HTTP behavior | A frozen old client against the future scoped schema |
| `apps/olp/tests/catalog_http_postgres.rs` | Current default provider, route, and API-key HTTP behavior | Unchanged semantics under every mixed role combination |
| `apps/olp/tests/operations_http_postgres.rs` | Current default usage, audit, settings, pricing, and cursor HTTP behavior | Acceptance of frozen legacy cursors after a version handoff |
| `tests/sdk-smoke/run.sh` | Current OpenAI, Anthropic, and Google SDK inference surfaces remain callable | Management API compatibility or an N-1 gateway binary |
| `apps/olp/tests/openapi_drift.rs`, the immutable v1.0.0 baseline, and `scripts/check-openapi-compatibility.sh` | The generated current document agrees with its code and remains semantically compatible with the frozen baseline | Runtime behavior not represented by OpenAPI or compatibility of future stored/event envelopes |

The required sanitized 2.x fixture shape and current evidence mapping live in
[`../../../tests/migration-fixtures/representative-2x.fixture-manifest.json`](../../../tests/migration-fixtures/representative-2x.fixture-manifest.json).
Its `qualification_status` remains `specification_only` until a fixture is
created by an immutable released artifact and its digest is recorded. The
future default-scope suite must replay frozen HTTP bytes and official SDK calls
against fresh, upgraded, restored, and all mixed-role deployments; current
application types cannot serve as that frozen corpus.

## Consequences

- Scope migrations take more than one release but remain operable and
  reversible at the application layer during the supported window.
- Writers and stored envelopes carry explicit compatibility work instead of
  relying on permissive deserialization by accident.
- Destructive schema cleanup is delayed and requires measurable retirement
  evidence.
- Operators have a deterministic failure decision rather than attempting an
  unsafe binary downgrade after forward-only state changes.
- Existing migrations 0001-0027 keep their checksum and behavior; future work
  cannot cite their single-step backfills or fences as precedent.

## Rejected alternatives

- **Test only the latest binary against an old schema.** This does not execute
  old read/write code or old wire decoders.
- **Treat generated OpenAPI drift as compatibility.** Updating generator and
  snapshot together can preserve drift while removing a public field.
- **Use backward migrations.** They cannot safely undo concurrent writes,
  external side effects, secrets, or queued events.
- **Add columns and immediately require them.** Defaults and triggers can hide
  the incompatibility until an N-1 writer reaches the affected path.
- **Depend on operator ordering alone.** Helm migration hooks and workload
  failures can expose combinations outside the happy path; the database and
  binaries must fail safely.
