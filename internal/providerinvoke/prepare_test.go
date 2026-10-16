package providerinvoke

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestEmbeddingDefaultsReachEachActualNativeRequest(t *testing.T) {
	cfg := connectors.Config{Kind: "gemini", AuthMode: "api_key", ProfileID: "gemini-generation", ProfileRevision: "1", OperationDefaults: map[string]connectors.DefaultSet{"embeddings": {Dialect: "gemini-embeddings", Values: map[string]json.RawMessage{"taskType": json.RawMessage(`"RETRIEVAL_DOCUMENT"`), "outputDimensionality": json.RawMessage(`8`)}}}}
	for _, explicit := range []string{"", `,"dimensions":3`} {
		request, err := openai.Parse(wire.Embeddings, []byte(`{"model":"route","input":["first","second"]`+explicit+`}`))
		if err != nil {
			t.Fatal(err)
		}
		result, err := Prepare(request, cfg, "embedding-model", nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Wire != wire.GeminiEmbeddingsBatch || result.Prepared.Descriptor().Dialect.ID != "gemini-embeddings-batch" {
			t.Fatal("actual batch dialect was lost")
		}
		var body struct {
			Requests []struct {
				Task       string `json:"taskType"`
				Dimensions int    `json:"outputDimensionality"`
				Content    struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		if json.Unmarshal(result.Prepared.Document().Bytes(), &body) != nil || len(body.Requests) != 2 {
			t.Fatal("batch shape changed")
		}
		want := 8
		if explicit != "" {
			want = 3
		}
		for i, entry := range body.Requests {
			if entry.Task != "RETRIEVAL_DOCUMENT" || entry.Dimensions != want || len(entry.Content.Parts) != 1 || entry.Content.Parts[0].Text != []string{"first", "second"}[i] {
				t.Fatalf("native input/default changed: %+v", entry)
			}
		}
	}
	request, err := openai.Parse(wire.Embeddings, []byte(`{"model":"route","input":"first","dimensions":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(request, cfg, "embedding-model", nil); err == nil {
		t.Fatal("translated explicit null was overwritten by a default")
	}
}
