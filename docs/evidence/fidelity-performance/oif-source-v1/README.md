# OIF source migration measurements

These are the first OIF source-layer measurements against the unchanged
[v1 frozen budgets](../v1/replacement-budgets.json). Both captures use legacy
route contracts, the full 22-workload/concurrency/path inventory, the exact
frozen harness and runner, and clean committed runtime revisions. They do not
qualify later strict route, continuation or networking implementations.

| Capture | Runtime revision | Result |
| --- | --- | --- |
| [Initial](initial.json) | `42c4784e20ecac5bdbf94f36b4c2f473c9dbfdfe` | Numeric limits passed; native asset c1 had 15/18/20 samples against a minimum of 19. |
| [Optimized](optimized.json) | `291d4b23a3fa14cdf60f684f8c5a0e3f9a584ff1` | Gateway limits and sample floors passed; unchanged slow-relay c1 p99 gap exceeded its limit (3,943 µs versus 3,544 µs). |

The optimization reuses dialect validation only for the exact immutable source
and family already validated, and removes a redundant destination
serialize/parse cycle. Constructors, changed overlays and defaults remain
validated; the same provider-bound bytes, successful work, rejections and event
counts are checked by the independent benchmark fixture.

The [comparison results](comparisons.json) were produced by the frozen
`compareBudgets` function. Neither full comparison passes, so these artifacts
are retained evidence rather than a complete performance qualification. The
remaining relay-control timing failure must stay visible in the integrated
qualification work; it cannot be repaired by relaxing or replacing budgets.
Shared test services were provisioned but unused by these benchmark workloads;
background service activity was not isolated. Integrated G6 qualification in
[#218](https://github.com/tyk-swe/olp/issues/218) will investigate that condition
after service suites stop, retaining the original harness and budgets.

Reproduce either comparison with:

```sh
node scripts/fidelity-benchmark.mjs compare docs/evidence/fidelity-performance/oif-source-v1/initial.json docs/evidence/fidelity-performance/v1/replacement-budgets.json
node scripts/fidelity-benchmark.mjs compare docs/evidence/fidelity-performance/oif-source-v1/optimized.json docs/evidence/fidelity-performance/v1/replacement-budgets.json
```
