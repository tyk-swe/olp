//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/tests/harness"
)

func TestRouteLatencySilenceMutation(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	projectRule := addLatencyRule(t, f, "maintenance", &f.projectID, "webhook", "https://hooks.example.test")
	installationRule := addLatencyRule(t, f, "installation", nil, "webhook", "https://hooks.example.test")
	slackRule := addLatencyRule(t, f, "chat", &f.projectID, "slack", "https://hooks.example.test")
	otherProject := uuid.NewString()
	mustExec(t, pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,'other',$1,$2)`, otherProject, f.ownerID)
	manager := latencyManager(t, f, f.projectID)
	outsider := latencyManager(t, f, otherProject)
	owner := notificationManagement(t, pool, f)
	path := "/api/v1/notifications/rules/" + projectRule
	original := expectStatus(t, "", manager.call(t, "GET", path, "", ""), http.StatusOK)
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(time.Hour), now.Add(2*time.Hour)
	patch := windowPatch(start.In(time.FixedZone("offset", 3600)), end)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, patch, ""), http.StatusBadRequest)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, windowPatch(end, start)), http.StatusUnprocessableEntity)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, windowPatch(now.Add(-time.Hour), now)), http.StatusUnprocessableEntity)
	for _, id := range []string{installationRule, slackRule} {
		ineligible := "/api/v1/notifications/rules/" + id
		rule := expectStatus(t, "", owner.call(t, "GET", ineligible, "", ""), http.StatusOK)
		expectStatus(t, "", owner.call(t, "PATCH", ineligible, rule.etag, patch), http.StatusUnprocessableEntity)
	}
	const key = "project-latency-maintenance"
	set := expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, patch, key), http.StatusOK)
	replay := expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, patch, key), http.StatusOK)
	requireSilenceWindow(t, set, start, end)
	requireSilenceWindow(t, replay, start, end)
	if set.etag == original.etag || replay.etag != set.etag {
		t.Fatalf("ETags before set, after set, and after replay: %s, %s, %s", original.etag, set.etag, replay.etag)
	}
	expectStatus(t, "", outsider.call(t, "PATCH", path, set.etag, patch, key), http.StatusNotFound)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, `{"silence":null}`), http.StatusPreconditionFailed)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, windowPatch(start, end.Add(time.Hour)), key), http.StatusConflict)
	var auditRows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit
	 WHERE action='notification_rule.update' AND resource_id=$1 AND project_id=$2`, projectRule, f.projectID).Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("project-scoped update audit rows = %d, want 1 (%v)", auditRows, err)
	}
	const cancel = `{"silence":null}`
	canceled := expectStatus(t, "", manager.call(t, "PATCH", path, set.etag, cancel), http.StatusOK)
	active := expectStatus(t, "", manager.call(t, "PATCH", path, canceled.etag, windowPatch(now.Add(-time.Minute), end)), http.StatusOK)
	requireSilenceWindow(t, active, now.Add(-time.Minute), end)
	expectStatus(t, "", manager.call(t, "PATCH", path, active.etag, cancel), http.StatusOK)
	expectStatus(t, "", manager.call(t, "PATCH", path, original.etag, patch, key), http.StatusOK)
	current := expectStatus(t, "", manager.call(t, "GET", path, "", ""), http.StatusOK)
	var rule struct {
		Enabled bool            `json:"enabled"`
		Silence json.RawMessage `json:"silence"`
	}
	if err := json.Unmarshal(current.body, &rule); err != nil || !rule.Enabled || string(rule.Silence) != "null" {
		t.Fatalf("cancel followed by replay changed current rule: %s (%v)", current.body, err)
	}
}

func TestRouteLatencySilenceSuppressesOnlyNewAnnouncements(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	receiver, deliveries := notificationReceiver(t)
	ruleID := addLatencyRule(t, f, "silenced", &f.projectID, "webhook", receiver.URL)
	unrelated := addLatencyRule(t, f, "unrelated", &f.projectID, "webhook", receiver.URL)
	mustExec(t, pool, `UPDATE olp.notification_rules SET configuration='{"minimum_samples":1,"threshold":"8000"}' WHERE id=$1`, unrelated)
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(time.Minute), now.Add(30*time.Minute)
	mustExec(t, pool, `UPDATE olp.notification_rules SET silence_starts_at=$2,silence_ends_at=$3 WHERE id=$1`, ruleID, start, end)

	sample := func(slug string, value *int) {
		t.Helper()
		id := uuid.NewString()
		mustExec(t, pool, `INSERT INTO olp.routes(id,slug,project_id,created_by,latest_revision,latest_revision_id,etag)
		 VALUES($1,$2,$3,$4,1,$1,$1) ON CONFLICT(slug) DO NOTHING`, id, slug, f.projectID, f.ownerID)
		mustExec(t, pool, `UPDATE olp.requests SET completed_at=now()-interval '1 day' WHERE route_slug=$1`, slug)
		mustExec(t, pool, `INSERT INTO olp.requests(id,runtime_generation_id,api_key_id,route_slug,operation,surface,started_at,completed_at,total_latency_ms)
		 VALUES($1,$2,$3,$4,'generation','openai',now(),now(),$5)`, id, f.generationID, f.keyID, slug, value)
	}
	pass := func(at time.Time) {
		t.Helper()
		deliverNotificationsAt(t, pool, nil, func() time.Time { return at })
	}
	only := func(rule, subject string, resolved bool) notificationBody {
		t.Helper()
		if len(deliveries) != 1 {
			t.Fatalf("calls = %d, want one delivery for %s/%s (resolved=%t)", len(deliveries), rule, subject, resolved)
		}
		delivery := <-deliveries
		if delivery.RuleName != rule || delivery.Subject != subject || delivery.Resolved != resolved || delivery.IncidentKey == "" {
			t.Fatalf("unexpected delivery: %+v", delivery)
		}
		return delivery
	}
	slow, fast, verySlow := 2000, 300, 8500
	sample("announced", &slow)
	pass(start.Add(-time.Nanosecond))
	announced := only("silenced", "announced", false)

	sample("brief", &slow)
	sample("ongoing", &verySlow)
	pass(start)
	only("unrelated", "ongoing", false)

	sample("brief", &fast)
	sample("announced", &fast)
	pass(start.Add(time.Minute))
	if recovery := only("silenced", "announced", true); recovery.IncidentKey != announced.IncidentKey {
		t.Fatalf("recovery moved to a different incident: %+v, trigger %+v", recovery, announced)
	}

	sample("ongoing", nil)
	pass(end)
	if len(deliveries) != 0 {
		t.Fatal("missing latency data emitted a delivery at expiry")
	}
	sample("ongoing", &verySlow)
	pass(end)
	only("silenced", "ongoing", false)
	pass(end)
	if len(deliveries) != 0 {
		t.Fatal("repeated claims emitted duplicate deliveries")
	}
}

func addLatencyRule(t *testing.T, f usageFixture, name string, project *string, channel, url string) string {
	t.Helper()
	destination, rule := uuid.NewString(), uuid.NewString()
	mustExec(t, f.pool, `INSERT INTO olp.notification_destinations(id,name,url,project_id,type,etag,created_by)
	 VALUES($1,$2,$3,$4,$5,$1,$6)`, destination, name, url, project, channel, f.ownerID)
	mustExec(t, f.pool, `INSERT INTO olp.notification_rules(id,name,event,destination_id,project_id,etag,created_by,configuration)
	 VALUES($1,$2,'route.latency',$3,$4,$1,$5,'{"minimum_samples":1}')`, rule, name, destination, project, f.ownerID)
	return rule
}

func latencyManager(t *testing.T, f usageFixture, project string) *managementClient {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, f.pool, `INSERT INTO olp.users(id,email,display_name,role,access_scope,etag)
	 VALUES($1::uuid,$1::text||'@example.test','Project manager','developer','assigned',$1::uuid)`, id)
	mustExec(t, f.pool, `INSERT INTO olp.project_members(project_id,user_id,role,added_by) VALUES($1,$2,'manager',$3)`, project, id, f.ownerID)
	return notificationManagementAs(t, f.pool, id)
}

func windowPatch(start, end time.Time) string {
	return fmt.Sprintf(`{"silence":{"starts_at":%q,"ends_at":%q}}`, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
}

func requireSilenceWindow(t *testing.T, result apiResult, start, end time.Time) {
	t.Helper()
	var rule struct {
		Enabled bool `json:"enabled"`
		Silence struct {
			Start string `json:"starts_at"`
			End   string `json:"ends_at"`
		} `json:"silence"`
	}
	const utcTimestamp = "2006-01-02T15:04:05.000000Z"
	if err := json.Unmarshal(result.body, &rule); err != nil || !rule.Enabled ||
		rule.Silence.Start != start.UTC().Format(utcTimestamp) || rule.Silence.End != end.UTC().Format(utcTimestamp) {
		t.Fatalf("expected enabled rule with silence from %s to %s, got %s (%v)", start, end, result.body, err)
	}
}
