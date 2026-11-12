//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedworker"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/reports"
	"github.com/tyk-swe/olp/internal/usage/retention"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func testRetainedSettlementPollFailure(t *testing.T, fixture usageFixture) {
	t.Helper()
	pool := fixture.pool
	store := resources.New(pool)
	resource, err := store.Put(t.Context(), &resources.Resource{
		Kind: "fine_tuning_job", APIKeyID: fixture.keyID, RouteSlug: "retained-health",
		ProviderID: fixture.providerID, ProviderRevisionID: uuid.NewString(),
		RouteRevisionID: uuid.NewString(), SlotID: uuid.NewString(), TargetID: uuid.NewString(),
		UpstreamID: "fixture-job", State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.retained_settlements
		(resource_id,operation,templates,token_consuming) VALUES($1,'fine_tuning','{}',false)`, resource.UUID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	worker := retainedworker.Worker{Store: store, Resolver: resources.NewResolver(pool), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var failures, successes, resourceFailures int64
		var lastError string
		var lastSuccess *time.Time
		err := pool.QueryRow(t.Context(), `SELECT h.failures_total,h.successes_total,h.last_success_at,s.failures,s.last_error
			FROM olp.worker_task_health h CROSS JOIN olp.retained_settlements s
			WHERE h.task='retained_settlement' AND s.resource_id=$1`, resource.UUID).
			Scan(&failures, &successes, &lastSuccess, &resourceFailures, &lastError)
		if err == nil {
			t.Logf("resource failures=%d error=%s worker failures=%d successes=%d has success=%t", resourceFailures, lastError, failures, successes, lastSuccess != nil)
			if resourceFailures != 1 || lastError != "credential_or_pin_unavailable" || failures != 1 {
				t.Fatal("failed retained settlement was reported as a successful worker pass")
			}
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retained worker did not checkpoint its first pass")
}

func TestFocusSettlesMultipleAttemptsAndOverlappingCommitmentDays(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks
		(id,name,type,destination,streams,format,etag,created_by)
		VALUES ($1,'focus','s3','s3://fixture/focus',ARRAY['focus'],'csv_gzip',$1,$2)`,
		uuid.NewString(), fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	for _, commitment := range []bool{false, true} {
		t.Run(fmt.Sprintf("commitment=%t", commitment), func(t *testing.T) {
			if commitment {
				if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
					(provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
					VALUES ($1,$2,'fixture','openai_compatible',10,'USD','day',$3)`,
					fixture.providerID, uuid.NewString(), fixture.observedAt.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			var previousVersion int64
			if err := pool.QueryRow(t.Context(), `SELECT coalesce(max(version),0) FROM olp.focus_dirty_days`).Scan(&previousVersion); err != nil {
				t.Fatal(err)
			}
			event := fixture.event(fixture.attempt(1), fixture.attempt(2))
			accepted, err := usage.NewAccountingEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ingest.PersistEvent(t.Context(), pool, accepted)
			if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("settle FOCUS event: outcome=%v error=%v", result.Outcome, err)
			}
			var status string
			var facts, committedFacts int
			if err := pool.QueryRow(t.Context(), `SELECT r.status,count(f.attempt_id),count(f.commitment_period)
				FROM olp.request_metadata_event_receipts r
				JOIN olp.attempt_usage_facts f ON f.event_id=r.event_id
				WHERE r.event_id=$1 GROUP BY r.status`, event.EventID).Scan(&status, &facts, &committedFacts); err != nil {
				t.Fatal(err)
			}
			if status != "fact_persisted" || facts != len(event.Attempts) || (committedFacts == facts) != commitment {
				t.Fatalf("receipt=%s facts=%d committed facts=%d", status, facts, committedFacts)
			}
			var day time.Time
			var days int
			var version int64
			if err := pool.QueryRow(t.Context(), `SELECT count(*),min(day),min(version) FROM olp.focus_dirty_days`).Scan(&days, &day, &version); err != nil {
				t.Fatal(err)
			}
			if days != 1 || !day.Equal(event.ObservedAt.UTC().Truncate(24*time.Hour)) || version <= previousVersion {
				t.Fatalf("dirty days=%d day=%s version=%d previous version=%d", days, day, version, previousVersion)
			}
		})
	}
}

type usageFixture struct {
	pool         *pgxpool.Pool
	ownerID      string
	projectID    string
	providerID   string
	keyID        string
	generationID string
	observedAt   time.Time
}

func newUsageFixture(t *testing.T, pool *pgxpool.Pool) usageFixture {
	t.Helper()
	fixture := usageFixture{
		pool: pool, ownerID: uuid.NewString(), projectID: uuid.NewString(), providerID: uuid.NewString(),
		keyID: uuid.NewString(), generationID: uuid.NewString(), observedAt: time.Now().UTC().Add(-time.Minute),
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users (id,email,display_name,role,etag)
		VALUES ($1::uuid,$1::text || '@example.com','Usage fixture','owner',$1::uuid)`, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects (id,name,etag,created_by)
		VALUES ($1::uuid,$1::text,$1::uuid,$2)`, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
		(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
		VALUES ($1::uuid,$1::text,'openai_compatible','draft','{}',$1::uuid,$1::uuid,$2,$3)`,
		fixture.providerID, fixture.ownerID, fixture.projectID); err != nil {
		t.Fatal(err)
	}
	fixture.addKey(t, fixture.keyID)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.runtime_releases (id,sequence,sha256,snapshot,created_by)
		SELECT $1,coalesce(max(sequence),0)+1,repeat('0',64),'{}',$2 FROM olp.runtime_releases`,
		fixture.generationID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	pricingID := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_revisions (id,revision,effective_at,created_by)
		SELECT $1,coalesce(max(revision),0)+1,$2,$3 FROM olp.pricing_revisions`,
		pricingID, fixture.observedAt.Add(-time.Hour), fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,output_per_million,currency)
		VALUES ($1,'openai_compatible',$2,$3,'generation',1,2,'USD')`, pricingID, fixture.providerID, vendor.Model); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f usageFixture) addKey(t *testing.T, keyID string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
		VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Usage fixture',$2,'{}',$1::uuid,$3)`,
		keyID, f.ownerID, f.projectID); err != nil {
		t.Fatal(err)
	}
}

func (f usageFixture) attempt(ordinal int) usage.Attempt {
	status, input, output := http.StatusOK, int64(vendor.InputTokens), int64(vendor.OutputTokens)
	return usage.Attempt{
		ID: uuid.NewString(), Ordinal: ordinal, ProviderID: f.providerID, UpstreamModel: vendor.Model,
		StartedAt: f.observedAt, CompletedAt: f.observedAt, StatusCode: &status, Committed: true,
		Usage: &usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{InputTokens: &input, OutputTokens: &output}},
	}
}

func (f usageFixture) event(attempts ...usage.Attempt) *usage.Event {
	final := attempts[len(attempts)-1]
	return &usage.Event{
		Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
		RuntimeGenerationID: f.generationID, APIKeyID: f.keyID, ProviderID: &final.ProviderID,
		RouteSlug: "usage-fixture", UpstreamModel: &final.UpstreamModel, Operation: "generation", Surface: "openai",
		RequestStartedAt: f.observedAt, RequestCompletedAt: f.observedAt, ObservedAt: f.observedAt,
		StatusCode: final.StatusCode, Committed: final.Committed, UsageComplete: true,
		InputTokens: final.Usage.InputTokens, OutputTokens: final.Usage.OutputTokens, Attempts: attempts,
	}
}

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
	t.Run("identity expiry", func(t *testing.T) { testIdentityExpiry(t, fixture) })
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

func TestUsageReportsKeepWholePeriodAllocationAndCompleteness(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	selected, earlier, later := fixture.attempt(1), fixture.attempt(2), fixture.attempt(3)
	ineligible, legacy, absent := fixture.attempt(4), fixture.attempt(5), fixture.attempt(6)
	event := fixture.event(selected, earlier, later, ineligible, legacy, absent)
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingest.PersistEvent(t.Context(), pool, accepted)
	if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("persist report fixture: outcome=%v error=%v", result.Outcome, err)
	}
	selectedIDs := []string{selected.ID, ineligible.ID, legacy.ID, absent.ID}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_facts
		SET estimated_cost=0,commitment_period='day',commitment_amount=6,commitment_weight=1,
		 route_slug=CASE WHEN attempt_id=ANY($2::uuid[]) THEN 'report-selected' ELSE 'report-outside' END,
		 container_sessions=CASE WHEN attempt_id=$3 THEN '{"small":2}'::jsonb
		 WHEN attempt_id=$4 THEN '{}'::jsonb WHEN attempt_id=$5 THEN '{"small":3,"large":1}'::jsonb ELSE NULL END
		WHERE event_id=$1`, event.EventID, selectedIDs, selected.ID, legacy.ID, absent.ID); err != nil {
		t.Fatal(err)
	}
	scheduleID, durablePeriodID, legacyPeriodID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	previousDay := fixture.observedAt.UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
		(id,provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
		VALUES ($1,$2,$3,'report fixture','openai_compatible',1,'USD','day',$4)`,
		scheduleID, fixture.providerID, uuid.NewString(), previousDay); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_periods
		(id,schedule_id,bucket_start,bucket_end,amount,legacy)
		VALUES ($1,$2,$3::timestamptz,$3::timestamptz+interval '1 day',1,false),
		 ($4,$2,$3::timestamptz+interval '1 day',$3::timestamptz+interval '2 days',6,true)`,
		durablePeriodID, scheduleID, previousDay, legacyPeriodID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage
		(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
		SELECT attempt_id,$2,observed_at+CASE WHEN attempt_id=$3 THEN interval '-1 minute'
		 WHEN attempt_id=$4 THEN interval '1 minute' ELSE interval '0' END,
		 CASE WHEN attempt_id=$5 THEN 10000 ELSE 1 END,attempt_id<>$5,true,'{}'
		FROM olp.attempt_usage_facts WHERE attempt_id=ANY($1::uuid[])`,
		[]string{selected.ID, earlier.ID, later.ID, ineligible.ID}, durablePeriodID, earlier.ID, later.ID, ineligible.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage
		(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
		VALUES ($1,$2,$3,1,true,true,'{}')`, legacy.ID, legacyPeriodID, fixture.observedAt); err != nil {
		t.Fatal(err)
	}
	route := "report-selected"
	filters := reports.Filters{
		Start: fixture.observedAt.Add(-time.Minute), End: fixture.observedAt.Add(time.Minute),
		ProviderID: &fixture.providerID, Route: &route, Projects: reports.AllProjects(),
	}
	now := time.Now()
	summary, err := reports.ReadSummary(t.Context(), pool, filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AllocatedCommitment != "2.333333333334" || summary.RequestCount != 1 {
		t.Fatalf("selected request allocation changed: %+v", summary)
	}
	if len(summary.ContainerSessions) != 2 || summary.ContainerSessions["small"] != "5" || summary.ContainerSessions["large"] != "1" {
		t.Fatalf("container totals changed: %v", summary.ContainerSessions)
	}
	full, err := reports.ReadSummary(t.Context(), pool, reports.Filters{
		Start: filters.Start, End: filters.End, ProviderID: filters.ProviderID, Projects: reports.AllProjects(),
	}, now)
	if err != nil || full.AllocatedCommitment != "3.000000000000" {
		t.Fatalf("whole period lost its residual: allocation=%s error=%v", full.AllocatedCommitment, err)
	}
	completeness, err := reports.ReadCompleteness(t.Context(), pool, filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if completeness.RequestCount != summary.RequestCount || completeness.UnpricedCount != summary.UnpricedCount ||
		completeness.IncompleteCount != summary.IncompleteCount || completeness.PricedCount != summary.RequestCount-summary.UnpricedCount ||
		completeness.Complete != summary.Complete || completeness.Coverage != summary.Coverage ||
		completeness.GapEvents != summary.GapEvents || completeness.UncertainGapCount != summary.UncertainGapCount ||
		completeness.Consumer.State != summary.Consumer.State ||
		completeness.EstimatedCost == nil || summary.EstimatedCost == nil || *completeness.EstimatedCost != *summary.EstimatedCost ||
		completeness.Currency == nil || summary.Currency == nil || *completeness.Currency != *summary.Currency {
		t.Fatalf("completeness differs from summary: completeness=%+v summary=%+v", completeness, summary)
	}
	hour := fixture.observedAt.Truncate(time.Hour).Add(-2 * time.Hour)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_hourly
		(bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
		request_count,provider_request_count,model_request_count,target_request_count,
		input_tokens,output_tokens,cached_input_tokens,media_units,
		request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
		request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count)
		VALUES($1,'report-hourly',$2,$3,'generation','openai',$4,1,1,1,1,0,0,0,0,0,0,0,0,0,0,0,0)`,
		hour, fixture.providerID, vendor.Model, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, time.Minute} {
		retainedFilters := reports.Filters{Start: hour.Add(offset), End: hour.Add(time.Hour), ProviderID: &fixture.providerID, Projects: reports.AllProjects()}
		retainedSummary, err := reports.ReadSummary(t.Context(), pool, retainedFilters, now)
		if err != nil {
			t.Fatal(err)
		}
		retainedCompleteness, err := reports.ReadCompleteness(t.Context(), pool, retainedFilters, now)
		if err != nil {
			t.Fatal(err)
		}
		if retainedCompleteness.RequestCount != retainedSummary.RequestCount ||
			retainedCompleteness.Coverage != retainedSummary.Coverage ||
			retainedCompleteness.EstimatedCost != nil || retainedSummary.EstimatedCost != nil ||
			(retainedCompleteness.RequestCount == 1) != (offset == 0) {
			t.Fatalf("retained-hour completeness differs at offset %s: completeness=%+v summary=%+v", offset, retainedCompleteness, retainedSummary)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_hourly SET provider_unpriced_count=2
		WHERE bucket=$1 AND provider_id=$2`, hour, fixture.providerID); err != nil {
		t.Fatal(err)
	}
	invalidFilters := reports.Filters{Start: hour, End: hour.Add(time.Hour), ProviderID: &fixture.providerID, Projects: reports.AllProjects()}
	if _, err := reports.ReadSummary(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("summary accepted more unpriced requests than total requests")
	}
	if _, err := reports.ReadCompleteness(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("completeness accepted more unpriced requests than total requests")
	}
}

func TestBudgetThresholdNotificationsUseBudgetBasis(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer receiver.Close()
	destination := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by)
 VALUES($1,'budget basis',$2,$3,$1,$4)`, destination, receiver.URL, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	type expectation struct {
		name, rule string
		amount     int
	}
	var expected []expectation
	for _, kind := range []string{"api_key", "budget_group"} {
		for _, budget := range []struct {
			basis                  string
			cost, charge, expected int
		}{{"", 80, 40, 80}, {"cost", 80, 40, 80}, {"charge", 40, 80, 80}, {"charge", 80, 40, 0}} {
			basis := budget.basis
			subject := uuid.NewString()
			policy := map[string]string{"daily_cost_limit": "100", "weekly_cost_limit": "100", "monthly_cost_limit": "100"}
			if basis != "" {
				policy["cost_basis"] = basis
			}
			raw, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "api_key" {
				_, err = pool.Exec(t.Context(), `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id)
     VALUES($1::uuid,$1::text,decode(repeat('00',32),'hex'),$1::text,$2,$3,$1::uuid,$4)`, subject, fixture.ownerID, raw, fixture.projectID)
			} else {
				_, err = pool.Exec(t.Context(), `INSERT INTO olp.budget_groups(id,name,project_id,etag,created_by,daily_cost_limit,weekly_cost_limit,monthly_cost_limit,cost_basis)
     VALUES($1::uuid,$1::text,$2,$1::uuid,$3,100,100,100,COALESCE(NULLIF($4,''),'cost'))`, subject, fixture.projectID, fixture.ownerID, basis)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, window := range []string{"day", "week", "month"} {
				table, owner := "olp.api_key_cost_windows", "api_key_id"
				if kind == "budget_group" {
					table, owner = "olp.budget_group_cost_windows", "budget_group_id"
				}
				if _, err := pool.Exec(t.Context(), `INSERT INTO `+table+`(`+owner+`,window_kind,window_id,accrued,charged,unpriced_attempts,weekly_complete)
     SELECT $1,$2,window_id,$3,$4,0,true FROM olp.budget_window($2,now())`, subject, window, budget.cost, budget.charge); err != nil {
					t.Fatal(err)
				}
				rule := uuid.NewString()
				if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by)
     VALUES($1,'budget basis',$2,'budget.threshold',$3,$4,$5,75,$6,$1,$7)`, rule, fixture.projectID, kind, subject, window, destination, fixture.ownerID); err != nil {
					t.Fatal(err)
				}
				expected = append(expected, expectation{name: fmt.Sprintf("%s/%s/%s/cost%d/charge%d", kind, basis, window, budget.cost, budget.charge), rule: rule, amount: budget.expected})
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		notifications.RunDelivery(ctx, notifications.DeliveryConfig{Pool: pool, NewID: uuid.NewString, Now: time.Now,
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Sender: notifications.NewSender(egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}})})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var success bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='notification_delivery' AND successes_total>0)`).Scan(&success); err != nil {
			t.Fatal(err)
		}
		if success {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification worker did not finish its first pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range expected {
		var amount int
		var delivered bool
		if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(accrued),0)::int,COALESCE(bool_and(status='delivered'),false) FROM olp.notification_deliveries WHERE rule_id=$1`, want.rule).Scan(&amount, &delivered); err != nil {
			t.Fatal(err)
		}
		if amount != want.amount || delivered != (want.amount != 0) {
			t.Errorf("%s amount=%d delivered=%t, want amount=%d", want.name, amount, delivered, want.amount)
		}
	}
}

func TestFocusExportsHonorDefaultAndExplicitStartDates(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	var installation string
	if err := pool.QueryRow(t.Context(), `UPDATE olp.installation SET active_key_version=1 WHERE singleton RETURNING id::text`).Scan(&installation); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("42", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	credential := uuid.NewString()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := ring.Store(t.Context(), tx, installation, credential, secrets.SinkCredential, []byte(`{"region":"fixture-region","auth_mode":"static","secret":{"access_key_id":"synthetic-fixture-key-material","secret_access_key":"synthetic-fixture-key-material"}}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	uploads := make(chan string, 32)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/failure/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/manifest.json") {
			var manifest struct {
				ChargeDate       string                  `json:"charge_date"`
				CommitmentTotals *focus.CommitmentTotals `json:"commitment_totals"`
			}
			if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if manifest.CommitmentTotals == nil || !manifest.CommitmentTotals.Complete || manifest.CommitmentTotals.Provisional {
				t.Errorf("incomplete FOCUS commitment totals at %s: %+v", r.URL.Path, manifest.CommitmentTotals)
			}
			uploads <- strings.Split(r.URL.Path, "/")[1] + "/" + manifest.ChargeDate
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1).Format("2006-01-02")
	twoDaysAgo := today.AddDate(0, 0, -2).Format("2006-01-02")
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
 (provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at,ends_at)
 VALUES($1,$2,'fixture','openai_compatible',10,'USD','day',$3,$4)`, fixture.providerID, uuid.NewString(), today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		created time.Time
		start   string
	}{
		{"new", today, ""},
		{"existing", today.AddDate(0, 0, -1), ""},
		{"backfill", today, twoDaysAgo},
		{"later", today.AddDate(0, 0, -4), yesterday},
	} {
		settings := map[string]string{"cost_basis": "cost"}
		if c.start != "" {
			settings["start_date"] = c.start
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,credential_id,enabled,etag,created_by,created_at)
 VALUES($1,$2,'s3',$3,ARRAY['focus'],'csv_gzip',$4,$5,true,$1,$6,$7)`, id, c.name, receiver.URL+"/"+c.name, raw, credential, fixture.ownerID, c.created); err != nil {
			t.Fatal(err)
		}
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	worker := focus.Worker{Pool: pool, Keys: ring, Installation: installation, Sender: destination.NewSender(policy, func(string) string { return "" }, installation), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return today.Add(12 * time.Hour) }}
	if _, err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(uploads)
	expected := map[string]bool{"existing/" + yesterday: true, "backfill/" + twoDaysAgo: true, "backfill/" + yesterday: true, "later/" + yesterday: true}
	for upload := range uploads {
		if !expected[upload] {
			t.Errorf("unexpected FOCUS manifest %s", upload)
		}
		delete(expected, upload)
	}
	if len(expected) != 0 {
		t.Errorf("missing FOCUS manifests %v", expected)
	}
	var delivered int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.focus_datasets WHERE delivered_at IS NOT NULL`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 4 {
		t.Errorf("delivered %d FOCUS datasets, want 4", delivered)
	}
	failedSink := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,credential_id,enabled,etag,created_by,created_at) VALUES($1,'failure','s3',$2,ARRAY['focus'],'csv_gzip','{"cost_basis":"cost"}',$3,true,$1,$4,$5)`, failedSink, receiver.URL+"/failure", credential, fixture.ownerID, today.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(t.Context())
	defer stop()
	var deliveryErr error
	workerhealth.Run(runCtx, workerhealth.Loop{Task: workerhealth.TaskFocusExport, Health: pool, Log: worker.Log, Budget: time.Second * 10, Interval: time.Hour, Pass: func(ctx context.Context) (bool, error) {
		progress, err := worker.Pass(ctx)
		deliveryErr = err
		stop()
		return progress, err
	}})
	var sendErr *destination.SendError
	if !errors.As(deliveryErr, &sendErr) {
		t.Errorf("failed destination lost delivery error: %v", deliveryErr)
	}
	var failures, successes int64
	if err := pool.QueryRow(t.Context(), `SELECT failures_total,successes_total FROM olp.worker_task_health WHERE task='focus_export'`).Scan(&failures, &successes); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || successes != 0 {
		t.Errorf("failed export checkpoint failures=%d successes=%d", failures, successes)
	}
	var attempts, failed int
	var retryScheduled bool
	var code string
	if err := pool.QueryRow(t.Context(), `SELECT attempts,failures,next_attempt_at>now(),last_error_code FROM olp.focus_datasets WHERE sink_id=$1`, failedSink).Scan(&attempts, &failed, &retryScheduled, &code); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || failed != 1 || !retryScheduled || code == "" {
		t.Errorf("lost failure/retry state attempts=%d failures=%d scheduled=%t code=%s", attempts, failed, retryScheduled, code)
	}
	t.Logf("failed upload checkpoint failures=%d successes=%d, dataset attempts=%d failures=%d retry_scheduled=%t", failures, successes, attempts, failed, retryScheduled)

}

func TestBlockingStreamReadDoesNotStallOtherCommands(t *testing.T) {
	t.Parallel()
	client := harness.Valkey(t)
	stream := "service:usage:" + uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.Do(ctx, "DEL", stream); err != nil {
			t.Error(err)
		}
	})
	if _, err := client.Do(t.Context(), "XGROUP", "CREATE", stream, "readers", "0", "MKSTREAM"); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		reply, err := client.DoBlocking(t.Context(), "XREADGROUP", "GROUP", "readers", "reader", "BLOCK", "3000", "STREAMS", stream, ">")
		if err == nil && reply == nil {
			err = errors.New("blocking read returned no entries")
		}
		read <- err
	}()
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	if _, err := client.Do(t.Context(), "PING"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("PING waited %s behind a blocking stream read", elapsed)
	}
	if _, err := client.Do(t.Context(), "XADD", stream, "*", "event", "wake"); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

func TestMCPManualSettlementPreservesPricingState(t *testing.T) {
	for _, test := range []struct {
		pricing, price string
		expected       int64
	}{{"unpriced", "NULL", 1}, {"free", "0", 0}} {
		t.Run(test.pricing, func(t *testing.T) {
			pricing, price, expected := test.pricing, test.price, test.expected
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			ctx := t.Context()
			var installation string
			if err := pool.QueryRow(ctx, "SELECT id::text FROM olp.installation WHERE singleton").Scan(&installation); err != nil {
				t.Fatal(err)
			}
			auth := secrets.NewAuthKey(make([]byte, 32), installation)
			keys, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("01", 32) + `"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
				t.Fatal(err)
			}
			token := "audit-session"
			if _, err := pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), f.ownerID, auth.Digest(secrets.SessionDigest, token)); err != nil {
				t.Fatal(err)
			}
			pub, rev, server, srev, dispatch, call := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			sql := fmt.Sprintf(`
 BEGIN;
 INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES('%[1]s','%[2]s','toolset','audit','{}','%[1]s','%[3]s');
 INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES('%[4]s','%[1]s','%[2]s','toolset','{}',repeat('0',64),'%[3]s');
 INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES('%[5]s','%[2]s','audit','streamable_http','https://example.com/mcp',true,'%[6]s','%[5]s','%[3]s');
 INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES('%[6]s','%[5]s',1,'https://example.com/mcp','2025-03-26','[]',repeat('0',64),'%[3]s');
 INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until,dispatched_at) VALUES('%[7]s','%[8]s','%[2]s','tool','{}',decode(repeat('00',32),'hex'),'%[7]s','USD',0,0,'outcome_unknown',now(),now());
 INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES('%[7]s','%[8]s','key','cost',0,0,0,0);
 INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,status,started_at,pricing_state,currency,unit_price) VALUES('%[9]s','%[7]s','%[8]s','%[2]s','%[1]s','%[4]s','%[5]s','%[6]s','t_'||repeat('0',32)||'_'||repeat('0',32),repeat('0',64),'outcome_unknown',now(),'%[10]s','USD',%[11]s);
 COMMIT;`, pub, f.projectID, f.ownerID, rev, server, srev, dispatch, f.keyID, call, pricing, price)
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			a := access.New(access.Config{Pool: pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys})
			mux := http.NewServeMux()
			accounting.NewManagement(a).Register(mux)
			serverHTTP := httptest.NewServer(mux)
			defer serverHTTP.Close()
			for range 2 {
				r, _ := http.NewRequest("POST", serverHTTP.URL+"/api/v1/mcp-tool-calls/"+call+"/resolve", strings.NewReader(`{"outcome":"settled","evidence_digest":"`+strings.Repeat("a", 64)+`","observed_cost":"0","observed_charge":"0"}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "https://test.example")
				r.Header.Set("Idempotency-Key", "audit")
				r.Header.Set("X-CSRF-Token", base64.RawURLEncoding.EncodeToString(auth.Digest(secrets.CSRFDigest, token)))
				r.AddCookie(&http.Cookie{Name: "__Host-olp_session", Value: token})
				w, err := serverHTTP.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Body.Close()
				body, _ := io.ReadAll(w.Body)
				if w.StatusCode != 200 {
					t.Fatalf("resolve HTTP %d: %s", w.StatusCode, body)
				}
			}
			var attempts, uncharged int64
			if err := pool.QueryRow(ctx, `SELECT unpriced_attempts,uncharged_attempts FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='month'`, f.keyID).Scan(&attempts, &uncharged); err != nil {
				t.Fatal(err)
			}
			t.Logf("two resolve requests returned HTTP 200; %s attempts=%d, uncharged=%d", pricing, attempts, uncharged)
			if attempts != expected || uncharged != expected {
				t.Fatalf("pricing=%s: unpriced=%d uncharged=%d, want %d", pricing, attempts, uncharged, expected)
			}
		})
	}
}

func TestUsagePersistsPinnedRateCardsAtLaterObservation(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET project_id=NULL WHERE id=$1`, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	providerRevision := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
		VALUES($1,$2,1,'Rate card fixture','{}','[]','[]',$1,$3)`, providerRevision, fixture.providerID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	card, oldRevision, newRevision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_cards(id,project_id,created_by) VALUES($1,$2,$3)`, card, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_revisions(id,rate_card_id,revision,markup,effective_at,created_by)
		VALUES($1,$2,1,2,$3,$4),($5,$2,2,3,$6,$4)`, oldRevision, card, fixture.observedAt.Add(-time.Minute), fixture.ownerID, newRevision, fixture.observedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, policy, revision, charge string
	}{
		{"old_pin", fmt.Sprintf(`{"rate_card_pinned":true,"rate_card_revision_id":%q}`, oldRevision), oldRevision, "0.0004"},
		{"pinned_absence", `{"rate_card_pinned":true,"rate_card_revision_id":null}`, "", "0.0002"},
		{"stale_unpinned", `{"rate_card_pinned":false,"rate_card_revision_id":null}`, newRevision, "0.0006"},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := fixture.attempt(1)
			input, output := int64(100), int64(50)
			attempt.Usage.InputTokens, attempt.Usage.OutputTokens = &input, &output
			policy := json.RawMessage(test.policy)
			attempt.Routing = &usage.Routing{ProviderRevisionID: providerRevision, Policy: &policy}
			event := fixture.event(attempt)
			event.RequestCompletedAt = fixture.observedAt.Add(2 * time.Second)
			event.Attempts[0].CompletedAt = event.RequestCompletedAt
			event.ObservedAt = event.RequestCompletedAt
			accepted, err := usage.NewAccountingEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ingest.PersistEvent(t.Context(), pool, accepted)
			if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("persist rate-card pin: outcome=%v error=%v", result.Outcome, err)
			}
			var revision string
			var costMatches, chargeMatches bool
			if err := pool.QueryRow(t.Context(), `SELECT COALESCE(rate_card_revision_id::text,''),estimated_cost=0.0002,charge=$2::numeric
				FROM olp.attempt_usage_facts WHERE attempt_id=$1`, attempt.ID, test.charge).Scan(&revision, &costMatches, &chargeMatches); err != nil {
				t.Fatal(err)
			}
			if revision != test.revision || !costMatches || !chargeMatches {
				t.Fatalf("persisted revision=%q cost matches=%t charge matches=%t; want revision=%q charge=%s", revision, costMatches, chargeMatches, test.revision, test.charge)
			}
		})
	}
}

func TestFocusLateCommitmentUsageDirtiesLaterObservedDay(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	day := fixture.observedAt.UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.pricing_revisions SET effective_at=$1`, day.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks (id,name,type,destination,streams,format,etag,created_by) VALUES($1,'focus-midnight','s3','s3://fixture/focus',ARRAY['focus'],'csv_gzip',$1,$2)`, uuid.NewString(), fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules (provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at) VALUES($1,$2,'fixture','openai_compatible',10,'USD','day',$3)`, fixture.providerID, uuid.NewString(), day); err != nil {
		t.Fatal(err)
	}
	persist := func(started, observed time.Time) string {
		t.Helper()
		f := fixture
		f.observedAt = observed
		attempt := f.attempt(1)
		attempt.StartedAt = started
		event := f.event(attempt)
		event.RequestStartedAt = started
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ingest.PersistEvent(t.Context(), pool, accepted)
		if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist outcome=%v error=%v", result.Outcome, err)
		}
		return attempt.ID
	}
	first := persist(day.Add(23*time.Hour+50*time.Minute), day.Add(24*time.Hour+10*time.Minute))
	var initial, revised string
	var before, after int64
	read := func(amount *string, version *int64) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT a.allocated_cost::text,d.version FROM olp.commitment_allocations a JOIN olp.focus_dirty_days d ON d.day=(a.observed_at AT TIME ZONE 'UTC')::date WHERE a.attempt_id=$1`, first).Scan(amount, version); err != nil {
			t.Fatal(err)
		}
	}
	read(&initial, &before)
	persist(day.Add(12*time.Hour), day.Add(12*time.Hour+time.Minute))
	read(&revised, &after)
	if initial == revised {
		t.Fatalf("late usage did not change allocation %s", initial)
	}
	if after <= before {
		t.Fatalf("allocation changed %s to %s but observed-day dirty version stayed %d -> %d", initial, revised, before, after)
	}
	var updated string
	var updateVersion int64
	if _, err := pool.Exec(t.Context(), `UPDATE olp.commitment_usage SET weight=weight*2 WHERE attempt_id=$1`, first); err != nil {
		t.Fatal(err)
	}
	read(&updated, &updateVersion)
	if updated == revised || updateVersion <= after {
		t.Fatalf("weight update allocation %s -> %s, dirty version %d -> %d", revised, updated, after, updateVersion)
	}
}

func TestMCPLateSettlementAfterOrphanRecovery(t *testing.T) {
	t.Parallel()
	for _, status := range []accounting.Status{accounting.StatusSucceeded, accounting.StatusUpstreamError, accounting.StatusGuardrailBlocked} {
		t.Run(string(status), func(t *testing.T) {
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			ctx := t.Context()
			pub, rev, server, srev, dispatch, call := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			sql := fmt.Sprintf(`
 INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES('%[1]s','%[2]s','toolset','late','{}','%[1]s','%[3]s');
 INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES('%[4]s','%[1]s','%[2]s','toolset','{}',repeat('0',64),'%[3]s');
 INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES('%[5]s','%[2]s','late','streamable_http','https://example.com/mcp',true,'%[6]s','%[5]s','%[3]s');
 INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES('%[6]s','%[5]s',1,'https://example.com/mcp','2025-03-26','[]',repeat('0',64),'%[3]s');
 INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until) VALUES('%[7]s','%[8]s','%[2]s','tool','{}',decode(repeat('00',32),'hex'),'%[7]s','USD',1,1,'ready',now()+interval '1 hour');
 INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES('%[7]s','%[8]s','key','cost',1,0,0,0);`, pub, f.projectID, f.ownerID, rev, server, srev, dispatch, f.keyID)
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			price := "1"
			reservation := &accounting.Reservation{DispatchID: dispatch, ClaimID: dispatch, AttemptID: call, APIKeyID: f.keyID, ProjectID: f.projectID, ToolsetID: pub, ToolsetRevisionID: rev, ToolAlias: "t_" + strings.Repeat("0", 32) + "_" + strings.Repeat("0", 32), InputDigest: strings.Repeat("0", 64), CallDigest: strings.Repeat("1", 64), UnitPrice: &price, Currency: "USD"}
			evidence := accounting.AttemptEvidence{ServerID: server, ServerRevisionID: srev, StartedAt: time.Now().UTC(), Status: accounting.StatusDispatched}
			ledger := accounting.NewDurableLedger(pool, nil, nil, nil)
			if err := ledger.MarkDispatched(ctx, reservation, evidence); err != nil {
				t.Fatal(err)
			}
			if n, err := ledger.RecoverOrphanedDispatches(ctx, time.Now().Add(time.Minute), 1); err != nil || n != 1 {
				t.Fatalf("recover = %d, %v", n, err)
			}
			unknown := evidence
			unknown.Status = accounting.StatusOutcomeUnknown
			unknown.ResultDigest = strings.Repeat("3", 64)
			unknown.CompletedAt = time.Now().UTC()
			if err := ledger.MarkUnknown(ctx, reservation, unknown); err != nil {
				t.Fatal(err)
			}
			evidence.Status = status
			evidence.IsError = status == accounting.StatusUpstreamError
			evidence.CompletedAt = time.Now().UTC()
			evidence.ResultDigest = strings.Repeat("2", 64)
			for range 2 {
				if err := ledger.Settle(ctx, reservation, evidence); err != nil {
					t.Fatal(err)
				}
			}
			var callStatus, debitState, resultDigest string
			var chargedOnce bool
			var receipts int
			if err := pool.QueryRow(ctx, `SELECT c.status,d.state,c.result_digest,d.observed_cost=1 AND d.observed_charge=1,(SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=d.id) FROM olp.mcp_tool_calls c JOIN olp.execution_debits d ON d.id=c.dispatch_id WHERE c.id=$1`, call).Scan(&callStatus, &debitState, &resultDigest, &chargedOnce, &receipts); err != nil {
				t.Fatal(err)
			}
			t.Logf("call=%s debit=%s charged_once=%t receipts=%d", callStatus, debitState, chargedOnce, receipts)
			if callStatus != string(status) || debitState != "settled" || !chargedOnce || resultDigest != evidence.ResultDigest || receipts != 1 {
				t.Fatalf("late settlement: call=%s debit=%s receipts=%d", callStatus, debitState, receipts)
			}
		})
	}
}

func TestKeyReminderRestoredScheduleRetries(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	requests := make(chan struct{}, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		if len(requests) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	destinationID, ruleID := uuid.NewString(), uuid.NewString()
	due := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=$2 WHERE id=$1`, fixture.keyID, due); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by) VALUES($1,'restored schedule',$2,$3,$1,$4)`, destinationID, receiver.URL, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,destination_id,etag,created_by) VALUES($1,'restored schedule',$2,'key.expiring','api_key',$3,$4,$1,$5)`, ruleID, fixture.projectID, fixture.keyID, destinationID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	pass := func() {
		t.Helper()
		var before int64
		if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='notification_delivery'),0)`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			notifications.RunDelivery(ctx, notifications.DeliveryConfig{Pool: pool, NewID: uuid.NewString, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sender: notifications.NewSender(egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}})})
		}()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var successes int64
			if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='notification_delivery'),0)`).Scan(&successes); err != nil {
				t.Fatal(err)
			}
			if successes > before {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("reminder pass did not complete")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	assertDelivery := func(want string, attempts int) (string, time.Time) {
		t.Helper()
		var id, status string
		var got int
		var at time.Time
		if err := pool.QueryRow(t.Context(), `SELECT id::text,status,attempts,last_attempt_at FROM olp.notification_deliveries WHERE rule_id=$1 AND due_at=$2`, ruleID, due).Scan(&id, &status, &got, &at); err != nil {
			t.Fatal(err)
		}
		if status != want || got != attempts {
			t.Fatalf("restored reminder status=%s attempts=%d, want %s/%d", status, got, want, attempts)
		}
		return id, at
	}
	setDue := func(at time.Time) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=$2 WHERE id=$1`, fixture.keyID, at); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	id, attemptAt := assertDelivery("failed", 1)
	setDue(due.Add(48 * time.Hour))
	pass()
	assertDelivery("cancelled", 1)
	setDue(due)
	pass()
	restoredID, restoredAt := assertDelivery("pending", 1)
	if restoredID != id || !restoredAt.Equal(attemptAt) || len(requests) != 1 {
		t.Fatal("restoring the schedule reset identity, retry history or backoff")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.notification_deliveries SET last_attempt_at=now()-interval '2 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	pass()
	assertDelivery("delivered", 2)
	setDue(due.Add(48 * time.Hour))
	pass()
	setDue(due)
	pass()
	assertDelivery("delivered", 2)
	if len(requests) != 2 {
		t.Fatalf("sent %d attempts, want one failure and one successful delivery", len(requests))
	}
}

func TestFocusRetainedHistoryCompleteness(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	oldDay := today.AddDate(0, 0, -2)
	fixture.observedAt = oldDay.Add(12 * time.Hour)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.pricing_revisions SET effective_at=$1`, oldDay.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	event := fixture.event(fixture.attempt(1))
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := ingest.PersistEvent(t.Context(), pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("event fixture failed: %+v %v", result, err)
	}
	ledger := newUsageFixture(t, pool)
	ledger.observedAt = fixture.observedAt
	if _, err := pool.Exec(t.Context(), `UPDATE olp.pricing_revisions SET effective_at=$1`, oldDay.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
 (provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at,ends_at)
 VALUES($1,$2,'fixture','openai_compatible',10,'USD','day',$3,$4)`, ledger.providerID, uuid.NewString(), oldDay, oldDay.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	ledgerEvent := ledger.event(ledger.attempt(1))
	ledgerEvent.RouteSlug = "ledger-fixture"
	ledgerAccepted, err := usage.NewAccountingEvent(ledgerEvent)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := ingest.PersistEvent(t.Context(), pool, ledgerAccepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("ledger fixture failed: %+v %v", result, err)
	}
	partialLedger := newUsageFixture(t, pool)
	partialLedger.observedAt = fixture.observedAt
	if _, err := pool.Exec(t.Context(), `UPDATE olp.pricing_revisions SET effective_at=$1`, oldDay.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
 (provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at,ends_at)
 VALUES($1,$2,'partial','openai_compatible',10,'USD','day',$3,$4)`, partialLedger.providerID, uuid.NewString(), oldDay, oldDay.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	upsertEvent := partialLedger.event(partialLedger.attempt(1))
	upsertEvent.RouteSlug = "upsert-fixture"
	upsertAccepted, err := usage.NewAccountingEvent(upsertEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(t.Context(), pool, upsertAccepted); err != nil {
		t.Fatal(err)
	}
	for _, incomplete := range []bool{false, true} {
		attempt := partialLedger.attempt(1)
		route := "ledger-unpriced"
		if incomplete {
			route = "ledger-incomplete"
			attempt.Usage.Evidence = usage.EvidenceFrom(attempt.Usage.Evidence.Observed(), false, false)
		} else {
			attempt.UpstreamModel = "unpriced-fixture-model"
		}
		event := partialLedger.event(attempt)
		event.RouteSlug = route
		event.UsageComplete = !incomplete
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(t.Context(), pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("partial fixture failed: %+v %v", result, err)
		}
	}
	notBillable := fixture.attempt(1)
	notBillable.Usage = &usage.AttemptUsage{Evidence: usage.EvidenceFrom(false, true, false)}
	notBillableEvent := fixture.event(notBillable)
	notBillableEvent.RouteSlug = "not-billable"
	notBillableAccepted, err := usage.NewAccountingEvent(notBillableEvent)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := ingest.PersistEvent(t.Context(), pool, notBillableAccepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("nonbillable fixture failed: %+v %v", result, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) VALUES('retention.usage_days','1',gen_random_uuid(),$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	report, err := retention.RunMaintenance(t.Context(), pool, today.Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var facts, hourly int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.attempt_usage_facts),(SELECT count(*) FROM olp.attempt_usage_hourly)`).Scan(&facts, &hourly); err != nil {
		t.Fatal(err)
	}
	t.Logf("retention rolled=%d raw facts=%d hourly rows=%d", report.RollupRows, facts, hourly)
	if facts != 0 || hourly != 6 {
		t.Fatal("fixture did not roll up real usage")
	}
	for _, complete := range []bool{false, true} {
		attempt := partialLedger.attempt(1)
		attempt.Usage.Evidence = usage.EvidenceFrom(true, complete, false)
		event := partialLedger.event(attempt)
		event.RouteSlug = "upsert-fixture"
		event.UsageComplete = complete
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(t.Context(), pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("late fixture failed: %+v %v", result, err)
		}
		if _, err := retention.RunMaintenance(t.Context(), pool, today.Add(12*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_hourly
 (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
 request_count,provider_request_count,model_request_count,target_request_count,
 input_tokens,output_tokens,cached_input_tokens,media_units,estimated_cost,
 request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
 request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count)
 SELECT bucket,'legacy-fixture',provider_id,upstream_model,operation,surface,api_key_id,
 request_count,provider_request_count,model_request_count,target_request_count,
 input_tokens,output_tokens,cached_input_tokens,media_units,estimated_cost,
 request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
 request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count
 FROM olp.attempt_usage_hourly WHERE route_slug='usage-fixture'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.settings SET value='3' WHERE key='retention.usage_days'`); err != nil {
		t.Fatal(err)
	}
	var installation string
	if err := pool.QueryRow(t.Context(), `UPDATE olp.installation SET active_key_version=1 WHERE singleton RETURNING id::text`).Scan(&installation); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("42", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	credential := uuid.NewString()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := ring.Store(t.Context(), tx, installation, credential, secrets.SinkCredential, []byte(`{"region":"fixture-region","auth_mode":"static","secret":{"access_key_id":"synthetic-fixture-key-material","secret_access_key":"synthetic-fixture-key-material"}}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	type uploadedManifest struct {
		Case       string
		ChargeDate string `json:"charge_date"`
		Parts      []struct {
			Rows int64 `json:"rows"`
		} `json:"parts"`
		Completeness struct {
			Included    int64 `json:"included_attempts"`
			Gap         bool  `json:"ingestion_gap"`
			Unavailable bool  `json:"raw_detail_unavailable"`
		} `json:"completeness"`
		Metadata struct {
			Recency struct {
				Complete bool `json:"DatasetInstanceComplete"`
			} `json:"Recency"`
		} `json:"metadata"`
	}
	uploads := make(chan uploadedManifest, 64)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest.json") {
			var m uploadedManifest
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.Case = strings.Split(r.URL.Path, "/")[1]
			uploads <- m
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	cases := []struct {
		name, basis, route, project string
		unavailable                 bool
		rows                        bool
	}{
		{"ordinary-cost", "cost", "usage-fixture", "", true, false},
		{"ordinary-charge", "charge", "usage-fixture", "", true, false},
		{"ledger-cost", "cost", "ledger-fixture", "", false, true},
		{"ledger-charge", "charge", "ledger-fixture", "", true, false},
		{"ledger-unpriced", "cost", "ledger-unpriced", "", true, false},
		{"ledger-incomplete", "cost", "ledger-incomplete", "", true, false},
		{"mixed", "cost", "", "", true, true},
		{"nonbillable", "cost", "not-billable", "", false, false},
		{"legacy", "cost", "legacy-fixture", "", true, false},
		{"upsert", "cost", "upsert-fixture", "", true, true},
		{"other-route", "cost", "absent", "", false, false},
		{"other-project", "cost", "", uuid.NewString(), false, false},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]string{"cost_basis": c.basis, "start_date": oldDay.Format(time.DateOnly)})
		filterValues := map[string]string{}
		if c.route != "" {
			filterValues["route"] = c.route
		}
		if c.project != "" {
			filterValues["project"] = c.project
		}
		filter, _ := json.Marshal(filterValues)
		id := uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,settings,filter,credential_id,enabled,etag,created_by,created_at) VALUES($1,$2,'s3',$3,ARRAY['focus'],'csv_gzip',$4,$5,$6,true,$1,$7,$8)`, id, c.name, receiver.URL+"/"+c.name, raw, filter, credential, fixture.ownerID, today); err != nil {
			t.Fatal(err)
		}
	}
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	worker := focus.Worker{Pool: pool, Keys: ring, Installation: installation, Sender: destination.NewSender(policy, func(string) string { return "" }, installation), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return today.Add(12 * time.Hour) }}
	if _, err := worker.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(uploads)
	seen := map[string]int{}
	for m := range uploads {
		seen[m.Case]++
		var rows int64
		for _, part := range m.Parts {
			rows += part.Rows
		}
		for _, c := range cases {
			if c.name != m.Case {
				continue
			}
			wantUnavailable, wantRows := c.unavailable, c.rows
			if m.ChargeDate != oldDay.Format(time.DateOnly) {
				wantUnavailable, wantRows = false, false
			}
			t.Logf("case=%s day=%s rows=%d included=%d unavailable=%t complete=%t", c.name, m.ChargeDate, rows, m.Completeness.Included, m.Completeness.Unavailable, m.Metadata.Recency.Complete)
			if m.Completeness.Unavailable != wantUnavailable || m.Metadata.Recency.Complete == wantUnavailable {
				t.Errorf("unexpected completeness for %s on %s: %+v", c.name, m.ChargeDate, m)
			}
			if (rows > 0) != wantRows {
				t.Errorf("unexpected exported rows for %s on %s: %d", c.name, m.ChargeDate, rows)
			}
		}
	}
	for _, c := range cases {
		if seen[c.name] != 2 {
			t.Errorf("%s generated %d days, want 2", c.name, seen[c.name])
		}
	}
}
