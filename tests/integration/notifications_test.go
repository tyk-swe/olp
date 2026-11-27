//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

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
		name, rule, window string
		amount, limit      int
	}
	var expected []expectation
	for _, kind := range []string{"api_key", "budget_group"} {
		for _, budget := range []struct {
			basis                            string
			cost, charge, increase, expected int
		}{{"", 80, 40, 0, 80}, {"cost", 80, 40, 5, 80}, {"cost", 80, 40, 20, 0}, {"charge", 40, 80, 0, 80}, {"charge", 80, 40, 0, 0}} {
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
     SELECT $1::uuid,$2::text,current.window_id,$3::numeric,$4::numeric,0,true FROM olp.budget_window($2,now()) current
     UNION ALL SELECT $1::uuid,$2::text,prior.window_id,95,95,0,true FROM olp.budget_window($2,now()-$5::interval) prior`,
					subject, window, budget.cost, budget.charge, "1 "+window); err != nil {
					t.Fatal(err)
				}
				if budget.increase > 0 {
					increase := uuid.NewString()
					if _, err := pool.Exec(t.Context(), `INSERT INTO olp.budget_increases(id,owner_id,project_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag)
     SELECT $1::uuid,$2::uuid,$3,'{}'::jsonb,$4::text,current.window_id,$5,'budget basis',now()-interval '1 minute',current.end_at,current.end_at,$6,$1::uuid
     FROM olp.budget_window($4,now()) current`, increase, subject, fixture.projectID, window, budget.increase, fixture.ownerID); err != nil {
						t.Fatal(err)
					}
				}
				rule := uuid.NewString()
				if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by)
     VALUES($1,'budget basis',$2,'budget.threshold',$3,$4,$5,75,$6,$1,$7)`, rule, fixture.projectID, kind, subject, window, destination, fixture.ownerID); err != nil {
					t.Fatal(err)
				}
				expected = append(expected, expectation{name: fmt.Sprintf("%s/%s/%s/cost%d/charge%d/increase%d", kind, basis, window, budget.cost, budget.charge, budget.increase),
					rule: rule, window: window, amount: budget.expected, limit: 100 + budget.increase})
			}
		}
	}
	deliverNotifications(t, pool, nil)
	for _, want := range expected {
		var amount, deliveries int
		var delivered bool
		if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(d.accrued),0)::int,count(*)::int,
 COALESCE(bool_and(d.status='delivered' AND d.limit_amount=$3 AND d.window_id=(SELECT window_id FROM olp.budget_window($2,now()))),false)
 FROM olp.notification_deliveries d WHERE d.rule_id=$1`, want.rule, want.window, want.limit).Scan(&amount, &deliveries, &delivered); err != nil {
			t.Fatal(err)
		}
		if amount != want.amount || deliveries != min(want.amount, 1) || delivered != (want.amount != 0) {
			t.Errorf("%s amount=%d deliveries=%d delivered in current window with limit %d=%t, want amount=%d", want.name, amount, deliveries, want.limit, delivered, want.amount)
		}
	}
}

func TestSpendSpikeNotifications(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	receiver, bodies := notificationReceiver(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_consumer_health(singleton,pending_events,lag_events,checked_at)
 VALUES(true,0,0,now()) ON CONFLICT(singleton) DO UPDATE SET pending_events=0,lag_events=0,checked_at=now()`); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	seed := func(keyID string, day time.Time, cost string, unpriced int) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_hourly
		(bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
		request_count,provider_request_count,model_request_count,target_request_count,
		input_tokens,output_tokens,cached_input_tokens,media_units,estimated_cost,currency,
		request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
		request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count)
		VALUES($1,'spike',$2,$3,'generation','openai',$4,2,2,2,2,0,0,0,0,$5::numeric,'USD',$6,$6,$6,$6,0,0,0,0)`,
			day.Add(time.Hour), fixture.providerID, vendor.Model, keyID, cost, unpriced); err != nil {
			t.Fatal(err)
		}
	}
	history := func(keyID, baselineCost string, baselineDays int, yesterdayCost string, yesterdayUnpriced int) {
		for i := 1; i <= baselineDays; i++ {
			seed(keyID, yesterday.AddDate(0, 0, -i), baselineCost, 0)
		}
		seed(keyID, yesterday, yesterdayCost, yesterdayUnpriced)
	}
	spikeRule := func(projectID string) string {
		t.Helper()
		destination, rule := uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by)
 VALUES($1::uuid,$1::text,$2,$3,$1::uuid,$4)`, destination, receiver.URL, projectID, fixture.ownerID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,destination_id,etag,created_by,created_at)
 VALUES($1::uuid,$1::text,$2,'spend.spike',$3,$1::uuid,$4,$5)`, rule, projectID, destination, fixture.ownerID, today.AddDate(0, 0, -60)); err != nil {
			t.Fatal(err)
		}
		return rule
	}
	extraProject := func() (string, string) {
		t.Helper()
		projectID, keyID := uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1::uuid,$1::text,$1::uuid,$2)`,
			projectID, fixture.ownerID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id)
 VALUES($1::uuid,$1::text,decode(repeat('00',32),'hex'),'spike',$2,'{}',$1::uuid,$3)`, keyID, fixture.ownerID, projectID); err != nil {
			t.Fatal(err)
		}
		return projectID, keyID
	}

	fired := spikeRule(fixture.projectID)
	sameDay := spikeRule(fixture.projectID)
	history(fixture.keyID, "20", 7, "150", 0)
	gapDay := yesterday.AddDate(0, 0, -8)
	seed(fixture.keyID, gapDay, "2000", 0)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_ingestion_gaps(id,gateway_instance,event_count,reason,first_observed_at,last_observed_at)
 VALUES($1,'spike',1,'spike',$2,$2)`, uuid.NewString(), gapDay.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	quietProject, quietKey := extraProject()
	quiet := spikeRule(quietProject)
	history(quietKey, "20", 7, "22", 0)

	shortProject, shortKey := extraProject()
	shortHistory := spikeRule(shortProject)
	history(shortKey, "20", 3, "150", 0)

	unpricedProject, unpricedKey := extraProject()
	unpriced := spikeRule(unpricedProject)
	history(unpricedKey, "20", 7, "150", 1)

	smallProject, smallKey := extraProject()
	smallChange := spikeRule(smallProject)
	history(smallKey, "2", 7, "4", 0)

	deliverNotifications(t, pool, nil)
	deliverNotifications(t, pool, nil)

	for rule, want := range map[string]int{fired: 1, sameDay: 1, quiet: 0, shortHistory: 0, unpriced: 0, smallChange: 0} {
		var deliveries int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.notification_deliveries WHERE rule_id=$1`, rule).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != want {
			t.Errorf("rule %s queued %d deliveries, want %d", rule, deliveries, want)
		}
	}
	var status, usagePath string
	var daysUsed int
	var amountsMatch bool
	if err := pool.QueryRow(t.Context(), `SELECT status,payload->>'usage_path',(payload->>'baseline_days_used')::int,
 (payload->>'actual_spend')::numeric=150 AND (payload->>'baseline_spend')::numeric=20 AND (payload->>'increase_percent')::numeric=650
 FROM olp.notification_deliveries WHERE rule_id=$1`, fired).Scan(&status, &usagePath, &daysUsed, &amountsMatch); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || !amountsMatch || daysUsed != 7 {
		t.Errorf("spike delivery status=%s amounts match=%t days=%d, want delivered 150 over 20 (650%%, 7 days)",
			status, amountsMatch, daysUsed)
	}
	if !strings.Contains(usagePath, "project_id="+fixture.projectID) || !strings.Contains(usagePath, yesterday.Format("2006-01-02")) {
		t.Errorf("usage path %q does not link the fired project and day", usagePath)
	}
	var dedupKeys int
	if err := pool.QueryRow(t.Context(), `SELECT count(DISTINCT dedup_key) FROM olp.notification_deliveries WHERE rule_id=ANY($1::uuid[])`,
		[]string{fired, sameDay}).Scan(&dedupKeys); err != nil {
		t.Fatal(err)
	}
	if dedupKeys != 2 {
		t.Errorf("two rules alerting on one project and day share a deduplication key, want one key per rule")
	}
	if got, want := received(bodies), []string{"spend.spike resolved=false", "spend.spike resolved=false"}; !slices.Equal(got, want) {
		t.Errorf("receiver got %v, want %v", got, want)
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
		harness.Eventually(t, "reminder pass", 5*time.Second, func() (bool, string) {
			var successes int64
			if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='notification_delivery'),0)`).Scan(&successes); err != nil {
				t.Fatal(err)
			}
			return successes > before, fmt.Sprintf("successes=%d before=%d", successes, before)
		})
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

func deliverNotifications(t *testing.T, pool *pgxpool.Pool, circuits *fleet.State) {
	t.Helper()
	deliverNotificationsAt(t, pool, circuits, time.Now)
}

func deliverNotificationsAt(t *testing.T, pool *pgxpool.Pool, circuits *fleet.State, now func() time.Time) {
	t.Helper()
	const progressSQL = `SELECT COALESCE(max(successes_total),0),COALESCE(max(failures_total),0) FROM olp.worker_task_health WHERE task='notification_delivery'`
	var before, failures int64
	if err := pool.QueryRow(t.Context(), progressSQL).Scan(&before, &failures); err != nil {
		t.Fatal(err)
	}
	keys, installation := notificationKeys(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		notifications.RunDelivery(ctx, notifications.DeliveryConfig{Pool: pool, NewID: uuid.NewString, Now: now, Fleet: circuits, Keys: keys, Installation: installation,
			Log:    slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
			Sender: notifications.NewSender(egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}})})
	}()
	defer func() { cancel(); <-done }()
	harness.Eventually(t, "notification worker pass", 5*time.Second, func() (bool, string) {
		var successes, failed int64
		if err := pool.QueryRow(t.Context(), progressSQL).Scan(&successes, &failed); err != nil {
			t.Fatal(err)
		}
		if failed > failures {
			t.Fatal("notification worker pass failed")
		}
		return successes > before, fmt.Sprintf("successes=%d before=%d", successes, before)
	})
}

type notificationBody struct {
	Event       string `json:"event"`
	Resolved    bool   `json:"resolved"`
	APIKeyID    string `json:"api_key_id"`
	RuleName    string `json:"rule_name"`
	Subject     string `json:"subject"`
	IncidentKey string `json:"incident_key"`
}

func notificationReceiver(t *testing.T) (*httptest.Server, <-chan notificationBody) {
	t.Helper()
	bodies := make(chan notificationBody, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body notificationBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	return receiver, bodies
}

func received(bodies <-chan notificationBody) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for range len(bodies) {
			body := <-bodies
			if !yield(fmt.Sprintf("%s resolved=%t", body.Event, body.Resolved)) {
				return
			}
		}
	})
}

func TestKeyExpiryReminderIsDelivered(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	receiver, bodies := notificationReceiver(t)
	destination, rule := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=now()+interval '1 hour' WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by)
 VALUES($1,'key expiry',$2,$3,$1,$4)`, destination, receiver.URL, f.projectID, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,destination_id,etag,created_by)
 VALUES($1,'key expiry',$2,'key.expiring','api_key',$3,$4,$1,$5)`, rule, f.projectID, f.keyID, destination, f.ownerID); err != nil {
		t.Fatal(err)
	}
	deliverNotifications(t, pool, nil)
	if len(bodies) != 1 {
		t.Fatalf("receiver got %d deliveries, want 1", len(bodies))
	}
	if body := <-bodies; body.Event != "key.expiring" || body.APIKeyID != f.keyID {
		t.Errorf("delivered body = %+v, want key.expiring for key %s", body, f.keyID)
	}
	var status, reason string
	if err := pool.QueryRow(t.Context(), `SELECT status,reason FROM olp.notification_deliveries WHERE rule_id=$1 AND api_key_id=$2`, rule, f.keyID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || reason != "expiry" {
		t.Errorf("delivery status=%s reason=%s, want delivered expiry", status, reason)
	}
	listed := notificationManagement(t, pool, f).call(t, "GET", "/api/v1/notifications/deliveries?rule_id="+rule, "", "")
	var page struct {
		Items []struct {
			Event string `json:"event"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listed.body, &page); err != nil || len(page.Items) != 1 || page.Items[0].Event != "key.expiring" {
		t.Errorf("deliveries API answered %d %s, want one key.expiring delivery", listed.status, listed.body)
	}
}

func TestStoredDeliveryPayloadNumbersReachWebhookUnchanged(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	bodies := make(chan []byte, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	destination, rule := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by)
 VALUES($1,'numbers',$2,$3,$1,$4)`, destination, receiver.URL, f.projectID, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,destination_id,etag,created_by)
 VALUES($1,'numbers',$2,'key.expiring','api_key',$3,$4,$1,$5)`, rule, f.projectID, f.keyID, destination, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
 VALUES($1,$2,'key.expiring','numbers',false,'{"event":"spoofed","total":12345678901234567890,"rate":0.1000000000000000055511151231257827}','pending')`,
		uuid.NewString(), rule); err != nil {
		t.Fatal(err)
	}
	deliverNotifications(t, pool, nil)
	if len(bodies) != 1 {
		t.Fatalf("receiver got %d deliveries, want 1", len(bodies))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(<-bodies, &fields); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{"event": string(fields["event"]), "total": string(fields["total"]), "rate": string(fields["rate"])}
	want := map[string]string{"event": `"key.expiring"`, "total": "12345678901234567890", "rate": "0.1000000000000000055511151231257827"}
	if !maps.Equal(got, want) {
		t.Errorf("delivered fields = %v, want %v", got, want)
	}
}

func TestInterruptedFinalNotificationRecordsUnknownOutcome(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	sending := make(chan struct{}, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		sending <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(receiver.Close)
	destination, rule, delivery := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by)
			VALUES($1,'retry',$2,$3,$1,$4)`, []any{destination, receiver.URL, f.projectID, f.ownerID}},
		{`INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,destination_id,etag,created_by)
			VALUES($1,'retry',$2,'key.expiring','api_key',$3,$4,$1,$5)`, []any{rule, f.projectID, f.keyID, destination, f.ownerID}},
		{`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status,attempts,last_error_code,last_attempt_at)
			VALUES($1,$2,'key.expiring','final-attempt',false,'{}','failed',4,'http_5xx',now()-interval '1 hour')`, []any{delivery, rule}},
	} {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
		notifications.RunDelivery(ctx, notifications.DeliveryConfig{Pool: pool, NewID: uuid.NewString, Now: time.Now,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sender: notifications.NewSender(policy)})
	}()
	select {
	case <-sending:
		cancel()
	case <-ctx.Done():
		t.Fatal("final notification attempt never reached the receiver")
	}
	<-done
	var status, lastError string
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT status,last_error_code,attempts FROM olp.notification_deliveries WHERE id=$1`, delivery).
		Scan(&status, &lastError, &attempts); err != nil || status != "failed" || lastError != "outcome_unknown" || attempts != 5 {
		t.Fatalf("interrupted attempt left status=%s error=%s attempts=%d, query error=%v", status, lastError, attempts, err)
	}
}

func TestSignalNotificationsAnnounceTriggersAndRecoveries(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	circuits := fleet.New(newRunner(t))
	receiver, bodies := notificationReceiver(t)
	destination := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `UPDATE olp.providers SET state='active' WHERE id=$1`, f.providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.runtime_install_status(gateway_instance,desired_generation,installed_generation,failed) VALUES('gateway-1',2,1,true)`); err != nil {
		t.Fatal(err)
	}
	if err := circuits.PublishCircuit(t.Context(), f.providerID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,'signals',$2,$1,$3)`, destination, receiver.URL, f.ownerID); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"runtime.install_failed", "provider.circuit.open", "provider.circuit.closed"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,event,destination_id,etag,created_by) VALUES($1,$2,$2,$3,$1,$4)`, uuid.NewString(), event, destination, f.ownerID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
 SELECT $1,id,event,'malformed',false,'{"incident":"x","incident_key":"malformed"}','pending' FROM olp.notification_rules WHERE event='provider.circuit.open'`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	pages := pagerDutyReceiver(t, pool, f, "runtime.install_failed")
	deliverNotifications(t, pool, circuits)
	if got, want := received(bodies), []string{"provider.circuit.open resolved=false", "provider.circuit.open resolved=false", "runtime.install_failed resolved=false"}; !slices.Equal(got, want) {
		t.Errorf("triggers delivered %v, want %v", got, want)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.runtime_install_status SET installed_generation=2,failed=false,checked_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := circuits.PublishCircuit(t.Context(), f.providerID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	deliverNotifications(t, pool, circuits)
	if got, want := received(bodies), []string{"provider.circuit.closed resolved=true", "provider.circuit.closed resolved=true", "runtime.install_failed resolved=true"}; !slices.Equal(got, want) {
		t.Errorf("recoveries delivered %v, want %v", got, want)
	}
	if len(pages) != 2 {
		t.Fatalf("PagerDuty got %d events, want a trigger and a resolve", len(pages))
	}
	if trigger, resolve := <-pages, <-pages; trigger.action != "trigger" || resolve.action != "resolve" || trigger.dedupKey != resolve.dedupKey {
		t.Errorf("PagerDuty got %+v then %+v, want a trigger and a resolve sharing one dedup key", trigger, resolve)
	}
	var undelivered int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE status<>'delivered') FROM olp.notification_deliveries`).Scan(&undelivered); err != nil {
		t.Fatal(err)
	}
	if undelivered != 0 {
		t.Errorf("%d signal deliveries were not delivered", undelivered)
	}
}

type pagerDutyEvent struct {
	action   string
	dedupKey string
}

func pagerDutyReceiver(t *testing.T, pool *pgxpool.Pool, f usageFixture, event string) <-chan pagerDutyEvent {
	t.Helper()
	pages := make(chan pagerDutyEvent, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var page struct {
			Action   string `json:"event_action"`
			DedupKey string `json:"dedup_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&page); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pages <- pagerDutyEvent{action: page.Action, dedupKey: page.DedupKey}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "dedup_key": page.DedupKey})
	}))
	t.Cleanup(receiver.Close)
	keys, installation := notificationKeys(t, pool)
	secret, destination := uuid.NewString(), uuid.NewString()
	sealed, err := keys.Seal(installation, secrets.NotificationSecret, secret, []byte(`{"routing_key":"R0UT1NG"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext) VALUES($1,$2,$3,$4)`, secret, secrets.NotificationSecret, keys.Active, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,type,url,secret_id,etag,created_by) VALUES($1,'pager','pagerduty',$2,$3,$1,$4)`, destination, receiver.URL+"/v2/enqueue", secret, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_rules(id,name,event,destination_id,etag,created_by) VALUES($1,'paged',$2,$3,$1,$4)`, uuid.NewString(), event, destination, f.ownerID); err != nil {
		t.Fatal(err)
	}
	return pages
}

func notificationManagement(t *testing.T, pool *pgxpool.Pool, f usageFixture) *managementClient {
	t.Helper()
	return notificationManagementAs(t, pool, f.ownerID)
}

func notificationManagementAs(t *testing.T, pool *pgxpool.Pool, userID string) *managementClient {
	t.Helper()
	return notificationManagementWithPolicy(t, pool, userID, egress.Policy{})
}

func notificationManagementWithPolicy(t *testing.T, pool *pgxpool.Pool, userID string, policy egress.Policy) *managementClient {
	t.Helper()
	return managementHTTPAs(t, pool, userID, func(mux *http.ServeMux, server *access.Server) {
		notifications.NewManagement(server, policy).Register(mux)
	})
}

func TestNotificationDestinationAndRuleLifecycle(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	client := notificationManagement(t, pool, newUsageFixture(t, pool))
	type resource struct {
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		SecretConfigured bool            `json:"secret_configured"`
		Configuration    json.RawMessage `json:"configuration"`
	}
	decode := func(result apiResult) resource {
		t.Helper()
		var r resource
		if err := json.Unmarshal(result.body, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	const destinationKey = "notification-destination-create"
	const destinationBody = `{"name":"ops","url":"https://hooks.example.com/olp","secret":"signing-key"}`
	created := expectStatus(t, "create destination", client.call(t, "POST", "/api/v1/notifications/destinations", "", destinationBody, destinationKey), http.StatusCreated)
	destination := decode(created)
	path := "/api/v1/notifications/destinations/" + destination.ID
	conflict := client.call(t, "POST", "/api/v1/notifications/destinations", "", `{"name":"changed","url":"https://hooks.example.com/olp","secret":"signing-key"}`, destinationKey)
	var conflictProblem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(conflict.body, &conflictProblem); err != nil || conflict.status != http.StatusConflict || !strings.HasSuffix(conflictProblem.Type, "/idempotency_conflict") {
		t.Fatalf("create destination with changed name answered %d %s, want 409 idempotency_conflict (%v)", conflict.status, conflict.body, err)
	}
	replayed := expectStatus(t, "retry destination creation", client.call(t, "POST", "/api/v1/notifications/destinations", "", destinationBody, destinationKey), http.StatusCreated)
	if replay := decode(replayed); replay.ID != destination.ID || replayed.etag != created.etag {
		t.Fatalf("retried destination id=%q etag=%q, want id=%q etag=%q", replay.ID, replayed.etag, destination.ID, created.etag)
	}
	listed := expectStatus(t, "list after changed-name conflict", client.call(t, "GET", "/api/v1/notifications/destinations", "", ""), http.StatusOK)
	var page struct {
		Items []resource `json:"items"`
	}
	if err := json.Unmarshal(listed.body, &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != destination.ID || page.Items[0].Name != "ops" {
		t.Fatalf("destinations after changed-name conflict = %s (%v), want only original destination %q named ops", listed.body, err, destination.ID)
	}
	fetched := expectStatus(t, "get destination", client.call(t, "GET", path, "", ""), http.StatusOK)
	if fetched.etag == "" || fetched.etag != created.etag || decode(fetched).Name != "ops" || !decode(fetched).SecretConfigured || strings.Contains(string(fetched.body), "signing-key") {
		t.Fatalf("get destination etag=%q created etag=%q body=%s", fetched.etag, created.etag, fetched.body)
	}
	expectStatus(t, "patch without If-Match", client.call(t, "PATCH", path, "", `{"name":"renamed"}`), http.StatusPreconditionRequired)
	expectStatus(t, "patch with stale If-Match", client.call(t, "PATCH", path, `"stale"`, `{"name":"renamed"}`), http.StatusPreconditionFailed)
	moved := expectStatus(t, "move signed destination without its secret", client.call(t, "PATCH", path, fetched.etag, `{"url":"https://other.example.com/olp"}`), http.StatusUnprocessableEntity)
	var problem struct {
		Errors map[string]json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(moved.body, &problem); err != nil || problem.Errors["secret"] == nil {
		t.Fatalf("move without secret problem = %s (%v)", moved.body, err)
	}
	slack := decode(expectStatus(t, "create disabled slack destination", client.call(t, "POST", "/api/v1/notifications/destinations", "", `{"name":"chat","type":"slack","url":"https://hooks.slack.com","enabled":false}`), http.StatusCreated))
	slackPath := "/api/v1/notifications/destinations/" + slack.ID
	slackETag := expectStatus(t, "get slack destination", client.call(t, "GET", slackPath, "", ""), http.StatusOK).etag
	unsigned := decode(expectStatus(t, "create unsigned webhook destination", client.call(t, "POST", "/api/v1/notifications/destinations", "", `{"name":"plain","url":"https://hooks.example.com/plain"}`), http.StatusCreated))
	unsignedPath := "/api/v1/notifications/destinations/" + unsigned.ID
	unsignedETag := expectStatus(t, "get unsigned destination", client.call(t, "GET", unsignedPath, "", ""), http.StatusOK).etag
	for _, rejected := range []struct {
		step, method, path, ifMatch, body, field, message string
	}{
		{"create with non-object configuration", "POST", "/api/v1/notifications/destinations", "", `{"name":"bad","url":"nope","configuration":"x"}`, "configuration", "Use a configuration object."},
		{"create with non-object credential", "POST", "/api/v1/notifications/destinations", "", `{"name":"bad","type":"slack","url":"nope","secret":"s"}`, "secret", "Use the channel credential object or null."},
		{"patch with non-object configuration", "PATCH", path, fetched.etag, `{"configuration":[]}`, "configuration", "Use a configuration object."},
		{"patch with non-object credential", "PATCH", slackPath, slackETag, `{"url":"nope","secret":"s"}`, "secret", "Use the channel credential object or null."},
		{"patch blank name before malformed url", "PATCH", path, fetched.etag, `{"name":" ","url":5}`, "name", "Use 1–100 characters."},
		{"patch configuration before malformed enabled", "PATCH", path, fetched.etag, `{"enabled":"x","configuration":"y"}`, "configuration", "Use a configuration object."},
		{"patch null type", "PATCH", path, fetched.etag, `{"type":null}`, "type", "The destination type cannot be changed."},
		{"patch unsigned webhook to a malformed url", "PATCH", unsignedPath, unsignedETag, `{"url":"nope"}`, "destination", "The destination, configuration or secret is invalid."},
	} {
		got := expectStatus(t, rejected.step, client.call(t, rejected.method, rejected.path, rejected.ifMatch, rejected.body), http.StatusUnprocessableEntity)
		var rejection struct {
			Errors map[string][]struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		err := json.Unmarshal(got.body, &rejection)
		if err != nil || len(rejection.Errors) != 1 || len(rejection.Errors[rejected.field]) != 1 || rejection.Errors[rejected.field][0].Message != rejected.message {
			t.Errorf("%s: problem = %s, want %s: %s (%v)", rejected.step, got.body, rejected.field, rejected.message, err)
		}
	}
	renamed := expectStatus(t, "rename destination", client.call(t, "PATCH", path, fetched.etag, `{"name":"renamed"}`), http.StatusOK)
	if got := decode(expectStatus(t, "get renamed destination", client.call(t, "GET", path, "", ""), http.StatusOK)); renamed.etag == fetched.etag || got.Name != "renamed" || !got.SecretConfigured {
		t.Fatalf("renamed destination etag=%q (was %q) body=%+v", renamed.etag, fetched.etag, got)
	}
	rule := expectStatus(t, "create rule", client.call(t, "POST", "/api/v1/notifications/rules", "",
		`{"name":"stale workers","event":"worker.stale","destination_id":"`+destination.ID+`","configuration":{"cooldown_seconds":600}}`), http.StatusCreated)
	var configuration map[string]any
	if err := json.Unmarshal(decode(rule).Configuration, &configuration); err != nil || configuration["cooldown_seconds"] != float64(600) {
		t.Fatalf("rule configuration = %s (%v)", rule.body, err)
	}
	expectStatus(t, "delete destination in use", client.call(t, "DELETE", path, renamed.etag, ""), http.StatusConflict)
	expectStatus(t, "delete rule", client.call(t, "DELETE", "/api/v1/notifications/rules/"+decode(rule).ID, rule.etag, ""), http.StatusNoContent)
	expectStatus(t, "delete destination", client.call(t, "DELETE", path, renamed.etag, ""), http.StatusNoContent)
	expectStatus(t, "get deleted destination", client.call(t, "GET", path, "", ""), http.StatusNotFound)
}

func TestNotificationRetryPreservesBudgetSubject(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"subject", "kind", "window", "name"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			nextKey := uuid.NewString()
			f.addKey(t, nextKey)
			payloads := make(chan map[string]any, 4)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				payloads <- body
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer receiver.Close()
			destination, rule := uuid.NewString(), uuid.NewString()
			for _, s := range []struct {
				sql  string
				args []any
			}{
				{`UPDATE olp.api_keys SET policy='{"daily_cost_limit":"100"}' WHERE id=$1`, []any{f.keyID}},
				{`INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete) SELECT $1,'day',window_id,80,0,true FROM olp.budget_window('day',now())`, []any{f.keyID}},
				{`INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by) VALUES($1,'retry',$2,$3,$1,$4)`, []any{destination, receiver.URL, f.projectID, f.ownerID}},
				{`INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by) VALUES($1,'retry',$2,'budget.threshold','api_key',$3,'day',75,$4,$1,$5)`, []any{rule, f.projectID, f.keyID, destination, f.ownerID}},
			} {
				if _, err := pool.Exec(t.Context(), s.sql, s.args...); err != nil {
					t.Fatal(err)
				}
			}
			policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			run := func() {
				var prior int64
				if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT successes_total FROM olp.worker_task_health WHERE task='notification_delivery'),0)`).Scan(&prior); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					notifications.RunDelivery(ctx, notifications.DeliveryConfig{Pool: pool, NewID: uuid.NewString, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sender: notifications.NewSender(policy)})
				}()
				defer func() { cancel(); <-done }()
				harness.Eventually(t, "delivery pass", 5*time.Second, func() (bool, string) {
					var success bool
					if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='notification_delivery' AND successes_total>$1)`, prior).Scan(&success); err != nil {
						t.Fatal(err)
					}
					return success, "notification delivery worker has not finished a pass"
				})
			}
			run()
			var first map[string]any
			select {
			case first = <-payloads:
			case <-time.After(5 * time.Second):
				t.Fatal("first delivery absent")
			}
			if first["subject_id"] != f.keyID {
				t.Fatalf("first subject=%v", first)
			}
			client := notificationManagementWithPolicy(t, pool, f.ownerID, policy)
			patch := map[string]string{"subject_id": nextKey}
			switch change {
			case "kind":
				group := uuid.NewString()
				if _, err := pool.Exec(t.Context(), `INSERT INTO olp.budget_groups(id,name,project_id,etag,created_by,daily_cost_limit) VALUES($1,'new group',$2,$1,$3,100)`, group, f.projectID, f.ownerID); err != nil {
					t.Fatal(err)
				}
				patch = map[string]string{"subject_kind": "budget_group", "subject_id": group}
			case "window":
				patch = map[string]string{"window_kind": "month"}
			case "name":
				patch = map[string]string{"name": "renamed"}
			}
			patchBody, err := json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			expectStatus(t, "PATCH notification rule", client.call(t, http.MethodPatch, "/api/v1/notifications/rules/"+rule, `"`+rule+`"`, string(patchBody)), http.StatusOK)
			if _, err := pool.Exec(t.Context(), `UPDATE olp.notification_deliveries SET last_attempt_at=now()-interval '2 minutes' WHERE rule_id=$1`, rule); err != nil {
				t.Fatal(err)
			}
			run()
			select {
			case second := <-payloads:
				t.Logf("PATCH returned200, same delivery=%t first subject=%v retry subject=%v accrued=%v", first["delivery_id"] == second["delivery_id"], first["subject_id"], second["subject_id"], second["accrued"])
				if second["subject_id"] != first["subject_id"] || second["subject_kind"] != first["subject_kind"] || second["window_kind"] != first["window_kind"] || second["window_id"] != first["window_id"] || second["accrued"] != first["accrued"] || second["limit"] != first["limit"] || second["delivery_id"] != first["delivery_id"] {
					t.Fatalf("retry reassigned original budget evidence to another subject: first=%v retry=%v", first, second)
				}
			default:
				t.Fatal("original failed alert was not retried")
			}
		})
	}
}

func TestNotificationEventConstraintMatchesPublishedEvents(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	var definition string
	if err := pool.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='notification_rules_event_check'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	allowed := slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, match := range regexp.MustCompile(`'([a-z_.]+)'`).FindAllStringSubmatch(definition, -1) {
			if !yield(match[1]) {
				return
			}
		}
	}))
	raw, err := os.ReadFile(filepath.Join("..", "..", "management", "management.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				NotificationEvent struct {
					Enum []string `json:"enum"`
				} `json:"NotificationEvent"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	published := slices.Sorted(slices.Values(spec.Components.Schemas.NotificationEvent.Enum))
	if len(published) == 0 || !slices.Equal(allowed, published) {
		t.Fatalf("database allows %v, API publishes %v", allowed, published)
	}
}

func TestNotificationBudgetEvidenceFreezesSubjectAtDelivery(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	user, key, nextKey, destination, rule := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'snapshot@example.invalid','Snapshot','owner',$1)`, []any{user}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag) VALUES($1::uuid,$1::text,decode('00','hex'),'first',$3,'{}',$1::uuid),($2::uuid,$2::text,decode('01','hex'),'second',$3,'{}',$2::uuid)`, []any{key, nextKey, user}},
		{`INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,'snapshot','https://example.invalid/notify',$1,$2)`, []any{destination, user}},
		{`INSERT INTO olp.notification_rules(id,name,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by) VALUES($1,'snapshot','budget.threshold','api_key',$2,'day',75,$3,$1,$4)`, []any{rule, key, destination, user}},
		{`INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status) VALUES($1,$2,1,75,80,100,'failed')`, []any{uuid.NewString(), rule}},
	} {
		if _, err := pool.Exec(t.Context(), s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status) VALUES($1,$2,2,75,80,100,'pending')`, uuid.NewString(), rule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.notification_rules SET subject_id=$2,window_kind='month' WHERE id=$1`, rule, nextKey); err != nil {
		t.Fatal(err)
	}
	var count int
	var preserved bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*),bool_and(budget_subject_kind='api_key' AND budget_subject_id=$2 AND budget_window_kind='day' AND accrued=80 AND limit_amount=100) FROM olp.notification_deliveries WHERE rule_id=$1`, rule, key).Scan(&count, &preserved); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status,budget_subject_kind) VALUES($1,$2,3,75,80,100,'pending','api_key')`, uuid.NewString(), rule); err == nil {
		t.Fatal("partial budget identity snapshot accepted")
	}
	if count != 2 || !preserved {
		t.Fatalf("delivery evidence changed after retarget: rows=%d preserved=%t", count, preserved)
	}
}
