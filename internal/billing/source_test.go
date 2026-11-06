package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	return fetchImportedThrough(t, source, endpoint, nil)
}

func fetchImportedThrough(t *testing.T, source, endpoint string, through *string) []periodDays {
	t.Helper()
	adapter, ok := lookupSource(source)
	if !ok {
		t.Fatalf("source %s missing", source)
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	importer := &Importer{Egress: &policy}
	row := &importRow{ProviderID: "00000000-0000-0000-0000-000000000001", Source: source, Scope: json.RawMessage(`{}`), ImportedThrough: through}
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

func TestPollingImportBackfillsFromLastImportedDay(t *testing.T) {
	lastImported := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -21)
	through := lastImported.Format(dayFormat)
	for _, test := range []struct {
		source string
		start  func(url.Values) (time.Time, error)
	}{
		{SourceOpenAI, func(q url.Values) (time.Time, error) {
			unix, err := strconv.ParseInt(q.Get("start_time"), 10, 64)
			return time.Unix(unix, 0), err
		}},
		{SourceAnthropic, func(q url.Values) (time.Time, error) {
			return time.Parse(time.RFC3339, q.Get("starting_at"))
		}},
	} {
		t.Run(test.source, func(t *testing.T) {
			queries := make(chan url.Values, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries <- r.URL.Query()
				_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
			}))
			defer server.Close()
			fetchImportedThrough(t, test.source, server.URL, &through)
			start, err := test.start(<-queries)
			if err != nil {
				t.Fatal(err)
			}
			if !start.Equal(lastImported) {
				t.Fatalf("import started at %s, want the last imported day %s", start.UTC(), lastImported)
			}
		})
	}
}

func TestPollingImportStillRefreshesTheLastWeek(t *testing.T) {
	today := time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1).Format(dayFormat)
	start, end, err := window(&yesterday, today)
	if err != nil {
		t.Fatal(err)
	}
	if want := today.AddDate(0, 0, -7); !start.Equal(want) || !end.Equal(today) {
		t.Fatalf("window is %s to %s, want %s to %s", start, end, want, today)
	}
}

func TestPollingBackfillCheckpointsAndResumesBeyondPageBudget(t *testing.T) {
	today := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	first := today.AddDate(0, 0, -621)
	for _, source := range []string{SourceOpenAI, SourceAnthropic} {
		t.Run(source, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				parseDay := func(unixKey, dateKey string) (time.Time, error) {
					if source == SourceAnthropic {
						return time.Parse(time.RFC3339, q.Get(dateKey))
					}
					unix, err := strconv.ParseInt(q.Get(unixKey), 10, 64)
					return time.Unix(unix, 0).UTC(), err
				}
				start, startErr := parseDay("start_time", "starting_at")
				end, endErr := parseDay("end_time", "ending_at")
				limit, limitErr := strconv.Atoi(q.Get("limit"))
				if startErr != nil || endErr != nil || limitErr != nil || limit < 1 {
					t.Error("invalid report window or limit")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				offset := 0
				if page := q.Get("page"); page != "" {
					parsed, err := strconv.Atoi(page)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					offset = parsed
				}
				type bucket struct {
					StartTime  int64      `json:"start_time"`
					StartingAt string     `json:"starting_at"`
					Results    []struct{} `json:"results"`
				}
				var data []bucket
				for n := 0; n < limit && start.AddDate(0, 0, offset+n).Before(end); n++ {
					day := start.AddDate(0, 0, offset+n)
					data = append(data, bucket{StartTime: day.Unix(), StartingAt: day.Format(time.RFC3339), Results: []struct{}{}})
				}
				more := start.AddDate(0, 0, offset+len(data)).Before(end)
				if err := json.NewEncoder(w).Encode(struct {
					Data     []bucket `json:"data"`
					HasMore  bool     `json:"has_more"`
					NextPage string   `json:"next_page"`
				}{data, more, strconv.Itoa(offset + len(data))}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			importer := &Importer{Egress: &policy, Now: func() time.Time { return today }}
			adapter, ok := lookupSource(source)
			if !ok {
				t.Fatal("source missing")
			}
			run := func(ctx context.Context, through string, emit func(periodDays) error) error {
				return adapter.fetch(ctx, &fetchInput{
					importer: importer,
					row:      &importRow{ProviderID: "00000000-0000-0000-0000-000000000001", Source: source, Scope: json.RawMessage(`{}`), ImportedThrough: &through},
					cfg:      &providerConfig{Endpoint: &server.URL},
					key:      []byte(pollingKey),
				}, emit)
			}
			saved := reportAccumulator{}
			checkpoint := first.Format(dayFormat)
			save := func(period periodDays) error {
				if len(period.days) == 0 {
					t.Fatal("report emitted no covered days")
				}
				maps.Copy(saved, period.days)
				covered := slices.Sorted(maps.Keys(period.days))
				checkpoint = covered[len(covered)-1]
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := run(ctx, checkpoint, func(period periodDays) error {
				if err := save(period); err != nil {
					return err
				}
				cancel()
				return nil
			})
			if !errors.Is(err, context.Canceled) || checkpoint <= first.Format(dayFormat) || checkpoint >= today.AddDate(0, 0, -1).Format(dayFormat) {
				t.Fatalf("interrupted backfill saved through %s with error %v", checkpoint, err)
			}
			if err := run(context.Background(), checkpoint, save); err != nil {
				t.Fatalf("resuming backfill: %v", err)
			}
			for day := first; day.Before(today); day = day.AddDate(0, 0, 1) {
				if _, ok := saved[day.Format(dayFormat)]; !ok {
					t.Fatalf("backfill skipped %s", day.Format(dayFormat))
				}
			}
			if checkpoint != today.AddDate(0, 0, -1).Format(dayFormat) {
				t.Fatalf("backfill ended at %s", checkpoint)
			}
		})
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

func TestCloudBillingSourcesAgreeOnCurrencyCodes(t *testing.T) {
	day := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		currency string
		valid    bool
	}{
		{"ABC", true}, {"abc", true}, {"aBc", true},
		{"", false}, {"AB", false}, {"ABCD", false}, {"A1C", false}, {" A ", false}, {"ıab", false},
	}
	for _, tc := range cases {
		t.Run(tc.currency, func(t *testing.T) {
			azureDays := reportAccumulator{}
			data := "Date,SubscriptionId,MeterCategory,MeterSubCategory,MeterId,ChargeType,BillingCurrencyCode,CostInBillingCurrency\n" +
				day.Format(dayFormat) + ",subscription,Cognitive Services,Azure Open AI,meter,usage," + tc.currency + ",1\n"
			_, azureErr := new(azureReader).csv(strings.NewReader(data), azureDays, day, day, 2, 2)
			googleDays := reportAccumulator{}
			googleErr := addGoogleRow(googleDays, googleInvoiceRow{
				Day: day.Format(dayFormat), Service: googleVertexServiceID, SKU: "sku", Currency: tc.currency, Amount: "1", Kind: "cost",
			}, day, day.AddDate(0, 0, 1))
			if (azureErr == nil) != tc.valid || (googleErr == nil) != tc.valid {
				t.Fatalf("Azure error = %v, Google error = %v", azureErr, googleErr)
			}
			if !tc.valid {
				return
			}
			for _, days := range []reportAccumulator{azureDays, googleDays} {
				lines := days[day.Format(dayFormat)]
				if len(lines) != 1 {
					t.Fatalf("got %d lines, want one", len(lines))
				}
				for key, value := range lines {
					if key.currency != strings.ToUpper(tc.currency) || !value.amount.Equal(decimal.NewFromInt(1)) {
						t.Fatalf("unexpected normalized line %v: %v", key, value)
					}
				}
			}
		})
	}
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

func TestPollingFetchRejectsIncompletePagination(t *testing.T) {
	for _, source := range []struct {
		name string
		data string
	}{
		{SourceOpenAI, `[{"start_time":1893542400,"results":[{"amount":{"value":"1","currency":"USD"}}]}]`},
		{SourceAnthropic, `[{"starting_at":"2030-01-02T00:00:00Z","results":[{"amount":"100","currency":"USD"}]}]`},
	} {
		for _, cursor := range []string{"", `,"next_page":""`, `,"next_page":null`} {
			t.Run(source.name+"/"+cursor, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprintf(w, `{"data":%s,"has_more":true%s}`, source.data, cursor)
				}))
				defer server.Close()
				adapter, ok := lookupSource(source.name)
				if !ok {
					t.Fatal("source missing")
				}
				policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
				in := &fetchInput{
					importer: &Importer{Egress: &policy},
					row:      &importRow{ProviderID: "00000000-0000-0000-0000-000000000001", Source: source.name, Scope: json.RawMessage(`{}`)},
					cfg:      &providerConfig{Endpoint: &server.URL},
					key:      []byte(pollingKey),
				}
				err := adapter.fetch(context.Background(), in, func(periodDays) error {
					return errors.New("incomplete report emitted billing days")
				})
				if err == nil || fetchCode(err) != "response" {
					t.Fatalf("got %v, want a response failure before emitting billing days", err)
				}
			})
		}
	}
}
