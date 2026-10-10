//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type signalFixture struct {
	t      *testing.T
	h      *accessHarness
	owner  *browser
	posts  *channelPosts
	server *httptest.Server
}

func newSignalFixture(t *testing.T) *signalFixture {
	t.Helper()
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	posts := &channelPosts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		posts.add(r.URL.Path, buf)
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	f := &signalFixture{t: t, h: h, posts: posts, server: server}
	f.owner = h.owner()
	return f
}

func (f *signalFixture) destination(t *testing.T, project *string) string {
	t.Helper()
	body := map[string]any{"name": "signal-hook-" + uuid.NewString(), "url": f.server.URL + "/events"}
	if project != nil {
		body["project_id"] = *project
	}
	created := f.h.want(f.owner, "POST", "/api/v1/notifications/destinations", body,
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	return created["id"].(string)
}

func (f *signalFixture) rule(t *testing.T, event, destination string, configuration map[string]any, project *string) string {
	t.Helper()
	body := map[string]any{"name": "signal-" + event + "-" + uuid.NewString()[:8], "event": event, "destination_id": destination}
	if configuration != nil {
		body["configuration"] = configuration
	}
	if project != nil {
		body["project_id"] = *project
	}
	created := f.h.want(f.owner, "POST", "/api/v1/notifications/rules", body,
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	return created["id"].(string)
}

func (f *signalFixture) pass(t *testing.T, dependencies usage.NotificationDependencies) {
	t.Helper()
	deliveryPassWith(t, f.h, alertPolicy(), dependencies)
}

func (f *signalFixture) received(t *testing.T) []map[string]any {
	t.Helper()
	bodies, _ := f.posts.snapshot()
	var events []map[string]any
	for _, raw := range bodies {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("delivered body %s", raw)
		}
		events = append(events, decoded)
	}
	return events
}

func TestSignalProducerWorkerStaleLifecycle(t *testing.T) {
	f := newSignalFixture(t)
	ruleID := f.rule(t, "worker.stale", f.destination(t, nil), nil, nil)
	f.pass(t, usage.NotificationDependencies{})
	if events := f.received(t); len(events) != 0 {
		t.Fatalf("premature deliveries %v", events)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.Pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	stale := func() {
		exec(`INSERT INTO olp.worker_task_health(task,checked_at,last_success_at,first_seen_at,successes_total)
			 VALUES('media_reconciliation',now()-interval '10 minutes',NULL,now()-interval '10 minutes',0)
			 ON CONFLICT (task) DO UPDATE SET checked_at=now()-interval '10 minutes',last_success_at=NULL,first_seen_at=now()-interval '10 minutes'`)
	}
	gone := func() {
		exec(`DELETE FROM olp.worker_task_health WHERE task='media_reconciliation'`)
	}
	fresh := func() {
		exec(`INSERT INTO olp.worker_task_health(task,checked_at,last_success_at,first_seen_at,successes_total)
			 VALUES('media_reconciliation',now(),now(),now()-interval '10 minutes',1)
			 ON CONFLICT (task) DO UPDATE SET last_success_at=now(),checked_at=now()`)
	}
	stale()
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "worker.stale" || events[0]["resolved"] != false {
		t.Fatalf("stale trigger %v", events)
	}
	triggerKey, _ := events[0]["incident_key"].(string)
	if triggerKey == "" {
		t.Fatal("no incident_key")
	}
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("duplicate trigger %v", events)
	}
	gone()
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("missing source produced a false recovery: %v", events)
	}
	if activeRow := func() bool {
		var active bool
		if err := f.h.Pool.QueryRow(context.Background(),
			`SELECT active FROM olp.notification_signal_states WHERE rule_id=$1 AND subject='media_reconciliation'`, ruleID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		return active
	}(); !activeRow {
		t.Fatal("missing worker source resolved the incident")
	}
	fresh()
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != triggerKey {
		t.Fatalf("recovery %v", events[1:])
	}
	var stateIncident int64
	var stateActive bool
	if err := f.h.Pool.QueryRow(context.Background(),
		`SELECT incident,active FROM olp.notification_signal_states WHERE rule_id=$1 AND subject='media_reconciliation'`, ruleID).Scan(&stateIncident, &stateActive); err != nil {
		t.Fatal(err)
	}
	if stateActive {
		t.Fatal("stale signal still active after recovery")
	}
	stale()
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 2 {
		t.Fatalf("cooldown should suppress an immediate restale: %v", events)
	}
}

func TestSignalProducerRegionalWorkerStaleLifecycle(t *testing.T) {
	f := newSignalFixture(t)
	f.rule(t, "worker.stale", f.destination(t, nil), nil, nil)
	// An old unscoped checkpoint remains after this fleet adopts regions.
	if _, err := f.h.Pool.Exec(t.Context(), `INSERT INTO olp.worker_task_health(task,checked_at,first_seen_at)
	 VALUES('health_probes',now()-interval '1 hour',now()-interval '1 hour');
	 INSERT INTO olp.regional_worker_task_health(region,task,checked_at,last_success_at,first_seen_at)
	 VALUES('east','health_probes',now(),now(),now()-interval '10 minutes'),
	       ('west','health_probes',now()-interval '10 minutes',NULL,now()-interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["subject"] != "region:west:health_probes" || events[0]["resolved"] != false {
		t.Fatalf("regional stale alerts did not isolate the failed region: %v", events)
	}
	if events[0]["evidence"].(map[string]any)["region"] != "west" {
		t.Fatal("worker alert omitted its region")
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.regional_worker_task_health SET checked_at=now(),last_success_at=now() WHERE region='west'`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != events[0]["incident_key"] {
		t.Fatalf("regional worker did not recover its own incident: %v", events)
	}
	// A still-running unscoped fleet keeps its own independently stale signal.
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.worker_task_health SET checked_at=now()-interval '5 minutes' WHERE task='health_probes'`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 3 || events[2]["subject"] != "health_probes" || events[2]["resolved"] != false {
		t.Fatalf("regional checkpoints hid a current unscoped fleet: %v", events)
	}
}

func TestSignalProducerErrorRateAndCooldown(t *testing.T) {
	f := newSignalFixture(t)
	f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "signal key", "scopes": []string{"inference"}},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	destination := f.destination(t, nil)
	ruleID := f.rule(t, "provider.error_rate", destination,
		map[string]any{"threshold": "0.5", "minimum_samples": 3, "window_seconds": 300, "recovery_threshold": "0.2", "cooldown_seconds": 120}, nil)
	recent := f.h.Pool
	var providerID string
	if err := recent.QueryRow(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES($1,'signal-provider','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1))
		 RETURNING id::text`,
		uuid.New(), uuid.New(), uuid.New()).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	seedAttempts := func(statuses []int) {
		for i, status := range statuses {
			started := time.Now().Add(-time.Duration(i) * time.Second)
			requestID := uuid.NewString()
			if _, err := recent.Exec(context.Background(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count)
				VALUES($1,$2,(SELECT id FROM olp.api_keys LIMIT 1),'sig-route','chat','openai','caller',$3,$4,200,1)`,
				requestID, uuid.New(), started, started.Add(100*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, err := recent.Exec(context.Background(), `INSERT INTO olp.attempts(id,request_id,request_started_at,ordinal,provider_id,upstream_model,started_at,completed_at,status_code,committed)
				VALUES($1,$2,$3,1,$4,'m',$3,$5,$6,true)`,
				uuid.New(), requestID, started, providerID, started.Add(100*time.Millisecond), status); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedAttempts([]int{500, 200})
	f.pass(t, usage.NotificationDependencies{})
	if events := f.received(t); len(events) != 0 {
		t.Fatalf("insufficient samples still delivered %v", events)
	}
	seedAttempts([]int{500})
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "provider.error_rate" {
		t.Fatalf("error-rate trigger %v", events)
	}
	triggerKey := events[0]["incident_key"]
	seedAttempts([]int{200, 200, 200, 200, 200, 200, 200, 200, 200, 200})
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != triggerKey {
		t.Fatalf("error-rate recovery %v", events)
	}
	var active bool
	if err := f.h.Pool.QueryRow(context.Background(),
		`SELECT active FROM olp.notification_signal_states WHERE rule_id=$1`, ruleID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("error-rate signal still active")
	}
	seedAttempts([]int{500, 500, 500})
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 2 {
		t.Fatalf("cooldown should suppress an immediate refailure: %v", events)
	}
}

func TestSignalProducerRuntimeInstallAndUnknownPreserved(t *testing.T) {
	f := newSignalFixture(t)
	f.rule(t, "runtime.install_failed", f.destination(t, nil), nil, nil)
	f.h.Pool.Exec(context.Background(),
		`INSERT INTO olp.runtime_install_status(gateway_instance,desired_generation,installed_generation,failed,checked_at)
		 VALUES('gw-1',3,2,true,now())`)
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "runtime.install_failed" || events[0]["resolved"] != false {
		t.Fatalf("install-failed trigger %v", events)
	}
	triggerKey, _ := events[0]["incident_key"].(string)
	ruleID := events[0]["rule_id"].(string)
	if _, err := f.h.Pool.Exec(context.Background(),
		`UPDATE olp.runtime_install_status SET checked_at=now()-interval '5 minutes' WHERE gateway_instance='gw-1'`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("stale runtime source produced a false recovery: %v", events)
	}
	var stillActive bool
	if err := f.h.Pool.QueryRow(context.Background(),
		`SELECT active FROM olp.notification_signal_states WHERE rule_id=$1 AND subject='gw-1'`, ruleID).Scan(&stillActive); err != nil {
		t.Fatal(err)
	}
	if !stillActive {
		t.Fatal("stale runtime row resolved the incident")
	}
	f.h.Pool.Exec(context.Background(),
		`UPDATE olp.runtime_install_status SET installed_generation=3,failed=false,checked_at=now() WHERE gateway_instance='gw-1'`)
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != triggerKey {
		t.Fatalf("install recovery %v", events)
	}
	f.h.Pool.Exec(context.Background(),
		`DELETE FROM olp.runtime_install_status`)
	f.pass(t, usage.NotificationDependencies{})
	if events := f.received(t); len(events) != 2 {
		t.Fatalf("disappearing measurement produced %v", events)
	}
}

func TestSignalProducerCircuitWithRealLimiter(t *testing.T) {
	valkey := acctValkey(t)
	limiter, err := limits.New(valkey, "test:notif:"+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	f := newSignalFixture(t)
	destination := f.destination(t, nil)
	f.rule(t, "provider.circuit.open", destination, map[string]any{"cooldown_seconds": 60}, nil)
	f.h.Pool.Exec(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES($1,'circuit-provider','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1))`,
		uuid.New(), uuid.New(), uuid.New())
	var providerID string
	if err := f.h.Pool.QueryRow(context.Background(), "SELECT id::text FROM olp.providers WHERE name='circuit-provider'").Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	if events := f.received(t); len(events) != 0 {
		t.Fatalf("closed circuit triggered %v", events)
	}
	if err := limiter.PublishCircuit(context.Background(), providerID, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{Limiter: limiter})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "provider.circuit.open" || events[0]["resolved"] != false {
		t.Fatalf("circuit open trigger %v", events)
	}
	triggerKey, _ := events[0]["incident_key"].(string)
	if err := limiter.PublishCircuit(context.Background(), providerID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{Limiter: limiter})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != triggerKey {
		t.Fatalf("circuit closed recovery %v", events)
	}
	if events[1]["event"] != "provider.circuit.closed" {
		t.Fatalf("recovery emitted as %v", events[1]["event"])
	}
}

func TestSignalProducerRouteLatencyAndForeignProject(t *testing.T) {
	f := newSignalFixture(t)
	projectA := f.h.want(f.owner, "POST", "/api/v1/projects", map[string]any{"name": "Latency A"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectB := f.h.want(f.owner, "POST", "/api/v1/projects", map[string]any{"name": "Latency B"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectAID := projectA["id"].(string)
	projectBID := projectB["id"].(string)
	f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "lat key a", "scopes": []string{"inference"}, "project_id": projectAID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "lat key b", "scopes": []string{"inference"}, "project_id": projectBID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	destA := f.destination(t, &projectAID)
	f.rule(t, "route.latency", destA,
		map[string]any{"metric": "ttft", "threshold": "500", "minimum_samples": 2, "window_seconds": 300, "recovery_threshold": "400", "cooldown_seconds": 60}, &projectAID)

	var routeA, routeB string
	f.h.Pool.QueryRow(context.Background(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
		VALUES(gen_random_uuid(),'sig-latency-a',(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid(),$1) RETURNING slug`, projectAID).Scan(&routeA)
	f.h.Pool.QueryRow(context.Background(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
		VALUES(gen_random_uuid(),'sig-latency-b',(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid(),$1) RETURNING slug`, projectBID).Scan(&routeB)
	var providerID string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES($1,'latency-provider','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1)) RETURNING id::text`,
		uuid.New(), uuid.New(), uuid.New()).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	seed := func(route, project string, latency int, count int) {
		for i := 0; i < count; i++ {
			started := time.Now().Add(-time.Duration(i) * time.Second)
			requestID := uuid.NewString()
			if _, err := f.h.Pool.Exec(context.Background(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count,total_latency_ms)
				VALUES($1,$2,(SELECT id FROM olp.api_keys k WHERE k.project_id=$3::uuid LIMIT 1),$4,'chat','openai','caller',$5,$6,200,1,$7)`,
				requestID, uuid.New(), project, route, started, started.Add(time.Duration(latency)*time.Millisecond), latency); err != nil {
				t.Fatal(err)
			}
			if _, err := f.h.Pool.Exec(context.Background(), `INSERT INTO olp.attempts(id,request_id,request_started_at,ordinal,provider_id,upstream_model,started_at,completed_at,status_code,committed,routing)
				VALUES($1,$2,$3,1,$4,'m',$3,$5,200,true,$6::jsonb)`,
				uuid.New(), requestID, started, providerID, started.Add(50*time.Millisecond),
				`{"first_output_ms": `+fmt.Sprint(latency)+`}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(routeA, projectAID, 800, 3)
	seed(routeB, projectBID, 900, 3)
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "route.latency" {
		t.Fatalf("latency trigger %v", events)
	}
	evidence := events[0]["evidence"].(map[string]any)
	if evidence["route_slug"] != routeA || evidence["project_id"] != projectAID {
		t.Fatalf("latency evidence %v", evidence)
	}
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("duplicate latency trigger %v", events)
	}
	if _, err := f.h.Pool.Exec(context.Background(), `DELETE FROM olp.requests WHERE route_slug IN ('sig-latency-a','sig-latency-b')`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("insufficient samples produced a false resolve: %v", events)
	}
	seed(routeA, projectAID, 50, 3)
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != events[0]["incident_key"] {
		t.Fatalf("latency recovery %v", events)
	}
}

func TestSignalProducerModelRetirement(t *testing.T) {
	f := newSignalFixture(t)
	reference, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	dest := f.destination(t, nil)
	f.rule(t, "model.retirement", dest, map[string]any{"lead_days": 14, "cooldown_seconds": 60}, nil)
	var providerID string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,active_revision_id,created_by)
		 VALUES($1,'retire-provider','openai','active','{}'::jsonb,$2,$3,$4,(SELECT id FROM olp.users LIMIT 1))
		 RETURNING id::text`, uuid.New(), uuid.New(), uuid.New(), uuid.New()).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	var revisionID string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
		 VALUES($1,$2,1,'r1','{"vendor_id":"amazon-bedrock"}'::json,'[]'::jsonb,'[]'::jsonb,$3,(SELECT id FROM olp.users LIMIT 1))
		 RETURNING id::text`, uuid.New(), providerID, uuid.New()).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	f.h.Pool.Exec(context.Background(), "UPDATE olp.providers SET active_revision_id=$1 WHERE id=$2", revisionID, providerID)
	var routeID string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag)
		 VALUES(gen_random_uuid(),'sig-retire',(SELECT id FROM olp.users LIMIT 1),1,gen_random_uuid(),gen_random_uuid()) RETURNING id::text`).Scan(&routeID); err != nil {
		t.Fatal(err)
	}
	var routeRev string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity)
		 VALUES($1,$2,1,'sig-retire','["chat"]'::jsonb,60000,1,
		 $3::jsonb,$4,(SELECT id FROM olp.users LIMIT 1),'{}'::jsonb,'{"mode":"strict"}'::jsonb) RETURNING id::text`,
		uuid.New(), routeID,
		`[{"provider_id":"`+providerID+`","provider_model":"anthropic.claude-sonnet-4-20250514-v1:0","weight":1}]`,
		uuid.New()).Scan(&routeRev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(context.Background(), "UPDATE olp.routes SET latest_revision_id=$1 WHERE id=$2", routeRev, routeID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{Catalog: reference})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "model.retirement" {
		t.Fatalf("retirement trigger %v", events)
	}
	evidence := events[0]["evidence"].(map[string]any)
	if evidence["provider_model"] != "anthropic.claude-sonnet-4-20250514-v1:0" {
		t.Fatalf("retirement evidence %v", evidence)
	}
}

func TestSignalProducerSpendReportOncePerPeriod(t *testing.T) {
	f := newSignalFixture(t)
	dest := f.destination(t, nil)
	ruleID := f.rule(t, "report.spend", dest, map[string]any{"period": "daily"}, nil)
	if _, err := f.h.Pool.Exec(context.Background(),
		`UPDATE olp.notification_rules SET created_at=now()-interval '3 days' WHERE id=$1`, ruleID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "report.spend" {
		t.Fatalf("spend report deliveries %v", events)
	}
	key, _ := events[0]["dedup_key"].(string)
	if !strings.HasPrefix(key, "spend:daily:") {
		t.Fatalf("dedup key %q", key)
	}
}

func TestSignalProducerTwoReplicasOneTrigger(t *testing.T) {
	f := newSignalFixture(t)
	f.rule(t, "worker.stale", f.destination(t, nil), nil, nil)
	if _, err := f.h.Pool.Exec(context.Background(),
		`INSERT INTO olp.worker_task_health(task,checked_at,last_success_at,first_seen_at,successes_total)
		 VALUES('media_reconciliation',now()-interval '10 minutes',NULL,now()-interval '10 minutes',0)`); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(f.h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usage.RunNotificationDelivery(ctx, f.h.Pool, ring, alertInstallation(t, f.h), alertPolicy(),
				slog.New(slog.NewTextHandler(io.Discard, nil)), usage.NotificationDependencies{})
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	var events []map[string]any
	for {
		events = f.received(t)
		if len(events) >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	events = f.received(t)
	count := 0
	for _, e := range events {
		if e["resolved"] == false {
			count++
		}
	}
	rows, err := f.h.Pool.Query(context.Background(),
		`SELECT status,attempts,event FROM olp.notification_deliveries ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var marks []string
	for rows.Next() {
		var status, event string
		var attempts int
		if err := rows.Scan(&status, &attempts, &event); err != nil {
			t.Fatal(err)
		}
		marks = append(marks, status+":"+fmt.Sprint(attempts)+":"+event)
	}
	health, _ := f.h.Pool.Query(context.Background(), `SELECT task,successes_total,failures_total,skipped_total FROM olp.worker_task_health`)
	var healthRows []string
	for health.Next() {
		var task string
		var s, f, k int64
		health.Scan(&task, &s, &f, &k)
		healthRows = append(healthRows, fmt.Sprintf("%s(s%d,f%d,k%d)", task, s, f, k))
	}
	health.Close()
	if count != 1 {
		t.Fatalf("two replicas delivered %d triggers; deliveries %v; health %v", count, marks, healthRows)
	}
}

func TestSignalProducerBudgetExhausted(t *testing.T) {
	f := newSignalFixture(t)
	dest := f.destination(t, nil)
	f.rule(t, "budget.exhausted", dest, map[string]any{"cooldown_seconds": 60}, nil)
	key := f.h.want(f.owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "spent key", "scopes": []string{"inference"}, "monthly_cost_limit": "5.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyID := key["id"].(string)
	var windowID int64
	if err := f.h.Pool.QueryRow(context.Background(), `SELECT b.window_id FROM olp.budget_window('month',now()) b`).Scan(&windowID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(context.Background(),
		`INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts)
		 VALUES($1,'month',$2,'6.5',0)`, keyID, windowID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "budget.exhausted" || events[0]["resolved"] != false {
		t.Fatalf("budget exhaust trigger %v", events)
	}
	evidence := events[0]["evidence"].(map[string]any)
	if evidence["subject_kind"] != "api_key" || evidence["window_kind"] != "month" {
		t.Fatalf("budget evidence %v", evidence)
	}
	if _, err := f.h.Pool.Exec(context.Background(), `DELETE FROM olp.api_key_cost_windows WHERE api_key_id=$1`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(context.Background(), `UPDATE olp.api_keys SET policy=policy - 'monthly_cost_limit' WHERE id=$1`, keyID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != events[0]["incident_key"] {
		t.Fatalf("budget recovery %v", events)
	}
}

func TestSignalProducerCredentialFailures(t *testing.T) {
	f := newSignalFixture(t)
	dest := f.destination(t, nil)
	ruleID := f.rule(t, "provider.credential.failing", dest,
		map[string]any{"threshold": "3", "minimum_samples": 3, "window_seconds": 300, "recovery_threshold": "0", "cooldown_seconds": 60}, nil)
	var providerID, keyID string
	f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES($1,'cred-provider','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1)) RETURNING id::text`,
		uuid.New(), uuid.New(), uuid.New()).Scan(&providerID)
	f.h.Pool.QueryRow(context.Background(), `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag)
		VALUES($1,$2,$3,'cred-key',(SELECT id FROM olp.users LIMIT 1),'{}'::jsonb,$4) RETURNING id::text`,
		uuid.New(), "credlk", make([]byte, 32), uuid.New()).Scan(&keyID)
	credID := uuid.NewString()
	seedFailed := func(count int) {
		for i := 0; i < count; i++ {
			started := time.Now().Add(-time.Duration(i) * time.Second)
			requestID := uuid.NewString()
			f.h.Pool.Exec(context.Background(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count)
				VALUES($1,$2,$3,'sig-cred','chat','openai','caller',$4,$5,200,1)`,
				requestID, uuid.New(), keyID, started, started.Add(50*time.Millisecond))
			f.h.Pool.Exec(context.Background(), `INSERT INTO olp.attempts(id,request_id,request_started_at,ordinal,provider_id,upstream_model,started_at,completed_at,status_code,committed,routing)
				VALUES($1,$2,$3,1,$4,'m',$3,$5,401,true,$6::jsonb)`,
				uuid.New(), requestID, started, providerID, started.Add(50*time.Millisecond),
				`{"credential_version_id":"`+credID+`"}`)
		}
	}
	seedFailed(2)
	f.pass(t, usage.NotificationDependencies{})
	if events := f.received(t); len(events) != 0 {
		t.Fatalf("insufficient credential failures triggered %v", events)
	}
	seedFailed(2)
	f.pass(t, usage.NotificationDependencies{})
	events := f.received(t)
	if len(events) != 1 || events[0]["event"] != "provider.credential.failing" || events[0]["resolved"] != false {
		t.Fatalf("credential trigger %v", events)
	}
	triggerKey, _ := events[0]["incident_key"].(string)
	if _, err := f.h.Pool.Exec(context.Background(),
		`UPDATE olp.attempts a SET completed_at=now()-interval '20 minutes' FROM olp.requests r WHERE a.request_id=r.id AND r.route_slug='sig-cred'`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	if events = f.received(t); len(events) != 1 {
		t.Fatalf("aged-out credential attempts produced a false recovery: %v", events)
	}
	var stillActive bool
	if err := f.h.Pool.QueryRow(context.Background(),
		`SELECT active FROM olp.notification_signal_states WHERE rule_id=$1 AND subject=$2`, ruleID, credID).Scan(&stillActive); err != nil {
		t.Fatal(err)
	}
	if !stillActive {
		t.Fatal("aged-out credential source resolved the incident")
	}
	if _, err := f.h.Pool.Exec(context.Background(),
		`UPDATE olp.attempts a SET completed_at=now(),status_code=200 FROM olp.requests r WHERE a.request_id=r.id AND r.route_slug='sig-cred'`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	f.h.Pool.Exec(context.Background(), `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,origin,started_at,completed_at,status_code,attempt_count)
		VALUES($1,$2,$3,'sig-cred','chat','openai','caller',$4,$5,200,1)`,
		uuid.New(), uuid.New(), keyID, started, started.Add(50*time.Millisecond))
	if _, err := f.h.Pool.Exec(context.Background(), `INSERT INTO olp.attempts(id,request_id,request_started_at,ordinal,provider_id,upstream_model,started_at,completed_at,status_code,committed,routing)
		VALUES($1,(SELECT id FROM olp.requests ORDER BY started_at DESC LIMIT 1),(SELECT started_at FROM olp.requests ORDER BY started_at DESC LIMIT 1),1,$2,'m',now(),now()+interval '10 ms',200,true,$3::jsonb)`,
		uuid.New(), providerID, `{"credential_version_id":"`+credID+`"}`); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	events = f.received(t)
	if len(events) != 2 || events[1]["resolved"] != true || events[1]["incident_key"] != triggerKey {
		t.Fatalf("credential recovery %v", events)
	}
}

func TestSignalProducerCircuitUnavailableNoResolve(t *testing.T) {
	f := newSignalFixture(t)
	ruleID := f.rule(t, "provider.circuit.open", f.destination(t, nil), map[string]any{"cooldown_seconds": 60}, nil)
	var providerID string
	if err := f.h.Pool.QueryRow(context.Background(),
		`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES($1,'offline-circuit','openai','active','{}'::jsonb,$2,$3,(SELECT id FROM olp.users LIMIT 1)) RETURNING id::text`,
		uuid.New(), uuid.New(), uuid.New()).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(context.Background(),
		`INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident,last_trigger_at,checked_at)
		 VALUES($1,$2,true,1,now()-interval '1 hour',now()-interval '1 hour')`, ruleID, providerID); err != nil {
		t.Fatal(err)
	}
	f.pass(t, usage.NotificationDependencies{})
	var active bool
	if err := f.h.Pool.QueryRow(context.Background(),
		`SELECT active FROM olp.notification_signal_states WHERE rule_id=$1 AND subject=$2`, ruleID, providerID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("unavailable circuit source falsely resolved the incident")
	}
	if events := f.received(t); len(events) != 0 {
		t.Fatalf("unavailable source emitted %v", events)
	}
}
