package scripted

import (
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

var weatherDeclaration = map[string]any{"functionDeclarations": []any{map[string]any{
	"name": "get_weather", "parameters": map[string]any{"type": "OBJECT", "properties": map[string]any{"city": map[string]any{"type": "STRING"}}},
}}}

func geminiPrompt(text string) []any {
	return []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}
}

// Gemini refuses a member of a schema that is not in its OpenAPI subset, in the
// fields that take the subset, and takes JSON Schema as it is in the fields that
// take that. The fixture does the same, so that a gateway that sends one schema
// in the other's field fails a suite and not only a provider.
func TestGeminiRefusesJSONSchemaInTheOpenAPIFields(t *testing.T) {
	h := newHarness(t)
	jsonSchema := map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object", "additionalProperties": false,
		"properties": map[string]any{"city": map[string]any{"type": "string"}}}
	nested := map[string]any{"type": "OBJECT", "properties": map[string]any{"city": map[string]any{"anyOf": []any{map[string]any{"type": "STRING", "const": "x"}}}}}
	for _, tc := range []struct {
		name    string
		request map[string]any
		refused string
	}{
		{"parameters", map[string]any{"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "f", "parameters": jsonSchema}}}}}, `Unknown name "$schema" at 'tools[0].function_declarations[0].parameters'`},
		{"a member of a nested schema", map[string]any{"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "f", "parameters": nested}}}}},
			`Unknown name "const" at 'tools[0].function_declarations[0].parameters.properties["city"].value.any_of[0]'`},
		{"responseSchema", map[string]any{"generationConfig": map[string]any{"responseMimeType": "application/json", "responseSchema": jsonSchema}}, `Unknown name "$schema" at 'generation_config.response_schema'`},
		{"parametersJsonSchema", map[string]any{"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "f", "parametersJsonSchema": jsonSchema}}}}}, ""},
		{"responseJsonSchema", map[string]any{"generationConfig": map[string]any{"responseMimeType": "application/json", "responseJsonSchema": jsonSchema}}, ""},
		{"the subset", map[string]any{"tools": []any{weatherDeclaration}}, ""},
	} {
		for _, action := range []string{"generateContent", "countTokens"} {
			body := map[string]any{"contents": geminiPrompt("x")}
			maps.Copy(body, tc.request)
			r := h.gemini(action, body)
			if tc.refused == "" {
				want(t, r, 200)
				continue
			}
			want(t, r, 400)
			if message, _ := path(r.json(), "error", "message").(string); !strings.Contains(message, tc.refused) {
				t.Errorf("%s %s: %s, want it to refuse %s", tc.name, action, r.body, tc.refused)
			}
		}
	}
}

// The configuration does not change the count of a wrapped request either.

func TestGeminiEmbeddings(t *testing.T) {
	h := newHarness(t)
	single := h.gemini("embedContent", map[string]any{"model": "models/" + GeminiModel, "content": map[string]any{"parts": []any{map[string]any{"text": "alpha"}}}, "outputDimensionality": 5, "taskType": "RETRIEVAL_QUERY"})
	want(t, single, 200)
	values := path(single.json(), "embedding", "values").([]any)
	if len(values) != 5 {
		t.Fatalf("%s", single.body)
	}
	batch := h.gemini("batchEmbedContents", map[string]any{"requests": []any{
		map[string]any{"model": "models/" + GeminiModel, "content": map[string]any{"parts": []any{map[string]any{"text": "alpha"}}}, "outputDimensionality": 5},
		map[string]any{"model": "models/" + GeminiModel, "content": map[string]any{"parts": []any{map[string]any{"text": "beta"}}}, "outputDimensionality": 5},
	}})
	want(t, batch, 200)
	embeddings := batch.json()["embeddings"].([]any)
	if len(embeddings) != 2 || !reflect.DeepEqual(path(embeddings[0], "values"), path(single.json(), "embedding", "values")) || reflect.DeepEqual(path(embeddings[0], "values"), path(embeddings[1], "values")) {
		t.Fatalf("%s", batch.body)
	}
	// The same text embeds the same through the OpenAI vendor, so a
	// translated route is checkable end to end.
	openai := h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": "alpha", "dimensions": 5})
	if !reflect.DeepEqual(path(openai.json(), "data", 0, "embedding"), path(single.json(), "embedding", "values")) {
		t.Fatalf("openai %s, gemini %s", openai.body, single.body)
	}
	want(t, h.gemini("batchEmbedContents", map[string]any{"requests": []any{map[string]any{"model": "models/other", "content": map[string]any{"parts": []any{map[string]any{"text": "x"}}}}}}), http.StatusBadRequest)
	want(t, h.gemini("embedContent", map[string]any{}), http.StatusBadRequest)
}
