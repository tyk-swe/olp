package operationplan_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func TestSearchMappingRefusals(t *testing.T) {
	for _, body := range []string{
		`{"query":"q","country":"US"}`,
		`{"query":"q","max_results":21}`,
		`{"query":"q","search_type":"fast"}`,
	} {
		c := config("tavily-search", "search")
		c.Provider.VendorID, c.Provider.AuthMode = "tavily", "api_key"
		template, err := operationplan.Compile(c)
		if err != nil {
			t.Fatal(err)
		}
		source, err := operationplan.Parse("perplexity-search", []byte(body), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient}); err == nil {
			t.Fatalf("refused source bound: %s", body)
		}
	}
}

func TestBraveGetQueryPlan(t *testing.T) {
	c := config("brave-search", "search")
	c.Provider.VendorID, c.Provider.AuthMode = "brave", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("perplexity-search", []byte(`{"query":"hello world","max_results":2,"search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Method() != http.MethodGet || plan.WireBody() != nil || plan.Transport() != operations.TransportGetQuery {
		t.Fatalf("transport: method=%s body=%s transport=%s", plan.Method(), plan.WireBody(), plan.Transport())
	}
	rawURL, err := plan.RequestURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/v1/res/v1/web/search" {
		t.Fatalf("path: %s", parsed.Path)
	}
	query := parsed.Query()
	want := map[string]string{"q": "hello world", "count": "2", "freshness": "2026-01-02to2026-03-04", "result_filter": "web", "text_decorations": "false"}
	if len(query) != len(want) {
		t.Fatalf("query: %v", query)
	}
	for name, value := range want {
		if query.Get(name) != value {
			t.Fatalf("query %s=%q in %v", name, query.Get(name), query)
		}
	}
	endpoint, err := plan.Endpoint()
	if err != nil || endpoint != "https://example.com/v1/res/v1/web/search" {
		t.Fatalf("endpoint: %s %v", endpoint, err)
	}

	native, err := operationplan.Parse("brave-search", []byte(`{"q":"q","count":5,"offset":2}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	nativePlan, err := template.Bind(native, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	nativeURL, err := nativePlan.RequestURL()
	if err != nil {
		t.Fatal(err)
	}
	nativeParsed, _ := url.Parse(nativeURL)
	if nativeParsed.Query().Get("offset") != "2" || nativeParsed.Query().Get("count") != "5" {
		t.Fatalf("native query: %v", nativeParsed.Query())
	}

	refused, err := operationplan.Parse("perplexity-search", []byte(`{"query":"q","search_domain_filter":["a.com"]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := template.Bind(refused, operationplan.Context{Route: "route"}); err == nil {
		t.Fatal("domain filter bound to brave")
	}
}

func TestJSONPlanTransportUnchanged(t *testing.T) {
	c := config("tavily-search", "search")
	c.Provider.VendorID, c.Provider.AuthMode = "tavily", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("perplexity-search", []byte(`{"query":"q"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := plan.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	requestURL, err := plan.RequestURL()
	if err != nil {
		t.Fatal(err)
	}
	if plan.Method() != http.MethodPost || requestURL != endpoint || string(plan.WireBody()) != string(plan.Body()) || len(plan.WireBody()) == 0 {
		t.Fatalf("json transport: method=%s url=%s endpoint=%s", plan.Method(), requestURL, endpoint)
	}
}
