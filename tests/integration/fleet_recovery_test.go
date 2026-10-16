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

// fleetLeaderSQL finds the backend holding the cost reconciliation advisory lock.
// The lock identifier is one 64-bit key, which PostgreSQL reports split across
// the class and object columns, so it is reassembled here rather than written
// out as two magic numbers. Advisory locks are scoped to a database and every
// test here runs in its own, so the probe is scoped the same way: without the
// database predicate it would count the leaders of every other installation on
// the cluster and report several holders of a lock only one session can hold.
const fleetLeaderSQL = `SELECT pid FROM pg_locks
    WHERE locktype = 'advisory' AND objsubid = 1 AND granted
      AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
      AND ((classid::bigint << 32) | objid::bigint) = x'4f4c505f4352'::bigint`

// fleetLeader is the backend that currently holds cost reconciliation leadership,
// and whether anybody holds it at all.
func fleetLeader(t *testing.T, pool *pgxpool.Pool) (int32, bool) {
	t.Helper()
	rows, err := pool.Query(t.Context(), fleetLeaderSQL)
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

// fleetReconcile runs one cost reconciliation pass from the test itself, which is
// what a worker's leader does on its own schedule. Leadership is released again
// so a worker process in the same scenario can take it.
func fleetReconcile(t *testing.T, pool *pgxpool.Pool, limiter *limits.Limiter) limits.Report {
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
func (in *fleetInstall) costKeys(apiKeyID string) (string, string) {
	return limCostKeys(in.namespace, apiKeyID)
}

// fleetExists reports whether one shared Valkey key is present.
func fleetExists(t *testing.T, c *coordination.Client, key string) bool {
	t.Helper()
	return do(t, c, "EXISTS", key) == int64(1)
}

// fleetConsumerPending is how many deliveries one consumer still owes an
// acknowledgement for.
func fleetConsumerPending(t *testing.T, c *coordination.Client, stream, consumer string) int64 {
	t.Helper()
	for _, row := range fleetList(t, do(t, c, "XINFO", "CONSUMERS", stream, usage.Group), "XINFO CONSUMERS") {
		fields := fleetFields(t, row)
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

// Nothing consumes yet, so the events are still on the stream and one of
// them can be kept as the copy a crash would redeliver.

// The worker dies the way a lost machine does, before it could acknowledge
// anything more, and the delivery it already persisted arrives again.

// The worker is lost with deliveries it had taken and not acknowledged:
// the entries below are moved into exactly that consumer's pending list,
// which is the state its termination leaves behind.

// A survivor may not simply steal a delivery that might still be in
// progress; it claims it only once it has been idle past the reclaim window,
// which is why this takes longer than a restart otherwise would.

// Reclaiming must account for each delivery once, not once per claim.

// The neighbouring installation elects a leader under the same identifier.
// It is another installation's leader, so this one still has none.

// Both installations hold the identifier at once, which is exactly the
// state an unscoped probe reports as two leaders of one installation.

// The second worker finds leadership taken and waits, which is the state a
// standby is in when the leader is lost.

// The next pass of the standby elects it; passes are a minute apart, so the
// takeover is given two of them.

// Leadership is a lease on a session, so the last worker leaving returns it.

// Budget admission first needs a live consumer heartbeat; this fixture
// runs no worker, so seed the row a healthy consumer would report.

// Nothing has published a balance yet, so there is no amount to spend
// against and the request is refused rather than served unaccounted.

// Spend beyond the limit is recorded the way the consumer records it, and
// reconciliation is what turns stored spend into an admission decision.

// Losing the counters is not the same as having none: the key keeps its
// budget, so it is refused until the balance is republished.

// A snapshot of a window that has already closed says nothing about today,
// so it may not initialize today's counters at any accrued amount.

// The next reconciliation pass repairs exactly what was lost.

// TestGatewayEpochsRecordAndResolveLostReplicas proves that a replica which
// disappears leaves a record an operator can act on: its epoch is confirmed
// stale, the completeness gap that uncertainty represents is written down, the
// console lists it as unresolved until somebody acknowledges it, and a replica
// that stops properly closes its epoch instead.
func TestGatewayEpochsRecordAndResolveLostReplicas(t *testing.T) {
	in := fleetProvisioned(t)
	_, secret := in.key("epochs", nil)
	lost := in.replica("fleet-epoch-lost")
	lostInstance := lost.GatewayInstance
	worker := in.worker("fleet-epoch-worker")

	const requests = 2
	for range requests {
		if status, code, _ := fleetChat(t, lost.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference: %d %s", status, code)
		}
	}
	// Waiting for the accounting proves the replica's events were delivered,
	// so what the epoch below reports is a shutdown and not a backlog.
	eventually(t, "the worker to account for the requests", 60*time.Second, func() bool {
		return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == requests
	})
	// The epoch is checkpointed while the replica runs, so waiting for it to
	// report the events it accepted is what makes the record below a statement
	// about a shutdown rather than about a checkpoint that never happened.
	eventually(t, "the replica to report the events it accepted", 30*time.Second, func() bool {
		return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
            WHERE gateway_instance = $1 AND accepted = $2`, lostInstance, requests) == 1
	})
	if err := lost.Kill(); err != nil {
		t.Fatalf("kill the replica: %v", err)
	}

	// Detection deliberately waits before it declares a replica gone, and then
	// confirms on a later pass, so this takes over a minute by design.
	eventually(t, "the detector to confirm the lost replica's epoch",
		usage.EpochStaleAfter+usage.EpochConfirmAfter+90*time.Second, func() bool {
			return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
                WHERE gateway_instance = $1 AND stale_detected_at IS NOT NULL`, lostInstance) == 1
		})
	if total := in.count(`SELECT count(*) FROM olp.request_metadata_ingestion_gaps
        WHERE gateway_instance = $1 AND reason = 'gateway_epoch_unclean_shutdown'`,
		lostInstance); total != 1 {
		t.Fatalf("unclean shutdown gaps for the lost replica = %d, want 1", total)
	}

	unresolved := fleetEpoch(t, in, "unresolved", lostInstance)
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
	if resolved := fleetEpoch(t, in, "acknowledged", lostInstance); resolved["process_epoch"] != epoch {
		t.Fatalf("acknowledged epoch: %v", resolved)
	}
	if total := in.count(`SELECT count(*) FROM olp.audit
        WHERE action = 'request_metadata.gateway_epoch_acknowledge'`); total != 2 {
		t.Fatalf("acknowledgement audit rows = %d, want one per statement", total)
	}

	// A replica that stops properly closes its own epoch, so nothing is left
	// for the detector to find.
	kept := in.replica("fleet-epoch-kept")
	keptInstance := kept.GatewayInstance
	if status, code, _ := fleetChat(t, kept.PublicOrigin, secret); status != 200 {
		t.Fatalf("inference against the replica that stops cleanly: %d %s", status, code)
	}
	if err := kept.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop the replica: %v", err)
	}
	eventually(t, "the stopped replica to close its epoch", 30*time.Second, func() bool {
		return in.count(`SELECT count(*) FROM olp.request_metadata_gateway_epochs
            WHERE gateway_instance = $1 AND gracefully_closed_at IS NOT NULL`, keptInstance) == 1
	})
	closed := fleetEpoch(t, in, "gracefully_closed", keptInstance)
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

// fleetEpoch reads the console's epoch list in one state and returns the entry for
// one gateway instance, failing when the list does not carry exactly one.
func fleetEpoch(t *testing.T, in *fleetInstall, state, instance string) map[string]any {
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
