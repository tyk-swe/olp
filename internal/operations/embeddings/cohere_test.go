package embeddings_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/operations/embeddings"
)

func TestCohereDocumentedNativeStorageGroupsShareLogicalDimensions(t *testing.T) {
	d := codec(t, "cohere-embed-v2")
	r := request(t, d, `{"model":"m","input_type":"search_document","texts":["one"],"embedding_types":["float","int8","uint8","binary","ubinary","base64"]}`)
	view, err := result(t, d, r, `{"embeddings":{"float":[[0.10000000000000001,-0,1,2,3,4,5,6]],"int8":[[-128,127,0,1,2,3,4,5]],"uint8":[[0,255,1,2,3,4,5,6]],"binary":[[-128]],"ubinary":[[128]],"base64":["AQIDBA=="]}}`)
	if err != nil {
		t.Fatal(err)
	}
	vectors := view.(embeddings.Result).Vectors()
	if len(vectors) != 6 {
		t.Fatalf("native storage group disappeared: %d", len(vectors))
	}
	for _, vector := range vectors {
		if !vector.Format.DimensionsKnown || vector.Format.LogicalDimensions != 8 || vector.InputIndex != 0 {
			t.Fatalf("typed groups disagree on one native embedding: %+v", vector)
		}
	}
	if vectors[3].Format.Layout != "packed-binary" || vectors[4].Format.Layout != "packed-binary" || vectors[5].StoredBytes != 4 || vectors[5].StoredElements != 0 {
		t.Fatalf("native packed/opaque storage was normalized: %+v", vectors)
	}
	if _, err := result(t, d, r, `{"embeddings":{"float":[[1,2]],"int8":[[1,2]],"uint8":[[1,2]],"binary":[[1]],"ubinary":[[1]],"base64":["AQIDBA=="]}}`); err == nil {
		t.Fatal("contradictory native dtype widths were accepted")
	}
}
