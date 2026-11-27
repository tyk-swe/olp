//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/resources"
	resourcesmanage "github.com/tyk-swe/olp/internal/resources/manage"
	"github.com/tyk-swe/olp/internal/resources/retainedworker"
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
	templates, err := json.Marshal(map[string]*usage.Event{"fixture-model": fixture.event(fixture.attempt(1))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.retained_settlements
		(resource_id,operation,templates,token_consuming) VALUES($1,'fine_tuning',$2,false)`, resource.UUID, templates); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	worker := retainedworker.Worker{Store: store, Resolver: resources.NewResolver(pool), Connections: egress.NewConnectionClientCache(128), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	var failures, successes, resourceFailures int64
	var lastError string
	var lastSuccess *time.Time
	harness.Eventually(t, "retained worker first pass", 5*time.Second, func() (bool, string) {
		err := pool.QueryRow(t.Context(), `SELECT h.failures_total,h.successes_total,h.last_success_at,s.failures,s.last_error
			FROM olp.worker_task_health h CROSS JOIN olp.retained_settlements s
			WHERE h.task='retained_settlement' AND s.resource_id=$1`, resource.UUID).
			Scan(&failures, &successes, &lastSuccess, &resourceFailures, &lastError)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "retained worker has not checkpointed yet"
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, ""
	})
	t.Logf("resource failures=%d error=%s worker failures=%d successes=%d has success=%t", resourceFailures, lastError, failures, successes, lastSuccess != nil)
	if resourceFailures != 1 || lastError != "credential_or_pin_unavailable" || failures != 1 {
		t.Fatal("failed retained settlement was reported as a successful worker pass")
	}
	testRetainedManagement(t, fixture, resource)
}

func testRetainedManagement(t *testing.T, fixture usageFixture, resource *resources.Resource) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	client := managementHTTPAs(t, pool, fixture.ownerID, func(mux *http.ServeMux, server *access.Server) {
		resourcesmanage.New(server).Register(mux)
	})
	post := func(action, body string) (int, []byte) {
		t.Helper()
		res := client.call(t, http.MethodPost, "/api/v1/provider-resources/"+resource.ID+"/"+action, "", body)
		return res.status, res.body
	}
	testRetainedUsageEvidence(t, fixture, resource, post)
	list := client.call(t, http.MethodGet, fmt.Sprintf("/api/v1/provider-resources?kind=fine_tuning_job&api_key_id=%s&provider_id=%s", fixture.keyID, fixture.providerID), "", "")
	var listed contract.ProviderResourceListResponse
	if err := json.Unmarshal(list.body, &listed); err != nil || list.status != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Id != resource.ID || listed.Items[0].ApiKeyId.String() != fixture.keyID || listed.Items[0].ProviderId.String() != fixture.providerID || listed.Items[0].RetainedSettlement == nil {
		t.Fatalf("list provider resources = %d %s, want 200 with %s", list.status, list.body, resource.ID)
	}
	oversizedStatus, oversizedBody := post("reconcile", fmt.Sprintf(`{"upstream_id":%q}`, strings.Repeat("x", 513)))
	var oversizedProblem struct{ Type string }
	if err := json.Unmarshal(oversizedBody, &oversizedProblem); err != nil || oversizedStatus != http.StatusUnprocessableEntity || !strings.HasSuffix(oversizedProblem.Type, "/validation_failed") {
		t.Fatalf("reconcile oversized hint = %d %s, want 422 validation_failed", oversizedStatus, oversizedBody)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.retained_settlements SET state='closed' WHERE resource_id=$1`, resource.UUID); err != nil {
		t.Fatal(err)
	}
	status, body := post("reconcile", `{"upstream_id":"test-hint"}`)
	var problem struct{ Type string }
	if err := json.Unmarshal(body, &problem); err != nil || status != http.StatusConflict || !strings.HasSuffix(problem.Type, "/settlement_closed") {
		t.Fatalf("reconcile closed settlement = %d %s, want 409 settlement_closed", status, body)
	}
}

func testRetainedUsageEvidence(t *testing.T, fixture usageFixture, resource *resources.Resource, post func(action, body string) (int, []byte)) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	audited := func() int {
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.audit WHERE action='provider_resource.usage_evidence' AND resource_id=$1`, resource.UUID.String()).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	evidence := func(model string) string {
		return fmt.Sprintf(`{"idempotency_key":"evidence-1","evidence_digest":%q,"start":%q,"end":%q,"model":%q,"training_tokens":10}`,
			strings.Repeat("ab", 32), start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339), model)
	}
	if status, body := post("usage-evidence", evidence("unpinned-model")); status != http.StatusUnprocessableEntity || audited() != 0 {
		t.Fatalf("evidence for an unpinned model = %d %s with %d audit rows, want 422 and none", status, body, audited())
	}
	status, body := post("usage-evidence", evidence("fixture-model"))
	var accepted struct {
		ID         string `json:"id"`
		Provenance string `json:"provenance"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || status != http.StatusOK || accepted.ID != resource.ID || accepted.Provenance != "operator_verified" {
		t.Fatalf("usage evidence = %d %s, want 200 for %s", status, body, resource.ID)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.retained_usage_evidence WHERE resource_id=$1 AND idempotency_key='evidence-1'`, resource.UUID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 || audited() != 1 {
		t.Fatalf("accepted evidence stored %d rows and %d audit rows, want one of each", stored, audited())
	}
}

func TestRequestDetailKeepsTheObservedCacheLookupReason(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	event := fixture.event(fixture.attempt(1))
	event.CacheResult, event.CacheLookupReason = "bypass", usage.CacheLookupUnavailable
	record, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []ingest.PersistOutcome{ingest.PersistOutcomePersisted, ingest.PersistOutcomeDuplicate} {
		if result, err := ingest.PersistEvent(ctx, pool, record); err != nil || result.Outcome != want {
			t.Fatalf("delivery outcome=%v error=%v, want %v", result.Outcome, err, want)
		}
	}
	altered := *event
	altered.CacheLookupReason = usage.CacheLookupNoStore
	conflict, err := usage.NewAccountingEvent(&altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(ctx, pool, conflict); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("persisting an altered replay: %v", err)
	}
	legacy := fixture.event(fixture.attempt(1))
	legacy.CacheResult = "bypass"
	old, err := usage.NewAccountingEvent(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(ctx, pool, old); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM olp.requests WHERE id=$1", event.RequestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("requests after replay=%d, error=%v", count, err)
	}
	requestReport := requestReports(t, fixture)
	for _, tc := range []struct {
		token, id string
		status    int
		reason    usage.CacheLookupReason
	}{
		{"owner-session", event.RequestID, http.StatusOK, usage.CacheLookupUnavailable},
		{"owner-session", legacy.RequestID, http.StatusOK, ""},
		{"other-project-session", event.RequestID, http.StatusNotFound, ""},
	} {
		status, body := requestReport(tc.token, "/"+tc.id)
		if status != tc.status {
			t.Fatalf("request detail status=%d, want %d: %s", status, tc.status, body)
		}
		if tc.status == http.StatusOK {
			var detail struct {
				Result string                  `json:"cache_result"`
				Reason usage.CacheLookupReason `json:"cache_lookup_reason"`
			}
			if err := json.Unmarshal(body, &detail); err != nil || detail.Result != "bypass" || detail.Reason != tc.reason {
				t.Fatalf("unexpected cache detail: %s (decode error: %v)", body, err)
			}
		} else if bytes.Contains(body, []byte(usage.CacheLookupUnavailable)) {
			t.Fatalf("another project saw the reason: %s", body)
		}
	}
}

func TestRequestDetailKeepsProviderRequestIDsOnReplayAndWithinProject(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	failed, succeeded := fixture.attempt(1), fixture.attempt(2)
	failureStatus := http.StatusServiceUnavailable
	failed.StatusCode, failed.Committed = &failureStatus, false
	failed.Usage = &usage.AttemptUsage{Evidence: usage.EvidenceUnobservedComplete}
	failed.ProviderRequestID, succeeded.ProviderRequestID = "req_Unavailable123", "req_Served456"
	event := fixture.event(failed, succeeded)
	requestReport := requestReports(t, fixture)

	record, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []ingest.PersistOutcome{ingest.PersistOutcomePersisted, ingest.PersistOutcomeDuplicate} {
		if result, err := ingest.PersistEvent(t.Context(), pool, record); err != nil || result.Outcome != want {
			t.Fatalf("delivery outcome=%v error=%v, want %v", result.Outcome, err, want)
		}
	}
	altered := *event
	replacement := failed
	replacement.ID, replacement.ProviderRequestID = uuid.NewString(), succeeded.ProviderRequestID
	altered.Attempts = []usage.Attempt{replacement, succeeded}
	conflict, err := usage.NewAccountingEvent(&altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.PersistEvent(t.Context(), pool, conflict); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("persisting an altered replay: %v", err)
	}

	status, body := requestReport("owner-session", "/"+event.RequestID)
	var detail reports.RequestDetail
	if err := json.Unmarshal(body, &detail); status != http.StatusOK || err != nil || detail.ID != event.RequestID || len(detail.Attempts) != 2 {
		t.Fatalf("unexpected request detail: %d %s (decode error: %v)", status, body, err)
	}
	for i, want := range []usage.Attempt{failed, succeeded} {
		got := detail.Attempts[i]
		if got.ID != want.ID || got.ProviderRequestID == nil || *got.ProviderRequestID != want.ProviderRequestID {
			t.Fatalf("attempt %d changed after replay: %s", i+1, body)
		}
	}
	for _, tc := range []struct {
		token, path string
		status      int
	}{
		{"other-project-session", "/" + event.RequestID, http.StatusNotFound},
		{"owner-session", "", http.StatusOK},
		{"owner-session", "/export.csv", http.StatusOK},
	} {
		status, body := requestReport(tc.token, tc.path)
		if status != tc.status || bytes.Contains(body, []byte(failed.ProviderRequestID)) || bytes.Contains(body, []byte(succeeded.ProviderRequestID)) {
			t.Fatalf("%s %s returned %d and %s", tc.token, tc.path, status, body)
		}
	}
}

func requestReports(t *testing.T, fixture usageFixture) func(string, string) (int, []byte) {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	otherUser, otherProject := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users (id,email,display_name,role,etag,access_scope)
		VALUES ($1::uuid,'other@example.com','Other project','operator',$1::uuid,'assigned')`, otherUser); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.projects (id,name,etag,created_by)
		VALUES ($1::uuid,'other',$1::uuid,$2)`, otherProject, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.project_members (project_id,user_id,role,added_by)
		VALUES ($1,$2,'manager',$3)`, otherProject, otherUser, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	owner := managementHTTPAs(t, pool, fixture.ownerID, func(mux *http.ServeMux, server *access.Server) {
		(&reports.API{Access: server}).Register(mux)
	})
	clients := map[string]*managementClient{
		"owner-session":         owner,
		"other-project-session": owner.as(t, otherUser),
	}
	return func(token, suffix string) (int, []byte) {
		t.Helper()
		res := clients[token].call(t, http.MethodGet, "/api/v1/requests"+suffix, "", "")
		return res.status, res.body
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
	t.Run("expiring records skip locked rows", func(t *testing.T) { testExpiringRecordsSkipLockedRows(t, fixture) })
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

func TestUnpricedBreakdownKeepsAttemptCausesAfterPricesChangeAndUsageRollsUp(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	ctx := t.Context()
	hour := fixture.observedAt.Truncate(time.Hour)
	const missingModel, partialModel = "no-model-price", "partial-model-price"
	var pricingID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,currency)
		SELECT pricing_revision_id,provider_kind,provider_id,$2,operation,1,currency
		FROM olp.prices WHERE provider_id=$1 RETURNING pricing_revision_id`,
		fixture.providerID, partialModel).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	persist := func(event *usage.Event) {
		t.Helper()
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(ctx, pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist fixture: %+v %v", result, err)
		}
	}
	missing := func(ordinal int) usage.Attempt {
		attempt := fixture.attempt(ordinal)
		attempt.UpstreamModel = missingModel
		return attempt
	}
	noPrice := fixture.event(missing(1), missing(2), missing(3))
	persist(noPrice)

	incomplete := func(ordinal int) usage.Attempt {
		attempt := missing(ordinal)
		attempt.Usage.Evidence = usage.EvidenceObservedIncomplete
		return attempt
	}
	answeredWithoutUsage := missing(1)
	answeredWithoutUsage.Usage = &usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}
	unsettled := fixture.event(answeredWithoutUsage, incomplete(2))
	unsettled.UsageComplete = false
	persist(unsettled)

	partial := fixture.attempt(1)
	partial.UpstreamModel = partialModel
	persist(fixture.event(partial))
	persist(fixture.event(fixture.attempt(1)))
	historical := fixture.event(missing(1))
	persist(historical)
	if _, err := pool.Exec(ctx, `UPDATE olp.attempt_usage_facts SET unpriced_cause = NULL WHERE event_id = $1`, historical.EventID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,output_per_million,currency)
		SELECT pricing_revision_id,provider_kind,provider_id,$3,operation,input_per_million,output_per_million,currency
		FROM olp.prices WHERE provider_id=$1 AND model=$2`, fixture.providerID, vendor.Model, missingModel); err != nil {
		t.Fatal(err)
	}
	filters := reports.Filters{Start: hour, End: hour.Add(time.Hour), Projects: reports.OnlyProjects(fixture.projectID)}
	expected := []struct {
		model   string
		cause   contract.UnpricedBreakdownItemCause
		count   int64
		request string
	}{
		{missingModel, contract.NoApplicablePrice, 3, noPrice.RequestID},
		{missingModel, contract.IncompleteUsage, 2, unsettled.RequestID},
		{missingModel, contract.NotRecorded, 1, historical.RequestID},
		{partialModel, contract.MissingPriceComponent, 1, ""},
	}
	assertBreakdown := func(live bool) {
		t.Helper()
		report, err := reports.ReadUnpricedBreakdown(ctx, pool, filters, 50)
		if err != nil || len(report.Items) != len(expected) {
			t.Fatalf("read unpriced groups: %+v %v", report, err)
		}
		for i, want := range expected {
			row := report.Items[i]
			if row.ProviderId.String() != fixture.providerID || row.Operation != noPrice.Operation ||
				row.Model != want.model || row.Cause != want.cause || row.AttemptCount != want.count {
				t.Errorf("group %d = %+v, want %+v", i, row, want)
			}
			if live && want.request != "" && (row.SampleRequestId == nil || row.SampleRequestId.String() != want.request) {
				t.Errorf("group %d did not link the matching request %s", i, want.request)
			}
			if !live && row.SampleRequestId != nil {
				t.Errorf("retained group %d linked expired request %s", i, row.SampleRequestId)
			}
		}
	}
	assertBreakdown(true)
	limited, err := reports.ReadUnpricedBreakdown(ctx, pool, filters, 1)
	if err != nil || len(limited.Items) != 1 || limited.Items[0].AttemptCount != 3 {
		t.Fatalf("limit did not keep the highest-impact group: %+v %v", limited, err)
	}
	privateFilters := filters
	privateFilters.Projects = reports.OnlyProjects(uuid.NewString())
	private, err := reports.ReadUnpricedBreakdown(ctx, pool, privateFilters, 50)
	if err != nil || len(private.Items) != 0 {
		t.Fatalf("another project can see unpriced usage: %+v %v", private, err)
	}
	var pin *uuid.UUID
	var cost, charge *string
	if err := pool.QueryRow(ctx, `SELECT pricing_revision_id, estimated_cost::text, charge::text
		FROM olp.attempt_usage_facts WHERE attempt_id=$1`, partial.ID).Scan(&pin, &cost, &charge); err != nil ||
		pin == nil || *pin != pricingID || cost != nil || charge != nil {
		t.Fatalf("partial rate changed its pin or historical cost/charge: %v %v %v %v", pin, cost, charge, err)
	}

	maintenance, err := retention.RunMaintenance(ctx, pool, hour.AddDate(0, 0, 91))
	if err != nil || maintenance.UsageRows != 8 {
		t.Fatalf("roll up fixture: %+v %v", maintenance, err)
	}
	assertBreakdown(false)
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

func TestReconciliationContinuesExcludingCallerPaidSpendAfterRollup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name          string
		legacy        bool
		operatorSpend string
	}{{"durable", false, "8.25"}, {"legacy", true, "5.75"}} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			checkCallerPaidSpendAfterRollup(t, scenario.legacy, scenario.operatorSpend)
		})
	}
}

func checkCallerPaidSpendAfterRollup(t *testing.T, legacy bool, operatorSpend string) {
	t.Helper()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	day := f.observedAt.Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO olp.commitment_schedules
		(provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
		VALUES($1,$2,'reconciliation fixture','openai_compatible',5,'USD','day',$3)`,
		f.providerID, uuid.NewString(), day); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		caller     bool
		cost       string
		commitment bool
	}{{false, "3.25", false}, {true, "7.5", false}, {false, "0", true}, {true, "0", true}} {
		attempt := f.attempt(1)
		event, err := usage.NewAccountingEvent(f.event(attempt))
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(ctx, pool, event); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist reconciliation sample %+v: %v, %v", sample, result.Outcome, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE olp.attempt_usage_facts SET caller_credential=$2,
			estimated_cost=$3::text::numeric,commitment_period=CASE WHEN $4 THEN 'day' END,
			commitment_amount=CASE WHEN $4 THEN 5 END,commitment_weight=CASE WHEN $4 THEN 1 END
			WHERE attempt_id=$1`, attempt.ID, sample.caller, sample.cost, sample.commitment); err != nil {
			t.Fatal(err)
		}
	}
	if legacy {
		result, err := pool.Exec(ctx, `UPDATE olp.commitment_periods SET legacy=true WHERE provider_id=$1 AND bucket_start=$2`, f.providerID, day)
		if err != nil || result.RowsAffected() != 1 {
			t.Fatalf("mark fixture period legacy: %v, %v", result, err)
		}
	}
	check := func() {
		t.Helper()
		rows, err := reports.ReconciliationSpend(ctx, pool, day, day.Add(24*time.Hour), []string{f.providerID})
		if err != nil || len(rows) != 1 {
			t.Fatalf("reconciliation returned %+v, %v", rows, err)
		}
		row := rows[0]
		if strings.TrimRight(row.AttributedCost, "0") != operatorSpend || row.PricedAttempts != 2 || row.ExcludedCallerAttempts != 2 {
			t.Fatalf("reconciliation returned %+v, want gateway spend %s and two excluded caller attempts", row, operatorSpend)
		}
		totals, err := reports.ReadSummary(ctx, pool, reports.Filters{Start: day, End: day.Add(24 * time.Hour),
			ProviderID: &f.providerID, Projects: reports.AllProjects()}, day.Add(24*time.Hour))
		if err != nil || strings.TrimRight(strings.TrimRight(totals.AllocatedCommitment, "0"), ".") != "5" {
			t.Fatalf("usage summary commitment = %q, want 5, error = %v", totals.AllocatedCommitment, err)
		}
	}
	check()
	if result, err := retention.RunMaintenance(ctx, pool, day.AddDate(0, 0, 91)); err != nil || result.UsageRows != 4 {
		t.Fatalf("rollup returned %+v, %v", result, err)
	}
	check()
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
