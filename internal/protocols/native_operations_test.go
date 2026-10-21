package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func parseEmbeddings(t *testing.T, body string) *wire.Request {
	t.Helper()
	r, err := Parse(wire.Embeddings, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func decodeEmbedding(t *testing.T, encodedWire wire.Format, body, encoding string, r *wire.Request) *wire.Completion {
	t.Helper()
	c, err := DecodeRequest(encodedWire, wire.Embeddings, []byte(body), "route", encoding, r)
	if err != nil {
		t.Fatalf("decode %s: %v", encodedWire, err)
	}
	return c
}

func TestGeminiEmbeddingDecodeReportsTheUsageOfThePrompt(t *testing.T) {
	for _, tc := range []struct {
		wire     wire.Format
		vectors  string
		request  *wire.Request
		wantData int
	}{
		{wire.GeminiEmbeddings, `"embedding":{"values":[0.5]}`, nil, 1},
		{wire.GeminiEmbeddingsBatch, `"embeddings":[{"values":[0.5]},{"values":[0.25]}]`, parseEmbeddings(t, `{"model":"route","input":["a","b"]}`), 2},
	} {
		t.Run(string(tc.wire), func(t *testing.T) {
			c := decodeEmbedding(t, tc.wire, `{`+tc.vectors+`,"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`, "", tc.request)
			if c.Usage == nil || c.Usage.InputTokens != 7 || c.Usage.TotalTokens != 7 {
				t.Fatalf("usage: %+v", c.Usage)
			}
			var out struct {
				Data  []json.RawMessage `json:"data"`
				Usage struct {
					PromptTokens int64 `json:"prompt_tokens"`
					TotalTokens  int64 `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(c.Body, &out); err != nil || len(out.Data) != tc.wantData || out.Usage.PromptTokens != 7 || out.Usage.TotalTokens != 7 {
				t.Fatalf("the client's response carries no usage: %s", c.Body)
			}
			for _, absent := range []string{``, `,"usageMetadata":null`, `,"usageMetadata":{}`, `,"usageMetadata":{"promptTokenCount":null}`} {
				c = decodeEmbedding(t, tc.wire, `{`+tc.vectors+absent+`}`, "", tc.request)
				if c.Usage != nil || strings.Contains(string(c.Body), `"usage"`) {
					t.Fatalf("usage invented for %q: %+v %s", absent, c.Usage, c.Body)
				}
			}
			for _, invalid := range []string{`"x"`, `{"promptTokenCount":"7"}`, `{"promptTokenCount":-1}`} {
				if _, err := DecodeRequest(tc.wire, wire.Embeddings, []byte(`{`+tc.vectors+`,"usageMetadata":`+invalid+`}`), "route", "", tc.request); err == nil {
					t.Fatalf("invalid usage accepted: %s", invalid)
				}
			}
		})
	}
}

func parseRerank(t *testing.T, body string) *wire.Request {
	t.Helper()
	r, err := openai.Parse(wire.Rerank, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRerankDecodeRejects(t *testing.T) {
	r := parseRerank(t, `{"model":"route","query":"q","documents":["a","b"],"return_documents":true}`)
	for _, body := range []string{
		`{"results":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.6}]}`,
		`{"results":[{"index":2,"relevance_score":0.5}]}`,
		`{"results":[{"index":-1,"relevance_score":0.5}]}`,
		`{"results":[{"index":0,"relevance_score":1.5}]}`,
		`{"results":[{"index":0,"relevance_score":-0.1}]}`,
		`{"results":[{"index":0,"relevance_score":0.5,"document":{"text":"b"}}]}`,
		`{"results":[{"index":0,"relevance_score":0.5,"document":{"image":"a"}}]}`,
		`{"results":[]}`,
		`{"usage":{"total_tokens":-1},"data":[{"index":0,"relevance_score":0.5}]}`,
		`not json`,
	} {
		if _, err := DecodeRequest(wire.Rerank, wire.Rerank, []byte(body), "route", "", r); err == nil {
			t.Fatalf("invalid rerank response accepted: %s", body)
		}
	}
}
