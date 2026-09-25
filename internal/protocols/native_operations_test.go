package protocols_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func parseEmbeddings(t *testing.T, body string) *openai.Request {
	t.Helper()
	r, err := protocols.Parse(openai.FamilyEmbeddings, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func encode(t *testing.T, r *openai.Request, kind, vendor, model string) (map[string]any, openai.Family) {
	t.Helper()
	data, wire, err := protocols.Encode(r, kind, vendor, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f, wire
}

func TestGeminiEmbeddingEncode(t *testing.T) {
	single, wire := encode(t, parseEmbeddings(t, `{"model":"route","input":"hello","dimensions":8}`), "gemini", "google", "text-embedding-004")
	if wire != openai.FamilyGeminiEmbeddings {
		t.Fatalf("single input must use embedContent: %s", wire)
	}
	content, _ := single["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	part, _ := parts[0].(map[string]any)
	if part["text"] != "hello" || single["outputDimensionality"] != float64(8) {
		t.Fatalf("gemini single body: %v", single)
	}
	batch, wire := encode(t, parseEmbeddings(t, `{"model":"route","input":["a","b"],"dimensions":4}`), "gemini", "google", "text-embedding-004")
	if wire != openai.FamilyGeminiEmbeddingsBatch {
		t.Fatalf("array input must use batchEmbedContents: %s", wire)
	}
	requests, _ := batch["requests"].([]any)
	if len(requests) != 2 {
		t.Fatalf("batch body: %v", batch)
	}
	entry, _ := requests[0].(map[string]any)
	if entry["model"] != "models/text-embedding-004" || entry["outputDimensionality"] != float64(4) {
		t.Fatalf("batch entry: %v", entry)
	}
	r := parseEmbeddings(t, `{"model":"route","input":[1,2,3]}`)
	if _, _, err := protocols.Encode(r, "gemini", "google", "m", nil); err == nil {
		t.Fatal("native embeddings accepted token-array input")
	}
}

func TestVertexEmbeddingEncode(t *testing.T) {
	body, wire := encode(t, parseEmbeddings(t, `{"model":"route","input":["a","b"],"dimensions":4,"truncation":true}`), "vertex_ai", "google-vertex", "text-embedding-005")
	if wire != openai.FamilyVertexEmbeddings {
		t.Fatalf("vertex wire: %s", wire)
	}
	instances, _ := body["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("vertex instances: %v", body)
	}
	first, _ := instances[0].(map[string]any)
	if first["content"] != "a" {
		t.Fatalf("vertex instance: %v", first)
	}
	parameters, _ := body["parameters"].(map[string]any)
	if parameters["outputDimensionality"] != float64(4) || parameters["autoTruncate"] != true {
		t.Fatalf("vertex parameters: %v", parameters)
	}
	var long strings.Builder
	long.WriteString(`{"model":"route","input":[`)
	for i := 0; i < 6; i++ {
		if i > 0 {
			long.WriteString(",")
		}
		long.WriteString(`"x"`)
	}
	long.WriteString(`]}`)
	if _, _, err := protocols.Encode(parseEmbeddings(t, long.String()), "vertex_ai", "google-vertex", "m", nil); err == nil {
		t.Fatal("vertex accepted more than 5 inputs")
	}
}

func TestBedrockEmbeddingEncode(t *testing.T) {
	body, wire := encode(t, parseEmbeddings(t, `{"model":"route","input":"hello","dimensions":256,"normalize":true}`), "bedrock", "amazon-bedrock", "amazon.titan-embed-text-v2:0")
	if wire != openai.FamilyBedrockEmbeddings {
		t.Fatalf("bedrock wire: %s", wire)
	}
	if body["inputText"] != "hello" || body["dimensions"] != float64(256) || body["normalize"] != true {
		t.Fatalf("bedrock body: %v", body)
	}
	if _, _, err := protocols.Encode(parseEmbeddings(t, `{"model":"route","input":"hello"}`), "bedrock", "amazon-bedrock", "anthropic.claude-3", nil); err == nil {
		t.Fatal("unqualified bedrock model accepted")
	}
	if _, _, err := protocols.Encode(parseEmbeddings(t, `{"model":"route","input":["a","b"]}`), "bedrock", "amazon-bedrock", "amazon.titan-embed-text-v2:0", nil); err == nil {
		t.Fatal("bedrock accepted more than one input")
	}
}

func TestNativeEmbeddingURLs(t *testing.T) {
	gemini := connectors.Config{Kind: "gemini", Endpoint: "https://generativelanguage.googleapis.com/v1beta"}
	if u, e := gemini.URL(openai.FamilyGeminiEmbeddings, "text-embedding-004", false); e != nil || u != "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent" {
		t.Fatalf("gemini url %s %v", u, e)
	}
	if u, e := gemini.URL(openai.FamilyGeminiEmbeddingsBatch, "text-embedding-004", false); e != nil || u != "https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:batchEmbedContents" {
		t.Fatalf("gemini batch url %s %v", u, e)
	}
	vertex := connectors.Config{Kind: "vertex_ai", Endpoint: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google"}
	if u, e := vertex.URL(openai.FamilyVertexEmbeddings, "text-embedding-005", false); e != nil || !strings.HasSuffix(u, "/models/text-embedding-005:predict") {
		t.Fatalf("vertex url %s %v", u, e)
	}
	bedrock := connectors.Config{Kind: "bedrock", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com"}
	if u, e := bedrock.URL(openai.FamilyBedrockEmbeddings, "amazon.titan-embed-text-v2:0", false); e != nil || u != "https://bedrock-runtime.us-east-1.amazonaws.com/model/amazon.titan-embed-text-v2:0/invoke" {
		t.Fatalf("bedrock url %s %v", u, e)
	}
}

func decodeEmbedding(t *testing.T, wire openai.Family, body, encoding string, r *openai.Request) *openai.Completion {
	t.Helper()
	c, err := protocols.DecodeRequest(wire, openai.FamilyEmbeddings, []byte(body), "route", encoding, r)
	if err != nil {
		t.Fatalf("decode %s: %v", wire, err)
	}
	return c
}

func TestGeminiEmbeddingDecode(t *testing.T) {
	c := decodeEmbedding(t, openai.FamilyGeminiEmbeddings, `{"embedding":{"values":[0.5,0.25]}}`, "", nil)
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Model  string `json:"model"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(c.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || out.Model != "route" || len(out.Data) != 1 || out.Data[0].Object != "embedding" || out.Data[0].Embedding[0] != 0.5 {
		t.Fatalf("gemini normalization: %s", c.Body)
	}
	c = decodeEmbedding(t, openai.FamilyGeminiEmbeddingsBatch, `{"embeddings":[{"values":[0.5]},{"values":[0.25]}]}`, "base64", parseEmbeddings(t, `{"model":"route","input":["a","b"],"encoding_format":"base64"}`))
	var encoded struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(c.Body, &encoded); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded.Data[0].Embedding)
	if err != nil || len(raw) != 4 || float64(math.Float32frombits(binary.LittleEndian.Uint32(raw))) != 0.5 {
		t.Fatalf("base64 embedding: %v %v", raw, err)
	}
	if _, err := protocols.DecodeRequest(openai.FamilyGeminiEmbeddings, openai.FamilyEmbeddings, []byte(`{"embedding":{}}`), "route", "", nil); err == nil {
		t.Fatal("missing values accepted")
	}
}

func TestVertexEmbeddingDecode(t *testing.T) {
	c := decodeEmbedding(t, openai.FamilyVertexEmbeddings, `{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":3}}},{"embeddings":{"values":[0.25],"statistics":{"token_count":2}}}]}`, "", nil)
	if c.Usage == nil || c.Usage.InputTokens != 5 || c.Usage.TotalTokens != 5 {
		t.Fatalf("vertex usage: %+v", c.Usage)
	}
	if _, err := protocols.DecodeRequest(openai.FamilyVertexEmbeddings, openai.FamilyEmbeddings, []byte(`{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":"x"}}}]}`), "route", "", nil); err == nil {
		t.Fatal("invalid token count accepted")
	}
	if _, err := protocols.DecodeRequest(openai.FamilyVertexEmbeddings, openai.FamilyEmbeddings, []byte(`{"predictions":[{}]}`), "route", "", nil); err == nil {
		t.Fatal("missing vector accepted")
	}
}

func TestBedrockEmbeddingDecode(t *testing.T) {
	c := decodeEmbedding(t, openai.FamilyBedrockEmbeddings, `{"embedding":[0.5,0.25],"inputTextTokenCount":7}`, "", nil)
	if c.Usage == nil || c.Usage.InputTokens != 7 {
		t.Fatalf("bedrock usage: %+v", c.Usage)
	}
	if _, err := protocols.DecodeRequest(openai.FamilyBedrockEmbeddings, openai.FamilyEmbeddings, []byte(`{"embedding":[0.5]}`), "route", "", nil); err == nil {
		t.Fatal("missing token count accepted")
	}
	if _, err := protocols.DecodeRequest(openai.FamilyBedrockEmbeddings, openai.FamilyEmbeddings, []byte(`{"embedding":[0.5],"inputTextTokenCount":-1}`), "route", "", nil); err == nil {
		t.Fatal("negative token count accepted")
	}
}

func parseRerank(t *testing.T, body string) *openai.Request {
	t.Helper()
	r, err := openai.Parse(openai.FamilyRerank, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRerankCanonicalValidation(t *testing.T) {
	for _, body := range []string{
		`{"model":"route","documents":["a"]}`,
		`{"model":"route","query":"q","documents":[]}`,
		`{"model":"route","query":"q","documents":["a"],"top_n":2}`,
		`{"model":"route","query":"q","documents":["a"],"top_n":0}`,
		`{"model":"route","query":"q","documents":["a"],"return_documents":"yes"}`,
		`{"model":"route","query":"q","documents":["a"],"size":4}`,
		`{"model":"route","query":"q","documents":[""]}`,
		`{"model":"route","query":"q","documents":["` + strings.Repeat("d", 1<<20+1) + `"]}`,
		`{"model":"route","query":"q","documents":["a"],"stream":true}`,
	} {
		if _, err := openai.Parse(openai.FamilyRerank, []byte(body)); err == nil {
			t.Fatalf("invalid canonical rerank accepted: %.60s", body)
		}
	}
	if _, err := openai.Parse(openai.FamilyRerank, []byte(`{"model":"route","query":"q","documents":["a","b"],"top_n":2,"return_documents":true}`)); err != nil {
		t.Fatalf("valid canonical rerank rejected: %v", err)
	}
}

func TestRerankVendorEncode(t *testing.T) {
	r := parseRerank(t, `{"model":"route","query":"q","documents":["a","b"],"top_n":1,"truncation":true}`)
	f, wire := encode(t, r, "openai_compatible", "voyage", "rerank-2")
	if wire != openai.FamilyRerank {
		t.Fatalf("rerank wire: %s", wire)
	}
	if f["top_k"] != float64(1) || f["top_n"] != nil || f["truncation"] != true || f["model"] != "rerank-2" {
		t.Fatalf("voyage body: %v", f)
	}
	if _, _, err := protocols.Encode(r, "openai_compatible", "cohere", "rerank-v3.5", nil); err == nil {
		t.Fatal("cohere accepted truncation")
	}
	r = parseRerank(t, `{"model":"route","query":"q","documents":["a","b"],"top_n":1}`)
	f, _ = encode(t, r, "openai_compatible", "cohere", "rerank-v3.5")
	if f["top_n"] != float64(1) || f["model"] != "rerank-v3.5" {
		t.Fatalf("cohere body: %v", f)
	}
	if _, _, err := protocols.Encode(r, "openai_compatible", "mistral", "m", nil); err == nil {
		t.Fatal("unreviewed vendor accepted for rerank")
	}
	cohere := connectors.Config{Kind: "openai_compatible", VendorID: "cohere", Endpoint: "https://api.cohere.ai/compatibility/v1"}
	if u, e := cohere.URL(openai.FamilyRerank, "rerank-v3.5", false); e != nil || u != "https://api.cohere.ai/v2/rerank" {
		t.Fatalf("cohere preset rerank url %s %v", u, e)
	}
	custom := connectors.Config{Kind: "openai_compatible", VendorID: "cohere", Endpoint: "https://proxy.example/v1"}
	if u, e := custom.URL(openai.FamilyRerank, "rerank-v3.5", false); e != nil || u != "https://proxy.example/v1/rerank" {
		t.Fatalf("custom rerank url %s %v", u, e)
	}
	voyage := connectors.Config{Kind: "openai_compatible", VendorID: "voyage", Endpoint: "https://api.voyageai.com/v1"}
	if u, e := voyage.URL(openai.FamilyRerank, "rerank-2", false); e != nil || u != "https://api.voyageai.com/v1/rerank" {
		t.Fatalf("voyage rerank url %s %v", u, e)
	}
}

func TestRerankDecode(t *testing.T) {
	r := parseRerank(t, `{"model":"route","query":"q","documents":["a","b"],"return_documents":true}`)
	c, err := protocols.DecodeRequest(openai.FamilyRerank, openai.FamilyRerank, []byte(`{"data":[{"index":1,"relevance_score":0.9,"document":"b"},{"index":0,"relevance_score":0.1}],"usage":{"total_tokens":11}}`), "route", "", r)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Document       *string `json:"document"`
		} `json:"results"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(c.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || out.Model != "route" || len(out.Results) != 2 || out.Results[0].Index != 1 || out.Results[0].Document == nil || *out.Results[0].Document != "b" {
		t.Fatalf("voyage normalization: %s", c.Body)
	}
	if c.Usage == nil || c.Usage.InputTokens != 11 || c.Usage.TotalTokens != 11 || out.Usage["total_tokens"] != float64(11) {
		t.Fatalf("voyage usage: %+v %s", c.Usage, c.Body)
	}
	r = parseRerank(t, `{"model":"route","query":"q","documents":["a","b"]}`)
	c, err = protocols.DecodeRequest(openai.FamilyRerank, openai.FamilyRerank, []byte(`{"data":[{"index":1,"relevance_score":0.9,"document":"b"}]}`), "route", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(c.Body), `"document"`) {
		t.Fatalf("unrequested document leaked: %s", c.Body)
	}
	c, err = protocols.DecodeRequest(openai.FamilyRerank, openai.FamilyRerank, []byte(`{"id":"r1","results":[{"index":0,"relevance_score":0.5}],"meta":{"billed_units":{"search_units":2}}}`), "route", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if c.Usage == nil || c.Usage.MediaUnits == nil || *c.Usage.MediaUnits != "2" || c.UpstreamID != "r1" {
		t.Fatalf("cohere usage: %+v %q", c.Usage, c.UpstreamID)
	}
	var cohereOut struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Results []struct {
			Index int `json:"index"`
		} `json:"results"`
	}
	if err := json.Unmarshal(c.Body, &cohereOut); err != nil {
		t.Fatal(err)
	}
	if cohereOut.ID != "r1" || cohereOut.Object != "list" || len(cohereOut.Results) != 1 {
		t.Fatalf("cohere normalization: %s", c.Body)
	}
}

func TestRerankDecodeRejects(t *testing.T) {
	r := parseRerank(t, `{"model":"route","query":"q","documents":["a","b"],"return_documents":true}`)
	for _, body := range []string{
		`{"results":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.6}]}`,
		`{"results":[{"index":2,"relevance_score":0.5}]}`,
		`{"results":[{"index":-1,"relevance_score":0.5}]}`,
		`{"results":[{"index":0,"relevance_score":1.5}]}`,
		`{"results":[{"index":0,"relevance_score":-0.1}]}`,
		`{"results":[{"index":0,"relevance_score":0.5,"document":{"text":"a"}}]}`,
		`{"results":[]}`,
		`{"usage":{"total_tokens":-1},"data":[{"index":0,"relevance_score":0.5}]}`,
		`not json`,
	} {
		if _, err := protocols.DecodeRequest(openai.FamilyRerank, openai.FamilyRerank, []byte(body), "route", "", r); err == nil {
			t.Fatalf("invalid rerank response accepted: %s", body)
		}
	}
}
