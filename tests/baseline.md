# Test alignment baseline

Measured 2026-10-25 at commit 4476e63c. This report is the measured starting
point for strict alignment with the testing rules in AGENTS.md. Every number
below comes from a command quoted in this file, run on that commit with default
build tags. The integration lane is not part of these numbers, and that limit
matters when reading them.

## Inventory

The regenerable inventory lives in baseline-inventory.md next to this file.
Regenerate it with the command below. The inventory tracks the current tree,
so its counts move as tests land or are deleted, while the counts quoted in
this report stay anchored to the baseline commit above.

```sh
node scripts/test-baseline.mjs tests/baseline-inventory.md
```

At the baseline commit the tree holds 554 Go test files and 41 JS or TS test
files. Go tests concentrate in internal (332) and tests (216), with 6 in sdk.
JS and TS tests concentrate in console (19), tests (18) and scripts (4). The
gate and inventory scripts landing with this report add four script test files
and two Go test files, which brings the current inventory next to this report
to 556 Go test files and 45 JS or TS test files.

Placement is already mostly mapped. Of 127 Go packages with source files, 85
carry a sibling test file and 42 carry none, per the inventory. The substantive
zero-test packages are internal/batches (11 source files), internal/billing
(11), internal/egress (7) and internal/cache (6). The rest are single-file
contract packages, generated code, test helpers, or package main fixture code.

Three trees hold no test files at all and their coverage stories differ. The
plugins tree is genuinely covered from tests/integration, which builds and
installs each plugin through testutil.BuildPlugin. The cmd/olp binary modes are
exercised from tests/integration/process_test.go through OLP_TEST_BINARY, but
the argument and account glue inside the binary is untested, which makes cmd a
real gap. The clients/continuation helpers have importers under tests/sdk-smoke
with no current driver, which makes clients a real gap too.

The console is the sparsest surface. Only 6 of 103 source directories under
console/src hold a sibling test, per the inventory. The largest untested directories are
console/src/lib/features/providers (41 source files),
console/src/lib/features/routes (21), console/src/lib/components (14),
console/src/lib/features/access/api-keys (14) and
console/src/lib/features/access/session (14).

## Coverage, container-free lane only

Go coverage comes from the commands below. The first reports each package
against its own tests only. The second attributes cross-package coverage with
-coverpkg, deduplicated per coverage block because each test binary appends the
full block set to the merged profile.

```sh
go test -mod=readonly -timeout=5m -cover ./...
go test -mod=readonly -timeout=5m -coverpkg=github.com/tyk-swe/olp/... \
  -coverprofile=/tmp/olp-coverpkg.out ./...
awk 'NR>1 {key=$1; if(!(key in stmts)) stmts[key]=$2; if($3>0) hit[key]=1}
  END {for(k in stmts){split(k,a,":"); n=split(a[1],p,"/");
  pkg=""; for(i=1;i<n;i++) pkg=pkg p[i] "/";
  tot[pkg]+=stmts[k]; if(hit[k]) cov[pkg]+=stmts[k]}
  for(k in tot) printf "%.1f %s\n", 100*cov[k]/tot[k], k}' \
  /tmp/olp-coverpkg.out | sort -n
pnpm --dir console test --coverage
```

Both Go runs exit 0 with no failing package. The own-package view covers 116
packages with 2 at 90 percent or above, 13 between 70 and 90, 24 between 50 and
70, and 77 below 50, for a median of 27.6 percent. The attributed view lifts the
picture to 7 packages at 90 percent or above, 28 between 70 and 90, 26 between
50 and 70, and 55 below 50, for a median of 51.5 percent. Even attributed,
internal/billing measures 0 percent because no container-free test calls it.
internal/access measures 0.4 percent own and 4.2 percent attributed because its
real suites carry the integration build tag and this lane never selects them.

Read these numbers as the default lane floor, never as proof that a package is
untested end to end. Any coverage floor set from this baseline applies to the
same default-tag lane and only moves up.

The console run passes 8 test files and 28 tests. Coverage across all files is
39.6 percent statements, 25.31 percent branches, 34.25 percent functions and
39.34 percent lines. That number is low mostly because most component and route
code has no unit test at all and is only reached by Playwright journeys.

## Classification against the AGENTS.md rules

A mechanical sweep plus deep reading of the worst files found a suite that is
unusually clean for its size. There are no snapshot tests, no focused tests, no
mock-framework call verification, and no exact err.Error() equality anywhere.
Only four t.Skip calls exist. The sweep commands behind those zero counts are
the ones below. Time handling mostly follows the right pattern, with a fixed
injected clock for bucket and expiry logic, and wire dates such as anthropic
version headers are protocol conformance rather than calendar pins.

```sh
grep -r --include='*_test.go' -c 't\.Skip' internal tests cmd | awk -F: '{s+=$2} END {print s}'
grep -rn 'toMatchSnapshot\|toMatchInlineSnapshot\|\.only(' console/src console/tests
grep -rn 'err\.Error() !=\|err\.Error() ==' --include='*_test.go' internal tests
find internal/catalog/testdata/invalid -type f | wc -l
```

The cleanup backlog below is the whole deep-read verdict set, ranked by risk
area in the order access and security, billing and usage, gateway, protocols,
then catalog. Rewrite means the file keeps its mapped place and its assertions
become behavior or invariant assertions. Delete means the file carries no
salvageable behavior signal.

| File | Category | Verdict |
|---|---|---|
| internal/secrets/purpose_test.go | structure | rewrite |
| internal/access/testdata/operations.golden | orphan | delete |
| internal/management/contract/types_test.go | shallow | rewrite |
| internal/database/guardrail_types_test.go | structure | delete |
| internal/database/retained_types_test.go | structure | delete |
| internal/retainedapi/supports_test.go | structure | rewrite |
| internal/routes/retained_operations_test.go | structure | delete |
| internal/connectors/testdata/profiles.golden | orphan | delete |
| internal/connectors/testdata/supports.golden | orphan | delete |
| internal/providers/testdata/certifiable.golden | orphan | delete |
| internal/providers/testdata/kinds.golden | orphan | delete |
| internal/providers/testdata/vendors.golden | orphan | delete |
| tests/codecli/model_hint_test.go | outside-fact pin | rewrite |
| internal/operations/contract_test.go | structure | rewrite |
| scripts/m9-endpoints.test.mjs | structure | delete |
| tests/bench/mockupstream/dialects_test.go | outside-fact pin | rewrite |
| console/src/lib/features/resources/nativeLifecycle.test.ts | structure | rewrite |
| internal/catalog/catalog_test.go | shallow | rewrite |

Several outside-fact pins stay off the backlog on purpose because they already
meet the citation exception in AGENTS.md. Catalog vendor prices carry
provenance with a source URL and observed date enforced by the validator.
Billing fixtures cite the vendor cost APIs by URL and shift dates relative to
today. Retained native fixtures carry a review date and per-fixture vendor
documentation links. The FOCUS export pins its release URL and digest and
refuses any other source. The authorization golden has a live reader and gates
generated clients, so it stays while its six orphaned siblings go.

## Cleanup slice order

Slices run in the risk order used above. Security-owned files come first, with
internal/secrets and the access orphan. Database, retained, and routes
structure tests follow. Orphan goldens form one cheap deletion slice. The two
outside-fact pins come next because each rewrite needs a cited source or an
invariant in place of a pinned literal. The catalog shallow test closes the
list because wiring its 23 unconsumed invalid fixtures is additive behavior
coverage rather than a rewrite. The console lifecycle test rides with the first
console slice so its mutation pilot has a real rewrite to measure.

## Mutation pilot

The pilot ran Gremlins v0.6.0 on two Go packages and StrykerJS 10.1.1 on one
console module, using the commands below. Gremlins copies the full module per
worker with no ignore support, so both Go runs used a clean checkout without
node_modules, four workers, and a disk-backed TMPDIR.

```sh
go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 unleash ./internal/secrets
go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 unleash ./internal/protocols/wire --workers 4
pnpm --dir console test:mutation --mutate 'src/lib/features/usage/priceEntry.ts'
```

internal/secrets produced 92 mutants with 1 killed, 0 lived, and 91 not
covered. Efficacy reads 100 percent while mutant coverage reads 1.09 percent,
which proves its only test file leaves seal and unseal behavior untested and
proves an efficacy-only gate would pass vacuously. The Go gate therefore pairs
an efficacy threshold with a mutant coverage threshold.
internal/protocols/wire produced 17 mutants with 5 killed, 2 lived, and 10 not
covered, for 71.43 percent efficacy and 41.18 percent mutant coverage in about
two seconds. The console pilot produced 64 mutants in priceEntry.ts with 47
killed, 16 survived, and 1 with no coverage, for a 73.44 percent mutation score
in about eight seconds. Survivors cluster on the container fee count boundary,
which is a real missing boundary test.

Gremlins v0.6.0 accepts threshold flags but exits 0 even at 0 percent efficacy
against a 101 percent threshold, measured during the pilot. The Go gate
therefore follows the bench gate shape. Gremlins measures and writes a JSON
report, and scripts/check-mutation.mjs decides, with its own tests. Its diff
filtering also only matches changed lines when unleash runs at the module
root, so the changed-code gate snapshots the tree in a worktree and runs one
diff-scoped pass there. Whole-package runs over the packages this change
touched measure catalog at 77.66 percent efficacy, operations at 74.98,
retainedapi at 94.83, and secrets at 91.80, with connectors at 46.72 and
routes at 66.67 below the starting bar as pre-existing package debt for the
weekly report.

The starting gate values follow from those measurements. Changed Go packages
must reach 70 percent efficacy and 40 percent mutant coverage, and changed
console modules must reach a 70 percent mutation score. Both bars sit just
under the weakest well-tested pilot subject, and both rise toward over 90
percent as cleanup slices land. Stryker runs the Vitest unit project only,
because a component test helper imports the authorization golden from outside
the console root and the Stryker sandbox cannot resolve paths outside the
copied tree.

## Cleanup outcome

The six orphan golden artifacts were deleted after a reader search over the
tree confirmed zero readers for each. The secrets package test now asserts
digest domain separation and seal purpose binding as behavior, and new key
ring and password tests cover parsing, file mode, rotation, and rejection
paths. Measured on that package after the rewrite, efficacy is 91.80 percent
and mutant coverage 66.30 percent, against 100 percent vacuous efficacy and
1.09 percent mutant coverage before. The retained supports matrix, the route
registration test, and the management origin test now derive their expected
sets from the declaring registries and the OpenAPI enum instead of restating
them, and deriving the origin set surfaced one origin the old list had missed.
The operations contract test now checks every ledger row that declares an
admission contract instead of only pending rows. The catalog tests consume
the valid fixture and all 23 invalid fixtures, which are accepted or rejected
as their directory claims. The route registration test was rewritten and kept
rather than deleted, for the same derivation reason as the other registry
tests. Derived expected sets share one blind spot worth naming. Deleting or
renaming a declaration itself, a purpose, a family, or an origin, moves both
sides of the agreement at once, so only a deliberate removal or rename passes
silently. Purpose names keep one independent pin either way, since the
security architecture table lists every stored purpose name and renaming one
would orphan stored digests and ciphertext until that table is regenerated
deliberately. The code-mode model hint test derives its
expected body model from the selected and override models and no longer
asserts that an upstream header must be absent. The mock upstream dialect
test asserts status discrimination and the echoed numeric code instead of
vendor error taxonomy literals. The console lifecycle test asserts rejection
and no dispatch instead of exact error wording, deriving its size bounds from
the exported byte limit. Its module still measures 62.80 percent mutation
score against the 70 percent starting bar, so it stays on the weekly report
as known debt rather than being padded with shallow tests to clear the bar.

Three files were reclassified on review and kept unchanged. The two database
type tests and the M9 ledger test assert that independently owned project
artifacts agree with each other, between the API enums and the persistence
constraints in one case and between the endpoint ledger, the installed SDK
pins, and the recorded evidence paths in the other. That is the invariant
form the rules endorse, a value derived from another, and each side is read
from its owning source rather than restated. Deleting them would remove the
only container-free drift check between those layers, so they stay until a
service-level sweep can replace them.
