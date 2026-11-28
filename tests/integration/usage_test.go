//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/retention"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestConsumerCommitsBeforeLaterEventIsBlocked(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	first := fixture.event(fixture.attempt(1))
	second := fixture.event(fixture.attempt(1))
	firstPayload, err := usage.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := usage.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	client := harness.Valkey(t)
	stream := "service:usage:" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.Do(ctx, "DEL", stream); err != nil {
			t.Errorf("delete usage stream: %v", err)
		}
	})
	for _, payload := range [][]byte{firstPayload, secondPayload} {
		if _, err := client.Do(t.Context(), "XADD", stream, "*", "event", string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	barrier, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	digest := sha256.Sum256(secondPayload)
	if _, err := barrier.Exec(t.Context(), `INSERT INTO olp.request_metadata_event_receipts
		(event_id,request_id,event_sha256,status,observed_at) VALUES($1,$2,$3,'pending',$4)`, second.EventID, second.RequestID, digest[:], second.ObservedAt); err != nil {
		t.Fatal(err)
	}
	consumerContext, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- (ingest.Consumer{Pool: pool, Client: client, Stream: stream, Name: "service-consumer", Region: "service-region", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Run(consumerContext)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("consumer: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("consumer did not stop")
		}
	})
	harness.Eventually(t, "consumer reaching the blocked event", 5*time.Second, func() (bool, string) {
		var blocked bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE $1::integer=ANY(pg_blocking_pids(pid)))`, int32(barrier.Conn().PgConn().PID())).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		return blocked, "consumer has not reached the receipt barrier"
	})
	var settled bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.request_metadata_event_receipts r
		WHERE r.event_id=$1 AND r.status='fact_persisted'
		AND (SELECT count(*) FROM olp.attempt_usage_facts WHERE event_id=r.event_id)=1
		AND EXISTS(SELECT 1 FROM olp.api_key_cost_windows w WHERE w.api_key_id=$2 AND w.window_kind='day'
		AND w.accrued=(SELECT coalesce(sum(estimated_cost),0) FROM olp.attempt_usage_facts WHERE event_id=r.event_id)))`, first.EventID, first.APIKeyID).Scan(&settled); err != nil || !settled {
		t.Fatalf("earlier event did not commit before the later event blocked: settled=%v error=%v", settled, err)
	}
	if length, err := client.Do(t.Context(), "XLEN", stream); err != nil || length != int64(1) {
		t.Fatalf("earlier delivery was not acknowledged: length=%v error=%v", length, err)
	}
	if err := barrier.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	harness.Eventually(t, "both events acknowledged", 5*time.Second, func() (bool, string) {
		length, err := client.Do(t.Context(), "XLEN", stream)
		return err == nil && length == int64(0), fmt.Sprintf("length=%v error=%v", length, err)
	})
	if _, err := client.Do(t.Context(), "XADD", stream, "*", "event", string(firstPayload)); err != nil {
		t.Fatal(err)
	}
	harness.Eventually(t, "replayed event acknowledged", 5*time.Second, func() (bool, string) {
		length, err := client.Do(t.Context(), "XLEN", stream)
		return err == nil && length == int64(0), fmt.Sprintf("length=%v error=%v", length, err)
	})
	var facts int64
	var balanced bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*),
		(SELECT accrued FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='day')=coalesce(sum(estimated_cost),0)
		FROM olp.attempt_usage_facts WHERE api_key_id=$1`, first.APIKeyID).Scan(&facts, &balanced); err != nil || facts != 2 || !balanced {
		t.Fatalf("replay changed accounting: facts=%d balanced=%v error=%v", facts, balanced, err)
	}
}

func TestCleanupPreservesRetainedAndActiveEvidence(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	t.Run("anchor retention", func(t *testing.T) { testAnchorRetention(t, fixture) })
	t.Run("expiring records skip locked rows", func(t *testing.T) { testExpiringRecordsSkipLockedRows(t, fixture) })
	t.Run("identity expiry", func(t *testing.T) { testIdentityExpiry(t, fixture) })
	t.Run("expired secrets and audit", func(t *testing.T) { testExpiredSecretsAndAudit(t, fixture) })
	t.Run("workflow recovery", func(t *testing.T) { testWorkflowRecovery(t, fixture) })
	t.Run("retained settlement failure", func(t *testing.T) { testRetainedSettlementPollFailure(t, fixture) })
	t.Run("session admission after usage retention", func(t *testing.T) { testSessionAdmissionAfterUsageRetention(t, fixture) })
}

func testSessionAdmissionAfterUsageRetention(t *testing.T, fixture usageFixture) {
	t.Helper()
	store := limitdebits.NewStore(fixture.pool)
	session := func(complete bool) (string, usage.AccountingEvent) {
		key, dispatch := uuid.NewString(), uuid.NewString()
		if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
			VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Session fixture',$2,'{}',$1::uuid,$3)`, key, fixture.ownerID, fixture.projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until,dispatched_at)
			VALUES($1,$2,$3,'api',jsonb_build_object('release_id',$4::text,'revision_id',''),decode(repeat('00',32),'hex'),gen_random_uuid(),'USD',1,1,'dispatched',now(),now())`, dispatch, key, fixture.projectID, fixture.generationID); err != nil {
			t.Fatal(err)
		}
		attempt := fixture.attempt(1)
		attempt.UpstreamModel = "unpriced-session-model"
		attempt.Usage.Evidence = usage.EvidenceFrom(attempt.Usage.Evidence.Observed(), complete, false)
		attempt.Execution = &usage.ExecutionReference{DispatchID: dispatch}
		event := fixture.event(attempt)
		event.APIKeyID, event.UsageComplete = key, complete
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(t.Context(), fixture.pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist session: outcome=%v error=%v", result.Outcome, err)
		}
		return key, accepted
	}
	cleanKey, clean := session(true)
	incompleteKey, _ := session(false)
	admission := func(stage string) {
		t.Helper()
		for key, want := range map[string]bool{cleanKey: false, incompleteKey: true} {
			held, err := store.HasUnknownDebit(t.Context(), key, time.Hour)
			if err != nil || held != want {
				t.Fatalf("%s: key %s held=%v, want %v, error=%v", stage, key, held, want, err)
			}
		}
	}
	admission("before retention")
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by)
		VALUES ('retention.usage_days','1',gen_random_uuid(),$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if report, err := retention.RunMaintenance(t.Context(), fixture.pool, time.Now().Add(72*time.Hour)); err != nil || !report.LockAcquired {
		t.Fatalf("maintenance: lock=%v error=%v", report.LockAcquired, err)
	}
	var facts int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE api_key_id=ANY($1::uuid[])`, []string{cleanKey, incompleteKey}).Scan(&facts); err != nil || facts != 0 {
		t.Fatalf("retention kept %d session facts, error=%v", facts, err)
	}
	admission("after retention")
	if result, err := ingest.PersistEvent(t.Context(), fixture.pool, clean); err != nil || result.Outcome != ingest.PersistOutcomeDuplicate {
		t.Fatalf("replay after retention: outcome=%v error=%v", result.Outcome, err)
	}
	admission("after replay")
}

func testAnchorRetention(t *testing.T, fixture usageFixture) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour).Add(37 * time.Minute)
	for _, windows := range []struct{ requests, usage string }{{"1", "3"}, {"3", "1"}} {
		t.Run("requests="+windows.requests+" usage="+windows.usage, func(t *testing.T) {
			if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by)
				VALUES ('retention.requests_days',$1,gen_random_uuid(),$3),('retention.usage_days',$2,gen_random_uuid(),$3)
				ON CONFLICT(key) DO UPDATE SET value=excluded.value`, windows.requests, windows.usage, fixture.ownerID); err != nil {
				t.Fatal(err)
			}
			anchors := []struct {
				id       string
				started  time.Time
				retained bool
			}{
				{uuid.NewString(), now.Add(-96 * time.Hour), false},
				{uuid.NewString(), now.Add(-48 * time.Hour), true},
				{uuid.NewString(), now.Add(-72 * time.Hour).Truncate(time.Hour).Add(time.Minute), true},
				{uuid.NewString(), now.Add(-96 * time.Hour), true},
			}
			for _, anchor := range anchors {
				if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.usage_request_anchors(request_id,request_started_at) VALUES($1,$2)`, anchor.id, anchor.started); err != nil {
					t.Fatal(err)
				}
			}
			locked, err := fixture.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Rollback(context.WithoutCancel(t.Context()))
			if _, err := locked.Exec(t.Context(), `SELECT request_id FROM olp.usage_request_anchors WHERE request_id=$1 FOR UPDATE`, anchors[3].id); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			report, err := retention.RunMaintenance(ctx, fixture.pool, now)
			if err != nil || !report.LockAcquired {
				t.Fatalf("maintenance did not finish while an anchor was locked: lock=%v error=%v", report.LockAcquired, err)
			}
			for _, anchor := range anchors {
				var retained bool
				if err := fixture.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.usage_request_anchors WHERE request_id=$1)`, anchor.id).Scan(&retained); err != nil {
					t.Fatal(err)
				}
				if retained != anchor.retained {
					t.Errorf("anchor started at %s retained=%v, want %v", anchor.started, retained, anchor.retained)
				}
			}
		})
	}
}

func testExpiringRecordsSkipLockedRows(t *testing.T, fixture usageFixture) {
	t.Helper()
	now := time.Now().UTC()
	sessions := []struct {
		id       string
		expires  time.Time
		retained bool
	}{
		{uuid.NewString(), now.Add(-time.Hour), false},
		{uuid.NewString(), now.Add(-time.Hour), false},
		{uuid.NewString(), now.Add(-time.Hour), true},
		{uuid.NewString(), now.Add(time.Hour), true},
	}
	for _, session := range sessions {
		digest := sha256.Sum256([]byte(session.id))
		if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint)
			VALUES($1,$2,$3,$4,'test')`, session.id, fixture.ownerID, digest[:], session.expires); err != nil {
			t.Fatal(err)
		}
	}
	locked, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.WithoutCancel(t.Context()))
	if _, err := locked.Exec(t.Context(), `SELECT id FROM olp.sessions WHERE id=$1 FOR UPDATE`, sessions[2].id); err != nil {
		t.Fatal(err)
	}
	retained := func() {
		t.Helper()
		for _, session := range sessions {
			var exists bool
			if err := fixture.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.sessions WHERE id=$1)`, session.id).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != session.retained {
				t.Errorf("session expiring at %s retained=%v, want %v", session.expires, exists, session.retained)
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	report, err := retention.RunMaintenance(ctx, fixture.pool, now)
	if err != nil || !report.LockAcquired {
		t.Fatalf("maintenance did not finish while an expired session was locked: lock=%v error=%v", report.LockAcquired, err)
	}
	if report.SessionRows != 2 {
		t.Fatalf("maintenance purged %d expired sessions, want the 2 unlocked ones", report.SessionRows)
	}
	retained()
	if err := locked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	sessions[2].retained = false
	report, err = retention.RunMaintenance(t.Context(), fixture.pool, now)
	if err != nil || !report.LockAcquired {
		t.Fatalf("maintenance after unlock: lock=%v error=%v", report.LockAcquired, err)
	}
	if report.SessionRows != 1 {
		t.Fatalf("maintenance after unlock purged %d expired sessions, want the previously locked one", report.SessionRows)
	}
	retained()
}

func cleanupPublication(t *testing.T, fixture usageFixture, kind string) (string, string) {
	t.Helper()
	publication, revision := uuid.NewString(), uuid.NewString()
	if _, err := fixture.pool.Exec(t.Context(), `WITH publication AS (
		INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by)
		VALUES($1::uuid,$3,$5,($1::uuid)::text,'{}',gen_random_uuid(),$4) RETURNING id
	) INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by)
		SELECT $2,id,$3,$5,'{}',repeat('a',64),$4 FROM publication`, publication, revision, fixture.projectID, fixture.ownerID, kind); err != nil {
		t.Fatal(err)
	}
	return publication, revision
}

func testIdentityExpiry(t *testing.T, fixture usageFixture) {
	t.Helper()
	publication, revision := cleanupPublication(t, fixture, "toolset")
	const batchSize = 256
	const expiredCount = 2*batchSize + 7
	if _, err := fixture.pool.Exec(t.Context(), `WITH candidates AS (
		SELECT gen_random_uuid() AS id, ordinal FROM generate_series(0,$5::int) ordinal
	), debits AS (
		INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until)
		SELECT id,$1,$2,'tool','{}',decode(repeat('00',32),'hex'),gen_random_uuid(),'USD',0,0,now() FROM candidates RETURNING id
	) INSERT INTO olp.mcp_call_identities(api_key_id,project_id,call_digest,dispatch_id,attempt_id,toolset_id,toolset_revision_id,tool_alias,input_digest,created_at,expires_at)
		SELECT $1,$2,repeat(replace(d.id::text,'-',''),2),d.id,gen_random_uuid(),$3,$4,'t_'||repeat('0',32)||'_'||repeat('1',32),repeat('2',64),now()-interval '2 hours',
			CASE WHEN c.ordinal=0 THEN now()+interval '1 hour' ELSE now()-interval '1 hour'+c.ordinal*interval '1 millisecond' END
		FROM debits d JOIN candidates c ON c.id=d.id`, fixture.keyID, fixture.projectID, publication, revision, expiredCount); err != nil {
		t.Fatal(err)
	}
	locked, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.WithoutCancel(t.Context()))
	var lockedDigest string
	if err := locked.QueryRow(t.Context(), `SELECT call_digest FROM olp.mcp_call_identities WHERE api_key_id=$1 AND expires_at<now() ORDER BY expires_at LIMIT 1 FOR UPDATE`, fixture.keyID).Scan(&lockedDigest); err != nil {
		t.Fatal(err)
	}
	ledger := accounting.NewDurableLedger(fixture.pool, limitdebits.NewStore(fixture.pool), nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	first, err := ledger.ExpireCallIdentities(ctx, batchSize)
	if err != nil || first != batchSize {
		t.Fatalf("one expiry batch deleted %d, want %d, error=%v", first, batchSize, err)
	}
	drained, err := ledger.DrainExpiredCallIdentities(ctx, batchSize)
	if err != nil || first+drained != expiredCount-1 {
		t.Fatalf("expiry deleted %d of %d unlocked identities, error=%v", first+drained, expiredCount-1, err)
	}
	var future, expired int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE expires_at>now()),count(*) FILTER(WHERE expires_at<=now()) FROM olp.mcp_call_identities WHERE api_key_id=$1`, fixture.keyID).Scan(&future, &expired); err != nil {
		t.Fatal(err)
	}
	if future != 1 || expired != 1 {
		t.Fatalf("remaining identities future=%d expired=%d, want one of each", future, expired)
	}
	if err := locked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	last, err := ledger.DrainExpiredCallIdentities(ctx, batchSize)
	if err != nil || last != 1 {
		t.Fatalf("expiry after unlock deleted %d, want 1, error=%v", last, err)
	}
	var futureRetained bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(expires_at>now()) FROM olp.mcp_call_identities WHERE api_key_id=$1`, fixture.keyID).Scan(&futureRetained); err != nil || !futureRetained {
		t.Fatalf("future identity was not preserved: retained=%v error=%v", futureRetained, err)
	}
}

func testExpiredSecretsAndAudit(t *testing.T, fixture usageFixture) {
	t.Helper()
	expired, locked, live := placeholderSecret(t, fixture.pool, secrets.MutationReplay), placeholderSecret(t, fixture.pool, secrets.MutationReplay), placeholderSecret(t, fixture.pool, secrets.MutationReplay)
	defer mustExec(t, fixture.pool, "DELETE FROM olp.secrets WHERE id=ANY($1::uuid[])", []string{expired, locked, live})
	mustExec(t, fixture.pool, "UPDATE olp.secrets SET expires_at=now()-interval '1 minute' WHERE id=ANY($1::uuid[])", []string{expired, locked})
	stale, recent := uuid.NewString(), uuid.NewString()
	mustExec(t, fixture.pool, `INSERT INTO olp.audit(id,action,resource_type,outcome,occurred_at)
		VALUES($1,'fixture.stale','fixture','success',now()-interval '4000 days'),($2,'fixture.recent','fixture','success',now())`, stale, recent)
	holder, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := holder.Exec(t.Context(), `SELECT id FROM olp.secrets WHERE id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	report, err := retention.RunMaintenance(ctx, fixture.pool, time.Now().UTC())
	if err != nil || !report.LockAcquired || report.AuditRows != 1 {
		t.Fatalf("maintenance with an expired audit row: lock=%v audit rows=%d error=%v", report.LockAcquired, report.AuditRows, err)
	}
	if secretStored(t, fixture.pool, expired) || !secretStored(t, fixture.pool, locked) || !secretStored(t, fixture.pool, live) {
		t.Fatal("maintenance must purge only the expired secret that nothing holds")
	}
	var kept []string
	if err := fixture.pool.QueryRow(t.Context(), "SELECT array_agg(id::text) FROM olp.audit WHERE id=ANY($1::uuid[])", []string{stale, recent}).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{recent}) {
		t.Fatalf("maintenance kept audit rows %v, want only the recent one", kept)
	}
}

func testWorkflowRecovery(t *testing.T, fixture usageFixture) {
	t.Helper()
	_, revision := cleanupPublication(t, fixture, "agent")
	runID := uuid.NewString()
	if _, err := fixture.pool.Exec(t.Context(), `INSERT INTO olp.agent_runs(id,api_key_id,project_id,revision_id,principal_digest) VALUES($1,$2,$3,$4,decode(repeat('00',32),'hex'))`, runID, fixture.keyID, fixture.projectID, revision); err != nil {
		t.Fatal(err)
	}
	store := limitdebits.NewStore(fixture.pool)
	leases := []struct {
		id, claim string
		expiresIn time.Duration
	}{{uuid.NewString(), uuid.NewString(), -time.Hour}, {uuid.NewString(), uuid.NewString(), time.Hour}}
	for ordinal, lease := range leases {
		if _, err := fixture.pool.Exec(t.Context(), `WITH debit AS (
			INSERT INTO olp.execution_debits(id,api_key_id,project_id,run_id,step_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until)
			VALUES($1,$2,$3,$4,$5,'model','{}',decode(repeat('00',32),'hex'),$6,'USD',0,0,'ready',now()+interval '1 hour') RETURNING id
		) INSERT INTO olp.agent_steps(id,run_id,dispatch_id,ordinal) SELECT $5,$4,id,$7 FROM debit`, lease.id, fixture.keyID, fixture.projectID, runID, uuid.NewString(), lease.claim, ordinal); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkWorkflowModelDispatched(t.Context(), lease.id, lease.claim); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(t.Context(), `UPDATE olp.workflow_model_leases SET expires_at=now()+make_interval(secs=>$2) WHERE dispatch_id=$1`, lease.id, lease.expiresIn.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := store.RecoverOrphanedWorkflowModels(t.Context(), 256)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered %d workflow models, want 1, error=%v", recovered, err)
	}
	var expiredState, liveState string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT (SELECT state FROM olp.execution_debits WHERE id=$1),(SELECT state FROM olp.execution_debits WHERE id=$2)`, leases[0].id, leases[1].id).Scan(&expiredState, &liveState); err != nil {
		t.Fatal(err)
	}
	if expiredState != string(limitdebits.Unknown) || liveState != string(limitdebits.Dispatched) {
		t.Fatalf("workflow states expired=%s live=%s", expiredState, liveState)
	}
	if err := store.MarkNotSent(t.Context(), leases[0].id, leases[0].claim); err != nil {
		t.Fatalf("late not-sent proof was rejected: %v", err)
	}
	var proved bool
	if err := fixture.pool.QueryRow(t.Context(), `SELECT d.state='not_accepted' AND d.closed_at IS NOT NULL AND EXISTS(SELECT 1 FROM olp.workflow_model_nonacceptance_proofs p WHERE p.dispatch_id=d.id AND p.claim_id=d.claim_id) FROM olp.execution_debits d WHERE d.id=$1`, leases[0].id).Scan(&proved); err != nil || !proved {
		t.Fatalf("late not-sent proof did not close recovered exposure: proved=%v error=%v", proved, err)
	}
}
