//go:build integration

package integration_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestDeferredUsageChargesObservationCalendarAndKeepsSubmissionTiming(t *testing.T) {
	h := newAccessHarness(t)
	provider := newOpenAIFixture(t, "")
	owner, _, slug, _ := provisionOpenAI(t, h, provider.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	priceTime := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC).Add(-time.Hour - time.Second)
	h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": priceTime.Format(time.RFC3339Nano),
		"prices":       []any{repPrice("azure_openai", vendorModel, "generation")},
	}, idem("deferred-calendar-prices"), http.StatusCreated)
	// Serving prices have their own10-second refresh obligation. Install the
	// exact revision before dispatch so later usage preserves its price pin.
	time.Sleep(time.Until(h.Runtime.RoutingInputs().RefreshedAt.Add(10*time.Second + 10*time.Millisecond)))
	provider.resps["resp-up-1"]["status"], provider.resps["resp-up-1"]["usage"] = "in_progress", nil
	provider.resps["resp-up-1"]["expires_at"] = time.Now().Add(40 * 24 * time.Hour).Unix()
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	status, created, _ := h.gateway(http.MethodPost, "/v1/responses", key, map[string]any{"model": slug, "input": "calendar", "store": true, "background": true})
	if status != http.StatusOK {
		t.Fatalf("background creation: %d %v", status, created)
	}
	local := created["id"].(string)
	var raw []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage' FROM olp.provider_resources WHERE id=$1`, strings.TrimPrefix(local, "response_")).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	event, err := usage.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Microsecond)
	// Move the captured submission into the previous month while retaining its
	// measured exchange spans. The provider's explicit40-day retention covers
	// the delayed observation; the terminal clock remains the real DB clock.
	event.RequestStartedAt = time.Date(observed.Year(), observed.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-time.Hour)
	event.RequestCompletedAt = event.RequestStartedAt.Add(time.Duration(event.LatencyMS) * time.Millisecond)
	event.ObservedAt = event.RequestCompletedAt
	for index := range event.Attempts {
		event.Attempts[index].StartedAt = event.RequestStartedAt
		event.Attempts[index].CompletedAt = event.RequestStartedAt.Add(time.Duration(event.Attempts[index].LatencyMS) * time.Millisecond)
	}
	raw, err = usage.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb),created_at=$3::timestamptz,expires_at=$3::timestamptz+interval '40 days' WHERE id=$1`, strings.TrimPrefix(local, "response_"), raw, event.RequestStartedAt); err != nil {
		t.Fatal(err)
	}
	input, output := int64(5), int64(4)
	if _, err = h.Gateway.Resources.ObserveResponseUsage(t.Context(), local, "completed",
		usage.AttemptUsage{Observed: true, Complete: true, InputTokens: &input, OutputTokens: &output}, observed); err != nil {
		t.Fatal(err)
	}
	var started, ended, seen time.Time
	var latency int64
	if err = h.Pool.QueryRow(t.Context(), `SELECT r.started_at,r.completed_at,f.observed_at,r.total_latency_ms FROM olp.requests r JOIN olp.attempt_usage_facts f ON f.request_id=r.id AND f.request_started_at=r.started_at WHERE r.id=$1`, event.RequestID).Scan(&started, &ended, &seen, &latency); err != nil {
		t.Fatal(err)
	}
	if latency != event.LatencyMS || !started.Equal(event.RequestStartedAt) || !ended.Equal(event.RequestCompletedAt) || !seen.Equal(observed) {
		t.Fatalf("delayed observation changed exchange timing: started=%v ended=%v observed=%v latency=%d", started, ended, seen, latency)
	}
	windows := limits.BudgetWindows(observed)
	var accrued string
	if err = h.Pool.QueryRow(t.Context(), `SELECT accrued::text FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='month' AND window_id=$2`, event.APIKeyID, windows.MonthlyID).Scan(&accrued); err != nil {
		t.Fatal(err)
	}
	assertDeferredCost(t, accrued)
	// Reports share the observation calendar. Substitute only their clock,
	// as the existing calendar fixtures do, keeping the actual report SQL.
	reportSQL := strings.ReplaceAll(limits.BudgetSQL, "now()", "TIMESTAMPTZ '"+observed.Format(time.RFC3339Nano)+"'")
	var report []byte
	if err = h.Pool.QueryRow(t.Context(), `SELECT `+reportSQL+` FROM olp.api_keys k WHERE k.id=$1`, event.APIKeyID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	var budget struct{ Monthly struct{ Accrued string } }
	if err = json.Unmarshal(report, &budget); err != nil {
		t.Fatal(err)
	}
	assertDeferredCost(t, budget.Monthly.Accrued)
	// Enforce the public maintenance path after the raw retention horizon.
	// It must preserve the same observation-month ownership in hourly totals.
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.usage_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, event.APIKeyID); err != nil {
		t.Fatal(err)
	}
	maintenance, err := usage.RunMaintenance(t.Context(), h.Pool, observed.Add(48*time.Hour))
	if err != nil || maintenance.UsageRows != 1 {
		t.Fatalf("delayed fact rollup: %+v %v", maintenance, err)
	}
	if err = h.Pool.QueryRow(t.Context(), `SELECT `+reportSQL+` FROM olp.api_keys k WHERE k.id=$1`, event.APIKeyID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(report, &budget); err != nil {
		t.Fatal(err)
	}
	assertDeferredCost(t, budget.Monthly.Accrued)
}

func assertDeferredCost(t *testing.T, actual string) {
	t.Helper()
	got, ok := new(big.Rat).SetString(actual)
	want, _ := new(big.Rat).SetString("0.0000195")
	if !ok || got.Cmp(want) != 0 {
		t.Fatalf("observed month cost=%q want0.0000195", actual)
	}
}
