//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/reports"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestUsageComparisonRanksBothPeriodsBeforeLimiting(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	project := newUsageFixture(t, pool)
	otherProject := newUsageFixture(t, pool)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start, previous := end.AddDate(0, 0, -7), end.AddDate(0, 0, -14)
	seed := func(f usageFixture, at time.Time, route, model, cost string) {
		seedComparisonHour(t, f, at, route, model, "USD", &cost, 0)
	}
	for i := range 26 {
		route := fmt.Sprintf("steady-%02d", i)
		seed(project, previous, route, vendor.Model, "100")
		seed(project, start, route, vendor.Model, "100")
	}
	seed(project, previous, "chat", vendor.Model, "8")
	seedComparisonBoundary(t, project, start, "chat")
	seed(project, previous, "legacy", vendor.Model, "3")
	seed(project, start, "summary", vendor.Model, "2")
	seed(project, previous, "tie-a", vendor.Model, "2")
	seed(project, start, "tie-b", vendor.Model, "2")
	seed(project, end, "excluded-end", vendor.Model, "1000")
	seed(project, previous.Add(-time.Hour), "excluded-before", vendor.Model, "1000")
	seed(project, start, "excluded-model", "another-model", "1000")
	seed(otherProject, start, "excluded-project", vendor.Model, "1000")
	seed(otherProject, start, "chat", vendor.Model, "1000")

	call := usageComparisonClient(t, project, start, end)
	report := call(url.Values{"model": {vendor.Model}})
	if report.Status != "available" || len(report.Items) != 25 ||
		!report.Previous.Start.Equal(previous) || !report.Previous.End.Equal(start) ||
		!report.Current.Start.Equal(start) || !report.Current.End.Equal(end) {
		t.Fatalf("unexpected comparison range or ranking size: %+v", report)
	}
	expectComparisonSubtotal(t, report.Previous, "USD", "2613")
	expectComparisonSubtotal(t, report.Current, "USD", "2616")
	for i, want := range [][5]string{
		{"chat", "8", "12", "4", "50"},
		{"legacy", "3", "0", "-3", "-100"},
		{"summary", "0", "2", "2", ""},
		{"tie-a", "2", "0", "-2", "-100"},
		{"tie-b", "0", "2", "2", ""},
	} {
		got := report.Items[i]
		percent, _ := got.PercentChange.Get()
		if got.Route != want[0] || got.PreviousCost != want[1] || got.CurrentCost != want[2] ||
			got.Change != want[3] || percent != want[4] || got.PercentChange.IsNull() != (want[4] == "") {
			t.Errorf("rank %d: %+v, want %v", i+1, got, want)
		}
	}
	if last := report.Items[24]; last.Route != "steady-19" || last.Change != "0" {
		t.Errorf("unexpected cutoff row %+v", last)
	}
	filtered := call(url.Values{"route": {"summary"}, "model": {vendor.Model}})
	if len(filtered.Items) != 1 || filtered.Items[0].Route != "summary" || len(filtered.Previous.Subtotals) != 0 {
		t.Errorf("route filter did not apply to both periods: %+v", filtered)
	}
}

func TestUsageComparisonWithholdsChangesWhenSubtotalsAreNotComparable(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start, previous := end.AddDate(0, 0, -1), end.AddDate(0, 0, -2)
	usdCost, eurCost := "8", "12"
	seedComparisonHour(t, f, previous, "shared", vendor.Model, "USD", &usdCost, 0)
	seedComparisonHour(t, f, start, "shared", vendor.Model, "EUR", &eurCost, 0)
	call := usageComparisonClient(t, f, start, end)
	mismatch := call(nil)
	if mismatch.Status != "incompatible_currency" || len(mismatch.Items) != 0 ||
		!mismatch.Previous.Completeness.Complete || !mismatch.Current.Completeness.Complete {
		t.Fatalf("currencies were treated as comparable: %+v", mismatch)
	}
	expectComparisonSubtotal(t, mismatch.Previous, "USD", usdCost)
	expectComparisonSubtotal(t, mismatch.Current, "EUR", eurCost)
	seedComparisonHour(t, f, start, "usd-route", vendor.Model, "USD", &usdCost, 0)
	mixed := call(nil)
	if mixed.Status != "incompatible_currency" || len(mixed.Items) != 0 || len(mixed.Current.Subtotals) != 2 ||
		mixed.Current.Subtotals[0].EstimatedCost != eurCost || mixed.Current.Subtotals[1].EstimatedCost != usdCost {
		t.Fatalf("mixed currency subtotals were combined: %+v", mixed)
	}
	seedComparisonHour(t, f, previous.Add(time.Hour), "usd-route", vendor.Model, "USD", &usdCost, 0)
	seedComparisonHour(t, f, start.Add(time.Hour), "usd-route", vendor.Model, "USD", nil, 1)
	unpriced := call(url.Values{"route": {"usd-route"}})
	if unpriced.Status != "incomplete" || len(unpriced.Items) != 0 ||
		unpriced.Current.Completeness.UnpricedCount != 1 || !unpriced.Previous.Completeness.Complete {
		t.Fatalf("unpriced usage was treated as complete: %+v", unpriced)
	}
	expectComparisonSubtotal(t, unpriced.Current, "USD", usdCost)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_ingestion_gaps
		(id,gateway_instance,event_count,reason,first_observed_at,last_observed_at,certainty)
		VALUES($1,'fixture',0,'fixture',$2,$2,'lower_bound')`, uuid.NewString(), previous.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	gap := call(url.Values{"route": {"shared"}})
	if gap.Status != "incomplete" || len(gap.Items) != 0 ||
		gap.Previous.Completeness.UncertainRequestMetadataGapCount != 1 || !gap.Current.Completeness.Complete {
		t.Fatalf("uncertain prior coverage was treated as complete: %+v", gap)
	}
	expectComparisonSubtotal(t, gap.Previous, "USD", usdCost)
	expectComparisonSubtotal(t, gap.Current, "EUR", eurCost)
}

func seedComparisonHour(t *testing.T, f usageFixture, at time.Time, route, model, currency string, cost *string, unpriced int) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_hourly
		(bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
		request_count,provider_request_count,model_request_count,target_request_count,
		input_tokens,output_tokens,cached_input_tokens,media_units,estimated_cost,currency,
		request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
		request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count,unpriced_attempt_count)
		VALUES($1,$2,$3,$4,'generation','openai',$5,1,1,1,1,0,0,0,0,$6::numeric,$7,$8,$8,$8,$8,0,0,0,0,$8)`,
		at, route, f.providerID, model, f.keyID, cost, currency, unpriced); err != nil {
		t.Fatal(err)
	}
}

func seedComparisonBoundary(t *testing.T, fixture usageFixture, at time.Time, route string) {
	t.Helper()
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE olp.pricing_revisions SET effective_at=$1`, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := fixture
	f.observedAt = at
	input, output := int64(12_000_000), int64(0)
	attempt := f.attempt(1)
	attempt.Usage.InputTokens, attempt.Usage.OutputTokens = &input, &output
	event := f.event(attempt)
	event.RouteSlug = route
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := ingest.PersistEvent(t.Context(), f.pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("persist shared-boundary usage: %+v %v", result, err)
	}
}

func usageComparisonClient(t *testing.T, f usageFixture, start, end time.Time) func(url.Values) contract.UsageComparisonResponse {
	t.Helper()
	ctx := t.Context()
	keys, installation := notificationKeys(t, f.pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, f.pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint)
		VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), f.ownerID, auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO olp.request_metadata_consumer_health(singleton,pending_events,lag_events,checked_at)
		VALUES(true,0,0,now()) ON CONFLICT(singleton) DO UPDATE SET pending_events=0,lag_events=0,checked_at=now()`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := reports.API{Access: access.New(access.Config{Pool: f.pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys, Log: slog.New(slog.DiscardHandler)})}
	api.Register(mux)
	return func(extra url.Values) contract.UsageComparisonResponse {
		t.Helper()
		query := url.Values{"start": {start.Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}, "project_id": {f.projectID}}
		for key, values := range extra {
			query[key] = values
		}
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/usage/comparison?"+query.Encode(), nil)
		request.AddCookie(&http.Cookie{Name: "__Host-olp_session", Value: token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, request)
		var report contract.UsageComparisonResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &report); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("usage comparison: HTTP %d %s (%v)", rec.Code, rec.Body, err)
		}
		return report
	}
}

func expectComparisonSubtotal(t *testing.T, period contract.UsageComparisonPeriod, currency, cost string) {
	t.Helper()
	if len(period.Subtotals) != 1 || period.Subtotals[0].EstimatedCost != cost {
		t.Fatalf("subtotals = %+v, want %s %s", period.Subtotals, cost, currency)
	}
	if actual, _ := period.Subtotals[0].Currency.Get(); actual != currency {
		t.Fatalf("currency = %q, want %q", actual, currency)
	}
}
