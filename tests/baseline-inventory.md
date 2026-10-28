# Test inventory

Go test files: 556. JS/TS test files: 45.

| Area | Go tests | JS/TS tests |
|---|---:|---:|
| console | 0 | 19 |
| internal | 334 | 0 |
| scripts | 0 | 8 |
| sdk | 6 | 0 |
| tests | 216 | 18 |

## Go packages with source but no sibling test file

Go packages with source: 127. With a sibling test file: 85.

| Package | Source files |
|---|---:|
| cmd/olp | 1 |
| internal/attribution | 2 |
| internal/batches | 11 |
| internal/billing | 11 |
| internal/bodylimit | 1 |
| internal/branding | 1 |
| internal/budgetcalendar | 1 |
| internal/cache | 6 |
| internal/catalog/cmd/catalogdrift | 1 |
| internal/catalog/drift | 1 |
| internal/config | 1 |
| internal/console | 1 |
| internal/contentpolicy | 2 |
| internal/coordination | 1 |
| internal/durablecontract | 1 |
| internal/egress | 7 |
| internal/mediacontract | 1 |
| internal/operationregistry | 2 |
| internal/operations/generation | 1 |
| internal/pluginindex/cmd/pluginindex | 1 |
| internal/plugins/testdata/carrier | 1 |
| internal/plugins/testdata/fixture | 1 |
| internal/plugins/testdata/otherabi | 1 |
| internal/priority | 1 |
| internal/protocols/awsframe | 1 |
| internal/retainedworker | 1 |
| internal/surface | 1 |
| internal/testutil | 5 |
| openapi | 1 |
| plugins/codex | 1 |
| plugins/opencode-go | 1 |
| plugins/zai-coding | 1 |
| sdk/guardrails | 1 |
| sdk/plugin/abi | 1 |
| sdk/plugin/reference | 3 |
| tests/bench/benchtest | 1 |
| tests/clientfixture | 1 |
| tests/fixtures | 1 |
| tests/fixtures/codex-qualified | 2 |
| tests/fixtures/fidelity | 1 |
| tests/fixtures/research-migration | 1 |
| tests/samlfixture | 1 |

## Console source directories with no sibling test file

Console source directories: 103. With a sibling test file: 6.

| Directory | Source files |
|---|---:|
| console/src/lib/features/providers | 41 |
| console/src/lib/features/routes | 21 |
| console/src/lib/components | 14 |
| console/src/lib/features/access/api-keys | 14 |
| console/src/lib/features/access/session | 14 |
| console/src/lib/features/access/projects | 10 |
| console/src/lib/features/access/profile | 9 |
| console/src/lib/features/runtime/health | 9 |
| console/src/lib/api | 8 |
| console/src/lib/features/providers/api | 8 |
| console/src/lib/features/usage/api | 7 |
| console/src/lib/features/media | 6 |
| console/src/lib/features/model-catalog | 6 |
| console/src/lib/features/overview | 6 |
| console/src/lib/features/plugins | 6 |
| console/src/lib/features/usage/history | 6 |
| console/src/lib/forms | 6 |
| console/src/lib | 5 |
| console/src/lib/features/access/budgets | 5 |
| console/src/lib/features/access/invitations | 5 |
| console/src/lib/features/access/mfa | 5 |
| console/src/lib/features/observability | 5 |
| console/src/lib/features/settings | 5 |
| console/src/lib/features/access/audit | 4 |
| console/src/lib/features/access/oidc | 4 |
| console/src/lib/features/access/saml | 4 |
| console/src/lib/features/access/users | 4 |
| console/src/lib/features/providers/models | 4 |
| console/src/lib/lists | 4 |
| console/src/lib/features/access/budget-groups | 3 |
| console/src/lib/features/access/budget-increases | 3 |
| console/src/lib/features/access/notifications | 3 |
| console/src/lib/features/access/organizations | 3 |
| console/src/lib/features/access/scim | 3 |
| console/src/lib/features/access/setup | 3 |
| console/src/lib/features/access/tokens | 3 |
| console/src/lib/features/access/workload-issuers | 3 |
| console/src/lib/features/guardrails | 3 |
| console/src/routes | 3 |
| console/src/lib/features/access | 2 |
| console/src/lib/features/access/attribution | 2 |
| console/src/lib/features/access/attribution-budgets | 2 |
| console/src/lib/features/access/end-users | 2 |
| console/src/lib/features/access/limit-templates | 2 |
| console/src/lib/features/access/login | 2 |
| console/src/lib/features/access/route-groups | 2 |
| console/src/lib/features/access/route-limits | 2 |
| console/src/lib/features/access/sessions | 2 |
| console/src/lib/features/caching | 2 |
| console/src/lib/features/configuration | 2 |
| console/src/lib/features/fleet | 2 |
| console/src/routes/(console)/api-keys | 2 |
| console/src/routes/(console)/media-jobs | 2 |
| console/src/routes/(console)/providers | 2 |
| console/src/routes/(console)/requests | 2 |
| console/src/routes/(console)/routes | 2 |
| console/src/lib/features/access/session/test | 1 |
| console/src/lib/features/code-mode/test | 1 |
| console/src/lib/features/resources/api | 1 |
| console/src/lib/features/resources/test | 1 |
| console/src/lib/features/runtime | 1 |
| console/src/lib/features/settings/api | 1 |
| console/src/lib/json | 1 |
| console/src/routes/(console)/[...path] | 1 |
| console/src/routes/(console)/access | 1 |
| console/src/routes/(console)/agents | 1 |
| console/src/routes/(console)/api-keys/new | 1 |
| console/src/routes/(console)/audit | 1 |
| console/src/routes/(console)/budget-increases | 1 |
| console/src/routes/(console)/catalog | 1 |
| console/src/routes/(console)/code-mode | 1 |
| console/src/routes/(console)/execution-debits | 1 |
| console/src/routes/(console)/guardrails | 1 |
| console/src/routes/(console)/health | 1 |
| console/src/routes/(console)/media-jobs/[jobId] | 1 |
| console/src/routes/(console)/models | 1 |
| console/src/routes/(console)/organizations | 1 |
| console/src/routes/(console)/playground | 1 |
| console/src/routes/(console)/plugins | 1 |
| console/src/routes/(console)/project-policies | 1 |
| console/src/routes/(console)/provider-resources | 1 |
| console/src/routes/(console)/providers/[providerId] | 1 |
| console/src/routes/(console)/providers/new | 1 |
| console/src/routes/(console)/requests/[requestId] | 1 |
| console/src/routes/(console)/routes/[routeId] | 1 |
| console/src/routes/(console)/routes/[routeId]/revisions | 1 |
| console/src/routes/(console)/routes/new | 1 |
| console/src/routes/(console)/scim-provisioning | 1 |
| console/src/routes/(console)/settings | 1 |
| console/src/routes/(console)/settings/profile | 1 |
| console/src/routes/(console)/test | 1 |
| console/src/routes/(console)/usage | 1 |
| console/src/routes/(console)/workload-issuers | 1 |
| console/src/routes/catalog/public/[projectId] | 1 |
| console/src/routes/invitations/accept | 1 |
| console/src/routes/login | 1 |
| console/src/routes/setup | 1 |
