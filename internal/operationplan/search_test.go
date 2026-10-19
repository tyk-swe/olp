package operationplan_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func TestSearchMappingLowersAndProjects(t *testing.T) {
	c := config("tavily-search", "search")
	c.Provider.VendorID, c.Provider.AuthMode = "tavily", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("perplexity-search", []byte(`{"query":"airport delays"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(plan.Body(), &body)
	if string(body["query"]) != `"airport delays"` || string(body["max_results"]) != "10" || len(body) != 2 {
		t.Fatalf("lowered body: %s", plan.Body())
	}
	endpoint, err := plan.Endpoint()
	if err != nil || endpoint != "https://example.com/v1/search" {
		t.Fatalf("endpoint %q %v", endpoint, err)
	}
	result, err := plan.Decode([]byte(`{"query":"airport delays","results":[{"title":"t","url":"https://a","content":"c","score":0.9}],"response_time":1.0,"request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID      string `json:"id"`
		Results []struct {
			Title, URL, Snippet string
			Date                *string `json:"date"`
		} `json:"results"`
	}
	if json.Unmarshal(result.Body, &out) != nil || !strings.HasPrefix(out.ID, "search_") || out.Results[0].Snippet != "c" {
		t.Fatalf("projected %s", result.Body)
	}
}

// A full allow list expands to root plus wildcard domains at the target,
// inside the native codec's admitted bound.
func TestSearchExaAllowlistExpands(t *testing.T) {
	domains := []string{}
	for i := 0; i < 20; i++ {
		domains = append(domains, "d"+string(rune('a'+i))+".com")
	}
	body, _ := json.Marshal(map[string]any{"query": "q", "search_domain_filter": domains})
	c := config("exa-search", "search")
	c.Provider.VendorID, c.Provider.AuthMode = "exa", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("perplexity-search", body, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	var lowered struct {
		Include []string `json:"includeDomains"`
	}
	_ = json.Unmarshal(plan.Body(), &lowered)
	if len(lowered.Include) != 40 || lowered.Include[0] != "da.com" || lowered.Include[1] != "*.da.com" {
		t.Fatalf("expansion: %s", plan.Body())
	}
	if _, err := plan.Decode([]byte(`{"requestId":"r","results":[{"title":"t","url":"u"}]}`)); err != nil {
		t.Fatalf("target result refused by its own codec: %v", err)
	}
}

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

func TestOCRPlanBindsServingModel(t *testing.T) {
	c := config("mistral-ocr", "ocr")
	c.Provider.VendorID, c.Provider.AuthMode = "mistral", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("mistral-ocr", []byte(`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/a.pdf"}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(plan.Body(), &body)
	if string(body["model"]) != `"vendor-model"` {
		t.Fatalf("bound body: %s", plan.Body())
	}
	endpoint, err := plan.Endpoint()
	if err != nil || endpoint != "https://example.com/v1/ocr" {
		t.Fatalf("endpoint %q %v", endpoint, err)
	}
	result, err := plan.Decode([]byte(`{"model":"mistral-ocr-latest","pages":[{"index":0,"markdown":"# A"}],"usage_info":{"pages_processed":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(result.Body, &out)
	if out["model"] != "route" || result.Usage == nil || *result.Usage.MediaUnits != "2" {
		t.Fatalf("decoded %s usage %+v", result.Body, result.Usage)
	}
}
