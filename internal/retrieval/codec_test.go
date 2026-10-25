package retrieval

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetrievalFiltersAndNativeLimits(t *testing.T) {
	fields := Fields{Text: "content", ID: "doc_id", URL: "source", Metadata: map[string]string{"author": "author_name", "year": "year"}}
	query := []byte(`{"query":"test","max_num_results":7,"filters":{"type":"and","filters":[{"type":"eq","key":"author","value":"O'Brien' or true"},{"type":"gte","key":"year","value":2020}]}}`)
	body, err := Encode(Azure, "docs", fields, query)
	if err != nil {
		t.Fatal(err)
	}
	var azure map[string]any
	_ = json.Unmarshal(body, &azure)
	if azure["top"] != float64(7) || azure["filter"] != "((author_name eq 'O''Brien'' or true') and (year ge 2020))" {
		t.Fatalf("unsafe or lossy Azure filter: %s", body)
	}
	bedrockFields := Fields{Metadata: fields.Metadata}
	body, err = Encode(Bedrock, "ABCDEFGHIJ", bedrockFields, query)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"numberOfResults":7`) || !strings.Contains(string(body), `"greaterThanOrEquals":{"key":"year","value":2020}`) || !strings.Contains(string(body), `"andAll"`) {
		t.Fatalf("lost Bedrock controls: %s", body)
	}
	for _, raw := range []string{`{"query":"x","max_num_results":0}`, `{"query":"x","max_num_results":101}`, `{"query":"x","filters":{"type":"eq","key":"unmapped","value":"x"}}`, `{"query":"x","ranking_options":{}}`, `{"query":"x","filters":{"type":"eq","key":"year","value":{}}}`, `{"query":"x","filters":{"type":"and","filters":[]}}`} {
		if _, err = Encode(Azure, "docs", fields, []byte(raw)); err == nil {
			t.Fatalf("unsupported query admitted: %s", raw)
		}
	}
}
func TestRetrievalVertexThresholdsAndBackendRefusals(t *testing.T) {
	corpus := "projects/project/locations/us-central1/ragCorpora/123"
	body, err := Encode(Vertex, corpus, Fields{}, []byte(`{"query":"x","max_num_results":3,"vector_distance_threshold":0.125}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"ragCorpus":"` + corpus + `"`, `"topK":3`, `"vectorDistanceThreshold":0.125`} {
		if !strings.Contains(string(body), field) {
			t.Fatalf("Vertex control lost: %s", body)
		}
	}
	for _, raw := range []string{`{"query":"x","filters":{"type":"eq","key":"a","value":1}}`, `{"query":"x","vector_distance_threshold":1,"vector_similarity_threshold":0.5}`} {
		if _, err = Encode(Vertex, corpus, Fields{}, []byte(raw)); err == nil {
			t.Fatalf("unmappable Vertex query admitted: %s", raw)
		}
	}
	if _, err = Encode(Bedrock, "ABCDEFGHIJ", Fields{}, []byte(`{"query":"x","vector_distance_threshold":0.5}`)); err == nil {
		t.Fatal("Vertex threshold admitted for Bedrock")
	}
}
func TestRetrievalNativeScoresAndCitations(t *testing.T) {
	tests := []struct {
		backend                string
		fields                 Fields
		raw, text, score, kind string
	}{
		{Bedrock, Fields{}, `{"retrievalResults":[{"content":{"text":"answer","type":"TEXT"},"score":0.1234567890123456789,"location":{"type":"S3","s3Location":{"uri":"s3://bucket/key"}},"metadata":{"year":2024}}]}`, "answer", "0.1234567890123456789", "score"},
		{Vertex, Fields{}, `{"contexts":{"contexts":[{"text":"passage","distance":0.42,"sourceUri":"gs://bucket/file","sourceDisplayName":"file"}]}}`, "passage", "0.42", "distance"},
		{Azure, Fields{Text: "content", ID: "key", URL: "url"}, `{"value":[{"content":"document","key":"doc1","url":"https://example.org/doc1","@search.score":2.17}]}`, "document", "2.17", "score"},
	}
	for _, tc := range tests {
		t.Run(tc.backend, func(t *testing.T) {
			results, err := Decode(tc.backend, tc.fields, []byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Text != tc.text || results[0].Score.String() != tc.score || results[0].ScoreKind != tc.kind || results[0].Citation == nil {
				t.Fatalf("native values changed: %#v", results)
			}
		})
	}
	if _, err := Decode(Azure, Fields{Text: "content", ID: "key"}, []byte(`{"value":[{"content":"x"}]}`)); err == nil {
		t.Fatal("missing native score accepted")
	}
	for _, raw := range []string{`{"value":{}}`, `{"value":null}`, `{"value":[],"value":[{}]}`} {
		if _, err := Decode(Azure, Fields{}, []byte(raw)); err == nil {
			t.Fatalf("invalid result envelope accepted: %s", raw)
		}
	}
	if _, err := Encode(Azure, "docs", Fields{Text: "text", ID: "key"}, []byte(`{"query":"a","query":"b"}`)); err == nil {
		t.Fatal("duplicate query controls accepted")
	}
}
