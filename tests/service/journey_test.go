//go:build service

package service_test

import (
	"context"
	"crypto/sha256"
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
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedworker"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
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

// TestPublishedRouteServesAndSettlesUsage takes one request from publication
// through the gateway to durable usage, across the Valkey stream and the
// consumer that writes PostgreSQL.
func TestPublishedRouteServesAndSettlesUsage(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "journey"}, idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "journey-provider", newVendor(t).URL, project)
	slug := uniqueName("journey")
	s.publishedRoute(owner, slug, provider, project)
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "journey", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "journey key", token: key["secret"].(string)}
	request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}

	s.eventually("the published route serving a request", 20*time.Second, func() (bool, string) {
		status, raw := s.do(consumer, "POST", "/v1/chat/completions", request, nil)
		if status != http.StatusOK {
			return false, describe(status, raw)
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &completion); err != nil {
			t.Fatalf("invalid completion: %v: %s", err, raw)
		}
		if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, vendorAnswer) {
			t.Fatalf("unexpected completion: %s", raw)
		}
		return true, ""
	})

	s.eventually("usage reaching durable storage", 20*time.Second, func() (bool, string) {
		var attempts, input, output int64
		err := s.pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM olp.attempt_usage_facts`).Scan(&attempts, &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		return attempts == 1 && input == vendorInputTokens && output == vendorOutputTokens,
			fmt.Sprintf("attempts=%d input=%d output=%d", attempts, input, output)
	})

	t.Run("simulation_requires_all_predicates", func(t *testing.T) {
		classifier := &routing.ClassifierPredicate{Route: slug, Labels: []string{"complex"}, TimeoutMS: 500}
		plugin := &routing.PluginPredicate{Digest: strings.Repeat("a", 64)}
		draft := s.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
			"slug": uniqueName("simulation"), "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": "transformed"}, "project_id": project,
			"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
			"selectors": []routing.Selector{
				{ID: "both", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier, Plugin: plugin}},
				{ID: "plugin", Tags: []string{"selected"}, When: routing.Predicate{Plugin: plugin}},
				{ID: "classifier", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier}},
			},
		}, idempotent(), http.StatusCreated)
		result := s.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{
			"operation": "generation", "surface": "openai", "mode": "unary", "seed": "predicates",
			"classifier_labels": map[string]string{"both": "complex", "classifier": "complex"},
		}, nil, http.StatusOK)
		selectors, ok := result["selectors"].([]any)
		if !ok || len(selectors) != 3 {
			t.Fatalf("simulation selectors = %v, want both plugin predicates skipped before classifier", result["selectors"])
		}
		for i, id := range []string{"both", "plugin", "classifier"} {
			selector := selectors[i].(map[string]any)
			want := "plugin_not_simulated"
			if id == "classifier" {
				want = "matched"
			}
			if selector["id"] != id || selector["matched"] != (id == "classifier") || selector["outcome"] != want {
				t.Fatalf("selector %s = %v, want %s", id, selector, want)
			}
		}
	})
}

func TestFocusSettlesMultipleAttemptsAndOverlappingCommitmentDays(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
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
			result, err := usage.PersistEvent(t.Context(), pool, accepted)
			if err != nil || result.Outcome != usage.PersistOutcomePersisted {
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
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
		VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Usage fixture',$2,'{}',$1::uuid,$3)`,
		fixture.keyID, fixture.ownerID, fixture.projectID); err != nil {
		t.Fatal(err)
	}
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
		VALUES ($1,'openai_compatible',$2,$3,'generation',1,2,'USD')`, pricingID, fixture.providerID, vendorModel); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f usageFixture) attempt(ordinal int) usage.Attempt {
	status, input, output := http.StatusOK, int64(vendorInputTokens), int64(vendorOutputTokens)
	return usage.Attempt{
		ID: uuid.NewString(), Ordinal: ordinal, ProviderID: f.providerID, UpstreamModel: vendorModel,
		StartedAt: f.observedAt, CompletedAt: f.observedAt, StatusCode: &status, Committed: true,
		Usage: &usage.AttemptUsage{Observed: true, Complete: true, UsageQuantities: usage.UsageQuantities{InputTokens: &input, OutputTokens: &output}},
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
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
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
	configuration, err := coordination.Configuration(required(t, "OLP_TEST_VALKEY_URL"), "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.Open(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
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
		done <- usage.RunConsumer(consumerContext, pool, client, stream, "service-consumer", "service-region", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	s := &service{t: t, pool: pool}
	s.eventually("consumer reaching the blocked event", 5*time.Second, func() (bool, string) {
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
	s.eventually("both events acknowledged", 5*time.Second, func() (bool, string) {
		length, err := client.Do(t.Context(), "XLEN", stream)
		return err == nil && length == int64(0), fmt.Sprintf("length=%v error=%v", length, err)
	})
	if _, err := client.Do(t.Context(), "XADD", stream, "*", "event", string(firstPayload)); err != nil {
		t.Fatal(err)
	}
	s.eventually("replayed event acknowledged", 5*time.Second, func() (bool, string) {
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
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	fixture := newUsageFixture(t, pool)
	t.Run("anchor retention", func(t *testing.T) { testAnchorRetention(t, fixture) })
	t.Run("identity expiry", func(t *testing.T) { testIdentityExpiry(t, fixture) })
	t.Run("workflow recovery", func(t *testing.T) { testWorkflowRecovery(t, fixture) })
	t.Run("retained settlement failure", func(t *testing.T) { testRetainedSettlementPollFailure(t, fixture) })
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
			report, err := usage.RunMaintenance(ctx, fixture.pool, now)
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
	ledger := accounting.NewDurableLedger(fixture.pool, &limits.DebitStore{Pool: fixture.pool}, nil)
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
	store := &limits.DebitStore{Pool: fixture.pool}
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
	if expiredState != string(limits.DebitUnknown) || liveState != string(limits.DebitDispatched) {
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
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	fixture := newUsageFixture(t, pool)
	selected, earlier, later := fixture.attempt(1), fixture.attempt(2), fixture.attempt(3)
	ineligible, legacy, absent := fixture.attempt(4), fixture.attempt(5), fixture.attempt(6)
	event := fixture.event(selected, earlier, later, ineligible, legacy, absent)
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	result, err := usage.PersistEvent(t.Context(), pool, accepted)
	if err != nil || result.Outcome != usage.PersistOutcomePersisted {
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
	filters := usage.Filters{
		Start: fixture.observedAt.Add(-time.Minute), End: fixture.observedAt.Add(time.Minute),
		ProviderID: &fixture.providerID, Route: &route, AllProjects: true,
	}
	now := time.Now()
	summary, err := usage.ReadSummary(t.Context(), pool, filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AllocatedCommitment != "2.333333333334" || summary.RequestCount != 1 {
		t.Fatalf("selected request allocation changed: %+v", summary)
	}
	if len(summary.ContainerSessions) != 2 || summary.ContainerSessions["small"] != "5" || summary.ContainerSessions["large"] != "1" {
		t.Fatalf("container totals changed: %v", summary.ContainerSessions)
	}
	full, err := usage.ReadSummary(t.Context(), pool, usage.Filters{
		Start: filters.Start, End: filters.End, ProviderID: filters.ProviderID, AllProjects: true,
	}, now)
	if err != nil || full.AllocatedCommitment != "3.000000000000" {
		t.Fatalf("whole period lost its residual: allocation=%s error=%v", full.AllocatedCommitment, err)
	}
	completeness, err := usage.ReadCompleteness(t.Context(), pool, filters, now)
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
		hour, fixture.providerID, vendorModel, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, time.Minute} {
		retainedFilters := usage.Filters{Start: hour.Add(offset), End: hour.Add(time.Hour), ProviderID: &fixture.providerID, AllProjects: true}
		retainedSummary, err := usage.ReadSummary(t.Context(), pool, retainedFilters, now)
		if err != nil {
			t.Fatal(err)
		}
		retainedCompleteness, err := usage.ReadCompleteness(t.Context(), pool, retainedFilters, now)
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
	invalidFilters := usage.Filters{Start: hour, End: hour.Add(time.Hour), ProviderID: &fixture.providerID, AllProjects: true}
	if _, err := usage.ReadSummary(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("summary accepted more unpriced requests than total requests")
	}
	if _, err := usage.ReadCompleteness(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("completeness accepted more unpriced requests than total requests")
	}
}

func TestBudgetThresholdNotificationsUseBudgetBasis(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
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
