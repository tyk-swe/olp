//go:build integration

package integration_test

import (
	"context"
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
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/workerhealth"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/retention"
	"github.com/tyk-swe/olp/tests/harness"
)

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
	ring, err := secrets.ParseRingWithUnwrapper(context.Background(), []byte(`{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("42", 32)+`"}]}`), nil)
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

func TestFocusExportsHonorDefaultAndExplicitStartDates(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	var installation string
	if err := pool.QueryRow(t.Context(), `UPDATE olp.installation SET active_key_version=1 WHERE singleton RETURNING id::text`).Scan(&installation); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRingWithUnwrapper(context.Background(), []byte(`{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("42", 32)+`"}]}`), nil)
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
