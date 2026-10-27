package search

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func request(t *testing.T, dialect, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("search"), Dialect: identity(dialect)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func result(t *testing.T, dialect, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("search"), Dialect: identity(dialect)}, d, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPerplexityRequestContract(t *testing.T) {
	for _, body := range []string{
		`{"query":"q"}`,
		`{"query":"q","max_results":20,"search_type":"web","max_tokens":1,"max_tokens_per_page":1000000,"country":"US","search_language_filter":["en"],"search_domain_filter":["example.com","docs.example.com"],"search_after_date_filter":"01/01/2026","search_before_date_filter":"12/31/2026","last_updated_after_filter":"01/01/2026","last_updated_before_filter":"12/31/2026","search_recency_filter":"week"}`,
	} {
		if _, err := parseRequest(request(t, Perplexity, body), Perplexity); err != nil {
			t.Fatalf("valid request refused: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"query":["a","b"]}`,
		`{"query":"q","max_results":0}`,
		`{"query":"q","max_results":21}`,
		`{"query":"q","max_results":1.5}`,
		`{"query":"q","country":"USA"}`,
		`{"query":"q","search_language_filter":["eng"]}`,
		`{"query":"q","search_domain_filter":["example.com","-bad.com"]}`,
		`{"query":"q","search_domain_filter":[]}`,
		`{"query":"q","search_domain_filter":["a.com","b.com","c.com","d.com","e.com","f.com","g.com","h.com","i.com","j.com","k.com","l.com","m.com","n.com","o.com","p.com","q.com","r.com","s.com","t.com","u.com"]}`,
		`{"query":"q","model":"x"}`,
		`{"query":"q","stream":true}`,
		`{"query":"q","search_after_date_filter":2026}`,
		`{}`,
	} {
		if _, err := parseRequest(request(t, Perplexity, body), Perplexity); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

func TestNativeRequestContracts(t *testing.T) {
	good := map[string]string{
		Tavily:   `{"query":"q","max_results":3}`,
		Exa:      `{"query":"q","numResults":2,"contents":{"highlights":true},"includeDomains":["a.com"],"excludeDomains":["b.com"]}`,
		Parallel: `{"search_queries":["q"],"advanced_settings":{"max_results":1,"source_policy":{"include_domains":["a.com"],"exclude_domains":["b.com/path"]}}}`,
		Brave:    `{"q":"q","count":3,"offset":1,"freshness":"pw","result_filter":"web"}`,
	}
	for id, body := range good {
		if _, err := parseRequest(request(t, id, body), id); err != nil {
			t.Fatalf("%s refused a valid request: %v", id, err)
		}
	}
	bad := map[string]string{
		Tavily:   `{"query":"q","max_results":3,"include_domains":["a.com"]}`,
		Exa:      `{"query":"q","numResults":2,"contents":{"highlights":false}}`,
		Parallel: `{"search_queries":[],"advanced_settings":{"max_results":1}}`,
		Brave:    `{"q":"q","count":3,"country":"US"}`,
	}
	for id, body := range bad {
		if _, err := parseRequest(request(t, id, body), id); err == nil {
			t.Fatalf("%s admitted a foreign field: %s", id, body)
		}
	}
	nonObject := `{"search_queries":["q"],"advanced_settings":{"max_results":1,"source_policy":"x"}}`
	if _, err := parseRequest(request(t, Parallel, nonObject), Parallel); err == nil {
		t.Fatal("non-object source_policy admitted")
	}
}

func TestPerplexityResultContract(t *testing.T) {
	r := request(t, Perplexity, `{"query":"q","max_results":1}`)
	good := `{"id":"s-1","results":[{"title":"t","url":"https://x","snippet":"s","date":"2026-01-01","last_updated":null}],"server_time":"t"}`
	if _, err := parseResult(r, result(t, Perplexity, good), Perplexity); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"results":[]}`,
		`{"id":"s","results":[{"title":"t","url":"https://x"}]}`,
		`{"id":"s","results":[{"title":"t","url":"https://x","snippet":"s"},{"title":"t","url":"https://y","snippet":"s"}]}`,
		`{"id":"s","results":{}}`,
	} {
		if _, err := parseResult(r, result(t, Perplexity, body), Perplexity); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestNativeResultRequiresURL(t *testing.T) {
	r := request(t, Tavily, `{"query":"q","max_results":2}`)
	if _, err := parseResult(r, result(t, Tavily, `{"query":"q","results":[{"title":"t","url":"https://x","content":"c","score":0.9}],"response_time":1.5,"request_id":"r"}`), Tavily); err != nil {
		t.Fatal(err)
	}
	if _, err := parseResult(r, result(t, Tavily, `{"results":[{"title":"t"}]}`), Tavily); err == nil {
		t.Fatal("result without url admitted")
	}
}

func TestSearchInputOutputText(t *testing.T) {
	texts, err := inputText(request(t, Perplexity, `{"query":"needle"}`), Perplexity)
	if err != nil || len(texts) != 1 || texts[0].Value != "needle" {
		t.Fatalf("input: %+v %v", texts, err)
	}
	texts, err = outputText(result(t, Exa, `{"requestId":"r","results":[{"title":"t","url":"u","publishedDate":"2026-01-02T03:04:05Z","highlights":["a","b"]}],"costDollars":0.01}`), Exa)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range texts {
		got = append(got, tx.Value)
	}
	if strings.Join(got, ",") != "t,u,a,b" {
		t.Fatalf("coverage: %v", got)
	}
	if _, err := outputText(result(t, Tavily, `{"results":[{"url":"u","unknown":"s"}]}`), Tavily); err == nil {
		t.Fatal("unknown string field admitted")
	}
}
func TestBraveRequestContract(t *testing.T) {
	for _, body := range []string{
		`{"q":"q","count":1}`,
		`{"q":"q","count":20,"offset":0}`,
		`{"q":"q","count":2,"offset":9,"freshness":"2026-01-02to2026-03-04"}`,
		`{"q":"q","count":2,"freshness":"pd"}`,
	} {
		if _, err := parseRequest(request(t, Brave, body), Brave); err != nil {
			t.Fatalf("valid request refused: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"q":"","count":1}`,
		`{"count":1}`,
		`{"q":"q"}`,
		`{"q":"q","count":0}`,
		`{"q":"q","count":21}`,
		`{"q":"q","count":1.5}`,
		`{"q":"q","count":1,"offset":-1}`,
		`{"q":"q","count":1,"offset":10}`,
		`{"q":"q","count":1,"freshness":"px"}`,
		`{"q":"q","count":1,"freshness":"2026-01-02"}`,
		`{"q":"q","count":1,"freshness":"2026-02-30to2026-03-01"}`,
		`{"q":"q","count":1,"freshness":"2026-03-04to2026-01-02"}`,
		`{"q":"q","count":1,"result_filter":"news"}`,
		`{"q":"q","count":1,"result_filter":"web,news"}`,
		`{"q":"q","count":1,"safesearch":"off"}`,
		`{"q":"q","count":1,"text_decorations":false}`,
		`{"q":"` + strings.Repeat("x", 5000) + `","count":1}`,
	} {
		if _, err := parseRequest(request(t, Brave, body), Brave); err == nil {
			t.Fatalf("refused request admitted: %.80s", body)
		}
	}
}

func TestBraveResultContract(t *testing.T) {
	r := request(t, Brave, `{"q":"q","count":1,"result_filter":"web"}`)
	good := `{"type":"search","query":{"original":"q"},"web":{"type":"search","results":[{"title":"t","url":"https://x","description":"d","page_age":"2026-01-02T03:04:05Z"}],"family_friendly":true}}`
	if _, err := parseResult(r, result(t, Brave, good), Brave); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"type":"search","query":{"original":"q"}}`,
		`{"web":{"results":[{"title":"t"}]}}`,
		`{"web":{"results":[{"title":"t","url":"https://x"},{"title":"t","url":"https://y"}]}}`,
		`{"web":{"results":{}}}`,
		`{"results":[{"title":"t","url":"https://x"}]}`,
	} {
		if _, err := parseResult(r, result(t, Brave, body), Brave); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestBraveInputOutputText(t *testing.T) {
	texts, err := inputText(request(t, Brave, `{"q":"needle","count":1}`), Brave)
	if err != nil || len(texts) != 1 || texts[0].Pointer != "/q" || texts[0].Value != "needle" {
		t.Fatalf("input: %+v %v", texts, err)
	}
	body := `{"type":"search","query":{"original":"q","altered":"qq"},"web":{"type":"search","results":[{"title":"t","url":"u","description":"d","page_age":"2026-01-02","language":"en","family_friendly":true,"profile":{"name":"n","url":"https://p","img":"https://i"}}],"family_friendly":true}}`
	texts, err = outputText(result(t, Brave, body), Brave)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range texts {
		got = append(got, tx.Value)
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"t", "u", "d", "n", "q"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("coverage missing %q: %v", want, got)
		}
	}
	for _, bad := range []string{
		`{"web":{"results":[]},"answer":"x"}`,
		`{"web":{"results":[{"url":"u","unknown":"s"}]}}`,
		`{"web":{"results":[{"url":"u","deep_results":{"news":[{"title":"t"}]}}]}}`,
		`{"news":{"results":[{"title":"t","url":"u"}]},"web":{"results":[]}}`,
	} {
		if _, err := outputText(result(t, Brave, bad), Brave); err == nil {
			t.Fatalf("uninspected result admitted: %s", bad)
		}
	}
}

func TestSearchOutputTextFailsClosed(t *testing.T) {
	// A non-string field the contract does not name conflicts; a field that
	// carries no text passes.
	for _, tc := range []struct{ dialect, body string }{
		{Perplexity, `{"id":"i","results":[],"answer":"x"}`},
		{Tavily, `{"results":[],"images":[{"url":"u"}]}`},
		{Exa, `{"results":[],"output":{"text":"x"}}`},
		{Parallel, `{"results":[],"context":["c"]}`},
		{Tavily, `{"results":[{"url":"u","subpages":[{"title":"t"}]}]}`},
		{Parallel, `{"results":[{"url":"u","raw_content":"page"}]}`},
		{Exa, `{"results":[{"url":"u","extras":{"links":[]}}]}`},
	} {
		if _, err := outputText(result(t, tc.dialect, tc.body), tc.dialect); err == nil {
			t.Fatalf("%s admitted %s", tc.dialect, tc.body)
		}
	}
	for _, tc := range []struct{ dialect, body string }{
		{Perplexity, `{"id":"i","results":[],"images":[],"meta":{},"n":3}`},
		{Tavily, `{"results":[],"answer":null,"images":[]}`},
		{Exa, `{"results":[{"url":"u","id":"d","score":0.5,"extras":{}}]}`},
		{Parallel, `{"results":[{"url":"u","highlightScores":[0.9],"image":null}]}`},
	} {
		if _, err := outputText(result(t, tc.dialect, tc.body), tc.dialect); err != nil {
			t.Fatalf("%s refused %s: %v", tc.dialect, tc.body, err)
		}
	}
}
