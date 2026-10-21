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

func resBodyFor(t *testing.T, dialect string) string {
	t.Helper()
	switch dialect {
	case Tavily:
		return `{"query":"q","max_results":2}`
	case Exa:
		return `{"query":"q","numResults":2,"contents":{"highlights":true}}`
	case Parallel:
		return `{"search_queries":["q"],"advanced_settings":{"max_results":2}}`
	}
	return ""
}
