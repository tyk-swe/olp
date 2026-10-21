package scripted

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"reflect"
	"testing"
)

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

const weatherPrompt = `What is the weather in Paris? [[olp:tool get_weather {"city":"Paris"}]]`

func TestEmbeddingsAreDeterministicUnitVectorsInBothEncodings(t *testing.T) {
	h := newHarness(t)
	floats := h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": []string{"alpha", "beta"}, "dimensions": 6})
	want(t, floats, 200)
	data := floats.json()["data"].([]any)
	if len(data) != 2 || path(data[1], "index") != float64(1) || path(data[0], "object") != "embedding" {
		t.Fatalf("%s", floats.body)
	}
	vector := func(v any) []float64 {
		var out []float64
		for _, x := range v.([]any) {
			out = append(out, x.(float64))
		}
		return out
	}
	first, second := vector(path(data[0], "embedding")), vector(path(data[1], "embedding"))
	if len(first) != 6 || reflect.DeepEqual(first, second) {
		t.Fatalf("vectors %v %v", first, second)
	}
	var norm float64
	for _, x := range first {
		norm += x * x
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("norm %v", norm)
	}
	again := h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": "alpha", "dimensions": 6, "encoding_format": "base64"})
	raw, err := base64.StdEncoding.DecodeString(path(again.json(), "data", 0, "embedding").(string))
	if err != nil || len(raw) != 24 {
		t.Fatalf("base64 %v %d", err, len(raw))
	}
	for i := range first {
		decoded := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:])))
		if math.Abs(decoded-first[i]) > 1e-6 {
			t.Fatalf("base64 element %d is %v, float form %v", i, decoded, first[i])
		}
	}
	if defaulted := h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": "alpha"}); len(path(defaulted.json(), "data", 0, "embedding").([]any)) != defaultDimensions {
		t.Fatalf("default dimensions: %s", defaulted.body)
	}
	want(t, h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": []int{1, 2}}), 200)
	want(t, h.openai("/embeddings", map[string]any{"model": OpenAIModel}), http.StatusBadRequest)
	want(t, h.openai("/embeddings", map[string]any{"model": OpenAIModel, "input": "x", "encoding_format": "hex"}), http.StatusBadRequest)
}

// --- Responses ---

var weatherFunction = map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}}

func respond(h *harness, body map[string]any) response {
	body["model"] = OpenAIModel
	return h.openai("/responses", body)
}

// A call without its output, and an output without its call, are refused as
// OpenAI refuses them.

func TestStoredResponseContinuation(t *testing.T) {
	h := newHarness(t)
	first := respond(h, map[string]any{"input": weatherPrompt, "tools": []any{weatherFunction}})
	want(t, first, 200)
	id := first.json()["id"].(string)
	if first.json()["store"] != true {
		t.Fatalf("store defaults to true: %s", first.body)
	}
	call := path(first.json(), "output", 0).(map[string]any)

	// The continuation sends only the new item; the tool call and the prompt
	// with its script come from the stored response.
	second := respond(h, map[string]any{"previous_response_id": id, "tools": []any{weatherFunction},
		"input": []any{map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "rainy"}}})
	want(t, second, 200)
	if path(second.json(), "output", 0, "content", 0, "text") != "The tools returned: rainy" || second.json()["previous_response_id"] != id {
		t.Fatalf("continuation: %s", second.body)
	}

	// Without the stored history the same input is an orphan output.
	want(t, respond(h, map[string]any{"tools": []any{weatherFunction}, "input": []any{map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "rainy"}}}), http.StatusBadRequest)

	missing := respond(h, map[string]any{"previous_response_id": "resp_nope", "input": "hi"})
	want(t, missing, http.StatusNotFound)
	if path(missing.json(), "error", "code") != "previous_response_not_found" {
		t.Fatalf("%s", missing.body)
	}

	// A response sent with store false cannot be continued.
	unstored := respond(h, map[string]any{"input": "hi", "store": false})
	want(t, respond(h, map[string]any{"previous_response_id": unstored.json()["id"], "input": "again"}), http.StatusNotFound)

	// Retrieval, input items and deletion follow the same store.
	auth := map[string]string{"Authorization": "Bearer " + testCredential}
	got := h.do("GET", OpenAIPrefix+"/responses/"+id, auth, nil)
	want(t, got, 200)
	if got.json()["id"] != id {
		t.Fatalf("%s", got.body)
	}
	items := h.do("GET", OpenAIPrefix+"/responses/"+id+"/input_items", auth, nil)
	want(t, items, 200)
	if n := len(items.json()["data"].([]any)); n != 1 || path(items.json(), "data", 0, "id") == nil {
		t.Fatalf("input items: %s", items.body)
	}
	deleted := h.do("DELETE", OpenAIPrefix+"/responses/"+id, auth, nil)
	want(t, deleted, 200)
	want(t, h.do("GET", OpenAIPrefix+"/responses/"+id, auth, nil), http.StatusNotFound)
	want(t, h.do("DELETE", OpenAIPrefix+"/responses/"+id, auth, nil), http.StatusNotFound)
}

// A long non-ASCII result is clipped to whole characters.
