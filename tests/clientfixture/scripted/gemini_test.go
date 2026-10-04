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

func TestGeminiTextStreamAssemblesToTheUnaryText(t *testing.T) {
	h := newHarness(t)
	request := map[string]any{"contents": geminiPrompt(`[[olp:reply "one two three"]]`)}
	unary := h.gemini("generateContent", request)
	if joinText(path(unary.json(), "candidates", 0, "content", "parts").([]any)) != "one two three" {
		t.Fatalf("%s", unary.body)
	}
	stream := h.do("POST", GeminiPrefix+"/models/"+GeminiModel+":streamGenerateContent?alt=sse", map[string]string{"X-Goog-Api-Key": testCredential}, request)
	parts, _, _ := streamedParts(t, stream, true)
	if len(parts) < 3 || joinText(parts) != "one two three" {
		t.Fatalf("streamed parts %v", parts)
	}
}

func TestGeminiToolConfigAndStructuredOutput(t *testing.T) {
	h := newHarness(t)
	forced := h.gemini("generateContent", map[string]any{"contents": geminiPrompt("x"), "tools": []any{weatherDeclaration},
		"toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []any{"get_weather"}}}})
	if path(forced.json(), "candidates", 0, "content", "parts", 0, "functionCall", "args", "city") != "fixture" {
		t.Fatalf("%s", forced.body)
	}
	none := h.gemini("generateContent", map[string]any{"contents": geminiPrompt(weatherPrompt), "tools": []any{weatherDeclaration},
		"toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}})
	if !strings.Contains(joinText(path(none.json(), "candidates", 0, "content", "parts").([]any)), "not declared") {
		t.Fatalf("%s", none.body)
	}
	for name, config := range map[string]map[string]any{
		"responseSchema":     {"responseMimeType": "application/json", "responseSchema": map[string]any{"type": "OBJECT", "properties": map[string]any{"a": map[string]any{"type": "INTEGER"}}}},
		"responseJsonSchema": {"responseMimeType": "application/json", "responseJsonSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "integer"}}}},
	} {
		r := h.gemini("generateContent", map[string]any{"contents": geminiPrompt("x"), "generationConfig": config})
		if joinText(path(r.json(), "candidates", 0, "content", "parts").([]any)) != `{"a":42}` {
			t.Errorf("%s: %s", name, r.body)
		}
	}
	jsonOnly := h.gemini("generateContent", map[string]any{"contents": geminiPrompt("x"), "generationConfig": map[string]any{"responseMimeType": "application/json"}})
	if joinText(path(jsonOnly.json(), "candidates", 0, "content", "parts").([]any)) != `{"result":"fixture"}` {
		t.Fatalf("%s", jsonOnly.body)
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

func TestGeminiCountTokens(t *testing.T) {
	h := newHarness(t)
	direct := h.gemini("countTokens", map[string]any{"contents": geminiPrompt("count these words please")})
	want(t, direct, 200)
	wrapped := h.gemini("countTokens", map[string]any{"generateContentRequest": map[string]any{"model": "models/" + GeminiModel, "contents": geminiPrompt("count these words please")}})
	if direct.json()["totalTokens"] != wrapped.json()["totalTokens"] || direct.json()["totalTokens"].(float64) < 1 ||
		path(direct.json(), "promptTokensDetails", 0, "modality") != "TEXT" {
		t.Fatalf("direct %s, wrapped %s", direct.body, wrapped.body)
	}
	want(t, h.gemini("countTokens", map[string]any{}), http.StatusBadRequest)
}

// The prompt tokens of a reply are what countTokens counts for the same prompt:
// the contents and the system instruction, however the request is configured.
func TestGeminiPromptTokensDoNotDependOnTheConfiguration(t *testing.T) {
	h := newHarness(t)
	prompt := map[string]any{"contents": geminiPrompt("count these words please"), "systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "be brief"}}}}
	counted := h.gemini("countTokens", prompt)
	want(t, counted, 200)
	total := counted.json()["totalTokens"].(float64)
	bare := h.gemini("countTokens", map[string]any{"contents": prompt["contents"]})
	if bare.json()["totalTokens"].(float64) >= total {
		t.Fatalf("the system instruction is not counted: %s, with it %s", bare.body, counted.body)
	}

	for name, configuration := range map[string]map[string]any{
		"nothing":     {},
		"generation":  {"generationConfig": map[string]any{"temperature": 0.5, "maxOutputTokens": 64, "stopSequences": []any{"END"}}},
		"tools":       {"tools": []any{weatherDeclaration}},
		"tool config": {"tools": []any{weatherDeclaration}, "toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}},
		"thinking":    {"generationConfig": map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			request := map[string]any{}
			for k, v := range prompt {
				request[k] = v
			}
			for k, v := range configuration {
				request[k] = v
			}
			reply := h.gemini("generateContent", request)
			want(t, reply, 200)
			if got := path(reply.json(), "usageMetadata", "promptTokenCount"); got != total {
				t.Errorf("generateContent reported %v prompt tokens, countTokens counted %v: %s", got, total, reply.body)
			}
			if got := path(reply.json(), "usageMetadata", "promptTokensDetails", 0, "tokenCount"); got != total {
				t.Errorf("the prompt token details are %v, want %v", got, total)
			}
			// The configuration does not change the count of a wrapped request either.
			wrapped := h.gemini("countTokens", map[string]any{"generateContentRequest": request})
			want(t, wrapped, 200)
			if got := wrapped.json()["totalTokens"]; got != total {
				t.Errorf("a wrapped request counted %v tokens, want %v: %s", got, total, wrapped.body)
			}
		})
	}
}

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

func TestGeminiRoutingAndFailures(t *testing.T) {
	h := newHarness(t)
	key := map[string]string{"X-Goog-Api-Key": testCredential}
	r := h.do("POST", GeminiPrefix+"/models/other:generateContent", key, map[string]any{"contents": geminiPrompt("x")})
	want(t, r, http.StatusNotFound)
	if path(r.json(), "error", "status") != "NOT_FOUND" {
		t.Fatalf("%s", r.body)
	}
	want(t, h.do("POST", GeminiPrefix+"/models/"+GeminiModel+":unknownAction", key, map[string]any{}), http.StatusNotFound)
	want(t, h.gemini("generateContent", map[string]any{}), http.StatusBadRequest)
	r = h.gemini("generateContent", map[string]any{"contents": geminiPrompt("[[olp:fail 503]]")})
	want(t, r, http.StatusServiceUnavailable)
	if path(r.json(), "error", "status") != "UNAVAILABLE" || path(r.json(), "error", "code") != float64(503) {
		t.Fatalf("%s", r.body)
	}
	for _, rec := range h.recorded("?dialect=gemini") {
		if rec.Model == "" {
			t.Errorf("record %d has no model", rec.Seq)
		}
	}
}
