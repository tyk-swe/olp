package embeddings_test

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
	"testing"
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
func TestNativeStorageAndLogicalShape(t *testing.T) {
	for _, test := range []struct {
		name, dtype, encoding, storage string
		dimension, stored, bytes       int
	}{
		{"signed bytes", "int8", "null", `[-128,127]`, 2, 2, 0}, {"unsigned bytes", "uint8", "null", `[0,255]`, 2, 2, 0}, {"packed signed", "binary", "null", `[-128,127]`, 16, 2, 0}, {"packed unsigned", "ubinary", "null", `[0,255]`, 16, 2, 0},
		{"packed base64", "ubinary", `"base64"`, `"AP8="`, 16, 2, 2}, {"signed base64", "int8", `"base64"`, `"gH8="`, 2, 2, 2}, {"float bytes", "float", `"base64"`, `"AACAPwAAAMA="`, 2, 2, 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := codec(t, "voyage-embeddings")
			raw := `{"model":"m","input":"a","output_dtype":"` + test.dtype + `","encoding_format":` + test.encoding + `}`
			r := request(t, d, raw)
			response := `{"data":[{"index":0,"embedding":` + test.storage + `}],"usage":{"total_tokens":1},"provider_metadata":{"huge":9007199254740993}}`
			view, err := result(t, d, r, response)
			if err != nil {
				t.Fatal(err)
			}
			got := view.(embeddings.Result)
			v := got.Vectors()[0]
			if v.Source.Raw() != test.storage || v.Format.LogicalDimensions != int64(test.dimension) || v.StoredElements != test.stored || v.StoredBytes != test.bytes || got.Source().Source().Raw() != response {
				t.Fatalf("native representation changed: %+v", v)
			}
		})
	}
}
func TestCorruptNativeVectorsRejectWithoutConversions(t *testing.T) {
	d := codec(t, "voyage-embeddings")
	for _, test := range []struct{ request, response string }{
		{`{"model":"m","input":"a","output_dtype":"int8"}`, `{"data":[{"index":0,"embedding":[128]}]}`},
		{`{"model":"m","input":"a","output_dtype":"uint8"}`, `{"data":[{"index":0,"embedding":[-1]}]}`},
		{`{"model":"m","input":"a","output_dtype":"binary","output_dimension":16}`, `{"data":[{"index":0,"embedding":[1]}]}`},
		{`{"model":"m","input":"a","encoding_format":"base64"}`, `{"data":[{"index":0,"embedding":"AA=="}]}`},
		{`{"model":"m","input":["a","b"]}`, `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`},
	} {
		r := request(t, d, test.request)
		if _, err := result(t, d, r, test.response); err == nil {
			t.Fatalf("invalid vector accepted: %s", test.response)
		}
	}
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
