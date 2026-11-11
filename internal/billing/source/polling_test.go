package source_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/management/contract"
)

func readPolling(ctx context.Context, t *testing.T, kind source.Kind, server *httptest.Server, req source.Polling, emit func(source.Period) error) error {
	t.Helper()
	imp, err := lookup(t, kind).Open(json.RawMessage(`{}`), []byte(pollingKey))
	if err != nil {
		t.Fatal(err)
	}
	req.Endpoint = server.URL
	req.Client = func(string) (*http.Client, error) { return server.Client(), nil }
	resume, err := imp.Read(ctx, pollingInputs(req), emit)
	if resume != nil {
		t.Fatalf("polling read returned a resume cursor %+v", resume)
	}
	return err
}

type pollingInputs source.Polling

func (in pollingInputs) Polling(context.Context) (source.Polling, error) {
	return source.Polling(in), nil
}

func (pollingInputs) Cloud(context.Context) (source.Cloud, error) {
	return source.Cloud{}, errors.New("a polling source asked for cloud inputs")
}

func collectPolling(t *testing.T, kind source.Kind, server *httptest.Server, req source.Polling) []source.Period {
	t.Helper()
	var periods []source.Period
	if err := readPolling(context.Background(), t, kind, server, req, func(period source.Period) error {
		periods = append(periods, period)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return periods
}

func fixtureServer(t *testing.T, page1, page2 string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(server.Close)
	return server
}

func wantLines(t *testing.T, days source.Days, day string, want map[string]string) {
	t.Helper()
	parsed, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	lines := days.Lines(parsed)
	if len(lines) != len(want) {
		t.Fatalf("day %s has lines %v, want %v", day, lines, want)
	}
	for _, line := range lines {
		amount, err := decimal.NewFromString(want[line.Item])
		if err != nil || line.Currency != "USD" || !line.Amount.Equal(amount) {
			t.Fatalf("day %s line %+v, want %s USD", day, line, want[line.Item])
		}
	}
}

func onlyPeriod(t *testing.T, periods []source.Period) source.Days {
	t.Helper()
	if len(periods) != 1 || periods[0].Revision != nil {
		t.Fatalf("got %d periods, want one without revision", len(periods))
	}
	return periods[0].Days
}

func TestOpenAIReportReadsEveryPageIntoExactDailyLines(t *testing.T) {
	days := onlyPeriod(t, collectPolling(t, contract.OpenaiCosts, fixtureServer(t, "openai_costs_page1.json", "openai_costs_page2.json"), source.Polling{Today: time.Now()}))
	wantLines(t, days, "2025-09-29", map[string]string{"gpt-4o, input_tokens": "0.3", "gpt-4o, output_tokens": "1234.5678901234", "unspecified": "5"})
	wantLines(t, days, "2025-09-30", map[string]string{})
	wantLines(t, days, "2025-10-01", map[string]string{"gpt-4o-mini, input_tokens": "0.0005000001"})
}

func TestAnthropicReportReadsEveryPageIntoExactDailyLines(t *testing.T) {
	days := onlyPeriod(t, collectPolling(t, contract.AnthropicCostReport, fixtureServer(t, "anthropic_cost_report_page1.json", "anthropic_cost_report_page2.json"), source.Polling{Today: time.Now()}))
	wantLines(t, days, "2025-10-01", map[string]string{
		"claude-sonnet-4-5 input tokens": "1.9134", "web search requests": "0.1", "tokens": "0.0001", "claude-opus-4-1 output tokens": "1234.5678901234",
	})
	wantLines(t, days, "2025-10-02", map[string]string{})
	wantLines(t, days, "2025-10-03", map[string]string{"code execution tool calls": "0.001", "unspecified": "0.01"})
}

var pollingWindows = map[source.Kind]func(url.Values, string) (time.Time, error){
	contract.OpenaiCosts: func(q url.Values, edge string) (time.Time, error) {
		unix, err := strconv.ParseInt(q.Get(map[string]string{"start": "start_time", "end": "end_time"}[edge]), 10, 64)
		return time.Unix(unix, 0).UTC(), err
	},
	contract.AnthropicCostReport: func(q url.Values, edge string) (time.Time, error) {
		return time.Parse(time.RFC3339, q.Get(map[string]string{"start": "starting_at", "end": "ending_at"}[edge]))
	},
}

func TestPollingImportBackfillsFromTheLastImportedDay(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	through := today.AddDate(0, 0, -21)
	for kind, parse := range pollingWindows {
		t.Run(string(kind), func(t *testing.T) {
			queries := make(chan url.Values, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries <- r.URL.Query()
				_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
			}))
			defer server.Close()
			collectPolling(t, kind, server, source.Polling{Today: today, ImportedThrough: &through})
			start, err := parse(<-queries, "start")
			if err != nil || !start.Equal(through) {
				t.Fatalf("import started at %s (%v), want the last imported day %s", start, err, through)
			}
		})
	}
}

func TestPollingBackfillCheckpointsAndResumesBeyondPageBudget(t *testing.T) {
	today := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	first := today.AddDate(0, 0, -621)
	for kind, parse := range pollingWindows {
		t.Run(string(kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				start, startErr := parse(q, "start")
				end, endErr := parse(q, "end")
				limit, limitErr := strconv.Atoi(q.Get("limit"))
				offset, offsetErr := strconv.Atoi(cmpOr(q.Get("page"), "0"))
				if startErr != nil || endErr != nil || limitErr != nil || offsetErr != nil || limit < 1 {
					t.Error("invalid report window, limit or page")
					w.WriteHeader(http.StatusBadRequest)
					return
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
			saved := map[time.Time]bool{}
			checkpoint := first
			save := func(period source.Period) error {
				covered := period.Days.Days()
				if len(covered) == 0 {
					t.Fatal("report emitted no covered days")
				}
				for _, day := range covered {
					saved[day] = true
				}
				checkpoint = covered[len(covered)-1]
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := readPolling(ctx, t, kind, server, source.Polling{Today: today, ImportedThrough: &checkpoint}, func(period source.Period) error {
				cancel()
				return save(period)
			})
			if !errors.Is(err, context.Canceled) || !checkpoint.After(first) || !checkpoint.Before(today.AddDate(0, 0, -1)) {
				t.Fatalf("interrupted backfill saved through %s with error %v", checkpoint, err)
			}
			resumed := checkpoint
			if err := readPolling(context.Background(), t, kind, server, source.Polling{Today: today, ImportedThrough: &resumed}, save); err != nil {
				t.Fatalf("resuming backfill: %v", err)
			}
			for day := first; day.Before(today); day = day.AddDate(0, 0, 1) {
				if !saved[day] {
					t.Fatalf("backfill skipped %s", day.Format(time.DateOnly))
				}
			}
			if !checkpoint.Equal(today.AddDate(0, 0, -1)) {
				t.Fatalf("backfill ended at %s", checkpoint)
			}
		})
	}
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func TestPollingImportFailsWithoutEmittingOnRejectedKeysAndBrokenPagination(t *testing.T) {
	pages := map[source.Kind]string{
		contract.OpenaiCosts:         `[{"start_time":1893542400,"results":[{"amount":{"value":"1","currency":"%s"}}]}]`,
		contract.AnthropicCostReport: `[{"starting_at":"2030-01-02T00:00:00Z","results":[{"amount":"100","currency":"%s"}]}]`,
	}
	for kind, page := range pages {
		data := fmt.Sprintf(page, "USD")
		for name, tc := range map[string]struct {
			respond func(http.ResponseWriter)
			want    source.Code
		}{
			"unauthorized": {func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, source.CodeCredential},
			"no cursor":    {func(w http.ResponseWriter) { _, _ = fmt.Fprintf(w, `{"data":%s,"has_more":true}`, data) }, source.CodeResponse},
			"empty cursor": {func(w http.ResponseWriter) { _, _ = fmt.Fprintf(w, `{"data":%s,"has_more":true,"next_page":""}`, data) }, source.CodeResponse},
			"null cursor": {func(w http.ResponseWriter) {
				_, _ = fmt.Fprintf(w, `{"data":%s,"has_more":true,"next_page":null}`, data)
			}, source.CodeResponse},
			"invalid currency": {func(w http.ResponseWriter) {
				_, _ = fmt.Fprintf(w, `{"data":%s,"has_more":false}`, fmt.Sprintf(page, "US1"))
			}, source.CodeResponse},
		} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.respond(w) }))
				defer server.Close()
				err := readPolling(context.Background(), t, kind, server, source.Polling{Today: time.Date(2030, 1, 10, 0, 0, 0, 0, time.UTC)}, func(source.Period) error {
					return errors.New("failed report emitted billing days")
				})
				if source.CodeOf(err) != tc.want {
					t.Fatalf("got %v (%s), want a %s failure before emitting billing days", err, source.CodeOf(err), tc.want)
				}
			})
		}
	}
}
