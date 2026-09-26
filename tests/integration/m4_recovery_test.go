//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// m4LeaderSQL finds the backend holding the cost reconciliation advisory lock.
// The lock identifier is one 64-bit key, which PostgreSQL reports split across
// the class and object columns, so it is reassembled here rather than written
// out as two magic numbers. Advisory locks are scoped to a database and every
// test here runs in its own, so the probe is scoped the same way: without the
// database predicate it would count the leaders of every other installation on
// the cluster and report several holders of a lock only one session can hold.
const m4LeaderSQL = `SELECT pid FROM pg_locks
    WHERE locktype = 'advisory' AND objsubid = 1 AND granted
      AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
      AND ((classid::bigint << 32) | objid::bigint) = x'4f4c505f4352'::bigint`

// m4Leader is the backend that currently holds cost reconciliation leadership,
// and whether anybody holds it at all.
func m4Leader(t *testing.T, pool *pgxpool.Pool) (int32, bool) {
	t.Helper()
	rows, err := pool.Query(t.Context(), m4LeaderSQL)
	if err != nil {
		t.Fatalf("read reconciliation leadership: %v", err)
	}
	defer rows.Close()
	var holders []int32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("read reconciliation leadership: %v", err)
		}
		holders = append(holders, pid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read reconciliation leadership: %v", err)
	}
	if len(holders) > 1 {
		t.Fatalf("reconciliation leadership is held by %d backends at once: %v", len(holders), holders)
	}
	if len(holders) == 0 {
		return 0, false
	}
	return holders[0], true
}

// m4Reconcile runs one cost reconciliation pass from the test itself, which is
// what a worker's leader does on its own schedule. Leadership is released again
// so a worker process in the same scenario can take it.
func m4Reconcile(t *testing.T, pool *pgxpool.Pool, limiter *limits.Limiter) limits.Report {
	t.Helper()
	leader, err := limits.TryAcquireLeader(t.Context(), pool)
	if err != nil {
		t.Fatalf("acquire reconciliation leadership: %v", err)
	}
	if leader == nil {
		t.Fatal("reconciliation leadership was held by another replica")
	}
	defer leader.Close(context.WithoutCancel(t.Context()))
	report, err := leader.Reconcile(t.Context(), limiter, time.Now())
	if err != nil {
		t.Fatalf("reconcile cost budgets: %v", err)
	}
	return report
}

// costKeys are the day and month hashes one API key's spend is admitted
// against, named exactly as the admission scripts name them.
func (in *m4Install) costKeys(apiKeyID string) (string, string) {
	return limCostKeys(in.namespace, apiKeyID)
}

// m4Exists reports whether one shared Valkey key is present.
func m4Exists(t *testing.T, c *coordination.Client, key string) bool {
	t.Helper()
	return do(t, c, "EXISTS", key) == int64(1)
}

// m4ConsumerPending is how many deliveries one consumer still owes an
// acknowledgement for.
func m4ConsumerPending(t *testing.T, c *coordination.Client, stream, consumer string) int64 {
	t.Helper()
	for _, row := range m4List(t, do(t, c, "XINFO", "CONSUMERS", stream, usage.Group), "XINFO CONSUMERS") {
		fields := m4Fields(t, row)
		if fields["name"] != consumer {
			continue
		}
		pending, err := strconv.ParseInt(fields["pending"], 10, 64)
		if err != nil {
			t.Fatalf("consumer %s pending %q: %v", consumer, fields["pending"], err)
		}
		return pending
	}
	return 0
}

// TestM4CrashBeforeAcknowledgementReplaysAsDuplicate proves that a delivery a
// worker persisted but was killed before acknowledging costs nothing when it
// comes back: the replacement worker recognises it, the ledger does not move,
// and the receipt that recorded it the first time is the receipt that stays.
func TestM4CrashBeforeAcknowledgementReplaysAsDuplicate(t *testing.T) {
	in := m4Provisioned(t)
	_, secret := in.key("replayed", nil)
	replica := in.replica("m4-replay-gateway")

	const requests = 3
	for range requests {
		if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference: %d %s", status, code)
		}
	}
	// Nothing consumes yet, so the events are still on the stream and one of
	// them can be kept as the copy a crash would redeliver.
	var payloads []string
	m4Eventually(t, "every event to reach the stream", 30*time.Second, func() bool {
		payloads = m4Payloads(t, in.valkey, in.stream)
		return len(payloads) == requests
	})
	replayed := payloads[0]

	worker := in.worker("m4-replay-worker")
	m4Eventually(t, "the worker to account for every request", 60*time.Second, func() bool {
		return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == requests
	})
	receipts := in.count("SELECT count(*) FROM olp.request_metadata_event_receipts")
	if receipts != requests {
		t.Fatalf("receipts = %d, want %d", receipts, requests)
	}
	_, duplicates, _ := m4Counters(t, in.h.Pool)

	// The worker dies the way a lost machine does, before it could acknowledge
	// anything more, and the delivery it already persisted arrives again.
	if err := worker.Kill(); err != nil {
		t.Fatalf("kill the worker: %v", err)
	}
	do(t, in.valkey, "XADD", in.stream, "*", "event", replayed)

	replacement := in.worker("m4-replay-replacement")
	m4Eventually(t, "the replacement worker to recognise the replay", 60*time.Second, func() bool {
		_, seen, _ := m4Counters(t, in.h.Pool)
		return seen > duplicates
	})
	m4Eventually(t, "the replayed delivery to be acknowledged and removed", 30*time.Second,
		func() bool { return len(m4Payloads(t, in.valkey, in.stream)) == 0 })

	if total := in.count("SELECT count(*) FROM olp.attempt_usage_facts"); total != requests {
		t.Fatalf("usage facts = %d after a replay, want %d", total, requests)
	}
	if total := in.count("SELECT count(*) FROM olp.requests"); total != requests {
		t.Fatalf("requests = %d after a replay, want %d", total, requests)
	}
	if total := in.count(
		"SELECT count(*) FROM olp.request_metadata_event_receipts"); total != receipts {
		t.Fatalf("receipts = %d after a replay, want the original %d", total, receipts)
	}
	if total := in.count(
		`SELECT count(*) FROM olp.request_metadata_ingestion_gaps
            WHERE reason = 'invalid_request_metadata_event'`); total != 0 {
		t.Fatalf("a replayed delivery was recorded as %d metadata gaps", total)
	}
	if err := replacement.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the replacement worker: %v", err)
	}
}

// TestM4PendingDeliveriesAreReclaimedAfterAWorkerIsLost proves that deliveries
// a worker took and never acknowledged are not lost with it: once they have been
// idle long enough, another worker claims them and accounts for each exactly
// once.
func TestM4PendingDeliveriesAreReclaimedAfterAWorkerIsLost(t *testing.T) {
	in := m4Provisioned(t)
	_, secret := in.key("reclaimed", nil)
	replica := in.replica("m4-reclaim-gateway")

	const served = 2
	for range served {
		if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference: %d %s", status, code)
		}
	}
	lost := in.worker("m4-reclaim-lost")
	m4Eventually(t, "the first worker to account for the first requests", 60*time.Second,
		func() bool {
			return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == served
		})
	consumers := m4Consumers(t, in.valkey, in.stream)
	if len(consumers) != 1 {
		t.Fatalf("consumers in the group = %v, want exactly the first worker", consumers)
	}
	owner := consumers[0]

	// The worker is lost with deliveries it had taken and not acknowledged:
	// the entries below are moved into exactly that consumer's pending list,
	// which is the state its termination leaves behind.
	if err := lost.Kill(); err != nil {
		t.Fatalf("kill the first worker: %v", err)
	}
	const stranded = 2
	for range stranded {
		if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference after the worker was lost: %d %s", status, code)
		}
	}
	m4Eventually(t, "the stranded events to reach the stream", 30*time.Second, func() bool {
		return len(m4Payloads(t, in.valkey, in.stream)) == stranded
	})
	do(t, in.valkey, "XREADGROUP", "GROUP", usage.Group, owner, "COUNT", "100",
		"STREAMS", in.stream, ">")
	if pending := m4ConsumerPending(t, in.valkey, in.stream, owner); pending != stranded {
		t.Fatalf("pending deliveries owned by the lost worker = %d, want %d", pending, stranded)
	}

	// A survivor may not simply steal a delivery that might still be in
	// progress; it claims it only once it has been idle past the reclaim window,
	// which is why this takes longer than a restart otherwise would.
	survivor := in.worker("m4-reclaim-survivor")
	m4Eventually(t, "the survivor to reclaim and account for the stranded deliveries",
		usage.ReclaimIdle+90*time.Second, func() bool {
			return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == served+stranded
		})
	if pending := m4ConsumerPending(t, in.valkey, in.stream, owner); pending != 0 {
		t.Fatalf("the lost worker still owns %d deliveries after they were reclaimed", pending)
	}
	if _, _, reclaimed := m4Counters(t, in.h.Pool); reclaimed < stranded {
		t.Fatalf("reclaimed deliveries = %d, want at least %d", reclaimed, stranded)
	}
	// Reclaiming must account for each delivery once, not once per claim.
	if total := in.count("SELECT count(*) FROM olp.requests"); total != served+stranded {
		t.Fatalf("requests = %d, want %d", total, served+stranded)
	}
	if total := in.count(
		"SELECT count(*) FROM olp.request_metadata_event_receipts"); total != served+stranded {
		t.Fatalf("receipts = %d, want %d", total, served+stranded)
	}
	if err := survivor.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the surviving worker: %v", err)
	}
}

// TestM4ReconciliationLeadershipIsScopedToTheInstallation proves that the
// leadership probe answers for one installation and not for the cluster.
// Reconciliation leadership is a PostgreSQL advisory lock, which is scoped to a
// database, so two installations sharing a cluster each elect their own leader
// under the same lock identifier. A probe that could not tell them apart would
// report several holders of a lock only one session can hold, and would also
// let a neighbour's leader stand in for a takeover this installation never
// performed, so every leadership assertion in this file rests on this.
func TestM4ReconciliationLeadershipIsScopedToTheInstallation(t *testing.T) {
	pool, _ := accessDatabase(t)
	neighbour, _ := accessDatabase(t)

	if pid, held := m4Leader(t, pool); held {
		t.Fatalf("backend %d holds leadership in an installation that elected nobody", pid)
	}
	// The neighbouring installation elects a leader under the same identifier.
	// It is another installation's leader, so this one still has none.
	theirs, err := limits.TryAcquireLeader(t.Context(), neighbour)
	if err != nil {
		t.Fatalf("elect a leader in the neighbouring installation: %v", err)
	}
	if theirs == nil {
		t.Fatal("leadership in a fresh neighbouring installation was already held")
	}
	defer theirs.Close(context.WithoutCancel(t.Context()))
	theirPID, held := m4Leader(t, neighbour)
	if !held {
		t.Fatal("the neighbouring installation reports no leader after electing one")
	}
	if pid, held := m4Leader(t, pool); held {
		t.Fatalf("backend %d leads the neighbouring installation and was reported as this one's leader", pid)
	}

	// Both installations hold the identifier at once, which is exactly the
	// state an unscoped probe reports as two leaders of one installation.
	mine, err := limits.TryAcquireLeader(t.Context(), pool)
	if err != nil {
		t.Fatalf("elect a leader in this installation: %v", err)
	}
	if mine == nil {
		t.Fatal("a neighbouring installation's leader blocked this installation's election")
	}
	defer mine.Close(context.WithoutCancel(t.Context()))
	myPID, held := m4Leader(t, pool)
	if !held {
		t.Fatal("this installation reports no leader after electing one")
	}
	if myPID == theirPID {
		t.Fatalf("both installations report backend %d as their leader", myPID)
	}
	if pid, _ := m4Leader(t, neighbour); pid != theirPID {
		t.Fatalf("the neighbouring installation's leader moved to %d, want %d", pid, theirPID)
	}
}

// TestM4CostReconciliationSurvivesLeaderLoss proves that reconciliation is a
// responsibility and not a process: exactly one worker holds it, and when that
// worker is lost another takes it over and resumes publishing spend windows.
func TestM4CostReconciliationSurvivesLeaderLoss(t *testing.T) {
	in := m4Installation(t)
	leaderWorker := in.worker("m4-leader-first")
	var elected int32
	m4Eventually(t, "the first worker to take reconciliation leadership", 40*time.Second,
		func() bool {
			pid, held := m4Leader(t, in.h.Pool)
			elected = pid
			return held
		})
	// The second worker finds leadership taken and waits, which is the state a
	// standby is in when the leader is lost.
	standby := in.worker("m4-leader-standby")
	m4Eventually(t, "the standby to record a pass it skipped", 40*time.Second, func() bool {
		return in.count(`SELECT coalesce(max(skipped_total), 0) FROM olp.worker_task_health
            WHERE task = $1`, string(usage.TaskCostReconciliation)) > 0
	})
	if pid, held := m4Leader(t, in.h.Pool); !held || pid != elected {
		t.Fatalf("leadership moved to %d (held %v) while the leader was alive, want %d",
			pid, held, elected)
	}

	lostAt := time.Now().UTC()
	if err := leaderWorker.Kill(); err != nil {
		t.Fatalf("kill the leader: %v", err)
	}
	// The next pass of the standby elects it; passes are a minute apart, so the
	// takeover is given two of them.
	m4Eventually(t, "the standby to take leadership and reconcile", 150*time.Second, func() bool {
		pid, held := m4Leader(t, in.h.Pool)
		if !held || pid == elected {
			return false
		}
		return in.count(`SELECT count(*) FROM olp.worker_task_health
            WHERE task = $1 AND last_success_at >= $2`,
			string(usage.TaskCostReconciliation), lostAt) == 1
	})
	if err := standby.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the surviving worker: %v", err)
	}
	// Leadership is a lease on a session, so the last worker leaving returns it.
	if pid, held := m4Leader(t, in.h.Pool); held {
		t.Fatalf("backend %d still holds reconciliation leadership after every worker stopped", pid)
	}
}

// TestM4BudgetsRecoverFromLostSpendState proves what a cost budget does when the
// shared counters it is admitted against are wrong or gone: a key over its limit
// is refused, a key whose balance cannot be read is refused rather than served,
// reconciliation repairs the counters, and a snapshot from a window that has
// already closed can never be mistaken for today's balance.
func TestM4BudgetsRecoverFromLostSpendState(t *testing.T) {
	in := m4Provisioned(t)
	keyID, secret := in.key("budgeted", map[string]any{"daily_cost_limit": "0.01"})
	replica := in.replica("m4-budget-gateway")
	limiter := limLimiter(t, in.valkey, in.namespace)
	day, month := in.costKeys(keyID)

	// Nothing has published a balance yet, so there is no amount to spend
	// against and the request is refused rather than served unaccounted.
	served := in.vendor.chats.Load()
	if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != http.StatusServiceUnavailable ||
		code != "distributed_limits_unavailable" {
		t.Fatalf("unreconciled budget: %d %s", status, code)
	}
	if in.vendor.chats.Load() != served {
		t.Fatal("a request whose budget could not be read reached the upstream")
	}

	// Spend beyond the limit is recorded the way the consumer records it, and
	// reconciliation is what turns stored spend into an admission decision.
	kbFact(t, in.h, keyID, in.provider, time.Now().UTC(), "billable", kbText("0.020000000000"), false)
	if report := m4Reconcile(t, in.h.Pool, limiter); report.KeysReconciled != 1 ||
		report.DailyWindowsReconciled != 1 {
		t.Fatalf("reconciliation report = %+v, want one key and one daily window", report)
	}
	status, code, header := m4Chat(t, replica.PublicOrigin, secret)
	if status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("exhausted budget: %d %s", status, code)
	}
	if retry := m4RetryAfter(t, header); retry < 1 || retry > 86400 {
		t.Fatalf("Retry-After %d is outside the day the budget resets over", retry)
	}
	if in.vendor.chats.Load() != served {
		t.Fatal("a request over budget reached the upstream")
	}

	// Losing the counters is not the same as having none: the key keeps its
	// budget, so it is refused until the balance is republished.
	do(t, in.valkey, "DEL", day)
	do(t, in.valkey, "DEL", month)
	if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != http.StatusServiceUnavailable ||
		code != "distributed_limits_unavailable" {
		t.Fatalf("flushed budget counters: %d %s", status, code)
	}

	// A snapshot of a window that has already closed says nothing about today,
	// so it may not initialize today's counters at any accrued amount.
	windows := limits.BudgetWindows(time.Now().UTC())
	stale := limits.CostSnapshot{CostOwnerID: keyID,
		DailyWindowID: windows.DailyID - 1, DailyAccrued: "0.00",
		MonthlyWindowID: windows.MonthlyID - 1, MonthlyAccrued: "0.00"}
	daily, monthly, err := limiter.ApplyCostSnapshot(t.Context(), stale)
	if err != nil {
		t.Fatalf("apply a snapshot from a closed window: %v", err)
	}
	if daily || monthly {
		t.Fatalf("a snapshot from a closed window initialized today (daily %v, monthly %v)",
			daily, monthly)
	}
	if m4Exists(t, in.valkey, day) || m4Exists(t, in.valkey, month) {
		t.Fatal("a snapshot from a closed window created today's counters")
	}
	if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != http.StatusServiceUnavailable ||
		code != "distributed_limits_unavailable" {
		t.Fatalf("budget after a stale snapshot: %d %s", status, code)
	}

	// The next reconciliation pass repairs exactly what was lost.
	if report := m4Reconcile(t, in.h.Pool, limiter); report.DailyWindowsReconciled != 1 {
		t.Fatalf("repair report = %+v, want one daily window", report)
	}
	if !m4Exists(t, in.valkey, day) || !m4Exists(t, in.valkey, month) {
		t.Fatal("reconciliation did not republish the counters it owns")
	}
	if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != http.StatusTooManyRequests ||
		code != "budget_exhausted" {
		t.Fatalf("repaired budget: %d %s", status, code)
	}
	if in.vendor.chats.Load() != served {
		t.Fatal("a request over budget reached the upstream after recovery")
	}
}

// TestM4GatewayEpochsRecordAndResolveLostReplicas proves that a replica which
// disappears leaves a record an operator can act on: its epoch is confirmed
// stale, the completeness gap that uncertainty represents is written down, the
// console lists it as unresolved until somebody acknowledges it, and a replica
// that stops properly closes its epoch instead.
func TestM4GatewayEpochsRecordAndResolveLostReplicas(t *testing.T) {
	in := m4Provisioned(t)
	_, secret := in.key("epochs", nil)
	lost := in.replica("m4-epoch-lost")
	lostInstance := lost.GatewayInstance
	worker := in.worker("m4-epoch-worker")

	const requests = 2
	for range requests {
		if status, code, _ := m4Chat(t, lost.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference: %d %s", status, code)
		}
	}
	// Waiting for the accounting proves the replica's events were delivered,
	// so what the epoch below reports is a shutdown and not a backlog.
	m4Eventually(t, "the worker to account for the requests", 60*time.Second, func() bool {
		return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == requests
	})
	// The epoch is checkpointed while the replica runs, so waiting for it to
	// report the events it accepted is what makes the record below a statement
	// about a shutdown rather than about a checkpoint that never happened.
	m4Eventually(t, "the replica to report the events it accepted", 30*time.Second, func() bool {
		return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
            WHERE gateway_instance = $1 AND accepted = $2`, lostInstance, requests) == 1
	})
	if err := lost.Kill(); err != nil {
		t.Fatalf("kill the replica: %v", err)
	}

	// Detection deliberately waits before it declares a replica gone, and then
	// confirms on a later pass, so this takes over a minute by design.
	m4Eventually(t, "the detector to confirm the lost replica's epoch",
		usage.EpochStaleAfter+usage.EpochConfirmAfter+90*time.Second, func() bool {
			return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
                WHERE gateway_instance = $1 AND stale_detected_at IS NOT NULL`, lostInstance) == 1
		})
	if total := in.count(`SELECT count(*) FROM olp.request_metadata_ingestion_gaps
        WHERE gateway_instance = $1 AND reason = 'gateway_epoch_unclean_shutdown'`,
		lostInstance); total != 1 {
		t.Fatalf("unclean shutdown gaps for the lost replica = %d, want 1", total)
	}

	unresolved := m4Epoch(t, in, "unresolved", lostInstance)
	if unresolved["state"] != "unresolved" || unresolved["gracefully_closed_at"] != nil ||
		unresolved["acknowledged_at"] != nil {
		t.Fatalf("lost epoch: %v", unresolved)
	}
	if unresolved["stale_detected_at"] == nil || unresolved["uncertainty_gap_id"] == nil {
		t.Fatalf("a confirmed epoch must carry its detection and its gap: %v", unresolved)
	}
	if unresolved["accepted"] != float64(requests) {
		t.Fatalf("epoch accepted = %v, want %d", unresolved["accepted"], requests)
	}

	epoch := unresolved["process_epoch"].(string)
	path := "/api/v1/request-metadata/gateway-epochs/" + epoch + "/acknowledge"
	acknowledged := in.h.want(in.owner, http.MethodPost, path, nil, nil, 200)
	if acknowledged["process_epoch"] != epoch || acknowledged["gateway_instance"] != lostInstance {
		t.Fatalf("acknowledgement: %v", acknowledged)
	}
	if acknowledged["acknowledged_by"] == nil || acknowledged["acknowledged_at"] == nil {
		t.Fatalf("an acknowledgement records who resolved it and when: %v", acknowledged)
	}
	// Acknowledging is an operator statement, so repeating it is not an error
	// and does not rewrite the moment the loss was first accepted.
	if replayed := in.h.want(in.owner, http.MethodPost, path, nil, nil, 200); replayed["acknowledged_at"] != acknowledged["acknowledged_at"] {
		t.Fatalf("a repeated acknowledgement moved the record: %v", replayed)
	}
	if resolved := m4Epoch(t, in, "acknowledged", lostInstance); resolved["process_epoch"] != epoch {
		t.Fatalf("acknowledged epoch: %v", resolved)
	}
	if total := in.count(`SELECT count(*) FROM olp.audit
        WHERE action = 'request_metadata.gateway_epoch_acknowledge'`); total != 2 {
		t.Fatalf("acknowledgement audit rows = %d, want one per statement", total)
	}

	// A replica that stops properly closes its own epoch, so nothing is left
	// for the detector to find.
	kept := in.replica("m4-epoch-kept")
	keptInstance := kept.GatewayInstance
	if status, code, _ := m4Chat(t, kept.PublicOrigin, secret); status != 200 {
		t.Fatalf("inference against the replica that stops cleanly: %d %s", status, code)
	}
	if err := kept.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the replica: %v", err)
	}
	m4Eventually(t, "the stopped replica to close its epoch", 30*time.Second, func() bool {
		return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
            WHERE gateway_instance = $1 AND gracefully_closed_at IS NOT NULL`, keptInstance) == 1
	})
	closed := m4Epoch(t, in, "gracefully_closed", keptInstance)
	if closed["state"] != "gracefully_closed" || closed["stale_detected_at"] != nil {
		t.Fatalf("cleanly closed epoch: %v", closed)
	}
	if closed["writer_closed"] != true {
		t.Fatalf("a cleanly closed epoch reports its writer drained: %v", closed)
	}
	if err := worker.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the worker: %v", err)
	}
}

// m4Epoch reads the console's epoch list in one state and returns the entry for
// one gateway instance, failing when the list does not carry exactly one.
func m4Epoch(t *testing.T, in *m4Install, state, instance string) map[string]any {
	t.Helper()
	listed := in.h.want(in.owner, http.MethodGet,
		"/api/v1/request-metadata/gateway-epochs?state="+state, nil, nil, 200)
	items, ok := listed["items"].([]any)
	if !ok && listed["items"] != nil {
		t.Fatalf("%s epochs are not a list: %v", state, listed["items"])
	}
	var found []map[string]any
	for _, item := range items {
		entry := item.(map[string]any)
		if entry["gateway_instance"] == instance {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s epochs for %s = %v, want exactly one", state, instance, listed["items"])
	}
	return found[0]
}
