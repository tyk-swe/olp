package scripted

import (
	"encoding/json"
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

// streamedParts folds a Gemini stream, in either framing, into its parts and
// the usage of the final chunk.
func streamedParts(t *testing.T, r response, sse bool) (parts []any, finish string, usage any) {
	t.Helper()
	var chunks []map[string]any
	if sse {
		chunks = decodeFrames(t, r.body)
	} else if err := json.Unmarshal(r.body, &chunks); err != nil {
		t.Fatalf("stream is not one JSON array: %v: %s", err, r.body)
	}
	for _, c := range chunks {
		parts = append(parts, path(c, "candidates", 0, "content", "parts").([]any)...)
		if f, ok := path(c, "candidates", 0, "finishReason").(string); ok {
			finish = f
		}
		if u := c["usageMetadata"]; u != nil {
			usage = u
		}
	}
	return parts, finish, usage
}

func joinText(parts []any) string {
	var text string
	for _, p := range parts {
		if s, ok := path(p, "text").(string); ok && path(p, "thought") != true {
			text += s
		}
	}
	return text
}

func TestGeminiFunctionCallingLoop(t *testing.T) {
	h := newHarness(t)
	request := map[string]any{"contents": geminiPrompt(weatherPrompt), "tools": []any{weatherDeclaration}, "generationConfig": map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true}}}
	unary := h.gemini("generateContent", request)
	want(t, unary, 200)
	parts := path(unary.json(), "candidates", 0, "content", "parts").([]any)
	if len(parts) != 2 || path(parts[0], "thought") != true || !reflect.DeepEqual(path(parts[1], "functionCall"), map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}) ||
		path(unary.json(), "usageMetadata", "thoughtsTokenCount").(float64) < 1 || path(unary.json(), "candidates", 0, "finishReason") != "STOP" {
		t.Fatalf("unary: %s", unary.body)
	}
	// The step that reasoned signs its call; one that did not leaves it unsigned.
	if path(parts[1], "thoughtSignature") != GeminiThoughtSignature {
		t.Fatalf("function call is not signed: %s", unary.body)
	}
	plain := h.gemini("generateContent", map[string]any{"contents": geminiPrompt(weatherPrompt), "tools": []any{weatherDeclaration}})
	want(t, plain, 200)
	if path(plain.json(), "candidates", 0, "content", "parts", 0, "thoughtSignature") != nil {
		t.Fatalf("a step without reasoning is signed: %s", plain.body)
	}

	for name, sse := range map[string]bool{"sse": true, "array": false} {
		t.Run(name, func(t *testing.T) {
			url := GeminiPrefix + "/models/" + GeminiModel + ":streamGenerateContent"
			if sse {
				url += "?alt=sse"
			}
			stream := h.do("POST", url, map[string]string{"X-Goog-Api-Key": testCredential}, request)
			want(t, stream, 200)
			streamed, finish, usage := streamedParts(t, stream, sse)
			if !reflect.DeepEqual(streamed, parts) || finish != "STOP" || usage == nil {
				t.Fatalf("streamed %v finish %s usage %v, want %v", streamed, finish, usage, parts)
			}
			wantType := "application/json"
			if sse {
				wantType = "text/event-stream"
				if !strings.Contains(string(stream.body), "\r\n\r\n") {
					t.Fatalf("SSE frames are not CRLF delimited: %q", stream.body)
				}
			}
			if got := stream.headers.Get("Content-Type"); got != wantType {
				t.Fatalf("content type %q", got)
			}
		})
	}

	followup := append(geminiPrompt(weatherPrompt),
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "get_weather", "response": map[string]any{"output": "sunny"}}}}})
	final := h.gemini("generateContent", map[string]any{"contents": followup, "tools": []any{weatherDeclaration}})
	want(t, final, 200)
	if joinText(path(final.json(), "candidates", 0, "content", "parts").([]any)) != "The tools returned: sunny" {
		t.Fatalf("final: %s", final.body)
	}
	// A structured function response is reduced to its JSON.
	followup[2].(map[string]any)["parts"] = []any{map[string]any{"functionResponse": map[string]any{"name": "get_weather", "response": map[string]any{"temp": 21, "sky": "clear"}}}}
	structured := h.gemini("generateContent", map[string]any{"contents": followup, "tools": []any{weatherDeclaration}})
	if joinText(path(structured.json(), "candidates", 0, "content", "parts").([]any)) != `The tools returned: {"sky":"clear","temp":21}` {
		t.Fatalf("%s", structured.body)
	}
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
