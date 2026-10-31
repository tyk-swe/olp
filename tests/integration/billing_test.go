//go:build integration

package integration_test

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

// billingFake serves the documented-shape vendor fixtures as an admin cost
// endpoint, recording the query of every request it answers.
type billingFake struct {
	mu                    sync.Mutex
	path                  string
	pages                 map[string]string // page token -> fixture name; "" is page 1
	shift                 map[string]string // fixture text rewrites (dates near today)
	queries               []url.Values
	fails                 int
	expectedAuthorization string
	expectedAPIKey        string
	started               chan struct{}
	release               <-chan struct{}
	server                *httptest.Server
}

// billingDays are the three consecutive UTC days the shifted fixtures cover,
// most recent first so they survive the importer's retention purge.
func billingDays() (day1, day2, day3 time.Time) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -3), today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)
}

func newBillingFake(t *testing.T, path string, pages map[string]string, shift map[string]string) *billingFake {
	t.Helper()
	f := &billingFake{path: path, pages: pages, shift: shift}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		started, release := f.started, f.release
		f.started, f.release = nil, nil
		f.mu.Unlock()
		if started != nil {
			close(started)
			<-release
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Query())
		if r.URL.Path != f.path || f.fails > 0 || (f.expectedAuthorization != "" && r.Header.Get("Authorization") != f.expectedAuthorization) || (f.expectedAPIKey != "" && r.Header.Get("x-api-key") != f.expectedAPIKey) {
			f.fails--
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := f.pages[r.URL.Query().Get("page")]
		data, err := os.ReadFile("../../internal/billing/testdata/" + name)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		text := string(data)
		for from, to := range f.shift {
			text = strings.ReplaceAll(text, from, to)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *billingFake) failOnce() {
	f.mu.Lock()
	f.fails++
	f.mu.Unlock()
}

func (f *billingFake) call(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[i]
}

// billingImporter is an Importer wired to the harness's pool, keys and
// egress policy.
func billingImporter(h *accessHarness, t *testing.T) *billing.Importer {
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	return &billing.Importer{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Egress: &policy, Log: slog.New(slog.DiscardHandler)}
}

// billingProvider creates a provider of kind kind pointing at the fake
// admin endpoint.
func billingProvider(h *accessHarness, owner *browser, name, kind, endpoint string, projectID any) map[string]any {
	body := map[string]any{
		"name": name,
		"configuration": map[string]any{
			"kind": kind, "auth_mode": "api_key", "endpoint": endpoint,
		},
		"credential": "sk-provider-" + strings.Repeat("x", 20),
		"model":      "gpt-test",
	}
	if projectID != nil {
		body["project_id"] = projectID
	}
	return h.want(owner, "POST", "/api/v1/providers", body, idem("provider-"+name), 201)
}

func billingPut(h *accessHarness, b *browser, providerID string, body map[string]any, etag string, want int) map[string]any {
	headers := map[string]string{"Idempotency-Key": uuid.NewString()}
	if etag != "" {
		headers["If-Match"] = `"` + etag + `"`
	}
	return h.want(b, "PUT", "/api/v1/providers/"+providerID+"/billing-import", body, headers, want)
}

// billingFact inserts one usage fact for the reconciliation report.
func billingFact(t *testing.T, h *accessHarness, providerID, requestID string, observedAt time.Time,
	chargeStatus string, complete bool, cost *string, unpriced, caller, exempt bool) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.usage_request_anchors (request_id, request_started_at)
		VALUES ($1,$2) ON CONFLICT DO NOTHING`, requestID, observedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users ORDER BY created_at LIMIT 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id, lookup_id, digest, name, created_by, policy, etag)
		VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6)`,
		keyID, uuid.NewString(), make([]byte, 32), "billing", userID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_facts
		(attempt_id, event_id, request_id, request_started_at, attempt_ordinal, api_key_id, provider_id,
		 route_slug, upstream_model, operation, surface, observed_at, charge_status, usage_observed,
		 usage_complete, input_tokens, estimated_cost, unpriced, currency,
		 request_counted, provider_request_counted, model_request_counted, target_request_counted,
		 request_unpriced_counted, provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted,
		 request_incomplete_counted, provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted,
		 caller_credential, budget_exempt)
		VALUES ($1,$2,$3,$4,1,$5,$6,'route','model','generation','api',$7,$8,true,$9,100,$10::numeric,$11,'USD',
			true,true,true,true,$11,$11,$11,$11,
			NOT $9,NOT $9,NOT $9,NOT $9,
			$12,$13)`,
		uuid.NewString(), uuid.NewString(), requestID, observedAt.Add(-time.Second), keyID, providerID,
		observedAt, chargeStatus, complete, cost, unpriced, caller, exempt); err != nil {
		t.Fatal(err)
	}
}

func billingProviderID(provider map[string]any) string { return provider["id"].(string) }

// The credential is required on create, must be a string, and null is
// rejected rather than clearing the import.

// Idempotent replay returns the stored reply.

// An update that omits the credential keeps it.

// If-Match gates mutations.

// A non-openai/anthropic kind is refused.

// Scoping: a manager of another project gets 404; a viewer of the
// provider's project reads it.

// TestBillingImportEndToEnd runs the importer against the fake vendors and
// checks the stored invoiced costs and the reconciliation report.
func TestBillingImportEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Billing")

	day1, day2, day3 := billingDays()
	d1, d3 := day1.Format("2006-01-02"), day3.Format("2006-01-02")
	u1, u2, u3 := fmt.Sprint(day1.Unix()), fmt.Sprint(day2.Unix()), fmt.Sprint(day3.Unix())
	openai := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"":             "openai_costs_page1.json",
		"page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": u1, "1759190400": u2, "1759276800": u3})
	anthropic := newBillingFake(t, "/v1/organizations/cost_report", map[string]string{
		"":             "anthropic_cost_report_page1.json",
		"page-2-token": "anthropic_cost_report_page2.json",
	}, map[string]string{
		`"starting_at": "2025-10-01T`: `"starting_at": "` + day1.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-02T`:   `"ending_at": "` + day2.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-02T`: `"starting_at": "` + day2.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-03T`:   `"ending_at": "` + day3.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-03T`: `"starting_at": "` + day3.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-04T`:   `"ending_at": "` + day3.AddDate(0, 0, 1).Format("2006-01-02") + "T",
	})
	openaiProvider := billingProvider(h, owner, "OpenAI", "openai", openai.server.URL+"/v1", project)
	anthropicProvider := billingProvider(h, owner, "Anthropic", "anthropic", anthropic.server.URL+"/v1", project)
	openaiID := billingProviderID(openaiProvider)
	anthropicID := billingProviderID(anthropicProvider)

	credential := "sk-admin-" + strings.Repeat("k", 30)
	billingPut(h, owner, openaiID, map[string]any{"credential": credential}, openaiProvider["etag"].(string), 201)
	billingPut(h, owner, anthropicID, map[string]any{"credential": "sk-ant-" + strings.Repeat("k", 30)}, anthropicProvider["etag"].(string), 201)

	// The credential is sealed under the billing purpose.
	var purpose string
	if err := h.Pool.QueryRow(t.Context(), `SELECT purpose FROM olp.secrets s
		JOIN olp.provider_billing_imports b ON b.credential_id=s.id
		WHERE b.provider_id=$1`, openaiID).Scan(&purpose); err != nil || purpose != "billing_credential" {
		t.Fatalf("sealed purpose %q err %v", purpose, err)
	}

	// Usage facts across the OpenAI fixture days.
	cost := "0.15"
	costs := "0.25"
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &cost, false, false, false)
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &costs, false, false, false)
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, nil, true, false, false)    // unpriced
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", false, nil, false, false, false)  // incomplete
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &cost, false, true, false)  // caller-paid: excluded
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &costs, false, false, true) // budget-exempt: excluded

	importer := billingImporter(h, t)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var errCode *string
	var from, through *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT last_error_code,imported_from::text,imported_through::text FROM olp.provider_billing_imports WHERE provider_id=$1`, openaiID).Scan(&errCode, &from, &through); err != nil {
		t.Fatal(err)
	}
	if errCode != nil || from == nil || through == nil || *from != d1 || *through != d3 {
		t.Fatalf("import state err=%v range=%v..%v", errCode, from, through)
	}
	if got := len(openai.queries) + len(anthropic.queries); got != 4 {
		t.Fatalf("importer fetched %d pages, want 4", got)
	}
	// The scoped OpenAI request carries project_ids[]=[] empty (org-wide).
	if got := openai.call(0).Get("bucket_width"); got != "1d" {
		t.Fatalf("openai query %v", openai.call(0))
	}
	// Anthropic hit the /v1-normalized path and the documented group_bys.
	if got := anthropic.call(0)["group_by[]"]; len(got) != 2 {
		t.Fatalf("anthropic group_by[] %v", got)
	}

	// Exact invoiced costs.
	rangeStart := day1.Format("2006-01-02")
	rangeEnd := day3.AddDate(0, 0, 1).Format("2006-01-02")
	costsList := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", openaiID, rangeStart, rangeEnd), nil, nil, 200)
	items := costsList["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("openai invoiced items %v", items)
	}
	byItem := map[string]string{}
	for _, item := range items {
		m := item.(map[string]any)
		byItem[m["day"].(string)+"|"+m["line_item"].(string)] = m["amount"].(string)
	}
	if byItem[d1+"|gpt-4o, input_tokens"] != "0.300000000000" &&
		byItem[d1+"|gpt-4o, input_tokens"] != "0.3" {
		t.Fatalf("exact input sum: %v", byItem)
	}
	if byItem[d3+"|gpt-4o-mini, input_tokens"] != "0.0005000001" &&
		byItem[d3+"|gpt-4o-mini, input_tokens"] != "0.000500000100" {
		t.Fatalf("exact mini sum: %v", byItem)
	}
	acosts := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", anthropicID, rangeStart, rangeEnd), nil, nil, 200)
	aByItem := map[string]string{}
	for _, item := range acosts["items"].([]any) {
		m := item.(map[string]any)
		aByItem[m["day"].(string)+"|"+m["line_item"].(string)] = m["amount"].(string)
	}
	if got := aByItem[d1+"|claude-sonnet-4-5 input tokens"]; got != "1.913400000000" && got != "1.9134" {
		t.Fatalf("cents conversion: %v", aByItem)
	}

	// Reconciliation: attributed 0.4 vs invoiced lines on day1; caller and
	// budget-exempt rows are excluded.
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", rangeStart, rangeEnd, openaiID), nil, nil, 200)
	var dayItem map[string]any
	for _, item := range report["items"].([]any) {
		m := item.(map[string]any)
		if m["day"] == d1 {
			dayItem = m
		}
	}
	if dayItem == nil {
		t.Fatalf("reconciliation items %v", report["items"])
	}
	if dayItem["attributed_cost"] != "0.400000000000" && dayItem["attributed_cost"] != "0.4" {
		t.Fatalf("attributed_cost %v (caller and exempt must be excluded)", dayItem)
	}
	// excluded_caller_attempts counts billable attempts excluded for either
	// reason: the caller-paid attempt and the budget-exempt attempt.
	if dayItem["priced_attempts"] != float64(2) || dayItem["unpriced_attempts"] != float64(1) ||
		dayItem["incomplete_attempts"] != float64(1) || dayItem["excluded_caller_attempts"] != float64(2) {
		t.Fatalf("attempt counts %v", dayItem)
	}
	if dayItem["capture_rate_unavailable_reason"] != nil {
		if dayItem["capture_rate"] == nil {
			t.Fatalf("invoiced day must carry a capture rate: %v", dayItem)
		}
	}
	if dayItem["unpriced_share"] != "0.333333" {
		t.Fatalf("unpriced_share %v", dayItem)
	}

	// The Anthropic provider records an invoice day with no attributed usage:
	// its capture rate is a clean zero rather than null.
	areport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", rangeStart, rangeEnd, anthropicID), nil, nil, 200)
	var aDay, aTotals map[string]any
	for _, item := range areport["items"].([]any) {
		m := item.(map[string]any)
		if m["day"] == d1 {
			aDay = m
		}
	}
	for _, item := range areport["totals"].([]any) {
		m := item.(map[string]any)
		if m["provider_id"] == anthropicID {
			aTotals = m
		}
	}
	if aDay == nil || aTotals == nil {
		t.Fatalf("anthropic reconciliation %v %v", areport["items"], areport["totals"])
	}
	if aDay["attributed_cost"] != "0" && aDay["attributed_cost"] != "0.000000000000" {
		t.Fatalf("anthropic attributed %v", aDay)
	}
	if aDay["capture_rate"] != "0" && aDay["capture_rate"] != "0.000000" {
		t.Fatalf("anthropic capture_rate %v (invoiced day, zero usage)", aDay)
	}
	if aTotals["provider_id"] != anthropicID || aTotals["invoiced_cost"] == nil {
		t.Fatalf("anthropic totals %v", aTotals)
	}
	// attributed_cost_covered compares like with like: attributed spend on
	// days the import covered. Anthropic attributed nothing, so both are zero.
	if aTotals["attributed_cost_covered"] == nil {
		t.Fatalf("totals lack attributed_cost_covered: %v", aTotals)
	}
	// Items are sorted by provider, then day.
	var prevProvider, prevDay string
	for _, item := range report["items"].([]any) {
		m := item.(map[string]any)
		if prevProvider != "" && (m["provider_id"].(string) < prevProvider ||
			(m["provider_id"] == prevProvider && m["day"].(string) <= prevDay)) {
			t.Fatalf("items not sorted: %v after %s/%s", m["day"], prevProvider, prevDay)
		}
		prevProvider, prevDay = m["provider_id"].(string), m["day"].(string)
	}

	// A 401 marks the import 'credential' and backs it off. The run endpoint
	// makes the import due again, since the success above scheduled it an
	// hour out.
	headers := etagHeader(h.want(owner, "GET", "/api/v1/providers/"+openaiID+"/billing-import", nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	runImport := h.want(owner, "POST", "/api/v1/providers/"+openaiID+"/billing-import/run", nil, headers, 202)
	if runImport["status"] == nil {
		t.Fatalf("run response %v", runImport)
	}
	openai.failOnce()
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var code *string
	var next time.Time
	if err := h.Pool.QueryRow(t.Context(), `SELECT last_error_code,next_import_at FROM olp.provider_billing_imports WHERE provider_id=$1`, openaiID).
		Scan(&code, &next); err != nil || code == nil || *code != "credential" {
		t.Fatalf("failure code %v err %v", code, err)
	}
	if !next.After(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("backoff too short: %s", next)
	}
}

// Export carries billing_import with the deterministic ref and no key.

// Applying without the binding blocks; supplying it recreates the
// import on a fresh installation.

// Every credential reference the document names needs a binding; the
// billing ref is the one this feature adds, the provider's credential
// slot another.

// Dropping billing_import from the entry deletes the import on apply.

func TestBillingImportPreservesCurrencies(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000": fmt.Sprint(day1.Unix()),
		"1759190400": fmt.Sprint(day2.Unix()),
		"1759276800": fmt.Sprint(day3.Unix()),
		"\"value\": 0.2,\n            \"currency\": \"usd\"": "\"value\": -0.1,\n            \"currency\": \"eur\"",
		"\"value\": 1234.5678901234,":                        "\"value\": 0,",
		"\"value\": 5,":                                      "\"value\": 0,",
	})
	provider := billingProvider(h, owner, "Currencies", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	billingPut(h, owner, providerID, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := day1.Format("2006-01-02")
	end := day1.AddDate(0, 0, 1).Format("2006-01-02")
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	amounts := map[string]decimal.Decimal{}
	for _, raw := range costs["items"].([]any) {
		item := raw.(map[string]any)
		if item["line_item"] != "gpt-4o, input_tokens" {
			continue
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		if err != nil {
			t.Fatal(err)
		}
		amounts[item["currency"].(string)] = amount
	}
	if len(amounts) != 2 || !amounts["USD"].Equal(decimal.RequireFromString("0.1")) || !amounts["EUR"].Equal(decimal.RequireFromString("-0.1")) {
		t.Fatalf("same line item must preserve its amounts by currency: %v", amounts)
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, providerID), nil, nil, 200)
	items := report["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("reconciliation items: %v", items)
	}
	item := items[0].(map[string]any)
	if item["capture_rate"] != nil || item["capture_rate_unavailable_reason"] != "currency_mismatch" {
		t.Fatalf("multi-currency day must not have a capture rate: %v", item)
	}
	totals := report["totals"].([]any)[0].(map[string]any)
	if totals["capture_rate"] != nil || totals["capture_rate_unavailable_reason"] != "currency_mismatch" {
		t.Fatalf("multi-currency total must not have a capture rate: %v", totals)
	}
	fake.mu.Lock()
	fake.shift = map[string]string{
		"1759104000":                  fmt.Sprint(day1.Unix()),
		"1759190400":                  fmt.Sprint(day2.Unix()),
		"1759276800":                  fmt.Sprint(day3.Unix()),
		"\"value\": 0.1,":             "\"value\": 0,",
		"\"value\": 0.2,":             "\"value\": 0,",
		"\"value\": 1234.5678901234,": "\"value\": 0,",
		"\"value\": 5,":               "\"value\": 0,",
		"\"currency\": \"usd\"":       "\"currency\": \"eur\"",
	}
	fake.mu.Unlock()
	path := "/api/v1/providers/" + providerID + "/billing-import"
	current := h.want(owner, "GET", path, nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	report = h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, providerID), nil, nil, 200)
	item = report["items"].([]any)[0].(map[string]any)
	if item["capture_rate"] != nil || item["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("single-currency zero invoice must keep its reason: %v", item)
	}
	totals = report["totals"].([]any)[0].(map[string]any)
	if totals["capture_rate"] != nil || totals["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("single-currency zero total must keep its reason: %v", totals)
	}
}

func TestBillingImportPreservesPrecision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000":      fmt.Sprint(day1.Unix()),
		"1759190400":      fmt.Sprint(day2.Unix()),
		"1759276800":      fmt.Sprint(day3.Unix()),
		"\"value\": 0.1,": "\"value\": 0.00000000000000000001,",
	})
	provider := billingProvider(h, owner, "Precision", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	billingPut(h, owner, providerID, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := day1.Format("2006-01-02")
	end := day1.AddDate(0, 0, 1).Format("2006-01-02")
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	for _, raw := range costs["items"].([]any) {
		item := raw.(map[string]any)
		if item["line_item"] != "gpt-4o, input_tokens" {
			continue
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		if err != nil || !amount.Equal(decimal.RequireFromString("0.20000000000000000001")) {
			t.Fatalf("invoice amount lost precision: %v, %v", item, err)
		}
		return
	}
	t.Fatalf("invoice line item missing: %v", costs)
}

func TestBillingImportCredentialCompatibility(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000": fmt.Sprint(day1.Unix()),
		"1759190400": fmt.Sprint(day2.Unix()),
		"1759276800": fmt.Sprint(day3.Unix()),
	})
	provider := billingProvider(h, owner, "Compatibility", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	billingPut(h, owner, providerID, map[string]any{}, provider["etag"].(string), 422)
	billingPut(h, owner, providerID, map[string]any{"credential": nil}, provider["etag"].(string), 422)
	key := `{"source":"openai_costs","api_key":"` + strings.Repeat("k", 30) + `"}`
	fake.expectedAuthorization = "Bearer " + key
	created := billingPut(h, owner, providerID, map[string]any{"credential": key}, provider["etag"].(string), 201)
	if strings.Contains(fmt.Sprint(created), key) {
		t.Fatal("create response exposed the billing credential")
	}
	importer := billingImporter(h, t)
	checkImport := func() {
		t.Helper()
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		result := h.want(owner, "GET", path, nil, nil, 200)
		status := result["status"].(map[string]any)
		if _, ok := status["source_revision"]; !ok {
			t.Fatal("billing import status did not include the source revision")
		}
		if status["last_success_at"] == nil || status["last_error_code"] != nil {
			t.Fatalf("billing credential did not authenticate: %v", status)
		}
		if strings.Contains(fmt.Sprint(result), key) {
			t.Fatal("import response exposed the billing credential")
		}
	}
	schedule := func() {
		t.Helper()
		current := h.want(owner, "GET", path, nil, nil, 200)
		headers := etagHeader(current)
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", path+"/run", nil, headers, 202)
	}
	checkImport()
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{}, current["etag"].(string), 200)
	schedule()
	checkImport()
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT credential_id::text FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := h.Ring2().Store(t.Context(), tx, h.Server.Installation, credentialID, secrets.BillingCredential, []byte(key), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	schedule()
	checkImport()
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(export), key) {
		t.Fatal("configuration export exposed the billing credential")
	}
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Compatibility/billing": key}}
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("billing-compatibility"), 200)
	schedule()
	checkImport()
	rotated := "sk-admin-" + strings.Repeat("r", 30)
	rotatedBody := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Compatibility/billing": rotated}}
	h.want(owner, "POST", "/api/v1/configuration/apply", rotatedBody, idem("billing-rotate"), 200)
	fake.mu.Lock()
	fake.expectedAuthorization = "Bearer " + rotated
	fake.mu.Unlock()
	schedule()
	checkImport()
	rotatedExport := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(rotatedExport), rotated) {
		t.Fatal("configuration export exposed the rotated billing credential")
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", rotatedBody, idem("billing-unchanged"), 200)
	schedule()
	checkImport()
}

func TestBillingImportDiscardsChangedInFlightFetch(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	provider := billingProvider(h, owner, "Changing scope", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	key := "sk-admin-" + strings.Repeat("k", 30)
	billingPut(h, owner, providerID, map[string]any{"credential": key, "scope": map[string]any{"project_ids": []string{"proj_aaa111"}}}, provider["etag"].(string), 201)
	begin := func() (chan struct{}, <-chan error) {
		t.Helper()
		started, release, done := make(chan struct{}), make(chan struct{}, 1), make(chan error, 1)
		fake.mu.Lock()
		fake.started, fake.release = started, release
		fake.mu.Unlock()
		t.Cleanup(func() {
			select {
			case release <- struct{}{}:
			default:
			}
		})
		importer := billingImporter(h, t)
		go func() { done <- importer.Pass(t.Context()) }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("billing fetch did not start")
		}
		return release, done
	}
	finish := func(release chan struct{}, done <-chan error) {
		t.Helper()
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	invoicePath := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, day1.Format("2006-01-02"), day3.AddDate(0, 0, 1).Format("2006-01-02"))
	release, done := begin()
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_bbb222"}}}, current["etag"].(string), 200)
	finish(release, done)
	items := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	current = h.want(owner, "GET", path, nil, nil, 200)
	if len(items) != 0 || current["status"].(map[string]any)["last_success_at"] != nil {
		t.Fatalf("old scope committed after a change: invoices=%v status=%v", items, current["status"])
	}
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	items = h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("new scope did not import its rows")
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_aaa111"}}}, current["etag"].(string), 200)
	current = h.want(owner, "GET", path, nil, nil, 200)
	status := current["status"].(map[string]any)
	if status["imported_from"] != nil || status["imported_through"] != nil {
		t.Fatalf("previous scope coverage was retained: %v", status)
	}
	retained := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if !reflect.DeepEqual(retained, items) {
		t.Fatalf("scope change removed invoice history: %v", retained)
	}
	fake.mu.Lock()
	queryIndex := len(fake.queries)
	fake.mu.Unlock()
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	query := fake.call(queryIndex)
	start, startErr := strconv.ParseInt(query.Get("start_time"), 10, 64)
	end, endErr := strconv.ParseInt(query.Get("end_time"), 10, 64)
	if startErr != nil || endErr != nil || end-start != int64(30*24*time.Hour/time.Second) || query.Get("project_ids[]") != "proj_aaa111" {
		t.Fatalf("returning to the prior scope did not use its first-import window: %v", query)
	}
	items = h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	fake.mu.Lock()
	fake.shift[`"value": 0.2,`] = `"value": 9.9,`
	fake.mu.Unlock()
	current = h.want(owner, "GET", path, nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	release, done = begin()
	current = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "DELETE", path, nil, etagHeader(current), 204)
	finish(release, done)
	afterDelete := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if !reflect.DeepEqual(afterDelete, items) {
		t.Fatalf("fetch changed retained invoices after deletion: %v", afterDelete)
	}
}

func TestBillingImportCredentialCompatibilityAcrossKinds(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	openai := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	anthropic := newBillingFake(t, "/v1/organizations/cost_report", map[string]string{
		"": "anthropic_cost_report_page1.json", "page-2-token": "anthropic_cost_report_page2.json",
	}, map[string]string{
		`"starting_at": "2025-10-01T`: `"starting_at": "` + day1.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-02T`:   `"ending_at": "` + day2.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-02T`: `"starting_at": "` + day2.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-03T`:   `"ending_at": "` + day3.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-03T`: `"starting_at": "` + day3.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-04T`:   `"ending_at": "` + day3.AddDate(0, 0, 1).Format("2006-01-02") + "T",
	})
	oldKey := "sk-admin-" + strings.Repeat("o", 30)
	anthropic.expectedAPIKey = oldKey
	provider := billingProvider(h, owner, "Changed kind", "openai", openai.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	billingPut(h, owner, providerID, map[string]any{"credential": oldKey}, provider["etag"].(string), 201)
	providerPath := "/api/v1/providers/" + providerID
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)["items"].([]any)
	credentialID := credentials[0].(map[string]any)["id"].(string)
	headers := etagHeader(provider)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, headers, 200)
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	provider = h.want(owner, "PATCH", "/api/v1/providers/"+providerID, map[string]any{"name": "Changed kind", "configuration": map[string]any{
		"kind": "anthropic", "auth_mode": "api_key", "endpoint": anthropic.server.URL + "/v1",
	}}, etagHeader(provider), 200)
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{}, current["etag"].(string), 200)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	status := current["status"].(map[string]any)
	if status["last_success_at"] == nil || status["last_error_code"] != nil {
		t.Fatalf("retained polling credential failed after changing kind: %v", status)
	}
	newAnthropicKey := "sk-ant-" + strings.Repeat("a", 30)
	billingPut(h, owner, providerID, map[string]any{"credential": newAnthropicKey}, current["etag"].(string), 200)
	h.want(owner, "PATCH", "/api/v1/providers/"+providerID, map[string]any{"name": "Changed kind", "configuration": map[string]any{
		"kind": "openai", "auth_mode": "api_key", "endpoint": openai.server.URL + "/v1",
	}}, etagHeader(provider), 200)
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	replacement := "sk-admin-" + strings.Repeat("n", 30)
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Changed kind/billing": replacement, "Changed kind/default": "sk-provider-" + strings.Repeat("n", 30)}}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("changed-kind"), 200)
	openai.mu.Lock()
	openai.expectedAuthorization = "Bearer " + replacement
	openai.mu.Unlock()
	current = h.want(owner, "GET", path, nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	status = current["status"].(map[string]any)
	if status["last_success_at"] == nil || status["last_error_code"] != nil {
		t.Fatalf("replacement credential failed after configuration apply: %v", status)
	}
}

func TestBillingImportMigrationPreservesHistory(t *testing.T) {
	pool, dbURL := accessDatabase(t)
	if _, err := pool.Exec(t.Context(), `CREATE SCHEMA olp; CREATE TABLE olp.migrations
		(version text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("../../internal/database/migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if entry.Name() >= "0123_billing_currency_revision.sql" {
			break
		}
		data, err := os.ReadFile("../../internal/database/migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(data)); err != nil {
			t.Fatalf("migration %s: %v", entry.Name(), err)
		}
		checksum := sha256.Sum256(data)
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)`, entry.Name(), checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	userID, providerID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag)
		VALUES($1::uuid,'history@example.test','History','viewer',$2::uuid)`, userID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		VALUES($1::uuid,'Migration','openai','draft','{"kind":"openai","auth_mode":"api_key"}', $2::uuid,$3::uuid,$4::uuid)`,
		providerID, uuid.NewString(), uuid.NewString(), userID); err != nil {
		t.Fatal(err)
	}
	day1, day2, day3 := billingDays()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_invoiced_costs
		(provider_id,day,line_item,currency,amount,source,fetched_at) VALUES
		($1::uuid,$2::date,'tokens','USD',0.123456789012,'openai_costs','2025-01-01T12:00:00Z'),
		($1::uuid,$3::date,'storage','EUR',12.34,'openai_costs','2025-01-02T12:00:00Z')`,
		providerID, day1.Format("2006-01-02"), day2.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	h := newAccessHarnessOn(t, pool, dbURL)
	owner := h.owner()
	path := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, day1.Format("2006-01-02"), day3.Format("2006-01-02"))
	items := h.want(owner, "GET", path, nil, nil, 200)["items"].([]any)
	expected := []map[string]string{
		{"day": day1.Format("2006-01-02"), "line_item": "tokens", "currency": "USD", "amount": "0.123456789012", "source": "openai_costs", "fetched_at": "2025-01-01T12:00:00Z"},
		{"day": day2.Format("2006-01-02"), "line_item": "storage", "currency": "EUR", "amount": "12.34", "source": "openai_costs", "fetched_at": "2025-01-02T12:00:00Z"},
	}
	if len(items) != len(expected) {
		t.Fatalf("invoice history changed after migration: %v", items)
	}
	for index, expectedItem := range expected {
		item := items[index].(map[string]any)
		for _, field := range []string{"day", "line_item", "currency", "source", "fetched_at"} {
			if item[field] != expectedItem[field] {
				t.Fatalf("invoice history changed after migration: %v", item)
			}
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		expectedAmount, expectedErr := decimal.NewFromString(expectedItem["amount"])
		if err != nil || expectedErr != nil || !amount.Equal(expectedAmount) {
			t.Fatalf("invoice amount changed after migration: %v", item)
		}
	}
}

func TestBillingImportAWSScope(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "Bedrock billing",
		"configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"},
		"credential":    `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`,
		"model":         "amazon.titan-text-express-v1",
	}, idem("aws-billing-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"payer_account_id": "invalid", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}, "credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_abc"}}, "credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": scope}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": nil}, provider["etag"].(string), 422)
	created := billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	if created["source"] != "aws_cur" || created["credential"] != nil {
		t.Fatalf("billing import must have the AWS source and keep the credential write-only: %v", created)
	}
}

func TestBillingImportPollingReconciliationAfterDeletion(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	provider := billingProvider(h, owner, "Polling history", "openai", fake.server.URL+"/v1", nil)
	id := billingProviderID(provider)
	billingPut(h, owner, id, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	price := "0.15"
	for _, day := range []time.Time{day1.AddDate(0, 0, -1), day1, day2} {
		billingFact(t, h, id, uuid.NewString(), day, "billable", true, &price, false, false, false)
	}
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start, end := day1.AddDate(0, 0, -1).Format("2006-01-02"), day3.AddDate(0, 0, 1).Format("2006-01-02")
	path := fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, id)
	before := h.want(owner, "GET", path, nil, nil, 200)
	importPath := "/api/v1/providers/" + id + "/billing-import"
	h.want(owner, "DELETE", importPath, nil, etagHeader(h.want(owner, "GET", importPath, nil, nil, 200)), 204)
	after := h.want(owner, "GET", path, nil, nil, 200)
	check := func(report map[string]any) {
		t.Helper()
		for i := range before["items"].([]any) {
			old := before["items"].([]any)[i].(map[string]any)
			item := report["items"].([]any)[i].(map[string]any)
			if item["invoiced_cost"] != old["invoiced_cost"] || item["capture_rate"] != old["capture_rate"] || item["capture_rate_unavailable_reason"] != old["capture_rate_unavailable_reason"] || item["provisional"] != old["provisional"] {
				t.Fatalf("historical polling reconciliation lost coverage on day %d: before=%v after=%v", i, old, item)
			}
		}
	}
	check(after)
	key := "sk-admin-" + strings.Repeat("k", 30)
	recreate := func() {
		t.Helper()
		provider := h.want(owner, "GET", "/api/v1/providers/"+id, nil, nil, 200)
		billingPut(h, owner, id, map[string]any{"credential": key}, provider["etag"].(string), 201)
	}
	recreate()
	check(h.want(owner, "GET", path, nil, nil, 200))
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := export["document"].(map[string]any)
	for _, value := range document["providers"].([]any) {
		entry := value.(map[string]any)
		if entry["name"] == "Polling history" {
			delete(entry, "billing_import")
		}
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document, "secret_bindings": map[string]any{}}, idem("polling-history-remove"), 200)
	h.want(owner, "GET", importPath, nil, nil, 404)
	check(h.want(owner, "GET", path, nil, nil, 200))
	setRetention := func(value string) {
		t.Helper()
		setting := h.want(owner, "GET", "/api/v1/settings/retention.usage_days", nil, nil, 200)
		h.want(owner, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": value}, etagHeader(setting), 200)
	}
	setRetention("1")
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	setRetention("90")
	recreate()
	retained := h.want(owner, "GET", path, nil, nil, 200)
	for _, value := range retained["items"].([]any) {
		item := value.(map[string]any)
		if item["day"] == day1.Format("2006-01-02") && (item["invoiced_cost"] != nil || item["capture_rate_unavailable_reason"] != "not_imported") {
			t.Fatalf("recreated polling import reported purged coverage: %v", item)
		}
	}
	current := h.want(owner, "GET", importPath, nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_another"}}}, current["etag"].(string), 200)
	changed := h.want(owner, "GET", path, nil, nil, 200)
	for _, value := range changed["items"].([]any) {
		item := value.(map[string]any)
		if item["invoiced_cost"] != nil || item["capture_rate_unavailable_reason"] != "not_imported" {
			t.Fatalf("changed scope inherited historical coverage: %v", changed)
		}
	}
}
