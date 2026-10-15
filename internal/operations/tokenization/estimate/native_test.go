package estimate

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// TestNativeGenerationDialectsAreCounted covers the prompts of the native
// generation dialects: the code around a fill-in-the-middle gap, and Cohere's
// messages and grounding documents.
func TestNativeGenerationDialectsAreCounted(t *testing.T) {
	for family, body := range map[wire.Format]string{
		wire.MistralFIM: `{"prompt":"def add(a, b):\n    return ","suffix":"\n\nprint(add(1, 2))","max_tokens":64}`,
		wire.CohereChat: `{"messages":[{"role":"user","content":"Summarize the report."}],"documents":[{"id":"q3","data":{"text":"Revenue grew nine percent in the third quarter."}}],"max_tokens":64}`,
	} {
		doc, err := oif.ParseJSON([]byte(body), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		request, err := wire.NewRequest(family, "route", false, doc)
		if err != nil {
			t.Fatal(err)
		}
		estimate := Walk(request).Estimate(ForModel("codestral-2508"), nil)
		if estimate.Input < 8 || estimate.Output == nil || *estimate.Output != 64 {
			t.Fatalf("%s estimate = %+v", family, estimate)
		}
	}
}
