//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
)

// TestAPIKeyBudgetReportsLiveSpend proves the key detail carries live
// accounting rather than stored policy: accrued spend and unpriced attempts are
// summed from the recorded facts and the rollups they are folded into, measured
// over the UTC day and month the installation is actually billed by, while the
// amounts they are measured against stay the ones on the key.
func TestAPIKeyBudgetReportsLiveSpend(t *testing.T) {
	h := newAccessHarness(t)
	// Enforcement is decided during composition, before anything is served.
	h.Server.LimitsEnforced = true
	owner := h.owner()
	key := kbKey(t, h, owner, "5.00", "50.00")
	provider := kbProvider(t, h)

	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// On the first of the month every stored hour is also today, so the month
	// only outgrows the day where an earlier day of it exists.
	earlier := dayStart.Add(-2 * time.Hour)
	monthlyOnly := !earlier.Before(monthStart)

	kbFact(t, h, key, provider, now, "billable", kbText("0.030000000000"), false)
	kbFact(t, h, key, provider, now, "billable", nil, true)
	kbFact(t, h, key, provider, now, "not_billable", nil, false)
	// Spend older than the month window is not this month's spend.
	kbFact(t, h, key, provider, monthStart.Add(-time.Hour), "billable", kbText("9.990000000000"), false)
	// Retention folds attempts into hourly rollups; a key's total must not dip
	// when it does, so both sources are read as one set.
	kbHourly(t, h, key, provider, dayStart, "1.250000000000", 0)
	daily, monthly, unpriced := "1.280000000000", "1.280000000000", float64(1)
	if monthlyOnly {
		kbHourly(t, h, key, provider, earlier, "2.500000000000", 1)
		monthly, unpriced = "3.780000000000", 2
	}

	path := "/api/v1/api-keys/" + key
	detail := h.want(owner, "GET", path, nil, nil, 200)
	validateManagementResponse(t, "GET", path, 200, detail)
	kbAssert(t, detail, kbExpect{daily: daily, monthly: monthly, dailyLimit: "5.00",
		monthlyLimit: "50.00", unpriced: unpriced, enforced: true}, now)

	// The inventory reports the same accounting as the detail page.
	list := h.want(owner, "GET", "/api/v1/api-keys", nil, nil, 200)
	items, ok := list["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("api key inventory: %v", list)
	}
	kbAssert(t, items[0].(map[string]any), kbExpect{daily: daily, monthly: monthly,
		dailyLimit: "5.00", monthlyLimit: "50.00", unpriced: unpriced, enforced: true}, now)
}

// TestAPIKeyBudgetWithoutEnforcement proves an installation that cannot admit
// against its budgets still reports the windows and the stored amounts, and
// says plainly that nothing is enforced.
func TestAPIKeyBudgetWithoutEnforcement(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := kbKey(t, h, owner, "5.00", "")
	path := "/api/v1/api-keys/" + key
	detail := h.want(owner, "GET", path, nil, nil, 200)
	validateManagementResponse(t, "GET", path, 200, detail)
	kbAssert(t, detail, kbExpect{daily: "0", monthly: "0", dailyLimit: "5.00",
		monthlyLimit: "", unpriced: 0, enforced: false}, time.Now().UTC())
}

// kbExpect is one expected budget document. An empty limit is a key that has
// no amount for that window, which the contract reports as null.
type kbExpect struct {
	daily, monthly           string
	dailyLimit, monthlyLimit string
	unpriced                 float64
	enforced                 bool
}

func kbAssert(t *testing.T, body map[string]any, want kbExpect, now time.Time) {
	t.Helper()
	budget, ok := body["budget"].(map[string]any)
	if !ok {
		t.Fatalf("api key carries no budget: %v", body)
	}
	if budget["enforcement_active"] != want.enforced {
		t.Fatalf("enforcement_active: %v, want %v", budget["enforcement_active"], want.enforced)
	}
	if budget["unpriced_attempts"] != want.unpriced {
		t.Fatalf("unpriced_attempts: %v, want %v", budget["unpriced_attempts"], want.unpriced)
	}
	kbWindow(t, budget, "daily", want.daily, want.dailyLimit, now.Truncate(24*time.Hour).Add(24*time.Hour))
	kbWindow(t, budget, "monthly", want.monthly, want.monthlyLimit,
		time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC))
}

// kbWindow asserts one budget window: money travels as an exact decimal string
// and the window ends at a UTC boundary the console can render.
func kbWindow(t *testing.T, budget map[string]any, name, accrued, limit string, ends time.Time) {
	t.Helper()
	window, ok := budget[name].(map[string]any)
	if !ok {
		t.Fatalf("%s budget window is missing: %v", name, budget)
	}
	if window["accrued"] != accrued {
		t.Fatalf("%s accrued: %v, want %q", name, window["accrued"], accrued)
	}
	if limit == "" {
		if window["limit"] != nil {
			t.Fatalf("%s limit: %v, want null", name, window["limit"])
		}
	} else if window["limit"] != limit {
		t.Fatalf("%s limit: %v, want %q", name, window["limit"], limit)
	}
	raw, ok := window["window_ends_at"].(string)
	if !ok {
		t.Fatalf("%s window_ends_at: %v", name, window["window_ends_at"])
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("%s window_ends_at %q: %v", name, raw, err)
	}
	if !at.Equal(ends) {
		t.Fatalf("%s window_ends_at: %s, want %s", name, at.UTC(), ends)
	}
}

// kbKey issues one API key with the given cost budgets and returns its id. An
// empty amount leaves that budget unset.
func kbKey(t *testing.T, h *accessHarness, owner *browser, daily, monthly string) string {
	t.Helper()
	body := map[string]any{"name": "budgeted", "scopes": []string{"inference"}, "allowed_routes": []string{}}
	if daily != "" {
		body["daily_cost_limit"] = daily
	}
	if monthly != "" {
		body["monthly_cost_limit"] = monthly
	}
	created := h.want(owner, "POST", "/api/v1/api-keys", body, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	id, ok := created["id"].(string)
	if !ok {
		t.Fatalf("created key: %v", created)
	}
	return id
}

func kbExec(t *testing.T, h *accessHarness, query string, args ...any) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("seed accounting: %v", err)
	}
}

// kbProvider records the provider every seeded attempt is attributed to.
func kbProvider(t *testing.T, h *accessHarness) string {
	t.Helper()
	var owner string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users ORDER BY created_at LIMIT 1").Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	id := access.NewID()
	kbExec(t, h, `INSERT INTO olp.providers (id, name, kind, state, configuration, etag, slots_etag, created_by)
	    VALUES ($1, 'Budget vendor', 'openai', 'active', '{}'::jsonb, $2, $3, $4)`,
		id, access.NewID(), access.NewID(), owner)
	return id
}

// kbFact records one attempt usage fact for the key, with the request anchor
// its foreign key requires. A priced attempt carries a cost and a currency; an
// unpriced one carries neither and is what unpriced_attempts counts.
func kbFact(t *testing.T, h *accessHarness, key, provider string, observed time.Time, charge string, cost *string, unpriced bool) {
	t.Helper()
	request := access.NewID()
	kbExec(t, h, `INSERT INTO olp.usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)`, request, observed)
	billable := charge != "not_billable"
	var tokens *int64
	var currency *string
	if billable {
		tokens = kbCount(10)
	}
	if cost != nil {
		currency = kbText("USD")
	}
	kbExec(t, h, `INSERT INTO olp.attempt_usage_facts (attempt_id, event_id, request_id, request_started_at,
	        attempt_ordinal, api_key_id, provider_id, route_slug, upstream_model, operation, surface,
	        observed_at, charge_status, usage_observed, usage_complete, input_tokens, output_tokens,
	        cached_input_tokens, media_units, estimated_cost, unpriced, pricing_revision_id, currency,
	        request_counted, provider_request_counted, model_request_counted, target_request_counted,
	        request_unpriced_counted, provider_unpriced_counted, model_unpriced_counted,
	        target_unpriced_counted, request_incomplete_counted, provider_incomplete_counted,
	        model_incomplete_counted, target_incomplete_counted)
	    VALUES ($1, $2, $3, $4, 1, $5, $6, 'budget', 'gpt-4o-mini', 'generation', 'openai', $4, $7, $8,
	        true, $9, $9, NULL, NULL, $10::text::numeric, $11, NULL, $12,
	        true, true, true, true, $11, $11, $11, $11, false, false, false, false)`,
		access.NewID(), access.NewID(), request, observed, key, provider, charge, billable,
		tokens, cost, unpriced, currency)
}

// kbHourly records one retained rollup bucket for the key.
func kbHourly(t *testing.T, h *accessHarness, key, provider string, bucket time.Time, cost string, unpricedAttempts int64) {
	t.Helper()
	kbExec(t, h, `INSERT INTO olp.attempt_usage_hourly (bucket, route_slug, provider_id, upstream_model,
	        operation, surface, api_key_id, request_count, provider_request_count, model_request_count,
	        target_request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
	        estimated_cost, request_unpriced_count, provider_unpriced_count, model_unpriced_count,
	        target_unpriced_count, request_incomplete_count, provider_incomplete_count,
	        model_incomplete_count, target_incomplete_count, currency, unpriced_attempt_count)
	    VALUES ($1, 'budget', $2, 'gpt-4o-mini', 'generation', 'openai', $3, 1, 1, 1, 1, 10, 10, 0, 0,
	        $4::text::numeric, $5, $5, $5, $5, 0, 0, 0, 0, 'USD', $5)`,
		bucket, provider, key, cost, unpricedAttempts)
}

func kbText(value string) *string { return &value }
func kbCount(value int64) *int64  { return &value }
