//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
)

// Enforcement is decided during composition, before anything is served.

// On the first of the month every stored hour is also today, so the month
// only outgrows the day where an earlier day of it exists.

// Spend older than the month window is not this month's spend.

// Retention folds attempts into hourly rollups; a key's total must not dip
// when it does, so both sources are read as one set.

// The inventory reports the same accounting as the detail page.

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

// The fixture model is priced at 100 per million input tokens and 1000 per
// million output tokens. The fixture upstream reports 4 input and 6 output
// tokens, so a request costs 0.0064. A prompt of "hi" is one token: bounded to
// sixteen reply tokens the most it can cost is 0.0161, and with no bound the
// default reply of 4096 tokens makes that 4.0961.
var kbPrice = glPrice{input: "100", output: "1000"}

const (
	kbActual  = "0.0064"
	kbBounded = "0.0161"
)

// kbBudgetedKey mints a key with a daily budget and the reconciled balance
// admission needs to spend against it.
func kbBudgetedKey(t *testing.T, f *glFixture, name, daily string, policy map[string]any) (string, string) {
	t.Helper()
	body := map[string]any{"daily_cost_limit": daily}
	for field, value := range policy {
		body[field] = value
	}
	id, secret := f.key(name, body)
	kbOpen(t, f, id)
	return id, secret
}

// kbOpen publishes a zero balance for an owner, as accounting does the first
// time it reconciles it.
func kbOpen(t *testing.T, f *glFixture, owner string) {
	t.Helper()
	windows := limits.BudgetWindows(time.Now().UTC())
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID: owner, DailyWindowID: windows.DailyID, DailyAccrued: "0",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0",
	}); err != nil {
		t.Fatalf("open balance: %v", err)
	}
}

// kbReserved is the cost requests in flight hold against an owner.
func kbReserved(t *testing.T, f *glFixture, owner string) string {
	t.Helper()
	total, err := f.limiter.Reserved(t.Context(), owner)
	if err != nil {
		t.Fatalf("Reserved: %v", err)
	}
	return total
}

// kbWaitReserved waits for the settlement that runs as a request ends.
func kbWaitReserved(t *testing.T, f *glFixture, owner, want string) {
	t.Helper()
	var got string
	eventually(t, "the reservation to read "+want, 15*time.Second, func() bool {
		got = kbReserved(t, f, owner)
		return got == want
	})
	if got != want {
		t.Fatalf("reserved %q, want %q", got, want)
	}
}

// With no reply bound the request may produce 4096 tokens, which could cost
// 4.0961 against a budget of 1.00 of which nothing is spent.

// Nothing has been spent, so the answer cannot say the budget was used up: it
// says it is the request's own estimate that does not fit, and what to change.

// No wait helps a request that cannot fit alone, so the hint is the window's.

// Bounded to sixteen tokens it could cost 0.0161, which fits.

// 0.0161 in flight and another 0.0161 would be 0.0322 of a budget of 0.03.

// It would fit once the first request is released, so the wait is brief.

// The request ended and cost 0.0064 of the 0.0161 it held. Accounting is not
// running here, so the settled cost stays against the budget.

// 0.0192 held and 0.0161 more would exceed 0.03: the arithmetic is the sum.

// The request was settled at its actual cost and then accounted for, in
// whichever order those reached Valkey: the reservation is gone either way.

// A caller that names its request is not the one whose name accounting
// records it under: that is an identifier of the gateway's own, so the
// reservation is found by it and not by what the caller sent.

// No price list: the same unbounded request a priced route refuses is
// admitted, because there is nothing to estimate it by.

// TestKeyWithoutACostBudgetMakesNoExtraValkeyCall proves a key pays for the
// budgets it has and no others. A key with only a request limit runs the rate
// script and never a cost one, a key with only a cost budget runs no rate script,
// and a key with neither runs nothing: the scripts that ran are counted by Valkey,
// which is the only thing that cannot be fooled by a call that wrote nothing.
func TestKeyWithoutACostBudgetMakesNoExtraValkeyCall(t *testing.T) {
	f := glSeedIn(t, "script-calls", kbPrice)
	_, plain := f.key("plain", nil)
	_, limited := f.key("rpm-only", map[string]any{"requests_per_minute": 100})
	budgetID, budgeted := kbBudgetedKey(t, f, "cost-only", "1.00", nil)
	bounded := map[string]any{"max_tokens": 16}
	scripts := func() int64 {
		byDigest, bySource := limScriptCalls(t, f.valkey)
		return byDigest + bySource
	}
	// One request of each kind first, so that Valkey holds the scripts and the
	// counts below are of the calls and not of loading them.
	for _, secret := range []string{plain, limited, budgeted} {
		if status, code, _ := f.chatWith(secret, bounded); status != http.StatusOK {
			t.Fatalf("warm-up request: %d %s", status, code)
		}
	}
	kbWaitReserved(t, f, budgetID, kbActual)

	measure := func(secret string) int64 {
		t.Helper()
		before := scripts()
		if status, code, _ := f.chatWith(secret, bounded); status != http.StatusOK {
			t.Fatalf("request: %d %s", status, code)
		}
		return scripts() - before
	}
	none, rpm := measure(plain), measure(limited)
	before := scripts()
	if status, code, _ := f.chatWith(budgeted, bounded); status != http.StatusOK {
		t.Fatalf("budgeted request: %d %s", status, code)
	}
	// Its settlement runs as the request ends.
	kbWaitReserved(t, f, budgetID, "0.0128")
	cost := scripts() - before
	if none != 0 {
		t.Fatalf("a key without limits ran %d scripts", none)
	}
	if rpm != 1 {
		t.Fatalf("a request-limited key ran %d scripts, want just the rate script", rpm)
	}
	if cost != 2 {
		t.Fatalf("a cost-budgeted key ran %d scripts, want its reservation and its settlement and no rate script", cost)
	}
}
