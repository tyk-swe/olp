package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type reportFetcher struct {
	name  string
	fetch func(context.Context, *http.Client, *string, []byte, Scope, time.Time, time.Time) (map[string]map[string]lineSum, error)
	// Each provider keeps its own wire format and monetary unit.
	bucket string
	rows   []string
}

var reportFetchers = []reportFetcher{
	{
		name:   "openai",
		fetch:  fetchOpenAI,
		bucket: `"start_time":1759276800`,
		rows: []string{
			`{"amount":{"value":0.1,"currency":"usd"},"line_item":%s},{"amount":{"value":"3","currency":""},"line_item":null}`,
			`{"amount":{"value":"0.2","currency":"eur-extra"},"line_item":%s},{"amount":{"value":-1,"currency":""},"line_item":""}`,
			`{"amount":{"value":0,"currency":""},"line_item":%s}`,
		},
	},
	{
		name:   "anthropic",
		fetch:  fetchAnthropic,
		bucket: `"starting_at":"2025-10-01T00:00:00Z"`,
		rows: []string{
			`{"amount":"10","currency":"usd","description":%s,"cost_type":"ignored"},{"amount":"300","currency":"","description":null,"cost_type":""}`,
			`{"amount":"20","currency":"eur-extra","description":"","cost_type":%s},{"amount":"-100","currency":"","description":null,"cost_type":null}`,
			`{"amount":"0","currency":"","description":%s}`,
		},
	},
}

func (f reportFetcher) page(rows string, more bool, next string) string {
	return fmt.Sprintf(`{"data":[{%s,"results":[%s]}],"has_more":%t,"next_page":%q}`, f.bucket, rows, more, next)
}

func TestReportAccumulationAcrossPages(t *testing.T) {
	for _, provider := range reportFetchers {
		t.Run(provider.name, func(t *testing.T) {
			label := strings.Repeat("界", 255) + "🌍"
			var queries []url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := len(queries)
				queries = append(queries, r.URL.Query())
				if page >= len(provider.rows) {
					t.Error("unexpected extra page")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				// Distinct suffixes collide after truncation to 256 Unicode characters.
				encodedLabel, _ := json.Marshal(label + strings.Repeat("x", page))
				rows := fmt.Sprintf(provider.rows[page], encodedLabel)
				fmt.Fprint(w, provider.page(rows, page < 2, fmt.Sprintf("cursor %d/+", page+1)))
			}))
			defer server.Close()
			endpoint := server.URL
			days, err := provider.fetch(t.Context(), server.Client(), &endpoint, nil,
				Scope{ProjectIDs: []string{"proj_one", "proj_two"}}, time.Now(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			lines := days["2025-10-01"]
			if len(days) != 1 || len(lines) != 2 {
				t.Fatalf("unexpected days or labels: %v", days)
			}
			if got := lines[label]; got.amount.String() != "0.3" || got.currency != "EUR" {
				t.Fatalf("truncated label sum = %s %q, want 0.3 EUR", got.amount, got.currency)
			}
			if got := lines["unspecified"]; got.amount.String() != "2" || got.currency != "" {
				t.Fatalf("unspecified sum = %s %q, want 2 with blank currency", got.amount, got.currency)
			}
			if len(queries) != 3 || queries[0].Has("page") {
				t.Fatalf("initial pagination: %v", queries)
			}
			for i := 1; i < len(queries); i++ {
				if got := queries[i].Get("page"); got != fmt.Sprintf("cursor %d/+", i) {
					t.Fatalf("page %d cursor = %q", i, got)
				}
				queries[i].Del("page")
				if !reflect.DeepEqual(queries[0], queries[i]) {
					t.Fatalf("page %d changed base query: %v != %v", i, queries[i], queries[0])
				}
			}
		})
	}
}

// Invalid amounts on excluded workspaces must not be parsed.
