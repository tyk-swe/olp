# Named guardrails

Reusable, project-scoped guardrail definitions use OLP's existing bounded RE2
content-policy engine. The `builtin.regex` type supports input block/redact rules
and unary output rules with the same bounds and fidelity checks as route
[content policies](gateway.md#content-policy).

The management API provides list/create/get/update/retire operations under
`/api/v1/guardrails`, plus immutable policy reads at
`/api/v1/guardrails/{guardrail_id}/revisions/{revision_id}`. Reading requires
`read` and visibility of the owning project. Changing a definition requires
`configure` and project change authority. Updates and retirement require the
observed `If-Match`; writes use `Idempotency-Key` and the ordinary audit path.

```json
{
  "name": "Private account filter",
  "project_id": "PROJECT_UUID",
  "type": "builtin.regex",
  "policy": {
    "rules": [{
      "id": "account",
      "phase": "input",
      "pattern": "ACCOUNT-[0-9]{8}",
      "action": "block"
    }]
  }
}
```

Creation and each update return the guardrail identity, observed ETag, immutable
revision identity/version, and canonical policy. Copy the selected `policy` into
a route's `content_policy` when publishing. Terraform can compose that field
from `jsondecode(openllmproxy_guardrail.filter.definition).policy`.
The route revision then owns its copied policy, so changing or retiring the
named definition cannot change an already published invocation. Publication
continues to enforce the route's fidelity and inspectable-surface requirements.
Serving uses the pinned runtime policy and performs no definition lookup.

Retirement removes a definition from the active collection and preserves its
immutable revisions. Retained history also preserves the project boundary;
deleting a project with guardrail history returns `project_in_use`. Rule evidence
continues to contain only rule ID, phase, action and outcome. Validation errors
omit patterns and replacements.

Configuration export includes active definitions as project/name/type/policy
entries, with portable project names. Local UUIDs, revision identities, authors
and retired history remain local. Planning describes create/replace/reuse;
repeated apply reuses unchanged definitions. Routes in the artifact retain their
own copied content policies, including when they differ from a definition's
current revision.
