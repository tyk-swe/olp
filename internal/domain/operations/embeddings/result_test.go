package embeddings

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

func TestEmbeddingResultUsage(t *testing.T) {
	t.Parallel()
	definitions := Definitions()
	const openaiRequest = `{"model":"m","input":"a"}`
	const openaiData = `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[0.5]}]`
	const vertexRequest = `{"instances":[{"content":"a"},{"content":"b"}]}`
	const titanRequest = `{"inputText":"a"}`
	const cohereRequest = `{"model":"m","input_type":"search_document","texts":["a"],"embedding_types":["float"]}`
	const cohereVectors = `{"id":"e","embeddings":{"float":[[0.5]]},"meta":{"billed_units":`
	for _, tc := range []struct{ name, dialect, request, result, want string }{
		{"OpenAI", "openai-embeddings", openaiRequest, openaiData + `,"usage":{"prompt_tokens":4,"total_tokens":4}}`, "input 4 total 4"},
		{"OpenAI usage absent", "openai-embeddings", openaiRequest, openaiData + `}`, "unobserved"},
		{"OpenAI total missing", "openai-embeddings", openaiRequest, openaiData + `,"usage":{"prompt_tokens":4}}`, "rejected"},
		{"OpenAI total below prompt", "openai-embeddings", openaiRequest, openaiData + `,"usage":{"prompt_tokens":4,"total_tokens":3}}`, "rejected"},
		{"Voyage bills the total", "voyage-embeddings", openaiRequest, openaiData + `,"usage":{"total_tokens":9}}`, "input 9 total 9"},
		{"Gemini", "gemini-embeddings", `{"content":{"parts":[{"text":"a"}]}}`, `{"embedding":{"values":[0.5]},"usageMetadata":{"promptTokenCount":7}}`, "input 7 total 7"},
		{"Gemini count absent", "gemini-embeddings", `{"content":{"parts":[{"text":"a"}]}}`, `{"embedding":{"values":[0.5]},"usageMetadata":{}}`, "unobserved"},
		{"Gemini negative count", "gemini-embeddings", `{"content":{"parts":[{"text":"a"}]}}`, `{"embedding":{"values":[0.5]},"usageMetadata":{"promptTokenCount":-1}}`, "rejected"},
		{"Vertex predictions are summed", "vertex-embeddings", vertexRequest, `{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":3}}},{"embeddings":{"values":[0.5],"statistics":{"token_count":4}}}]}`, "input 7 total 7"},
		{"Vertex partial counts are unobserved", "vertex-embeddings", vertexRequest, `{"predictions":[{"embeddings":{"values":[0.5],"statistics":{"token_count":3}}},{"embeddings":{"values":[0.5]}}]}`, "unobserved"},
		{"Titan", "bedrock-embeddings", titanRequest, `{"embedding":[0.5],"inputTextTokenCount":5}`, "input 5 total 5"},
		{"Titan count absent", "bedrock-embeddings", titanRequest, `{"embedding":[0.5]}`, "unobserved"},
		{"Titan negative count", "bedrock-embeddings", titanRequest, `{"embedding":[0.5],"inputTextTokenCount":-1}`, "rejected"},
		{"Cohere billed tokens", "cohere-embed-v2", cohereRequest, cohereVectors + `{"input_tokens":2}}}`, "input 2 total 2"},
		{"Cohere billed images", "cohere-embed-v2", cohereRequest, cohereVectors + `{"images":1}}}`, "images 1"},
		{"Cohere tokens and images", "cohere-embed-v2", cohereRequest, cohereVectors + `{"input_tokens":2,"images":1}}}`, "input 2 total 2 images 1"},
		{"Cohere null counts", "cohere-embed-v2", cohereRequest, cohereVectors + `{"input_tokens":null,"images":null}}}`, "unobserved"},
		{"Cohere negative tokens", "cohere-embed-v2", cohereRequest, cohereVectors + `{"input_tokens":-1}}}`, "rejected"},
		{"Cohere negative images", "cohere-embed-v2", cohereRequest, cohereVectors + `{"images":-1}}}`, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			definition := definitions[slices.IndexFunc(definitions, func(d operations.Dialect) bool { return d.Identity.ID == tc.dialect })]
			descriptor := oif.Descriptor{Operation: definition.Operation, Dialect: definition.Identity}
			parse := func(body string) oif.Document {
				doc, err := oif.ParseJSON([]byte(body), oif.Limits{MaxBytes: 1 << 16, MaxDepth: 32, MaxNodes: 1024})
				if err != nil {
					t.Fatal(err)
				}
				return doc
			}
			request, err := oif.NewRequest(descriptor, parse(tc.request))
			if err != nil {
				t.Fatal(err)
			}
			result, err := oif.NewResult(descriptor, parse(tc.result), oif.Complete)
			if err != nil {
				t.Fatal(err)
			}
			view, err := definition.Result(request, result)
			if got := describeUsage(definition, view, err); got != tc.want {
				t.Fatalf("usage = %s, want %s: %v", got, tc.want, err)
			}
		})
	}
}

func describeUsage(definition operations.Dialect, view oif.View, err error) string {
	if err != nil {
		return "rejected"
	}
	usage := definition.Usage(view)
	switch {
	case usage == nil:
		return "unobserved"
	case usage.InputTokens == nil:
		return "images " + *usage.MediaUnits
	case usage.MediaUnits != nil:
		return fmt.Sprintf("input %d total %d images %s", *usage.InputTokens, *usage.TotalTokens, *usage.MediaUnits)
	}
	return fmt.Sprintf("input %d total %d", *usage.InputTokens, *usage.TotalTokens)
}
