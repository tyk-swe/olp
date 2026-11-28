package protocols

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
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

func TestEmbeddingDecodePreservesInputCorrespondence(t *testing.T) {
	request := parseEmbeddings(t, `{"model":"route","input":["a","b"]}`)
	for _, tc := range []struct {
		name                           string
		family                         wire.Format
		complete, missing, extra, null string
	}{
		{"Gemini", wire.GeminiEmbeddingsBatch,
			`{"embeddings":[{"values":[0.5,0]},{"values":[0.25,0.5]}]}`,
			`{"embeddings":[{"values":[0.5,0]}]}`,
			`{"embeddings":[{"values":[0.5,0]},{"values":[0.25,0.5]},{"values":[1,1]}]}`,
			`{"embeddings":[{"values":[0.5,null]},{"values":[0.25,0.5]}]}`},
		{"Vertex", wire.VertexEmbeddings,
			`{"predictions":[{"embeddings":{"values":[0.5,0]}},{"embeddings":{"values":[0.25,0.5]}}]}`,
			`{"predictions":[{"embeddings":{"values":[0.5,0]}}]}`,
			`{"predictions":[{"embeddings":{"values":[0.5,0]}},{"embeddings":{"values":[0.25,0.5]}},{"embeddings":{"values":[1,1]}}]}`,
			`{"predictions":[{"embeddings":{"values":[0.5,null]}},{"embeddings":{"values":[0.25,0.5]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []string{tc.missing, tc.extra, tc.null} {
				if c, err := DecodeRequest(tc.family, wire.Embeddings, []byte(body), "route", "float", request); err == nil {
					t.Fatalf("invalid response accepted %s", c.Body)
				}
			}
			for _, context := range []*wire.Request{request, nil, {Family: tc.family}} {
				c := decodeEmbedding(t, tc.family, tc.complete, "float", context)
				var result struct {
					Data []struct {
						Embedding []float64 `json:"embedding"`
					} `json:"data"`
				}
				if err := json.Unmarshal(c.Body, &result); err != nil || len(result.Data) != 2 || len(result.Data[0].Embedding) != 2 || result.Data[0].Embedding[0] != 0.5 || result.Data[0].Embedding[1] != 0 {
					t.Fatalf("complete vectors changed %s", c.Body)
				}
			}
			encoded := decodeEmbedding(t, tc.family, tc.complete, "base64", request)
			if !strings.Contains(string(encoded.Body), `"embedding":"AAAAPwAAAAA="`) {
				t.Fatalf("base64 vector changed %s", encoded.Body)
			}
			single := parseEmbeddings(t, `{"model":"route","input":"a"}`)
			decodeEmbedding(t, tc.family, tc.missing, "float", single)
			if _, err := Decode(tc.family, wire.Embeddings, []byte(tc.missing), "route", "float"); err != nil {
				t.Fatalf("requestless decoding failed %v", err)
			}
		})
	}
	for _, body := range []string{`{"data":[{"index":0,"embedding":[0.5,null]}]}`, `{"data":[{"index":0,"embedding":[null,0.5]}]}`} {
		if c, err := Decode(wire.Embeddings, wire.Embeddings, []byte(body), "route", "float"); err == nil {
			t.Fatalf("null coordinate accepted %s", c.Body)
		}
	}
}

func TestPreparedResponseUsesDispatchedInputsAndCallerProjection(t *testing.T) {
	caller := parseEmbeddings(t, `{"model":"route","input":["before","before","before"]}`)
	nativeDocument, err := oif.ParseJSON([]byte(`{"requests":[{"content":{"parts":[{"text":"first"}]}},{"content":{"parts":[{"text":"second"}]}}]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	native, err := wire.NewRequest(wire.GeminiEmbeddingsBatch, "route", false, nativeDocument)
	if err != nil {
		t.Fatal(err)
	}
	codec := PrepareResponse(native, caller, "route", "float")
	if _, err := codec.Project([]byte(`{"embeddings":[{"values":[1]},{"values":[2]}]}`)); err != nil {
		t.Fatalf("rejected vectors matching dispatched inputs: %v", err)
	}
	if _, err := codec.Project([]byte(`{"embeddings":[{"values":[1]},{"values":[2]},{"values":[3]}]}`)); err == nil {
		t.Fatal("accepted vectors matching stale caller inputs")
	}
	rerankCaller := parseRerank(t, `{"model":"route","query":"q","documents":["original"],"return_documents":true}`)
	rerankDocument, err := oif.ParseJSON([]byte(`{"query":"q","documents":["filtered"]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	rerankNative, err := wire.NewRequest(wire.Rerank, "route", false, rerankDocument)
	if err != nil {
		t.Fatal(err)
	}
	rerankCodec := PrepareResponse(rerankNative, rerankCaller, "route", "")
	projected, err := rerankCodec.Project([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`))
	if err != nil || !strings.Contains(string(projected.Body), `"document":"filtered"`) {
		t.Fatalf("caller projection lost dispatched document: %+v %v", projected, err)
	}
	if _, err := rerankCodec.Project([]byte(`{"results":[{"index":0,"relevance_score":0.5,"document":"original"}]}`)); err == nil {
		t.Fatal("accepted an echoed document absent from dispatched inputs")
	}
}

func TestEmbeddingAndRerankUsageAccounting(t *testing.T) {
	t.Parallel()
	embeddings := parseEmbeddings(t, `{"model":"route","input":["a","b"]}`)
	single := parseEmbeddings(t, `{"model":"route","input":"a"}`)
	rerank := parseRerank(t, `{"model":"route","query":"q","documents":["a","b"]}`)
	const openaiData = `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.5]},{"object":"embedding","index":1,"embedding":[0.25]}]`
	const results = `{"results":[{"index":0,"relevance_score":0.5}]`
	for _, tc := range []struct {
		name       string
		wire       wire.Format
		target     wire.Format
		request    *wire.Request
		body, want string
	}{
		{"OpenAI prompt tokens", wire.Embeddings, wire.Embeddings, embeddings, openaiData + `,"usage":{"prompt_tokens":4,"total_tokens":4}}`, `{"input_tokens":4,"output_tokens":0,"total_tokens":4}`},
		{"OpenAI total only", wire.Embeddings, wire.Embeddings, embeddings, openaiData + `,"usage":{"total_tokens":6}}`, `{"input_tokens":6,"output_tokens":0,"total_tokens":6}`},
		{"OpenAI usage absent", wire.Embeddings, wire.Embeddings, embeddings, openaiData + `}`, `null`},
		{"OpenAI usage without counts", wire.Embeddings, wire.Embeddings, embeddings, openaiData + `,"usage":{}}`, ""},
		{"Vertex predictions are summed", wire.VertexEmbeddings, wire.Embeddings, embeddings, `{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":3}}},{"embeddings":{"values":[0.25],"statistics":{"token_count":4}}}]}`, `{"input_tokens":7,"output_tokens":0,"total_tokens":7}`},
		{"Vertex negative count", wire.VertexEmbeddings, wire.Embeddings, embeddings, `{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":-1}}},{"embeddings":{"values":[0.25],"statistics":{"token_count":4}}}]}`, ""},
		{"Bedrock Titan", wire.BedrockEmbeddings, wire.Embeddings, single, `{"embedding":[0.5],"inputTextTokenCount":5}`, `{"input_tokens":5,"output_tokens":0,"total_tokens":5}`},
		{"Bedrock Titan without count", wire.BedrockEmbeddings, wire.Embeddings, single, `{"embedding":[0.5]}`, ""},
		{"rerank tokens", wire.Rerank, wire.Rerank, rerank, results + `,"usage":{"total_tokens":42}}`, `{"input_tokens":42,"output_tokens":0,"total_tokens":42}`},
		{"rerank search units", wire.Rerank, wire.Rerank, rerank, results + `,"meta":{"billed_units":{"search_units":2}}}`, `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"media_units":"2"}`},
		{"rerank negative search units", wire.Rerank, wire.Rerank, rerank, results + `,"meta":{"billed_units":{"search_units":-2}}}`, ""},
		{"rerank usage without total", wire.Rerank, wire.Rerank, rerank, results + `,"usage":{}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := DecodeRequest(tc.wire, tc.target, []byte(tc.body), "route", "", tc.request)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", tc.body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertUsageJSON(t, c.Usage, tc.want)
		})
	}
}
