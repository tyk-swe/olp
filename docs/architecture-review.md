# Architecture repair review

Historical evidence for the runtime-ownership repair merged in October 2026,
against baseline `e5a87b859fe25aa7b3287111899a7f4074d1ada5`. This is not a new
validation report for later documentation edits. Go 1.27.2, Node 26.11.1 and
pnpm 11.24.0 pins were unchanged.

## Scope and outcomes

The review covered process/runtime, ordinary/retained/media/realtime/code
execution, budgets/ingestion/retention, management/promotion, console lifetimes,
OpenAPI consumers, plugins, deployment and release scripts. All 32 evidenced
findings were closed with focused controls and integrated qualification.

| Boundary | Repair |
| --- | --- |
| Startup/shutdown | One producer-first cleanup path and shared deadline; forced unblock/join uncertainty |
| Runtime | Constructor deep freeze, independent authority progress, coherent grant/release installation |
| Historical credentials | Remote resolution outside short transactions; revalidate revocation |
| Native/retained transport | Separate accepted work, validated completion, usage and client delivery; join multipart/duplex tasks |
| Planning | Bound heavyweight plans to four, deterministic eviction/rebuild |
| Retained identities | Exact selected target/slot/version; ambiguous historical backfills refuse |
| Accounting | Original financial events, pending subject visibility, per-attempt project ownership, late group identity |
| Provider promotion | Shared feature-owned credential/configuration transitions and collection ETags |
| Console | Caller-owned mutation IDs, captured payload/ETag, session/resource fences and one-use enrollment clearing |
| SDK/CLI/MCP | Generated scalar serializers/read-only metadata; no hidden mutation retry identity |

Current invariants and implementation owners are in the
[architecture map](architecture.md), [gateway](gateway.md) and
[operations runbook](operations.md). Detailed test behavior lives beside each owner.

## Recorded qualification

The frozen October 12 source contained 62 migrations. Its final normal run passed:

| Gate | Recorded result |
| --- | --- |
| `make check GO_TEST_ARGS='-race -count=1'` | 85 tested Go packages; 147 console files/1228 tests; 91 script tests |
| Contract reproducibility | Exactly four expected generated outputs, including ignored console artifacts |
| Complete `make integration` | 4620 seconds; all 719 compiled public names once in disjoint cohorts; CodeCLI, eight internal packages, trusted extension, official SDKs, ten client/framework suites and browser phases |
| Go integration totals | 841 top-level/1975 nested tests, no failures/skips/races/panics |
| Console qualification | 37 feature browser tests, six fresh hosted journeys and restored journey; restored history preserved and grew after inference |
| Release/Helm/version | Binary/static console, binary version 0.1.0, chart/schema/confinement/egress guards |
| Benchmark gate | 93 shared/26 added/zero removed names; six alternating 200 ms samples, unchanged 10% policy; one initial flag cleared ordinary confirmation |

Earlier failed fixture runs and partial downstream preflights were not counted as
whole-source qualification. Fixture fixes retained constructor immutability and
native terminal obligations; the final normal run repeated every required phase.

## Measured tradeoffs and limits

Local focused measurements reduced Gemini Live parsing from 127 to 36 allocations/
frame (~13.6 to 3.55 KiB) and strict 16-target retained memory from ~16.9 to
1.14 MiB for a 1 MiB prompt. A measured malformed-authentication correctness cost
was +6.77% (69.69 to 74.41 ns, no allocations), below the unchanged gate threshold.

Benchmark datasets affected by unrelated CPU overlap were discarded; accepted
paired measurements retained normal sample order, thresholds and confirmation.
These are local measurements, not full-rate production throughput or live-provider
qualification. Closed findings do not prove absence of design debt, external
exactly-once effects or production IdP/provider conformance.
