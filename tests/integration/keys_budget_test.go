//go:build integration

package integration_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
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
	glEventually(t, "the reservation to read "+want, func() bool {
		got = kbReserved(t, f, owner)
		return got == want
	})
	if got != want {
		t.Fatalf("reserved %q, want %q", got, want)
	}
}

// kbMessage is the message a request was refused with, which is where the API
// tells a spent budget from a request that does not fit in one with room left.
func kbMessage(t *testing.T, f *glFixture, secret string, fields map[string]any) string {
	t.Helper()
	body := map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for name, value := range fields {
		body[name] = value
	}
	f.h.refresh()
	status, reply, _ := f.h.gateway("POST", "/v1/chat/completions", secret, body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("request: %d, want a refusal", status)
	}
	message, _ := reply["error"].(map[string]any)["message"].(string)
	return message
}

// kbAccrued reads the spend admission measures a key against.
func kbAccrued(t *testing.T, f *glFixture, owner string) string {
	t.Helper()
	day, _ := limCostKeys(f.namespace, owner)
	return limHashOrNone(t, f.valkey, day)["accrued"]
}

// TestKeyBudgetAdmissionRejectsOnTheEstimateBeforeSpendDoes proves a budget is no
// longer measured only against what has been spent: the most a request could cost
// has to fit beside it. A key that has spent nothing is refused a request that
// could cost more than the budget, and admitted one that cannot.
func TestKeyBudgetAdmissionRejectsOnTheEstimateBeforeSpendDoes(t *testing.T) {
	f := glSeedIn(t, "estimate", kbPrice)
	id, secret := kbBudgetedKey(t, f, "estimate", "1.00", nil)

	// With no reply bound the request may produce 4096 tokens, which could cost
	// 4.0961 against a budget of 1.00 of which nothing is spent.
	served := f.vendor.chats.Load()
	status, body, header := f.h.gateway("POST", "/v1/chat/completions", secret, map[string]any{
		"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if code := f.h.gatewayCode(status, body); status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("a request that could cost more than the budget: %d %s", status, code)
	}
	// Nothing has been spent, so the answer cannot say the budget was used up: it
	// says it is the request's own estimate that does not fit, and what to change.
	message, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "cannot cover this request's estimated cost") || strings.Contains(message, "exhausted") {
		t.Fatalf("message = %q, want the estimate that does not fit and not an exhausted budget", message)
	}
	// No wait helps a request that cannot fit alone, so the hint is the window's.
	if retry := glRetryAfter(t, header); retry < 2 {
		t.Fatalf("Retry-After %d for a request no release could admit", retry)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request the budget refused reached the upstream")
	}
	if got := kbReserved(t, f, id); got != "0" {
		t.Fatalf("a refused request reserved %q", got)
	}

	// Bounded to sixteen tokens it could cost 0.0161, which fits.
	if status, code, _ := f.chatWith(secret, map[string]any{"max_tokens": 16}); status != http.StatusOK {
		t.Fatalf("a bounded request: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served+1 {
		t.Fatal("the admitted request did not reach the upstream")
	}
}

// TestKeyBudgetEstimatesRequestsOnStrictRoutes proves a route that sends its
// targets exactly what the caller sent, which is what a route does unless it says
// otherwise, is estimated and reserved for as any other is. The estimate there is
// made from the request a target is prepared to receive, and a failure to make it
// would leave the budget measuring accrued spend alone without saying so.
func TestKeyBudgetEstimatesRequestsOnStrictRoutes(t *testing.T) {
	f := glSeedInFidelity(t, "strict", kbPrice, "strict")
	id, secret := kbBudgetedKey(t, f, "strict", "1.00", nil)

	served := f.vendor.chats.Load()
	if status, code, _ := f.chat(secret); status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("a request that could cost more than the budget on a strict route: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request the budget refused reached the upstream")
	}
	if status, code, _ := f.chatWith(secret, map[string]any{"max_tokens": 16}); status != http.StatusOK {
		t.Fatalf("a bounded request on a strict route: %d %s", status, code)
	}
	kbWaitReserved(t, f, id, kbActual)
}

// TestKeyBudgetHoldsInFlightCostAndSettlesItToTheActualCost proves requests in
// flight count against the budget, and that when one ends what it held becomes
// what it cost, which is what lets later requests in.
func TestKeyBudgetHoldsInFlightCostAndSettlesItToTheActualCost(t *testing.T) {
	f := glSeedIn(t, "inflight", kbPrice)
	id, secret := kbBudgetedKey(t, f, "inflight", "0.03", nil)
	bounded := map[string]any{"max_tokens": 16}

	f.vendor.delay.Store(int64(200 * time.Millisecond))
	defer f.vendor.delay.Store(0)
	served := f.vendor.chats.Load()
	streamed := make(chan int, 1)
	go func() {
		status, _ := glStreamWith(t.Context(), f.h, secret, bounded)
		streamed <- status
	}()
	glInFlight(t, f.vendor, served)
	if got := kbReserved(t, f, id); got != kbBounded {
		t.Fatalf("an in-flight request holds %q, want %q", got, kbBounded)
	}
	// 0.0161 in flight and another 0.0161 would be 0.0322 of a budget of 0.03.
	status, code, header := f.chatWith(secret, bounded)
	if status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("a sibling of an in-flight request: %d %s", status, code)
	}
	// It would fit once the first request is released, so the wait is brief.
	if retry := glRetryAfter(t, header); retry != 1 {
		t.Fatalf("Retry-After %d, want the one second a reservation takes to clear", retry)
	}
	if status := <-streamed; status != http.StatusOK {
		t.Fatalf("streamed request: %d", status)
	}
	f.vendor.delay.Store(0)

	// The request ended and cost 0.0064 of the 0.0161 it held. Accounting is not
	// running here, so the settled cost stays against the budget.
	kbWaitReserved(t, f, id, kbActual)
	for index, want := range []string{"0.0128", "0.0192"} {
		if status, code, _ := f.chatWith(secret, bounded); status != http.StatusOK {
			t.Fatalf("request %d against what the earlier ones actually cost: %d %s", index+2, status, code)
		}
		kbWaitReserved(t, f, id, want)
	}
	// 0.0192 held and 0.0161 more would exceed 0.03: the arithmetic is the sum.
	if status, code, _ := f.chatWith(secret, bounded); status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("a request that no longer fits: %d %s", status, code)
	}
}

// TestKeyBudgetReleasesWhatARequestNeverSpent proves a request that reached no
// upstream gives its reservation back, and that one whose client hangs up before
// the upstream reports anything holds nothing either: nothing is known to have
// been spent.
func TestKeyBudgetReleasesWhatARequestNeverSpent(t *testing.T) {
	f := glSeedIn(t, "release", kbPrice)
	bounded := map[string]any{"max_tokens": 16}

	t.Run("a client that hangs up", func(t *testing.T) {
		id, secret := kbBudgetedKey(t, f, "hangup", "1.00", nil)
		f.vendor.delay.Store(int64(300 * time.Millisecond))
		defer f.vendor.delay.Store(0)
		served := f.vendor.chats.Load()
		ctx, cancel := context.WithCancel(t.Context())
		abandoned := make(chan struct{})
		go func() {
			defer close(abandoned)
			glStreamWith(ctx, f.h, secret, bounded)
		}()
		glInFlight(t, f.vendor, served)
		if got := kbReserved(t, f, id); got != kbBounded {
			t.Fatalf("an in-flight request holds %q, want %q", got, kbBounded)
		}
		cancel()
		<-abandoned
		kbWaitReserved(t, f, id, "0")
	})

	t.Run("an upstream that is unreachable", func(t *testing.T) {
		id, secret := kbBudgetedKey(t, f, "unreachable", "1.00", nil)
		served := f.vendor.chats.Load()
		f.vendor.Close()
		for attempt := range 2 {
			status, code, _ := f.chatWith(secret, bounded)
			if status != http.StatusBadGateway || code != "upstream_unavailable" {
				t.Fatalf("request %d: %d %s, want the unreachable upstream", attempt+1, status, code)
			}
		}
		if f.vendor.chats.Load() != served {
			t.Fatal("an attempt reached an upstream that was no longer listening")
		}
		kbWaitReserved(t, f, id, "0")
	})
}

// TestKeyBudgetAccountingRemovesTheReservation proves the spend accounting
// records replaces the reservation instead of counting beside it. With the real
// pipeline running, a request's settled cost is installed as accrued spend and
// its reservation is gone, whether or not the caller named the request.
func TestKeyBudgetAccountingRemovesTheReservation(t *testing.T) {
	f := glSeedIn(t, "accrual", kbPrice)
	glAccounting(t, f)
	id, secret := kbBudgetedKey(t, f, "accrual", "1.00", nil)

	if status, code, _ := f.chatWith(secret, map[string]any{"max_tokens": 16}); status != http.StatusOK {
		t.Fatalf("request: %d %s", status, code)
	}
	glEventually(t, "the spend to be accrued", func() bool { return kbAccrued(t, f, id) == kbActual })
	// The request was settled at its actual cost and then accounted for, in
	// whichever order those reached Valkey: the reservation is gone either way.
	kbWaitReserved(t, f, id, "0")
	if status, code, _ := f.chatWith(secret, map[string]any{"max_tokens": 16}); status != http.StatusOK {
		t.Fatalf("second request: %d %s", status, code)
	}
	glEventually(t, "the second request to be accrued", func() bool { return kbAccrued(t, f, id) == "0.0128" })
	kbWaitReserved(t, f, id, "0")

	// A caller that names its request is not the one whose name accounting
	// records it under: that is an identifier of the gateway's own, so the
	// reservation is found by it and not by what the caller sent.
	body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.h.HTTP.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "caller-named-"+uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request that names itself: %d", resp.StatusCode)
	}
	glEventually(t, "the named request to be accrued", func() bool { return kbAccrued(t, f, id) == "0.0192" })
	kbWaitReserved(t, f, id, "0")
}

// TestKeyBudgetConcurrentAdmissionStaysWithinTheBound proves the documented
// bound on a burst: requests that arrive together are each judged against the
// others in flight, so a budget of 0.05 admits floor(0.05 / 0.0161) = 3 of them
// however many replicas race for it, and refuses the rest.
func TestKeyBudgetConcurrentAdmissionStaysWithinTheBound(t *testing.T) {
	f := glSeedIn(t, "burst", kbPrice)
	id, secret := kbBudgetedKey(t, f, "burst", "0.05", nil)
	f.h.refresh()

	f.vendor.delay.Store(int64(300 * time.Millisecond))
	defer f.vendor.delay.Store(0)
	statuses := make(chan int, 12)
	var group sync.WaitGroup
	for range cap(statuses) {
		group.Go(func() {
			status, err := glStreamWith(t.Context(), f.h, secret, map[string]any{"max_tokens": 16})
			if err != nil {
				t.Errorf("request: %v", err)
			}
			statuses <- status
		})
	}
	group.Wait()
	close(statuses)
	admitted, refused := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			admitted++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Errorf("status %d", status)
		}
	}
	if admitted != 3 || refused != 9 {
		t.Fatalf("admitted %d and refused %d, want 3 and 9", admitted, refused)
	}
	kbWaitReserved(t, f, id, "0.0192")
}

// TestKeyBudgetReservesNothingWhereNothingIsPriced proves the reservation is
// confined to requests it can describe. A key without a cost budget never has a
// cost estimated or stored, and a route nobody priced is judged on accrued spend
// alone, as it always was.
func TestKeyBudgetReservesNothingWhereNothingIsPriced(t *testing.T) {
	priced := glSeedIn(t, "unpriced", kbPrice)
	rpm := int64(100)
	for name, policy := range map[string]map[string]any{
		"no limits":         nil,
		"a request limit":   {"requests_per_minute": rpm},
		"a token limit":     {"tokens_per_minute": int64(100_000)},
		"a concurrency cap": {"max_concurrency": 4},
	} {
		id, secret := priced.key("priced-"+name, policy)
		if status, code, _ := priced.chat(secret); status != http.StatusOK {
			t.Fatalf("%s: %d %s", name, status, code)
		}
		pending, expiry := limPendingKeys(priced.namespace, id)
		if limInt(t, priced.valkey, "EXISTS", pending, expiry) != 0 {
			t.Fatalf("%s: a key without a cost budget stored a reservation", name)
		}
	}

	// No price list: the same unbounded request a priced route refuses is
	// admitted, because there is nothing to estimate it by.
	unpriced := glSeedIn(t, "unpriced-route", glPrice{})
	id, secret := kbBudgetedKey(t, unpriced, "unpriced-route", "1.00", nil)
	if status, code, _ := unpriced.chat(secret); status != http.StatusOK {
		t.Fatalf("a request nobody can price: %d %s", status, code)
	}
	if got := kbReserved(t, unpriced, id); got != "0" {
		t.Fatalf("an unpriced request reserved %q", got)
	}
}

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

func limPendingKeys(namespace, owner string) (string, string) {
	day, _ := limCostKeys(namespace, owner)
	prefix := day[:len(day)-len(":day")]
	return prefix + ":pending", prefix + ":expiry"
}
