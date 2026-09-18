package protocols

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestGeminiCountKeepsNestedGenerationAndExtensions(t *testing.T) {
	fields, err := object([]byte(`{
		"model":"models/route",
		"generateContentRequest":{
			"model":"models/upstream",
			"contents":[{"role":"user","parts":[{"text":"hello"}]}],
			"generationConfig":{"maxOutputTokens":23,"vendor_option":true},
			"inner_extension":true
		},
		"outer_extension":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := decodeCanonical(openai.FamilyGeminiCount, fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.Messages) != 1 || generation.Messages[0].Role != "user" || len(generation.Messages[0].Parts) != 1 || generation.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("nested messages were lost: %+v", generation.Messages)
	}
	if string(generation.Parameters["max_output_tokens"]) != "23" {
		t.Fatalf("nested parameters were lost: %v", generation.Parameters)
	}
	slices.Sort(generation.Extensions)
	want := []string{"/generationConfig/vendor_option", "/inner_extension", "/outer_extension"}
	if !slices.Equal(generation.Extensions, want) {
		t.Fatalf("extension paths = %v, want %v", generation.Extensions, want)
	}
	after, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("canonical decoding mutated the source envelope")
	}
}
