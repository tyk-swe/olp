//go:build integration

package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestBudgetCalendarSettingAndValkeyAdmission(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	setting := h.want(owner, "GET", "/api/v1/settings/budgets.time_zone", nil, nil, 200)
	for _, zone := range []string{"Local", "Invalid/Zone", "../UTC"} {
		h.want(owner, "PUT", "/api/v1/settings/budgets.time_zone", map[string]any{"value": zone}, etagHeader(setting), 422)
	}
	saved := h.want(owner, "PUT", "/api/v1/settings/budgets.time_zone", map[string]any{"value": "Asia/Kathmandu"}, etagHeader(setting), 200)
	if len(saved["calendar"].([]any)) != 3 {
		t.Fatal("missing transition calendar")
	}
	for _, item := range saved["calendar"].([]any) {
		v := item.(map[string]any)
		if v["time_zone"] != "UTC" || v["pending_time_zone"] != "Asia/Kathmandu" {
			t.Fatalf("window reset early: %+v", v)
		}
	}
	// Advance the fixture's schedule, not the server clock. Admission still uses Valkey TIME.
	if _, err := h.Pool.Exec(t.Context(), "DELETE FROM olp.budget_calendar_changes WHERE effective_at>'1970-01-01'; INSERT INTO olp.budget_calendar_changes SELECT k,'2020-01-01'::timestamptz,'Asia/Kathmandu' FROM unnest(ARRAY['day','week','month']) k"); err != nil {
		t.Fatal(err)
	}
	client := limClient(t)
	limiter := limLimiter(t, client, limNamespace(t, client, "calendar"))
	now := time.UnixMilli(limServerTimeMS(t, client))
	w, err := limits.CurrentBudgetWindows(t.Context(), h.Pool, now)
	if err != nil {
		t.Fatal(err)
	}
	s := limits.CostSnapshot{CostOwnerID: uuid.NewString(), DailyWindowID: w.DailyID, MonthlyWindowID: w.MonthlyID, WeeklyWindowID: w.WeeklyID, DailyAccrued: "9", MonthlyAccrued: "9", WeeklyAccrued: "9", DailyStart: w.DailyStart, DailyEnd: w.DailyEnd, MonthlyStart: w.MonthlyStart, MonthlyEnd: w.MonthlyEnd, WeeklyStart: w.WeeklyStart, WeeklyEnd: w.WeeklyEnd}
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	cap := "10"
	r := limits.Request{LookupID: "zone_key", CostOwnerID: s.CostOwnerID, DailyCostLimit: &cap, RequestedTokens: 1, LeaseTTL: time.Minute, CostEstimate: "2", RequestID: uuid.NewString()}
	_, err = limiter.Reserve(t.Context(), r)
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionDailyCost {
		t.Fatalf("calendar cap: %v", err)
	}
	r.CostEstimate = "1"
	lease, err := limiter.Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Refund(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A late legacy-UTC snapshot must not replace a live custom interval.
	utc := limits.BudgetWindows(now)
	old := limits.CostSnapshot{CostOwnerID: s.CostOwnerID, DailyWindowID: utc.DailyID, MonthlyWindowID: utc.MonthlyID, WeeklyWindowID: utc.WeeklyID, DailyAccrued: "0", MonthlyAccrued: "0", WeeklyAccrued: "0"}
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	r.CostEstimate = "2"
	_, err = limiter.Reserve(t.Context(), r)
	if !errors.As(err, &exceeded) {
		t.Fatalf("legacy snapshot changed balance: %v", err)
	}
	// Valkey enforces supplied current bounds even when the day is longer than UTC's 24 hours.
	extended := s
	extended.CostOwnerID = uuid.NewString()
	extended.DailyStart = now.Add(-time.Hour).Truncate(time.Second)
	extended.DailyEnd = now.Add(25 * time.Hour).Truncate(time.Second)
	extended.DailyWindowID = extended.DailyStart.Unix()
	extended.DailyAccrued = "10"
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), extended); err != nil {
		t.Fatal(err)
	}
	r.CostOwnerID = extended.CostOwnerID
	_, err = limiter.Reserve(t.Context(), r)
	if !errors.As(err, &exceeded) || exceeded.RetryAfter < 24*time.Hour || exceeded.RetryAfter > 25*time.Hour {
		t.Fatalf("extended civil day retry: %v", err)
	}
	future := s
	future.CostOwnerID = uuid.NewString()
	future.DailyStart = now.Add(time.Hour).Truncate(time.Second)
	future.DailyEnd = now.Add(25 * time.Hour).Truncate(time.Second)
	future.DailyWindowID = future.DailyStart.Unix()
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), future); err != nil {
		t.Fatal(err)
	}
	r.CostOwnerID = future.CostOwnerID
	if _, err = limiter.Reserve(t.Context(), r); !errors.Is(err, limits.ErrUninitializedCost) {
		t.Fatalf("future interval admitted: %v", err)
	}

}

func TestBudgetCalendarPromotionRequiresSettingsAndPreservesWindows(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	if document["budget_time_zone"] != "UTC" {
		t.Fatal("missing exported zone")
	}
	document["budget_time_zone"] = "America/New_York"
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	body := map[string]any{"document": document}
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("unchanged"), 200)
	detail := h.want(owner, "GET", "/api/v1/settings/budgets.time_zone", nil, nil, 200)
	for _, item := range detail["calendar"].([]any) {
		v := item.(map[string]any)
		if v["time_zone"] != "UTC" || v["pending_time_zone"] != "America/New_York" {
			t.Fatalf("promotion reset active window: %+v", v)
		}
	}
}
