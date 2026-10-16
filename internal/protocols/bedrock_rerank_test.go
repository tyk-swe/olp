package protocols

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

const rerankARN = "arn:aws:bedrock:us-west-2::foundation-model/cohere.rerank-v3-5:0"

func bedrockRerankRequest(t *testing.T, body string) *wire.Request {
	t.Helper()
	r, err := Parse(wire.Rerank, []byte(body), "team-rerank")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestBedrockRerankDecodesRankedSources decodes the documented response shape
// and meters a query per hundred sources.
func TestBedrockRerankDecodesRankedSources(t *testing.T) {
	documents := make([]string, 150)
	for i := range documents {
		documents[i] = fmt.Sprintf("document %d", i)
	}
	encoded, _ := json.Marshal(map[string]any{"model": "team-rerank", "query": "q", "documents": documents, "top_n": 2, "return_documents": true})
	r := bedrockRerankRequest(t, string(encoded))
	body := `{"results":[{"document":{"type":"TEXT","textDocument":{"text":"document 7"}},"index":7,"relevanceScore":0.91},{"index":3,"relevanceScore":0.25}]}`
	completion, err := DecodeRequest(wire.BedrockRerank, wire.Rerank, []byte(body), "team-rerank", "", r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"object":"list","model":"team-rerank","results":[{"index":7,"relevance_score":0.91,"document":"document 7"},{"index":3,"relevance_score":0.25,"document":"document 3"}]}`
	if diff := sameJSON(t, completion.Body, want); diff != "" || completion.Usage == nil || completion.Usage.MediaUnits == nil || *completion.Usage.MediaUnits != "2" {
		t.Fatalf("decoded %s %+v", completion.Body, completion.Usage)
	}
	for _, invalid := range []string{
		`{"results":[]}`,
		`{"results":[{"index":150,"relevanceScore":0.5}]}`,
		`{"results":[{"index":1,"relevanceScore":0.5},{"index":1,"relevanceScore":0.4}]}`,
		`{"results":[{"index":1,"relevanceScore":1.5}]}`,
		`{"results":[{"index":1,"relevanceScore":0.5}],"nextToken":"more"}`,
	} {
		if _, err := DecodeRequest(wire.BedrockRerank, wire.Rerank, []byte(invalid), "team-rerank", "", r); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func sameJSON(t *testing.T, got []byte, want string) string {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return err.Error()
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		return strings.Join([]string{string(x), string(y)}, " != ")
	}
	return ""
}
