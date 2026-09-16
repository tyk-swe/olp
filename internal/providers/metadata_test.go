package providers

import "testing"

func TestMetadataDefaultsKeepUnknownSeparateFromFalseAndEmpty(t *testing.T) {
	unknown, err := metadataJSON([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"supported_parameters", "data_collection", "zero_data_retention"} {
		if string(unknown[name]) != "null" {
			t.Fatalf("unknown %s became a fact: %s", name, unknown[name])
		}
	}
	if string(unknown["input_modalities"]) != "[]" || string(unknown["output_modalities"]) != "[]" {
		t.Fatal("non-nullable modality collections were omitted")
	}
	known, err := metadataJSON([]byte(`{"supported_parameters":[],"data_collection":false,"context_length":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(known["supported_parameters"]) != "[]" || string(known["data_collection"]) != "false" || string(known["context_length"]) != "9007199254740993" {
		t.Fatalf("metadata changed: %v", known)
	}
}
