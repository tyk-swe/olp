package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/egress"
)

func pollingFixtureServer(t *testing.T, page1, page2 string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := page1
		if r.URL.Query().Get("page") == "page-2-token" {
			name = page2
		}
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
}

func fetchThroughSource(t *testing.T, source, endpoint string) []periodDays {
	t.Helper()
	adapter, ok := lookupSource(source)
	if !ok {
		t.Fatalf("source %s missing", source)
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	importer := &Importer{Egress: &policy}
	row := &importRow{ProviderID: "00000000-0000-0000-0000-000000000001", Source: source, Scope: json.RawMessage(`{}`)}
	cfg := &providerConfig{Endpoint: &endpoint}
	var periods []periodDays
	err := adapter.fetch(context.Background(), &fetchInput{importer: importer, row: row, cfg: cfg, key: []byte(pollingKey)}, func(period periodDays) error {
		periods = append(periods, period)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return periods
}

func wantAmount(t *testing.T, days reportAccumulator, day, item, want string) {
	t.Helper()
	sum, ok := days[day][lineKey{item: item, currency: "USD"}]
	if !ok {
		t.Fatalf("day %s missing line %q in %v", day, item, days[day])
	}
	expected, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.amount.Equal(expected) {
		t.Fatalf("day %s line %q: got %s, want %s", day, item, sum.amount, expected)
	}
}

func TestOpenAIFetchEmitsDays(t *testing.T) {
	server := pollingFixtureServer(t, "openai_costs_page1.json", "openai_costs_page2.json")
	defer server.Close()
	periods := fetchThroughSource(t, SourceOpenAI, server.URL)
	if len(periods) != 1 || periods[0].revision != nil || periods[0].start != "" {
		t.Fatalf("got %d periods, want one without revision", len(periods))
	}
	days := periods[0].days
	wantAmount(t, days, "2025-09-29", "gpt-4o, input_tokens", "0.3")
	wantAmount(t, days, "2025-09-29", "gpt-4o, output_tokens", "1234.5678901234")
	wantAmount(t, days, "2025-09-29", "unspecified", "5")
	if lines, ok := days["2025-09-30"]; !ok || len(lines) != 0 {
		t.Fatalf("empty bucket day missing or non-empty: %v", days["2025-09-30"])
	}
	wantAmount(t, days, "2025-10-01", "gpt-4o-mini, input_tokens", "0.0005000001")
}

func TestAnthropicFetchEmitsDays(t *testing.T) {
	server := pollingFixtureServer(t, "anthropic_cost_report_page1.json", "anthropic_cost_report_page2.json")
	defer server.Close()
	periods := fetchThroughSource(t, SourceAnthropic, server.URL)
	if len(periods) != 1 || periods[0].revision != nil || periods[0].start != "" {
		t.Fatalf("got %d periods, want one without revision", len(periods))
	}
	days := periods[0].days
	wantAmount(t, days, "2025-10-01", "claude-sonnet-4-5 input tokens", "1.9134")
	wantAmount(t, days, "2025-10-01", "web search requests", "0.1")
	wantAmount(t, days, "2025-10-01", "tokens", "0.0001")
	wantAmount(t, days, "2025-10-01", "claude-opus-4-1 output tokens", "1234.5678901234")
	if lines, ok := days["2025-10-02"]; !ok || len(lines) != 0 {
		t.Fatalf("empty bucket day missing or non-empty: %v", days["2025-10-02"])
	}
	wantAmount(t, days, "2025-10-03", "code execution tool calls", "0.001")
	wantAmount(t, days, "2025-10-03", "unspecified", "0.01")
}

func TestPollingFetchRejectsCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	adapter, ok := lookupSource(SourceOpenAI)
	if !ok {
		t.Fatal("source missing")
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	endpoint := server.URL
	in := &fetchInput{
		importer: &Importer{Egress: &policy},
		row:      &importRow{ProviderID: "00000000-0000-0000-0000-000000000001", Source: SourceOpenAI, Scope: json.RawMessage(`{}`)},
		cfg:      &providerConfig{Endpoint: &endpoint},
		key:      []byte(pollingKey),
	}
	emitted := false
	err := adapter.fetch(context.Background(), in, func(periodDays) error {
		emitted = true
		return nil
	})
	if err == nil || fetchCode(err) != "credential" {
		t.Fatalf("got %v, want credential failure", err)
	}
	if emitted {
		t.Fatal("failed fetch emitted days")
	}
}
