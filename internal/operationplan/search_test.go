package operationplan_test

import (
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
