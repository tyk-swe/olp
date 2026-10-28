package search

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/operations"
)

func TestMappingLowerRefusals(t *testing.T) {
	for _, tc := range []struct{ dialect, body string }{
		{Tavily, `{"query":"q","search_domain_filter":["a.com"]}`},
		{Tavily, `{"query":"q","search_type":"fast"}`},
		{Tavily, `{"query":"q","country":"US"}`},
		{Exa, `{"query":"q","search_domain_filter":[".com"]}`},
		{Exa, `{"query":"q","search_domain_filter":["a.com/path"]}`},
		{Exa, `{"query":"q","search_domain_filter":["dotless"]}`},
		{Parallel, `{"query":"q","search_domain_filter":["dotless"]}`},
		{Tavily, `{"query":"q","search_recency_filter":"week"}`},
		{Exa, `{"query":"q","max_tokens":5}`},
		{Parallel, `{"query":"q","search_language_filter":["en"]}`},
	} {
		r := request(t, Perplexity, tc.body)
		view, err := parseRequest(r, Perplexity)
		if err != nil {
			t.Fatal(err)
		}
		var mapping operations.Mapping
		for _, mm := range Mappings() {
			if mm.Target.ID == tc.dialect {
				mapping = mm
			}
		}
		if _, _, err := mapping.Lower(r, view); err == nil {
			t.Fatalf("%s admitted %s", tc.dialect, tc.body)
		}
	}
}

func TestMappingProject(t *testing.T) {
	r := request(t, Perplexity, `{"query":"q","max_results":2}`)
	cases := []struct {
		dialect, body, snippet, date string
	}{
		{Tavily, `{"query":"q","results":[{"title":"t","url":"u1","content":"c","score":0.9},{"title":null,"url":"u2"}],"response_time":1.0,"request_id":"r"}`, "c", ""},
		{Exa, `{"requestId":"r","results":[{"title":"t","url":"u1","publishedDate":"2026-01-02T03:04:05Z","highlights":["a","b"]}],"costDollars":0.01}`, "a\n...\nb", "2026-01-02"},
		{Parallel, `{"search_id":"s","results":[{"url":"u1","title":"t","publish_date":"2026-01-02","excerpts":["x","y"]},{"url":"u2","publish_date":"not-a-date"}],"session_id":"ss"}`, "x\n...\ny", "2026-01-02"},
	}
	for _, tc := range cases {
		var mapping operations.Mapping
		for _, mm := range Mappings() {
			if mm.Target.ID == tc.dialect {
				mapping = mm
			}
		}
		res := result(t, tc.dialect, tc.body)
		view, err := parseResult(request(t, tc.dialect, resBodyFor(t, tc.dialect)), res, tc.dialect)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := mapping.Project(r, res, view, "route")
		if err != nil {
			t.Fatalf("project %s: %v", tc.dialect, err)
		}
		var out struct {
			ID      string `json:"id"`
			Results []struct {
				Title   string  `json:"title"`
				URL     string  `json:"url"`
				Snippet string  `json:"snippet"`
				Date    *string `json:"date"`
				Last    *string `json:"last_updated"`
			} `json:"results"`
		}
		if json.Unmarshal(doc.Bytes(), &out) != nil {
			t.Fatal("bad projection")
		}
		if !strings.HasPrefix(out.ID, "search_") || out.ID == "s" && tc.dialect == Parallel || strings.Contains(out.ID, "r") && len(out.ID) != 7+32 {
			t.Fatalf("upstream id leaked: %q", out.ID)
		}
		if out.Results[0].Snippet != tc.snippet {
			t.Fatalf("%s snippet: %+v", tc.dialect, out.Results[0])
		}
		if tc.dialect == Exa || tc.dialect == Parallel {
			if out.Results[0].Date == nil || *out.Results[0].Date != tc.date {
				t.Fatalf("%s date: %+v", tc.dialect, out.Results[0])
			}
		}
		if tc.dialect == Parallel && out.Results[1].Date != nil {
			t.Fatalf("bad date admitted: %+v", out.Results[1])
		}
		if tc.dialect == Tavily && (out.Results[0].Date != nil || out.Results[1].Title != "") {
			t.Fatalf("tavily projection: %+v", out.Results)
		}
	}
	// Row count beyond the request is a violation.
	var mapping operations.Mapping
	for _, mm := range Mappings() {
		if mm.Target.ID == Tavily {
			mapping = mm
		}
	}
	res := result(t, Tavily, `{"results":[{"url":"u1"},{"url":"u2"},{"url":"u3"}]}`)
	view, err := parseResult(request(t, Tavily, resBodyFor(t, Tavily)), res, Tavily)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapping.Project(r, res, view, "route"); err == nil {
		t.Fatal("over-limit rows projected")
	}
}

func mappingFor(dialect string) operations.Mapping {
	for _, mm := range Mappings() {
		if mm.Target.ID == dialect {
			return mm
		}
	}
	return operations.Mapping{}
}

func TestBraveMappingLower(t *testing.T) {
	mapping := mappingFor(Brave)
	for _, tc := range []struct {
		body string
		want map[string]any
	}{
		{`{"query":"q"}`, map[string]any{"q": "q", "count": float64(10), "result_filter": "web"}},
		{`{"query":"q","max_results":3}`, map[string]any{"q": "q", "count": float64(3), "result_filter": "web"}},
		{`{"query":"q","max_results":2,"search_type":"web","search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026"}`, map[string]any{"q": "q", "count": float64(2), "result_filter": "web", "freshness": "2026-01-02to2026-03-04"}},
	} {
		r := request(t, Perplexity, tc.body)
		view, err := parseRequest(r, Perplexity)
		if err != nil {
			t.Fatal(err)
		}
		doc, _, err := mapping.Lower(r, view)
		if err != nil {
			t.Fatalf("lower %s: %v", tc.body, err)
		}
		var got map[string]any
		if json.Unmarshal(doc.Bytes(), &got) != nil {
			t.Fatal("bad lowered document")
		}
		if len(got) != len(tc.want) {
			t.Fatalf("lowered %v, want %v", got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("lowered %v, want %v", got, tc.want)
			}
		}
	}
	for _, body := range []string{
		`{"query":"q","search_domain_filter":["a.com"]}`,
		`{"query":"q","search_domain_filter":["-a.com"]}`,
		`{"query":"q","search_after_date_filter":"01/02/2026"}`,
		`{"query":"q","search_before_date_filter":"03/04/2026"}`,
		`{"query":"q","search_after_date_filter":"2026-01-02","search_before_date_filter":"03/04/2026"}`,
		`{"query":"q","search_after_date_filter":"02/30/2026","search_before_date_filter":"03/04/2026"}`,
		`{"query":"q","search_after_date_filter":"03/04/2026","search_before_date_filter":"01/02/2026"}`,
		`{"query":"q","search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026","search_recency_filter":"week"}`,
		`{"query":"q","last_updated_after_filter":"01/01/2026"}`,
		`{"query":"q","search_recency_filter":"week"}`,
		`{"query":"q","country":"US"}`,
		`{"query":"q","search_language_filter":["en"]}`,
		`{"query":"q","max_tokens_per_page":5}`,
	} {
		r := request(t, Perplexity, body)
		view, err := parseRequest(r, Perplexity)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := mapping.Lower(r, view); err == nil {
			t.Fatalf("brave admitted %s", body)
		}
	}
}

func TestBraveMappingProject(t *testing.T) {
	r := request(t, Perplexity, `{"query":"q","max_results":2}`)
	mapping := mappingFor(Brave)
	body := `{"type":"search","query":{"original":"q"},"web":{"type":"search","results":[{"title":"t","url":"u1","description":"d1","page_age":"2026-01-02T03:04:05Z"},{"url":"u2"}],"family_friendly":true}}`
	res := result(t, Brave, body)
	view, err := parseResult(request(t, Brave, resBodyFor(t, Brave)), res, Brave)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := mapping.Project(r, res, view, "route")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID      string `json:"id"`
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Snippet string  `json:"snippet"`
			Date    *string `json:"date"`
		} `json:"results"`
	}
	if json.Unmarshal(doc.Bytes(), &out) != nil || !strings.HasPrefix(out.ID, "search_") {
		t.Fatalf("bad projection id: %q", out.ID)
	}
	if len(out.Results) != 2 || out.Results[0].Snippet != "d1" || out.Results[0].Date == nil || *out.Results[0].Date != "2026-01-02" || out.Results[1].Title != "" || out.Results[1].Date != nil {
		t.Fatalf("brave projection: %+v", out.Results)
	}
	for _, empty := range []string{`{"type":"search","query":{"original":"q"},"web":null}`, `{"type":"search","query":{"original":"q"}}`} {
		emptyRes := result(t, Brave, empty)
		emptyView, err := parseResult(request(t, Brave, resBodyFor(t, Brave)), emptyRes, Brave)
		if err != nil {
			t.Fatalf("nullable web rejected: %v", err)
		}
		emptyDoc, err := mapping.Project(r, emptyRes, emptyView, "route")
		if err != nil {
			t.Fatal(err)
		}
		var emptyOut struct {
			Results []any `json:"results"`
		}
		if json.Unmarshal(emptyDoc.Bytes(), &emptyOut) != nil || len(emptyOut.Results) != 0 {
			t.Fatalf("nullable web projected results: %s", emptyDoc.Bytes())
		}
	}
	over := result(t, Brave, `{"web":{"results":[{"url":"u1"},{"url":"u2"},{"url":"u3"}]}}`)
	overView, err := parseResult(request(t, Brave, `{"q":"q","count":3}`), over, Brave)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapping.Project(r, over, overView, "route"); err == nil {
		t.Fatal("over-limit rows projected")
	}
}

func resBodyFor(t *testing.T, dialect string) string {
	t.Helper()
	switch dialect {
	case Tavily:
		return `{"query":"q","max_results":2}`
	case Exa:
		return `{"query":"q","numResults":2,"contents":{"highlights":true}}`
	case Parallel:
		return `{"search_queries":["q"],"advanced_settings":{"max_results":2}}`
	case Brave:
		return `{"q":"q","count":2,"result_filter":"web"}`
	}
	return ""
}

func TestMappingProjectNormalizesCalendarDates(t *testing.T) {
	for _, target := range []struct{ dialect, field string }{
		{Exa, "publishedDate"},
		{Parallel, "publish_date"},
		{Brave, "page_age"},
	} {
		for _, tc := range []struct {
			name, date, want string
		}{
			{"date", "2026-01-02", "2026-01-02"},
			{"timestamp", "2026-01-02T03:04:05Z", "2026-01-02"},
			{"leap_day", "2024-02-29", "2024-02-29"},
			{"non_leap_day", "2025-02-29", ""},
			{"day_overflow", "2026-04-31T03:04:05Z", ""},
			{"month_overflow", "2026-13-01", ""},
			{"zero_month", "2026-00-01", ""},
			{"zero_day", "2026-01-00", ""},
			{"malformed", "not-a-date", ""},
			{"empty", "", ""},
		} {
			t.Run(target.dialect+"/"+tc.name, func(t *testing.T) {
				row := map[string]any{"url": "https://example.test/result", target.field: tc.date}
				results := map[string]any{"results": []any{row}}
				body := func() []byte {
					if target.dialect == Brave {
						return operations.Raw(map[string]any{"web": results})
					}
					return operations.Raw(results)
				}()
				res := result(t, target.dialect, string(body))
				view, err := parseResult(request(t, target.dialect, resBodyFor(t, target.dialect)), res, target.dialect)
				if err != nil {
					t.Fatal(err)
				}
				doc, err := mappingFor(target.dialect).Project(request(t, Perplexity, `{"query":"q"}`), res, view, "route")
				if err != nil {
					t.Fatal(err)
				}
				var out struct {
					Results []struct {
						Date *string `json:"date"`
					} `json:"results"`
				}
				if err := json.Unmarshal(doc.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if len(out.Results) != 1 {
					t.Fatalf("expected one result, got %s", doc.Bytes())
				}
				date := out.Results[0].Date
				if tc.want == "" {
					if date != nil {
						t.Fatalf("invalid date %q projected as %q instead of null", tc.date, *date)
					}
				} else if date == nil || *date != tc.want {
					t.Fatalf("valid date %q was not preserved in %s", tc.date, doc.Bytes())
				}
			})
		}
	}
}
