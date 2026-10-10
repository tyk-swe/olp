# Configuration promotion with GitHub Actions

The reusable
[`configuration-promotion.yml`](../.github/workflows/configuration-promotion.yml)
plans a configuration artifact on a pull request, comments its actions,
conflicts and blockers, and applies the merged artifact on a push to the
configuration repository's default branch. It uses the generated management
CLI and the same saved-plan destination digest checks as an interactive apply.

Call it from a configuration repository:

```yaml
name: OLP configuration
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
  pull-requests: write
jobs:
  promote:
    uses: tyk-swe/olp/.github/workflows/configuration-promotion.yml@REVIEWED_OLP_COMMIT
    with:
      installation_url: https://olp.example.com
      artifact_path: installation.json
      olp_ref: REVIEWED_OLP_COMMIT
      external_bindings_path: production-references.json
      reviewed_plan_path: olp-plan-review.json
      apply: ${{ github.event_name == 'push' }}
    secrets:
      management_token: ${{ secrets.OLP_MANAGEMENT_TOKEN }}
```

Replace both commit placeholders with the same reviewed, full 40-character OLP
commit containing this workflow and CLI. The workflow builds only that source;
configuration checkout contents are read as data. Artifact and reference paths
must resolve inside that checkout. Its token is written to a private temporary
file for each operation and removed afterward. Fork pull requests require the
caller's explicit secret-access policy; the workflow cannot manufacture missing
credentials or permissions.

The token needs installation-wide configuration/read authority and any
additional scopes required by the artifact: pricing and external credential bindings require `settings`, and
identity or public-catalog changes require `access`. Give a deployment token
only the required scopes. Destination secret references belong in the optional
external-bindings JSON; omit that input when there are no references. The
workflow accepts no plaintext secret bindings in committed configuration.
See [external bindings](external-secrets.md#configuration-promotion) and
[saved-plan semantics](operator-cli.md#configuration-promotion).

Planning failures fail the job after a review comment is prepared. The comment
contains only actions, conflicts and blockers. The complete review artifact
includes the destination origin, destination digest, external-reference digest,
plan digest and actions,
and is retained for seven days. Download `olp-plan-review.json`, commit it to
the configuration pull request, and review that file together with the desired
configuration. Set `reviewed_plan_path` to its path; apply refuses a missing
review. Neither the desired document nor credential bindings enter comments
or review artifacts.

Applies run only when `apply` is true and the event is a push to the caller's
default branch. The push plan must exactly match the committed review,
including the original destination digest and planned actions. Any intervening
management edit, changed desired state or changed binding requires a new
review. Per-installation workflow concurrency serializes jobs in that
repository, and the server rechecks the destination digest transactionally. An uncertain response should be reconciled
against installation state before rerunning deployment.
