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
the existing intentional transformation behavior, and legacy routes retain
their historical policy behavior.

This foundation does **not** implement strict inference. Strict activation and
runtime installation fail with `strict_execution_unavailable` until
[#213](https://github.com/tyk-swe/olp/issues/213) supplies complete compiled
interaction-plan admission. Activation checks policy conflicts before that
availability condition. Plan-only configuration staging is still available.

Migration 0023 only adds nullable fields; it does not reinterpret or republish
existing routes. Unknown nonempty contract fields change the runtime digest,
so an older reader cannot safely discard them. A reader that cannot install a
new release may retain its previous release: digest validation alone is not a
fleet-wide strict cutover. The integrated migration qualification must account
for old replicas and current authority during upgrades and rollback.

Validation evidence for this slice is recorded after the public API checks.
