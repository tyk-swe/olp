package embeddings_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/operations/embeddings"
)

func TestCohereBase64StorageDoesNotInventFloatWidth(t *testing.T) {
	d := codec(t, "cohere-embed-v2")
	r := request(t, d, `{"model":"m","input_type":"search_document","texts":["one"],"embedding_types":["base64"]}`)
	view, err := result(t, d, r, `{"embeddings":{"base64":["AACAPwAAAMA="]},"meta":{"billed_units":{"input_tokens":2}}}`)
	if err != nil {
		t.Fatal(err)
	}
	vector := view.(embeddings.Result).Vectors()[0]
	if vector.StoredBytes != 8 || vector.StoredElements != 0 || vector.Format.DimensionsKnown || vector.Format.DType != "native" || vector.Source.Raw() != `"AACAPwAAAMA="` {
		t.Fatalf("native encoded storage was cast to assumed float32: %+v", vector)
	}
	withFloat := request(t, d, `{"model":"m","input_type":"search_document","texts":["one"],"embedding_types":["float","base64"]}`)
	view, err = result(t, d, withFloat, `{"embeddings":{"float":[[1,2,3,4,5,6,7,8]],"base64":["AACAPwAAAMA="]}}`)
	if err != nil {
		t.Fatal(err)
	}
	encoded := view.(embeddings.Result).Vectors()[1]
	if !encoded.Format.DimensionsKnown || encoded.Format.LogicalDimensions != 8 || encoded.StoredBytes != 8 {
		t.Fatalf("the actual native float group did not provide dimensional correspondence: %+v", encoded)
	}
	if _, err := result(t, d, r, `{"embeddings":{"base64":["not-base64"]}}`); err == nil {
		t.Fatal("malformed provider base64 became successful native storage")
	}
}

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
