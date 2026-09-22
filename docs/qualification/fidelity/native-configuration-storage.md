# Native configuration storage conservation

#224 fixes native configuration values changing between the public API and
serving publication. The original public regression saved `-0` and read back
`0`. PostgreSQL `jsonb` also expands exponent notation; ordinary Go `any`
decoding rounded large integers and long decimals in encrypted response replay.
Neither change can be repaired by a browser editor after the values are lost.

[Migration 0024](../../../internal/database/migrations/0024_native_configuration_sources.sql)
changes the existing authoritative native-source columns to PostgreSQL `json`:

| Authority | Column | Reason |
| --- | --- | --- |
| Provider draft | `providers.configuration` | Owns dialect defaults, bindings and unknown native subtrees |
| Immutable provider revision | `provider_revisions.configuration` | Must retain exactly the activated native values |
| Published serving release | `runtime_releases.snapshot` | Carries those native values under the existing recorded digest |

There is no independently mutable shadow document. Existing transactions,
revision ownership, encryption, release installation and digest verification
remain authoritative. The digest algorithm is unchanged. The migration converts
historical `jsonb` representations as they stand and never rewrites their recorded
hashes. Historical signs, exponent spellings or object ordering already lost to
`jsonb` cannot be reconstructed; old normalized values remain normalized.

Apply the forward migration through the existing release procedure and restart
application/worker connections afterward. Column-type DDL invalidates prepared
statement result types; the migration qualification replaces the old connection
cache before the new reader starts. Older binaries retain the existing refusal
to start against migration history they do not understand. No historical SQL
migration file was edited.

The storage audit also covered the surrounding paths:

- Provider models/capabilities/slots and route/routing metadata stay in `jsonb`.
  Their revision arrays contain typed identities/certification metadata, while
  native model defaults remain inside the authoritative configuration. Model
  metadata extraction now uses a `json` fallback without coercing its parent.
- Export, plan/apply, revision restore, resource resolution and runtime reload
  already use typed configuration with `json.RawMessage` native members. They
  retain native values after the database stops normalizing them. Network-binding
  comparison now uses that typed representation instead of a `float64` map.
- Idempotency responses remain encrypted in the existing secrets authority.
  Their response body now stays raw JSON during replay, preserving the original
  stored response instead of decoding native values through `any`.
- Durable media job rows contain identities/lifecycle metadata, not persisted
  prompts or native request payloads. Their pinned snapshot slot reader now uses
  the `json` array operator. Reservation compatibility compares native
  configuration under the existing provider lock, preserving number spellings,
  presence and ordered values; only the existing quota-only change is ignored.
  It does not cast native configuration back to `jsonb` for equality.
- Resource metadata, accounting, pricing, policy and other unrelated metadata
  columns were not converted.

The public regression corpus contains `-0`, `1e-1000`, an exact long decimal,
`9007199254740993`, explicit null/false/zero/empty values, ordered arrays, schema
metadata including an inert `__proto__` property, and unknown native subtrees.
Assertions compare raw native JSON rather than decoding it through floating
point or reusing a production codec as the reference.

Validation on 2026-09-22:

- Public create/read, unrelated name edit, activation, immutable revision reads,
  changed revision/restore, exact encrypted replay, portable export/plan/apply/
  re-export and repeated no-op apply passed the complete corpus.
- A fresh runtime manager verifies and installs the persisted snapshot, then
  sends exact provider-bound defaults to the independently scripted local
  provider. Deliberately changing `-0` to `0` without changing the hash is rejected
  as a digest mismatch; the recorded hash remains unchanged.
- A real pre-0024 schema prefix retains its old normalized value, every prior
  release's source bytes and digest, and a working published route after upgrade.
  An injected failure on the second column alteration rolls back schema/history;
  retry and repeated migration succeed. New edits preserve `-0` after migration.
- The public configuration/promotion/migration suites passed with race detection
  in 15.429 seconds. Selected media lifecycle, quota and revocation service suites
  passed with race detection in 3.601 seconds; media claim checks also passed.
- Feature unit tests and focused feature race tests passed. All 617 console
  tests and 19 script tests passed separately. The original replay decoder was reintroduced only
  through a temporary Go overlay: the public restore replay failed on native
  response conservation, independently confirming the replay regression.

The initial `make check` encountered the separate existing egress proxy timeout
classification failure in
`TestConnectionPhaseTimeoutsAndCancellation/proxy-connect-timeout`. That failed
run remains recorded in `/tmp/olp-spec-context/native-config-check.log`.

After integrating the root workflow's validated proxy deadline fix `f39064c6`
and independent digest-test timestamp correction `4f1f47f0`, a fresh **`make
check` passed**: contract generation, Go formatting/vet/local tests, console
formatting/types/lint, all 617 console tests and 19 script tests. The final log
is `/tmp/olp-spec-context/native-config-check-final.log`. No storage test or
assertion was weakened to obtain that result.

This receipt does not claim complete specification qualification. Frozen
references and performance artifacts remain unchanged; the current release
inventory is regenerated for the new tests.
