package gateway

import (
	"testing"
)

func TestResponseRetrievalQueryForwardsOnlyRegisteredControls(t *testing.T) {
	query, stream, err := responseRetrievalQuery("stream=true&starting_after=7&include%5B%5D=reasoning.encrypted_content&include_obfuscation=false")
	if err != nil || !stream || query.Get("starting_after") != "7" || query.Get("include[]") != "reasoning.encrypted_content" || query.Get("include_obfuscation") != "false" {
		t.Fatalf("SDK retrieval controls changed: query=%v stream=%t err=%v", query, stream, err)
	}
	for _, raw := range []string{
		"starting_after=7", "stream=true&starting_after=-1", "stream=true&starting_after=1&starting_after=2",
		"stream=true&include%5B%5D=unqualified.field", "stream=true&include_obfuscation=maybe", "stream=true&endpoint=https%3A%2F%2Fevil.example",
	} {
		if _, _, err := responseRetrievalQuery(raw); err == nil {
			t.Fatalf("unqualified retrieval query admitted: %s", raw)
		}
	}
}
