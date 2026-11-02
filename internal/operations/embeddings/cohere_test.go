package embeddings_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
)

func TestCoherePublishedControlsAreAccepted(t *testing.T) {
	d := codec(t, "cohere-embed-v2")
	var schema struct {
		Properties struct {
			InputType struct {
				Enum []string `json:"enum"`
			} `json:"input_type"`
			Truncate struct {
				Enum []string `json:"enum"`
			} `json:"truncate"`
			OutputDimension struct {
				Enum []int64 `json:"enum"`
			} `json:"output_dimension"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.RequestSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties.InputType.Enum) == 0 || len(schema.Properties.Truncate.Enum) == 0 || len(schema.Properties.OutputDimension.Enum) == 0 {
		t.Fatal("published controls have no options")
	}
	for _, mode := range schema.Properties.Truncate.Enum {
		for _, dimension := range schema.Properties.OutputDimension.Enum {
			body, err := json.Marshal(map[string]any{
				"model": "m", "input_type": schema.Properties.InputType.Enum[0], "texts": []string{"one"},
				"truncate": mode, "output_dimension": dimension,
			})
			if err != nil {
				t.Fatal(err)
			}
			request(t, d, string(body))
			document, err := oif.ParseJSON(body, oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"truncate", "output_dimension"} {
				value, _ := document.Root().Lookup(field)
				if err := d.Defaults[field].Validate(value); err != nil {
					t.Errorf("published %s option was rejected as a default: %v", field, err)
				}
			}
		}
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
