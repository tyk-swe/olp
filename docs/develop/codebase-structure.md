# Codebase structure baseline

This page records how the codebase is put together, where that structure
makes changes expensive, and what the current untangle pass is doing about
it. It is the decision log for that pass. Every problem below was found by
asking one question of the code as of commit `97e63b08`. Should these pieces
change together, and does the layout say so. The ownership map itself lives
in [Architecture and gateway execution](architecture.md).

## Baseline

The numbers below come from `go list ./...`, a count of exported top level
declarations per package outside tests and generated files, and git
co-change over the last 400 commits with bulk commits over 60 files
excluded. The unit baseline is `make test-unit-go` at 8.3s of its 30s
budget.

| Measure | Baseline | Target for this pass |
| --- | --- | --- |
| Go packages in `internal/` and `cmd/` | 189 | no growth without a new owner |
| Import edges between project packages | 1142 | down, and no new edge without updating the recorded graph |
| Top level packages under `internal/` | 89 | 86 or fewer |
| `internal/gateway` kernel, own files | 34858 lines, 65 project imports | smaller, with the models surface extracted |
| `Run` in `cmd/olp/run.go` | about 600 lines in an 822 line file | under 350 lines for `Run` itself |
| Packages importing `internal/access` | 66 | 62 or fewer |
| Kernel call sites discovering runtime capabilities by type assertion | 8 | 0, with derivation in one place |
| Commits touching one Go package | 51 percent | recorded only, it follows from the rest |

A package with many exported declarations per line is not automatically a
problem. Contract packages such as `internal/oif` and
`internal/protocols/wire` are interfaces by design, and the audit found
`internal/oif` to be a healthy hub with no database, HTTP, or transitive
project dependencies. The same applies to most tiny packages. Of the 23
small top level packages audited, 18 are load-bearing vocabulary or cycle
breakers and stay, including `consumerscope` at five lines, which exists
because `access` imports `workload` and `workload` needs the scope names.

## Problems and fixes

The import graph is enforced only inside the gateway. The gateway subpackage
rules in `scripts/check-lint.sh` work well, and nothing equivalent stops a
new edge from a stable package to a volatile one anywhere else. The fix
records every production import edge in `scripts/project-imports.txt` and fails
the lint gate when the real graph and the recorded graph disagree, so a new
dependency becomes a reviewed decision instead of an accident.

Composition knowledge is concentrated in one function. `Run` predeclares
about twenty plane variables, builds the gateway dependencies by mutating a
struct across five conditional blocks, and relies on defer order and one
late bound variable for correctness. The MCP guardrail closure captures the
gateway variable before the gateway is constructed, which is safe today only
because requests cannot arrive until listeners bind. The fix extracts the
runtime core, the gateway templates, and the management bundle into
constructors called from the same sites, keeps every defer and shutdown
step in `Run`, and turns the twenty positional arguments of `startWorkers`
into a dependency struct.

The gateway kernel hides a second module. About 11900 lines of pinned and
retained resource handling use a generation `call` only as an accounting
bag, eight retained wrappers repeat one skeleton and have already drifted,
and five capabilities were discovered on the runtime by type assertion at
eight call sites rather than declared. This pass extracts the models catalog, which needs no
call at all, splits `Runtime` into named parts under one union so existing
implementations keep compiling, and declares the asserted capabilities as
explicit optional dependencies. Unset facets are still derived from the
runtime, now in one place at construction instead of at every call site. Extracting the pinned executor itself is
recorded as follow-up work because it changes the shape of settlement, and
it deserves its own change with its own review.

Four small packages hid almost nothing and had one owner each, and all four
are now folded into `internal/access`. The MFA package was one function
called only from `access`, which already owned the rest of MFA. The branding
package was a single field wrapper over the access server. The budget
calendar kept window boundary math with no callers, since production
windows come from SQL, and only its timezone validation survives. Settings
stapled two concerns together in one package, an HTTP server and transaction
helpers over access-owned tables, and both halves now live where their callers
already pointed.

`internal/access` is the one unhealthy hub, with 66 importers at baseline,
107 symbols in external use, and a concrete server passed whole to feature
packages. A full split is too wide for this pass, so the cheap cuts went
first. The end user identifier validator moved beside the digest validator
in `access/authority`, which removed the access edge from `consumerauth`
and from gateway ingress, and the two usage packages that imported access
only for the database querier shape now declare it locally, as
`internal/metric` already does. Access importers fell from 66 to 60.

Promotion repeats queries rather than structure. There is no section
interface, and the audit recommends against adding one, because apply order
is a global interleaved dependency order across sections and the split
applier constructors already express it. Loading the project directory
repeats at 31 call sites, and threading one load through planning and
applying was investigated and rejected. One apply runs in a single
transaction where the project applier creates projects that later appliers
resolve by name, so a directory loaded before the applier loop would miss
them and fresh installation promotions would fail. Planning could share one
load, but changing every planner and shared read helper for one small query
per section on a rare administrative path costs more than it saves. The
directory reads stay, with the reason recorded at `ReadDirectory`.

Destination dialect knowledge is derived three times. The connector profile
already declares operation dialects, yet protocols and connectors re-derive
the same family mapping in switches, and provider invocation computes it
twice on the request path. Unifying those derivations changes hot path
behavior wherever they disagree, so it stays follow-up work for its own
change. This pass took the provably safe instances. The Bedrock count token
format is now the named `wire.BedrockCount` constant everywhere it was
open-coded as a raw string, and `routing` now asks the connector whether it
envelops messages instead of re-listing hosting names. That second change
fixed a live bug. The re-listed bedrock name could never match a reviewed
profile, so an enveloped bedrock provider was wrongly offered for paid code
mode, and a regression test now pins the refusal. Whether
`BedrockCount.Operation` should report `token_count` the way its descriptor
already does is also follow-up work, since other accounting readers would
change with it.

Console repetition sits in component wiring, not in endpoint wrappers. The
typed client is already shared. ETag quoting was decided at about forty call
sites because the transport only quoted values that looked like UUIDs, so
correctness silently depended on every ETag being one. The transport now
quotes any unquoted value, which is the form the server requires, and the
shared header helpers express the mutation header pair in one place. The
editing module now folds the action runner and the mutation intent into one
mutation hook, adopted first by the forms that already combined both. A
query key factory was investigated and rejected. The thirty five key files
turn out to be short feature vocabularies with genuinely different shapes,
including nested roots composed from other features, so a generic factory
would fit a minority and obscure the rest. One audit claim here was checked
and rejected. Deleting a guardrail credential sends its id as the ETag, and
that is correct, because the server matches against the id for records that
have no separate ETag.

## Slice order

Work lands in the order above. Guardrails first so later moves cannot
regress the graph, then the composition root, the gateway, the package
folds and hub cuts, promotion, protocol data, the console, and the
architecture map last in the same changes that move code. Moves and logic
changes stay in separate commits, behavior is pinned by tests before code
moves, and no test budget changes to admit any of it.

## Outcome of the first pass

Measured the same way as the baseline, the first pass took top level
packages under `internal/` from 89 to 85, project import edges from 1142 to
1133 while adding the models package, and packages importing
`internal/access` from 66 to 60. `Run` in `cmd/olp/run.go` went from about
600 lines to 428, short of the 350 target set before the audit showed that
start ordering and shutdown are the composition root's own job and stay
there by design. The remaining named follow-up work is extracting the
pinned executor from the gateway kernel, unifying the triplicated
destination family derivation behind connector profile data, deciding the
`BedrockCount` operation string, splitting the access hub along its
principal and HTTP kit seam, and threading retry identity through the
guardrail policy and activation calls that still mint it inline.
