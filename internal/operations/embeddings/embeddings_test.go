package embeddings_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
)

func codec(t *testing.T, id string) operations.Dialect {
	t.Helper()
	for _, d := range embeddings.Definitions() {
		if d.Identity.ID == id {
			return d
		}
	}
	t.Fatal(id)
	return operations.Dialect{}
}
func request(t *testing.T, d operations.Dialect, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity}, doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Request(r); err != nil {
		t.Fatal(err)
	}
	return r
}
func result(t *testing.T, d operations.Dialect, r oif.Request, raw string) (oif.View, error) {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	native, _ := oif.NewResult(r.Descriptor(), doc, oif.Complete)
	return d.Result(r, native)
}

func TestSparseDimensionUnknownAndMultivectorRankRetained(t *testing.T) {
	d := codec(t, "tei-sparse-embeddings")
	r := request(t, d, `{"inputs":"a"}`)
	view, err := result(t, d, r, `[[{"index":999,"value":-0},{"index":2,"value":1e-4}]]`)
	if err != nil {
		t.Fatal(err)
	}
	v := view.(embeddings.Result).Vectors()[0]
	if v.Format.DimensionsKnown || v.StoredElements != 2 || v.Source.Elements()[0].Raw() != `{"index":999,"value":-0}` {
		t.Fatal("sparse logical dimension invented or source reordered")
	}
	d = codec(t, "tei-multivector-embeddings")
	r = request(t, d, `{"inputs":["a","b"]}`)
	view, err = result(t, d, r, `[[[1.1,2.2],[3.3,4.4]],[[5.5,6.6]]]`)
	if err != nil {
		t.Fatal(err)
	}
	vectors := view.(embeddings.Result).Vectors()
	if vectors[0].TokenVectors != 2 || vectors[1].TokenVectors != 1 || vectors[0].Format.LogicalDimensions != 2 {
		t.Fatal("token-vector layout flattened")
	}
}
func TestGeminiEmbeddingUsageIsThePromptTokens(t *testing.T) {
	for _, test := range []struct{ id, request, vectors string }{
		{"gemini-embeddings", `{"model":"models/m","content":{"parts":[{"text":"a"}]}}`, `"embedding":{"values":[0.5]}`},
		{"gemini-batch-embeddings", `{"requests":[{"model":"models/m","content":{"parts":[{"text":"a"}]}}]}`, `"embeddings":[{"values":[0.5]}]`},
	} {
		t.Run(test.id, func(t *testing.T) {
			d := codec(t, test.id)
			r := request(t, d, test.request)
			for _, usage := range []struct {
				metadata string
				tokens   *int64
			}{
				{`,"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}`, ptr(3)},
				{`,"usageMetadata":{"promptTokenCount":3}`, ptr(3)},
				{`,"usageMetadata":{"promptTokenCount":0}`, ptr(0)},
				{``, nil},
				{`,"usageMetadata":null`, nil},
				{`,"usageMetadata":{}`, nil},
				{`,"usageMetadata":{"promptTokenCount":null}`, nil},
			} {
				view, err := result(t, d, r, `{`+test.vectors+usage.metadata+`}`)
				if err != nil {
					t.Fatalf("%s: %v", usage.metadata, err)
				}
				got := d.Usage(view)
				switch {
				case usage.tokens == nil && got != nil:
					t.Fatalf("%s: usage invented: %+v", usage.metadata, got)
				case usage.tokens != nil && (got == nil || *got.InputTokens != *usage.tokens || *got.TotalTokens != *usage.tokens):
					t.Fatalf("%s: usage %+v, want %d prompt tokens", usage.metadata, got, *usage.tokens)
				}
			}
			for _, metadata := range []string{`"x"`, `[3]`, `{"promptTokenCount":-1}`, `{"promptTokenCount":"3"}`, `{"promptTokenCount":1.5}`} {
				if _, err := result(t, d, r, `{`+test.vectors+`,"usageMetadata":`+metadata+`}`); err == nil {
					t.Fatalf("invalid usage accepted: %s", metadata)
				}
			}
		})
	}
}

func ptr(n int64) *int64 { return &n }
