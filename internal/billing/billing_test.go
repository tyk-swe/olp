package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fixtureServer serves the named fixtures in order, recording requests.
type fixtureServer struct {
	t       *testing.T
	pages   [][]byte
	calls   atomic.Int64
	queries []url.Values
	server  *httptest.Server
}

func newFixtureServer(t *testing.T, path string, pages ...string) *fixtureServer {
	t.Helper()
	s := &fixtureServer{t: t}
	for _, page := range pages {
		s.pages = append(s.pages, readFixture(t, page))
	}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := s.calls.Add(1) - 1
		s.queries = append(s.queries, r.URL.Query())
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if int(call) >= len(s.pages) {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.pages[call])
	}))
	t.Cleanup(s.server.Close)
	return s
}

func TestFetchOpenAIExactDecimals(t *testing.T) {
	server := newFixtureServer(t, "/v1/organization/costs",
		"openai_costs_page1.json", "openai_costs_page2.json")
	endpoint := server.server.URL + "/v1"
	start := time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 3)
	days, err := fetchOpenAI(context.Background(), server.server.Client(), &endpoint,
		[]byte("sk-admin-fixture-key-000000"), Scope{}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if got := days["2025-09-29"]["gpt-4o, input_tokens"].amount.String(); got != "0.3" {
		t.Fatalf("input tokens sum: want exact 0.3, got %s", got)
	}
	if got := days["2025-09-29"]["gpt-4o, output_tokens"].amount.String(); got != "1234.5678901234" {
		t.Fatalf("output tokens: want 1234.5678901234, got %s", got)
	}
	if got := days["2025-09-29"]["unspecified"].amount.String(); got != "5" {
		t.Fatalf("null line_item sums as unspecified, got %s", got)
	}
	if got := days["2025-10-01"]["gpt-4o-mini, input_tokens"].amount.String(); got != "0.0005000001" {
		t.Fatalf("page 2 exact sum, got %s", got)
	}
	// The empty bucket still produces the day so the store records zero.
	if _, ok := days["2025-09-30"]; !ok {
		t.Fatal("empty bucket day is missing")
	}
	if len(days["2025-09-30"]) != 0 {
		t.Fatal("empty bucket day should hold no line items")
	}
	if calls := server.calls.Load(); calls != 2 {
		t.Fatalf("pagination fetched %d pages, want 2", calls)
	}
	if got := server.queries[0].Get("bucket_width"); got != "1d" {
		t.Fatalf("bucket_width = %q", got)
	}
	if got := server.queries[0]["group_by[]"]; len(got) != 1 || got[0] != "line_item" {
		t.Fatalf("group_by[] encoding = %v", got)
	}
	if got := server.queries[1].Get("page"); got != "page-2-token" {
		t.Fatalf("second page token = %q", got)
	}
	if got := days["2025-09-29"]["gpt-4o, input_tokens"].currency; got != "USD" {
		t.Fatalf("currency normalized to %q", got)
	}
}

func TestFetchOpenAIScopeQuery(t *testing.T) {
	server := newFixtureServer(t, "/v1/organization/costs", "openai_costs_page2.json")
	endpoint := server.server.URL + "/v1"
	scope := Scope{ProjectIDs: []string{"proj_aaa111", "proj_bbb222"}}
	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := fetchOpenAI(context.Background(), server.server.Client(), &endpoint,
		[]byte("sk-admin-fixture-key-000000"), scope, start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	got := server.queries[0]["project_ids[]"]
	if len(got) != 2 || got[0] != "proj_aaa111" || got[1] != "proj_bbb222" {
		t.Fatalf("project_ids[] = %v", got)
	}
	if got := server.queries[0].Get("start_time"); got != "1759276800" {
		t.Fatalf("start_time = %q", got)
	}
	if got := server.queries[0].Get("end_time"); got != "1759363200" {
		t.Fatalf("end_time = %q", got)
	}
}

func TestFetchAnthropicCents(t *testing.T) {
	server := newFixtureServer(t, "/v1/organizations/cost_report",
		"anthropic_cost_report_page1.json", "anthropic_cost_report_page2.json")
	endpoint := server.server.URL + "/v1"
	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	days, err := fetchAnthropic(context.Background(), server.server.Client(), &endpoint,
		[]byte("sk-ant-admin-fixture-key-0"), Scope{}, start, start.AddDate(0, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	// 123.45 + 67.89 cents → $1.9134 across both workspaces.
	if got := days["2025-10-01"]["claude-sonnet-4-5 input tokens"].amount.String(); got != "1.9134" {
		t.Fatalf("workspace sum: want 1.9134, got %s", got)
	}
	if got := days["2025-10-01"]["web search requests"].amount.String(); got != "0.1" {
		t.Fatalf("web search: want 0.1, got %s", got)
	}
	if got := days["2025-10-01"]["tokens"].amount.String(); got != "0.0001" {
		t.Fatalf("null description falls back to cost_type: got %s", got)
	}
	if got := days["2025-10-01"]["claude-opus-4-1 output tokens"].amount.String(); got != "1234.5678901234" {
		t.Fatalf("opus cents exact: got %s", got)
	}
	if got := days["2025-10-03"]["unspecified"].amount.String(); got != "0.01" {
		t.Fatalf("null description and cost_type reads unspecified: got %s", got)
	}
	if calls := server.calls.Load(); calls != 2 {
		t.Fatalf("fetched %d pages", calls)
	}
	if got := server.queries[0]["group_by[]"]; len(got) != 2 {
		t.Fatalf("group_by[] = %v", got)
	}
}

func TestFetchAnthropicWorkspaceFilter(t *testing.T) {
	server := newFixtureServer(t, "/v1/organizations/cost_report",
		"anthropic_cost_report_page1.json", "anthropic_cost_report_page2.json")
	endpoint := server.server.URL + "/v1"
	scope := Scope{WorkspaceIDs: []string{"wrkspc_alpha01"}}
	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	days, err := fetchAnthropic(context.Background(), server.server.Client(), &endpoint,
		[]byte("sk-ant-admin-fixture-key-0"), scope, start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := days["2025-10-01"]["claude-sonnet-4-5 input tokens"].amount.String(); got != "1.2345" {
		t.Fatalf("only wrkspc_alpha01 sums: got %s", got)
	}
}

func TestFetchStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{401, "credential"}, {403, "credential"}, {500, "status"}, {418, "status"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		endpoint := server.URL + "/v1"
		_, err := fetchOpenAI(context.Background(), server.Client(), &endpoint,
			[]byte("sk-admin-fixture-key-000000"), Scope{},
			time.Now().UTC(), time.Now().UTC())
		server.Close()
		var fe *fetchError
		if !errors.As(err, &fe) || fe.code != tc.code {
			t.Fatalf("status %d: want code %s, got %v", tc.status, tc.code, err)
		}
	}
}

func TestFetchMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": not-json`))
	}))
	defer server.Close()
	endpoint := server.URL + "/v1"
	_, err := fetchAnthropic(context.Background(), server.Client(), &endpoint,
		[]byte("sk-ant-admin-fixture-key-0"), Scope{}, time.Now().UTC(), time.Now().UTC())
	var fe *fetchError
	if !errors.As(err, &fe) || fe.code != "response" {
		t.Fatalf("malformed response: got %v", err)
	}
}

func TestFetchOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` + strings.Repeat(`{"starting_at":"2025-10-01T00:00:00Z","results":[]},`, maxBodyBytes/50) + `{"starting_at":"2025-10-01T00:00:00Z","results":[]}]}`))
	}))
	defer server.Close()
	endpoint := server.URL + "/v1"
	_, err := fetchAnthropic(context.Background(), server.Client(), &endpoint,
		[]byte("sk-ant-admin-fixture-key-0"), Scope{}, time.Now().UTC(), time.Now().UTC())
	var fe *fetchError
	if !errors.As(err, &fe) || fe.code != "response" {
		t.Fatalf("oversized response: got %v", err)
	}
}

func TestSourceFor(t *testing.T) {
	if s, err := SourceFor("openai"); err != nil || s != SourceOpenAI {
		t.Fatalf("openai: %v", err)
	}
	if s, err := SourceFor("anthropic"); err != nil || s != SourceAnthropic {
		t.Fatalf("anthropic: %v", err)
	}
	if _, err := SourceFor("gemini"); err == nil {
		t.Fatal("gemini must be refused")
	}
}

func TestValidateScope(t *testing.T) {
	for _, tc := range []struct {
		source string
		raw    string
		valid  bool
	}{
		{SourceOpenAI, `{"project_ids":["proj_abc"]}`, true},
		{SourceOpenAI, `{}`, true},
		{SourceOpenAI, `{"project_ids":["x"]}`, false},
		{SourceOpenAI, `{"project_ids":["proj_` + strings.Repeat("a", 64) + `"]}`, true},
		{SourceOpenAI, `{"project_ids":["proj_` + strings.Repeat("a", 65) + `"]}`, false},
		{SourceOpenAI, `{"workspace_ids":["w"]}`, false},
		{SourceOpenAI, `{"unknown":1}`, false},
		{SourceOpenAI, `not json`, false},
		{SourceAnthropic, `{"workspace_ids":["wrkspc_abc","default"]}`, true},
		{SourceAnthropic, `{"workspace_ids":["default"]}`, true},
		{SourceAnthropic, `{"workspace_ids":["wrkspc_"]}`, false},
		{SourceAnthropic, `{"project_ids":["proj_1"]}`, false},
	} {
		_, err := ValidateScope(tc.source, json.RawMessage(tc.raw))
		if tc.valid != (err == nil) {
			t.Fatalf("%s %s: valid=%v err=%v", tc.source, tc.raw, tc.valid, err)
		}
	}
	tooMany := `{"project_ids":[` + strings.Repeat(`"proj_a",`, 101)[:len(`"proj_a",`)*101-1] + `]}`
	if _, err := ValidateScope(SourceOpenAI, json.RawMessage(tooMany)); err == nil {
		t.Fatal("101 project ids must fail")
	}
}

func TestValidCredential(t *testing.T) {
	for _, tc := range []struct {
		credential string
		valid      bool
	}{
		{strings.Repeat("a", 20), true},
		{strings.Repeat("a", 19), false},
		{strings.Repeat("a", 512), true},
		{strings.Repeat("a", 513), false},
		{strings.Repeat("a", 19) + " ", false},
		{strings.Repeat("a", 30) + "\n", false},
		{strings.Repeat("a", 30) + "é", false},
	} {
		if got := ValidCredential(tc.credential) == nil; got != tc.valid {
			t.Fatalf("credential %q: got %v want %v", tc.credential[:10], got, tc.valid)
		}
	}
}

func TestWindow(t *testing.T) {
	today := time.Date(2025, 11, 15, 0, 0, 0, 0, time.UTC)
	start, end := window(nil, today)
	if start.Format(dayFormat) != "2025-10-16" || end.Format(dayFormat) != "2025-11-15" {
		t.Fatalf("first import window %s..%s", start.Format(dayFormat), end.Format(dayFormat))
	}
	through := "2025-11-10"
	start, _ = window(&through, today)
	if start.Format(dayFormat) != "2025-11-08" {
		t.Fatalf("refresh window start %s", start.Format(dayFormat))
	}
}

func TestDecimalValue(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`0.1`, "0.1"},
		{`"1234.5678901234"`, "1234.5678901234"},
		{`1234.5678901234`, "1234.5678901234"},
		{`0`, "0"},
	} {
		v, err := decimalValue(json.RawMessage(tc.raw))
		if err != nil || v.String() != tc.want {
			t.Fatalf("%s: got %s, %v", tc.raw, v, err)
		}
	}
	if _, err := decimalValue(json.RawMessage(`"abc"`)); err == nil {
		t.Fatal("non-decimal must fail")
	}
	if _, err := decimalValue(json.RawMessage(`null`)); err == nil {
		t.Fatal("null must fail")
	}
}

func TestEndpointURL(t *testing.T) {
	for _, tc := range []struct {
		base, path, want string
	}{
		{"https://api.openai.com/v1", "/organization/costs", "https://api.openai.com/v1/organization/costs"},
		{"https://api.anthropic.com/v1", "/v1/organizations/cost_report", "https://api.anthropic.com/v1/organizations/cost_report"},
		{"https://api.anthropic.com", "/v1/organizations/cost_report", "https://api.anthropic.com/v1/organizations/cost_report"},
		{"http://127.0.0.1:8080/v1/", "/v1/organizations/cost_report", "http://127.0.0.1:8080/v1/organizations/cost_report"},
	} {
		got, err := endpointURL(tc.base, tc.path)
		if err != nil || got != tc.want {
			t.Fatalf("%s + %s = %s (%v), want %s", tc.base, tc.path, got, err, tc.want)
		}
	}
}
