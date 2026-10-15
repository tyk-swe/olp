package protocols

import (
	"encoding/json"
	"errors"
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

// TestBedrockRerankEncodesTheAgentRuntimeRequest checks the request against
// the documented Rerank example.
func TestBedrockRerankEncodesTheAgentRuntimeRequest(t *testing.T) {
	if encodedWire := WireFamily("bedrock", "amazon-bedrock", wire.Rerank); encodedWire != wire.BedrockRerank {
		t.Fatalf("wire = %s", encodedWire)
	}
	r := bedrockRerankRequest(t, `{"model":"team-rerank","query":"What is Amazon Bedrock?","documents":["Amazon Bedrock is a fully managed service for foundation models.","Paris is in France."],"top_n":2}`)
	body, encodedWire, err := EncodeTarget(r, wire.BedrockRerank, "bedrock", "amazon-bedrock", rerankARN, nil)
	if err != nil || encodedWire != wire.BedrockRerank {
		t.Fatal(encodedWire, err)
	}
	want := `{"queries":[{"type":"TEXT","textQuery":{"text":"What is Amazon Bedrock?"}}],
		"sources":[{"type":"INLINE","inlineDocumentSource":{"type":"TEXT","textDocument":{"text":"Amazon Bedrock is a fully managed service for foundation models."}}},
		           {"type":"INLINE","inlineDocumentSource":{"type":"TEXT","textDocument":{"text":"Paris is in France."}}}],
		"rerankingConfiguration":{"type":"BEDROCK_RERANKING_MODEL","bedrockRerankingConfiguration":{"modelConfiguration":{"modelArn":"` + rerankARN + `"},"numberOfResults":2}}}`
	if sameJSON(t, body, want) != "" {
		t.Fatalf("body = %s", body)
	}
	truncating := bedrockRerankRequest(t, `{"model":"team-rerank","query":"q","documents":["a"],"truncation":true}`)
	if _, _, err := EncodeTarget(truncating, wire.BedrockRerank, "bedrock", "amazon-bedrock", rerankARN, nil); err == nil {
		t.Fatal("Bedrock accepted truncation, which its Rerank API has no control for")
	} else if refused, ok := errors.AsType[*wire.RequestError](err); !ok || refused.Param != "truncation" {
		t.Fatalf("truncation refusal = %v", err)
	}
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

// TestJinaEmbeddingsNameTheirEncodingType covers Jina's spelling of the
// OpenAI encoding format, which the official SDK sends by default.
func TestJinaEmbeddingsNameTheirEncodingType(t *testing.T) {
	r, err := Parse(wire.Embeddings, []byte(`{"model":"route","input":["a"],"encoding_format":"base64","dimensions":256}`), "route")
	if err != nil {
		t.Fatal(err)
	}
	body, encodedWire, err := Encode(r, "openai_compatible", "jina", "jina-embeddings-v3", nil)
	if err != nil || encodedWire != wire.Embeddings || sameJSON(t, body, `{"model":"jina-embeddings-v3","input":["a"],"embedding_type":"base64","dimensions":256}`) != "" {
		t.Fatalf("Jina body = %s, %v", body, err)
	}
}
