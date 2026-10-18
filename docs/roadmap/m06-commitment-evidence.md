# M6 commitment accounting evidence

Verification date: 2026-10-18. Base revision:
`ce190e7abda01e41f0a4abca29854f75b115b1fb`. This record describes the
commitment-completion working tree; it is separate from a release-image claim.

Implementation/test/contract file fingerprint (SHA-256 over sorted changed
paths, NUL, file contents, NUL; includes regenerated console contracts):
`4a228dca4fc8c37b98010e30941a4307c3a4f32e5a3bb284f8ac2daa74219f22`. Documentation is excluded from this fingerprint.

The implementation adds migration 0073, published effective/pending schedules,
period obligations independent of traffic, exact allocation with retained price
and ownership provenance, installation-scoped unused FOCUS rows, and nullable
manifest/API/console totals. Configuration promotion carries terms, while a
restored installation retains its original accounting history.

## Reproducible coverage

- [Lifecycle and migration tests](../../internal/database/commitment_migration_test.go):
  first activation without proration, hourly/daily/monthly periods, disablement,
  pending edit replacement, removal, cadence boundaries, concurrent bounded
  materialization, published-only backfill and exact fractional allocation.
- [Export accounting tests](../../internal/export/commitments_integration_test.go):
  empty, excluded-only and zero-weight periods; unpriced coverage; multiple
  consumers; exact period balances; ownership edits; retained allocation data;
  scoped exports; pinned FOCUS static rules and cross-row `D-082-C` assertions.
- [Delivery tests](../../internal/export/focus_integration_test.go):
  manifest-last delivery, failed object writes, retries with the same revision,
  late-fact restatements, dataset ledger totals and export-window boundaries.
- [Snapshot tests](../../internal/routing/commitment_test.go): boundary selection,
  immutable timeline ownership and signed commitment provenance.
  [Accounting pin regression](../../internal/gateway/commitment_accounting_test.go)
  keeps commitment, pricing and discount selection independent.
- [Public API checks](../../tests/integration/export_metering_test.go),
  [restore checks](../../tests/integration/recovery_test.go),
  [console checks](../../console/src/lib/features/observability/ObservabilityPanel.svelte.test.ts)
  and [browser/accessibility checks](../../console/tests/gateway/focus-datasets.spec.ts).

## Verification results

- Focused accounting and FOCUS tests: passed, including race detection.
- Database, export, usage and runtime integration packages: passed with race detection.
- Contract regeneration: passed against the proposed generated files using a
  temporary Git index; the working index was not changed.
- `make check`: passed, including formatting, vet, console type/lint/component checks, Go tests, benchmark-harness tests and script tests.
- Full `make integration`: not completed. Preliminary runs were interrupted;
  the final full run was cancelled at the user's request to avoid long tests.
  The new public-API, restore and browser checks are included as test source
  but have not been qualified through the full harness in this run.
- `BENCH_TIME=100ms make bench-gate`: stopped at the user's request after three
  completed sample pairs. There is no benchmark gate verdict. Scratch files
  used the workspace disk after `/tmp` reached its quota.

The implementation is complete. Extended integration and performance
qualification remain open; none of the interrupted runs is reported as passing.

## Remaining external qualification

A candidate release must rerun the implementation checks and the full-rate M1
performance gate on reference hardware, recording the image digest and benchmark
artifacts. Local merge-gate results do not establish release performance.

Live OpenAI and Anthropic invoice qualification requires scoped admin credentials,
at least one closed billing day, currency and covered-day reconciliation, and
pagination/restatement evidence. External sink qualification requires readback
and duplicate/retry verification for every claimed object transport, a CloudEvents
receiver and a Stripe test meter. No live credential or external delivery claim
is made by these fixtures.

AWS CUR, Google Cloud billing export and Azure Cost Management adapters remain
separate follow-up work. Unused cost is monetary allocation, not physical capacity
utilization; dynamic FOCUS rules outside the explicit reconciliation assertions
remain separately qualified.
