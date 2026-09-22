# Route fidelity configuration

Route drafts, revisions and configuration artifacts accept an optional
`fidelity` object:

```json
{"fidelity":{"mode":"strict"}}
```

The modes are `legacy`, `strict`, and `transformed`. Native identity and
qualified interaction are eventual per-plan classes, not provider-wide or
route-wide capability badges. Explicit `{}` defaults to strict. Null, empty
mode strings, unknown fields and duplicated JSON members are rejected; use
`{"mode":"legacy"}` for an explicit return to legacy behavior.

Omission on a new slug retains legacy behavior. Omission when replacing a draft
preserves its contract, and a new draft for an existing same-project slug
inherits the published contract. Restoring a revision copies that revision's
contract, including historical omission. Configuration plan/apply follows the
same precedence: a staged draft's contract, then the published contract, then
legacy for a new route. An omitted field cannot silently downgrade a strict
draft. ETags, dirty-draft behavior, authorization and atomic publication remain
in force.

Management responses represent historical omission as `fidelity: null`.
Configuration exports and runtime snapshots omit the field for those routes,
preserving the old canonical representation and runtime digest. Explicit modes
remain in exports and immutable revisions. Revision diffs report
`fidelity_changed`, `fidelity_before` and `fidelity_after` so transitions are
reviewable.

Drafts may hold a policy conflict for review. Strict drafts with any input or
output redaction fail validation and activation with `fidelity_policy_conflict`.
Pure blocking policies pass that policy check. Configuration promotion checks
both explicit and inherited fidelity before writing draft state; a conflicting
import cannot evade validation by omitting the field. Transformed routes retain
intentional transformation behavior, and legacy routes keep their legacy fidelity
class. Input policy covers effective configured defaults as well as caller text,
so configured tool/schema content cannot bypass the declared policy.

The initial foundation refused strict execution with
`strict_execution_unavailable`. The [compiled planner](strict-planning.md) now
replaces that temporary guard: explicit compatible profiles and generation
interaction templates compile at activation and installation. An implicit legacy
profile fails with `target_capability`; mutation policies still fail first with
`fidelity_policy_conflict`. Tuple-only simulation reports `not_inspected` rather
than claiming semantic admission. Plan-only configuration staging remains available.

Migration 0023 only adds nullable fields; it does not reinterpret or republish
existing routes. Unknown nonempty contract fields change the runtime digest,
so an older reader cannot safely discard them. A reader that cannot install a
new release may retain its previous release: digest validation alone is not a
fleet-wide strict cutover. The integrated migration qualification must account
for old replicas and current authority during upgrades and rollback.

Validation executed on 2026-09-22:

- `make check` passed contract generation, formatting, Go vet and local tests,
  console checks and 617 tests across 68 files, and 14 script tests.
- Route, runtime and configuration package tests passed with `-race`.
- The public fidelity tests and existing content-policy, configuration-promotion,
  route-retirement, response-lifecycle and migration recovery/history tests passed
  with `-race` against disposable PostgreSQL and Valkey services.
- The historical snapshot digest is checked against a value captured using the
  pre-fidelity serializer at checkpoint `1030e6ef` and the unchanged routing
  fixture. Populated forward migration from the pre-0021 prefix through 0023
  retains omitted legacy contracts, the installed digest, sessions, encrypted
  replays, OIDC identity and working inference.

These checks qualify this configuration foundation. Strict inference and the
complete planner remain unavailable until #213 is integrated; no inference
quality or complete-interaction claim follows from storing this mode.
