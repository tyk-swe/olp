# M4 evidence: distributed limits, pricing, and recovery

Implemented and qualified on Linux amd64 on 2026-09-14. The Go build now
enforces shared key, connection, and credential-slot limits and accrued-cost
budgets across replicas, prices every attempt exactly, persists durable request
and attempt facts through a recoverable Valkey stream, reconciles spend back
into the shared limiter, and serves usage, history, pricing, completeness, and
recovery reporting to the console. Operations are documented in the
[Go gateway guide](../../go-gateway.md); the budget contract and its documented
limits remain [spend-budget initialization and recovery](../../spend-budget-recovery.md)
and the [production contracts](../../production-guarantees.md).

All shared state is namespaced `olp:go:v1:<installation>:` by
[the installation namespace](../../../internal/database/migrate.go), and the
durable schema starts with
[`0005_accounting.sql`](../../../internal/database/migrations/0005_accounting.sql).
[`0006_receipt_identity.sql`](../../../internal/database/migrations/0006_receipt_identity.sql)
adds independent event and request identity constraints for concurrent replay
and for receipts whose raw facts have already been retained as aggregates.

## M4-01

[The limiter](../../../internal/limits/limits.go) reserves and settles every
shared dimension through the retained Lua scripts in
[`internal/limits/scripts/`](../../../internal/limits/scripts/): `reserve_limits`
charges the request, token, and concurrency windows atomically on server time,
`refund_limits` returns a reservation that dispatched nothing, `reconcile_limits`
adjusts a token reservation to the usage the upstream actually reported, and
`release_concurrency` returns the lease. Replies are parsed strictly by
[the result decoder](../../../internal/limits/results.go); a malformed or
unexpected reply is an error, never an allow. Keys carry the cluster hash tag of
their lookup so one key's dimensions stay on one slot, and API keys, provider
connections (`pc_<uuid>`), and credential slots (`ps_<uuid>`) share one key
format and one script.

[Gateway admission](../../../internal/gateway/limits.go) reserves the API key
budgets once the request is authenticated, parsed, and routed, with the route's
overall deadline as the lease TTL, and reserves the provider connection quota
and then the credential slot quota, with leases lasting through the remaining
overall deadline rather than only the first-byte timeout, before each attempt leaves
[the executor](../../../internal/gateway/executor.go). A slot rejection refunds
the connection lease it already took, is recorded as a rate-limit attempt
failure with its Retry-After, and failover continues to the next target. A
target whose configured quota cannot be consulted is skipped rather than used
unmetered; when no attempt is admitted the request ends
`503 distributed_limits_unavailable`. Settlement drops cancellation with
`context.WithoutCancel`, so a client that hangs up still releases its
concurrency. Connection quotas reach the gateway through the compiled snapshot
([`internal/runtime/snapshot.go`](../../../internal/runtime/snapshot.go),
[tests](../../../internal/runtime/snapshot_test.go)), which drops quota objects
that bound nothing.

[Unit tests](../../../internal/limits/limits_test.go) cover the reply-parsing
matrix, hash tags and namespace validation, request validation, and cooldown
scopes; [admission tests](../../../internal/gateway/limits_test.go) cover the
token estimate, the rejection-to-HTTP mapping, and the no-limiter behaviour.
[Valkey integration tests](../../../tests/integration/limits_test.go) port the
retained scenarios: idempotent refunds that cannot change a successor window,
one server clock for every caller, a single minute rollover, positive retry
hints at the minute end, rejections that consume nothing, token reconciliation
that matches actual usage and ignores a new window, concurrency expiry,
unlimited dimensions creating no state, malformed rate state failing closed
before mutation, exactness at the Lua-safe maximum, 32 goroutines across two
limiters enforcing one atomic limit, cooldowns keeping the longest expiry, and
slots sharing quota identity across gateways.
[Gateway integration tests](../../../tests/integration/gateway_limits_test.go)
drive the same enforcement through real HTTP traffic, including provider quota
exhaustion and a request that reached no provider being refunded in full.

## M4-02

[Pricing](../../../internal/usage/pricing.go) restores the currency singleton,
numbered revisions under a transaction advisory lock, per-scope price entries
with token, cached-input, and media unit rates, and the validation rules of the
reference: unique dimensions, one currency per installation, bounded decimal
sizes, and vendor-to-kind agreement checked against the provider catalogue.
[Price selection](../../../internal/usage/pricing_select.go) reproduces scope
precedence — a provider override, then a vendor match, then effective date, then
revision — reads the vendor from the provider revision configuration with the
default kind mapping, and honours a pinned revision only when the attempt
carries one. Money is decimal end to end: PostgreSQL `numeric`, canonical
decimal strings in Go and on the wire, and string comparison in Lua. No
float64 touches money or token totals.

An attempt with no usage, or with a component the selected revision does not
price, stays explicitly unpriced rather than being closed with a fabricated
value. [Unit tests](../../../internal/usage/pricing_test.go) cover validation,
normalization, independent scopes, mixed currencies and oversized decimals, and
decimal exactness. [Integration fixtures](../../../tests/integration/accounting_test.go)
price fractional rates, cached-input discounts, media units, a missing output
rate that yields an unpriced fact, and cached input reported without its total,
and [`TestPricingRevisionsOverHTTP`](../../../tests/integration/reports_test.go)
exercises revision creation and listing over the management API.

## M4-03

[The accounting sink](../../../internal/gateway/accounting.go) maps the M3
terminal envelope to a content-free usage event and drops requests no API key
owns. [Envelope evidence](../../../internal/gateway/events.go) now carries first
byte timing, the request mode, the runtime generation, the error class, and the
per-attempt usage state — observed, complete, and billing-uncertain — decided by
the retained rules rather than inferred later.
[The event contract](../../../internal/usage/event.go) pins the wire field names
and validates the usage state machine before anything is queued.

[Persistence](../../../internal/usage/persistence.go) admits each delivery by a
receipt keyed on the event and request identity and the SHA-256 of the original
payload, then writes the request, its ordered attempts, the usage anchors, the
priced facts, the cost delta against `api_key_cost_windows`, and the
completeness markers in one transaction, and marks the receipt persisted. A
duplicate delivery, or a crash after commit and before acknowledgement, is
reported as a duplicate and writes nothing twice. Failed and retried attempts
keep their own identities and their own pricing provenance.
[Unit tests](../../../internal/gateway/accounting_test.go) and
[envelope evidence tests](../../../internal/gateway/evidence_test.go) cover the
mapping and the uncertainty rules;
[integration tests](../../../tests/integration/accounting_test.go) cover replay
safety, the per-attempt identities, and the replay-window rejection, and
[`TestM4CrashBeforeAcknowledgementReplaysAsDuplicate`](../../../tests/integration/m4_recovery_test.go)
covers the crash between commit and acknowledgement.

## M4-04

[The emitter](../../../internal/usage/emitter.go) is a bounded handoff that
never blocks a request and counts what it could not accept;
[the writer](../../../internal/usage/writer.go) drains it into the installation
stream with bounded retries and counts the remainder as abandoned at shutdown.
[The consumer](../../../internal/usage/consumer.go) creates the group, replays
its own pending entries before reclaiming another consumer's idle deliveries
through [`claim_request_metadata.lua`](../../../internal/usage/scripts/claim_request_metadata.lua),
acknowledges and deletes in one step through
[`ack_delete.lua`](../../../internal/usage/scripts/ack_delete.lua) only after the
event is durable, and checkpoints its own health.
[The protocol decoder](../../../internal/usage/protocol.go) bounds what it
trusts from `XREADGROUP` and `XAUTOCLAIM` in both protocol shapes.

An unsupported wire version stays pending and is never acknowledged or deleted;
malformed payloads, invalid events, and deliveries whose payload is gone are
recorded once as explicit gaps and then drained.
[Epochs and loss](../../../internal/usage/epochs.go) checkpoint each gateway
process against what it actually delivered, turn buffer loss into gap rows,
mark a superseded epoch unclean, and confirm stale epochs in two passes;
[consumer health](../../../internal/usage/health.go) publishes pending and lag
counts for the completeness surface. The stream name and the
`olp:persistence` group are derived from the installation namespace in
[`usage.go`](../../../internal/usage/usage.go), so two installations sharing one
Valkey cannot read or acknowledge one another's events
([`TestM4SharedValkeyIsolatesInstallations`](../../../tests/integration/m4_isolation_test.go)).
[Consumer integration tests](../../../tests/integration/consumer_test.go) cover
distribution across workers, reclaiming a dead owner, unusable deliveries
recorded as gaps, unsupported versions left pending, a committed event replayed
as a duplicate, and a group rebuilt after its stream was lost;
[epoch tests](../../../tests/integration/epochs_test.go) cover exact buffer-loss
accounting, idempotent graceful close, superseded epochs, once-only gap
reporting, and consumer health samples.

## M4-05

[Budgets](../../../internal/limits/budgets.go) compute the daily and monthly UTC
windows in Go, carry cumulative PostgreSQL totals as `CostSnapshot` values, and
apply them through `reconcile_cost.lua`; `reserve_cost.lua` charges a request
against the current window only. Admission never initializes unknown spend:
a missing hash, a malformed hash, or a snapshot for another window is
`ErrUninitializedCost` or `ErrMalformedState`, which
[the gateway](../../../internal/gateway/limits.go) answers with
`503 distributed_limits_unavailable`. A known exhausted window answers
`429 budget_exhausted` with the message that states the documented limitation:
unpriced attempts accrue nothing. A key with any cost budget always fails
closed, whatever the installation's `limits.valkey_unavailable` setting says
([`internal/limits/policy.go`](../../../internal/limits/policy.go)).

The console reads accrued spend, window ends, unpriced attempt counts, and
whether enforcement is active from the key surface, which composes
`limits.BudgetSQL` into [`internal/access/keys.go`](../../../internal/access/keys.go).
That expression truncates `now()` to the day and the month after converting it
to UTC, in the timezone-free domain, and converts every boundary back with
`AT TIME ZONE 'UTC'`, so the windows it reports are the windows enforcement
charges against whatever `TimeZone` the reading connection carries.
The pool additionally pins every session's `TimeZone` to UTC
([`database.Configuration`](../../../internal/database/database.go)), so the
calendar-day intervals in the replay-horizon and retention cutoffs are exact
24-hour days on every server rather than a function of its default zone.
[`TestBudgetWindowsEndOnFixedUTCBoundaries`](../../../internal/limits/limits_test.go)
covers the window arithmetic including the 2028 leap day;
[`TestLimitsCostBudgetsFailClosedUntilReconciled`,
`TestLimitsMonthlyBudgetExhaustionRejectsWithItsOwnWindow`, and
`TestLimitsBudgetWindowsIgnoreTheSessionTimeZone`](../../../tests/integration/limits_test.go)
cover initialization, exhaustion, malformed-state repair that does not lower the
other window, stale or future snapshots that cannot initialize a window, and
reported windows that do not move under a non-UTC session timezone.
[`TestGatewayEnforcesKeyBudgets`](../../../tests/integration/gateway_limits_test.go)
and [the key budget contract](../../../tests/integration/keys_budget_test.go)
cover the served responses and the reported values, with and without
enforcement.

## M4-06

[Cost reconciliation](../../../internal/limits/reconciliation.go) rebuilds the
current daily and monthly totals from durable facts and applies them
monotonically: the script initializes only a matching current window and never
lowers a valid counter. Leadership is one hijacked PostgreSQL connection holding
a session advisory lock, acquired lazily and kept between minute ticks; a
follower reports a skipped pass rather than repeating the scan, and any error,
cancellation, or pass deadline drops and closes that connection instead of
returning a locked session to the pool.
[The worker plane](../../../internal/process/workers.go) checkpoints each pass
into the shared worker health table through
[`usage.CheckpointTask`](../../../internal/usage/usage.go), translating the
limiter's outcome vocabulary rather than aliasing it.

[`TestLimitsDurableSpendReconcilesIntoValkey` and
`TestLimitsCostReconciliationLeadershipIsExclusive`](../../../tests/integration/limits_test.go)
cover reconstruction, monotonicity, and exclusive leadership;
[`TestM4CostReconciliationSurvivesLeaderLoss`, `TestM4BudgetsRecoverFromLostSpendState`,
and `TestM4ReconciliationLeadershipIsScopedToTheInstallation`](../../../tests/integration/m4_recovery_test.go)
cover a leader that dies mid-pass, budgets recovering from lost Valkey state,
and leadership that does not cross installations.

## M4-07

[History](../../../internal/usage/history.go) serves filtered request listings
with opaque cursors ([`cursor.go`](../../../internal/usage/cursor.go)) and
request detail with ordered attempts and their pricing provenance, so a retry
remains a distinct attempt. [Reports](../../../internal/usage/reports.go) build
summaries, breakdowns, series, and completeness from live facts unioned with
retained hourly buckets, mark a partial boundary bucket as approximate and say
how much it excluded, fall back to the installation pricing currency, and fold
in gap evidence and [consumer status](../../../internal/usage/status.go).
[The HTTP surface](../../../internal/usage/http.go) registers the usage,
request, pricing, and gateway-epoch routes on the management mux and is
registered only in management modes.

[Retention](../../../internal/usage/retention.go) runs on its own advisory lock
and hijacked connection, rolls facts additively into hourly buckets before
purging them so current spend reconstruction stays exact, purges requests in
bounded `SKIP LOCKED` batches, and expires receipts, audit rows, gap rows,
epochs, sessions, invitations, replays, and OIDC flows from the stored
`retention.*` settings. [Unit tests](../../../internal/usage/reports_test.go)
cover hour rounding, count scope, range validation, and the union of live and
retained rows; [integration tests](../../../tests/integration/reports_test.go)
cover the seeded aggregates, count scope, excluded partial buckets, grouping,
the HTTP contracts, request paging and detail, pricing revisions, gateway
epochs, the rollup-and-purge pass, and the configured retention window.

## M4-08

The console reads the enforced branches of the same contracts.
[Budget presentation](../../../console/src/lib/features/access/api-keys/budgetPresentation.ts)
decides the loading, unknown, exhausted, and within-budget states by exact
decimal comparison, with [unit tests](../../../console/src/lib/features/access/api-keys/budgetPresentation.test.ts);
[the key inventory](../../../console/src/lib/features/access/api-keys/ApiKeyInventory.svelte)
and [policy form](../../../console/src/lib/features/access/api-keys/ApiKeyPolicyForm.svelte)
show accrued spend against the limit, the window end, and unpriced attempts.
[The usage page](../../../console/src/lib/features/usage/UsagePage.svelte) keeps
loading, unavailable, exhausted, and incomplete visually distinct,
[the overview](../../../console/src/lib/features/overview/Overview.svelte) and
[settings](../../../console/src/lib/features/settings/SettingsPage.svelte) carry
the retention and `limits.valkey_unavailable` branches and the pricing revision
form, and [the credential pool](../../../console/src/lib/features/providers/ProviderCredentialPool.svelte)
renders connection and slot usage and cooldowns, which the Go
[providers surface](../../../internal/providers/server.go) now fills from the
limiter ([quota tests](../../../internal/providers/quotas_test.go)); an
unreadable counter leaves the field null instead of failing the request.

[The accounting journey](../../../console/tests/gateway/accounting.spec.ts)
drives a browser user from an empty installation through provider onboarding,
route publication, and a budgeted key, runs priced and unpriced gateway traffic
through the mock upstream, creates a pricing revision, and then reads back the
request explorer, the attempt charge status, usage totals and completeness, and
the key's accrued spend and window end. The
[gateway](../../../console/tests/gateway/openai-route.spec.ts) and
[access](../../../console/tests/access/control.spec.ts) journeys were updated
for the capability flags, which they read from a binary run in `all` mode with
shared state configured, so the console takes its enforced branches there.
`limits_enforced` and `retention_enforced` both report the composition an
installation was given rather than a live worker replica, which a control
process cannot observe: [`internal/access/http.go`](../../../internal/access/http.go)
documents exactly that, [`internal/process/run.go`](../../../internal/process/run.go)
derives both from the configured shared state, and
[`TestCapabilitiesReportConfiguredEnforcement`](../../../tests/integration/gateway_test.go)
covers both answers.

## M4-09

[The process harness](../../../internal/testutil/process.go) starts real
binaries against real services.
[`TestM4ReplicasShareOneAdmissionDecision` and `TestM4AccountingIsDurableAcrossReplicas`](../../../tests/integration/m4_process_test.go)
run two gateway processes against one installation and prove that they share one
admission decision and that accounting from both replicas lands exactly once.
[`TestM4SharedValkeyIsolatesInstallations`](../../../tests/integration/m4_isolation_test.go)
runs two installations against one Valkey and proves neither consumes,
acknowledges, nor reconciles the other's state.
[The recovery suite](../../../tests/integration/m4_recovery_test.go) covers a
crash before acknowledgement, pending deliveries reclaimed after a worker is
lost, leader death, budgets recovering from lost spend state, and gateway epochs
recorded and resolved for a replica that never came back.
[Process tests](../../../tests/integration/process_test.go) keep the mode
matrix, private probes, and shutdown ordering honest.

## Exit scenarios

- **Shared key/connection/slot limits across replicas:**
  `TestLimitsConcurrentReplicasEnforceOneAtomicLimit`,
  `TestLimitsProviderSlotsShareQuotaAndCooldownsAcrossGateways`,
  `TestM4ReplicasShareOneAdmissionDecision`,
  `TestGatewayEnforcesProviderQuotas`.
- **Duplicate, cancelled, retried, partial, and unpriced attempts:**
  `TestAccountingPersistsPricedAttemptsAndIsReplaySafe`,
  `TestAccountingPricingProvenance`,
  `TestAccountingRefusesToPriceCachedTokensWithoutTheirTotal`,
  `TestRequestMetadataConsumerReplaysACommittedEventAsADuplicate`,
  `TestGatewayRefundsRequestsThatReachedNoProvider`.
- **New/expired UTC spend windows and missing or corrupt Valkey state:**
  `TestBudgetWindowsEndOnFixedUTCBoundaries`,
  `TestLimitsCostBudgetsFailClosedUntilReconciled`,
  `TestLimitsMonthlyBudgetExhaustionRejectsWithItsOwnWindow`,
  `TestLimitsMalformedRateStateFailsClosedBeforeMutation`,
  `TestM4BudgetsRecoverFromLostSpendState`.
- **Restart or isolate workers, PostgreSQL, and Valkey around persistence and
  acknowledgement:** `TestM4CrashBeforeAcknowledgementReplaysAsDuplicate`,
  `TestM4PendingDeliveriesAreReclaimedAfterAWorkerIsLost`,
  `TestM4CostReconciliationSurvivesLeaderLoss`,
  `TestRequestMetadataConsumerRebuildsAGroupLostWithItsStream`,
  `TestM4SharedValkeyIsolatesInstallations`.
- **Visible delivery gaps, retained history, exact pricing, and budget UI
  states:** `TestRequestMetadataGapsAreReportedOnlyOnce`,
  `TestM4GatewayEpochsRecordAndResolveLostReplicas`,
  `TestReportsExcludePartialBucketsAndSaySo`,
  `TestUsageMaintenanceRollsUpAndPurges`,
  `TestUsageRetentionHonoursTheConfiguredWindow`, and the
  [accounting journey](../../../console/tests/gateway/accounting.spec.ts).
- **Dependency inventory and capability map:** no new module is required —
  `go.mod` and `go.sum` are unchanged by this milestone — and the
  [capability map](../README.md#capability-ownership) now records
  [src/limits](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/limits), [src/usage](https://github.com/tyk-swe/olp/blob/6c21dfb917c9019161348ea24b532a77b6612e6e/src/usage), and `tests/ha` against their Go packages.

## Validation

Environment: Linux amd64, local PostgreSQL 18.6 and Valkey 9.0.4 reached over
loopback, no container runtime. Console commands need Node 26
(`source ~/.nvm/nvm.sh && nvm use 26`; `console/package.json` declares
`engines.node >= 26`, and an older Node fails `make go-api` before it does any
work). Every command below exited 0 on 2026-09-14.

- `make go-api` and `scripts/check-go-contracts.sh`: the generated management
  types and the TypeScript contract are unchanged by this milestone, so the
  M4 surfaces already existed in `openapi/management.json`.
- `gofmt -l cmd internal openapi/*.go tests/fixtures/*.go tests/integration tests/sdkfixture`:
  no output. `go vet ./...` and
  `go vet -tags=integration,oidctest ./tests/integration`: clean.
- `go test -race -count=1 ./...`: 15 packages pass, including the new
  `internal/limits` and `internal/usage` suites.
- `pnpm --dir console verify`: prettier and eslint clean, svelte-check reports
  662 files with 0 errors and 0 warnings, vitest runs 476 tests in 52 files.
- `CGO_ENABLED=1 go build -trimpath -o .local/bin/olp ./cmd/olp` and
  `go build -tags=oidctest -o .local/bin/olp-identity-test ./cmd/olp`.
- `OLP_TEST_BINARY=$PWD/.local/bin/olp go test -race -tags=integration,oidctest -count=1 -timeout=30m -skip 'TLS' ./tests/integration`:
  passes in 373 s against PostgreSQL 18.6 and Valkey 9.0.4. The run logs contain
  no `WARN` or `ERROR` records, so no limiter outage, stream write failure, or
  unknown execution outcome occurred while the suites ran.
- `pnpm --dir console build`: verified console manifest, 413 assets.
- `pnpm --dir console exec playwright test --config playwright.go.config.ts`:
  8 journeys pass in both the packaged and Vite projects (access, foundation,
  gateway route, and accounting) against freshly created databases.
- `OLP_SDK_SMOKE_BACKEND=go OLP_SDK_SMOKE_SURFACES=openai tests/sdk-smoke/run.sh`:
  official OpenAI SDK success and error contracts still pass.

The two TLS integration tests are skipped here: they need
`OLP_TEST_DATABASE_TLS_URL` and `OLP_TEST_VALKEY_TLS_URL`, which
`scripts/go-integration.sh` only produces from the container stack. They remain
qualified by that path, unchanged from M3.

## Open items

Recorded as found, not as accepted behaviour.

- `olp_limits_fail_open_total` is counted in process
  ([`Admission.FailOpenTotal`](../../../internal/gateway/limits.go)) but no
  Prometheus surface exports it yet; the metrics endpoint lands with
  [M6](../06-media-and-console-parity.md). Until then a fail-open admission is
  visible only as a warning log, and
  [configuration](../../configuration.md#runtime-variables) describes the
  counter, not an exported series.
- [`capabilities`](../../../internal/access/identity.go) reports
  `limits_enforced` and `retention_enforced` from the shared state this
  installation is configured with, which is the honest answer a control process
  can give, but it is configuration and not liveness: an installation that sets
  `OLP_VALKEY_URL` and then runs no `worker` or `all` replica reports
  `retention_enforced` as true while nothing rolls up or expires usage. A
  console claim that follows a live worker would have to read the per-task
  checkpoints in `olp_go.worker_task_health`
  ([`usage.CheckpointTask`](../../../internal/usage/usage.go)) instead of one
  composition flag.
- [`internal/process/workers.go`](../../../internal/process/workers.go) starts
  the request-metadata consumer once with no restart supervisor. Its only
  non-nil error path is startup misconfiguration, so nothing is lost today, but
  a future error path would retire the worker plane for the life of the process.
