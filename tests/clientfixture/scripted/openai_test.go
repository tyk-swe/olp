package scripted

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

var weatherTool = map[string]any{"type": "function", "function": map[string]any{
	"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
}}

const weatherPrompt = `What is the weather in Paris? [[olp:tool get_weather {"city":"Paris"}]]`

func TestChatToolLoopUnaryAndStreamingAgree(t *testing.T) {
	h := newHarness(t)
	request := func(stream bool) map[string]any {
		body := map[string]any{"model": OpenAIModel, "messages": []any{user(weatherPrompt)}, "tools": []any{weatherTool}}
		if stream {
			body["stream"], body["stream_options"] = true, map[string]any{"include_usage": true}
		}
		return body
	}
	unary := h.openai("/chat/completions", request(false))
	want(t, unary, 200)
	call := path(unary.json(), "choices", 0, "message", "tool_calls", 0)
	if path(unary.json(), "choices", 0, "finish_reason") != "tool_calls" || path(call, "function", "name") != "get_weather" ||
		path(call, "function", "arguments") != `{"city":"Paris"}` || path(call, "id") != "call_fx_1_1" || path(unary.json(), "choices", 0, "message", "content") != nil {
		t.Fatalf("unary tool call: %s", unary.body)
	}

	// The stream carries the same call, in pieces.
	stream := h.openai("/chat/completions", request(true))
	want(t, stream, 200)
	if ct := stream.headers.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var id, name, args, finish string
	var usage any
	chunks := decodeFrames(t, stream.body)
	for _, c := range chunks {
		if tc := path(c, "choices", 0, "delta", "tool_calls", 0); tc != nil {
			if v, ok := path(tc, "id").(string); ok {
				id, name = v, path(tc, "function", "name").(string)
			}
			args += path(tc, "function", "arguments").(string)
		}
		if f, ok := path(c, "choices", 0, "finish_reason").(string); ok {
			finish = f
		}
		if u := c["usage"]; u != nil {
			usage = u
		}
	}
	if id != "call_fx_1_1" || name != "get_weather" || args != `{"city":"Paris"}` || finish != "tool_calls" || usage == nil {
		t.Fatalf("streamed call id=%q name=%q args=%q finish=%q usage=%v", id, name, args, finish, usage)
	}
	if !strings.HasSuffix(string(stream.body), "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %q", stream.body)
	}
	// chunks[0] opens the message and chunks[1] the call; the arguments follow.
	if first := path(chunks[2], "choices", 0, "delta", "tool_calls", 0, "function", "arguments").(string); first == "" || first == `{"city":"Paris"}` {
		t.Fatalf("arguments were not split across deltas: first fragment %q", first)
	}

	// The follow-up carries the result and gets the final text.
	followup := []any{user(weatherPrompt),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_fx_1_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Paris"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_fx_1_1", "content": "sunny, 21C"},
	}
	final := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": followup, "tools": []any{weatherTool}})
	want(t, final, 200)
	if path(final.json(), "choices", 0, "message", "content") != "The tools returned: sunny, 21C" || path(final.json(), "choices", 0, "finish_reason") != "stop" {
		t.Fatalf("final: %s", final.body)
	}
	scripts := []string{}
	for _, rec := range h.recorded("") {
		scripts = append(scripts, rec.Script)
	}
	if !reflect.DeepEqual(scripts, []string{"tool_call", "tool_call", "tool_result"}) {
		t.Fatalf("scripts %v", scripts)
	}
}

func TestChatStreamingTextAssemblesToTheUnaryText(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"model": OpenAIModel, "messages": []any{user(`[[olp:reply "one two  three"]]`)}}
	unary := h.openai("/chat/completions", body)
	body["stream"] = true
	stream := h.openai("/chat/completions", body)
	var text strings.Builder
	for _, c := range decodeFrames(t, stream.body) {
		if s, ok := path(c, "choices", 0, "delta", "content").(string); ok {
			text.WriteString(s)
		}
		if c["usage"] != nil {
			t.Fatalf("usage streamed without include_usage: %v", c["usage"])
		}
	}
	if got := path(unary.json(), "choices", 0, "message", "content"); got != "one two  three" || text.String() != got {
		t.Fatalf("unary %v, streamed %q", got, text.String())
	}
}

func TestChatEnforcesToolMessageLinkage(t *testing.T) {
	h := newHarness(t)
	assistant := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}}
	for name, messages := range map[string][]any{
		"unanswered call":   {user("x"), assistant},
		"orphan tool":       {user("x"), map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"}},
		"wrong id":          {user("x"), assistant, map[string]any{"role": "tool", "tool_call_id": "other", "content": "r"}},
		"interrupted calls": {user("x"), assistant, user("again")},
		"late answer":       {user("x"), assistant, user("again"), map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": messages})
			want(t, r, http.StatusBadRequest)
			if path(r.json(), "error", "type") != "invalid_request_error" {
				t.Fatalf("%s", r.body)
			}
		})
	}
}

func TestChatRequestValidation(t *testing.T) {
	h := newHarness(t)
	want(t, h.openai("/chat/completions", map[string]any{"model": "other", "messages": []any{user("x")}}), http.StatusNotFound)
	want(t, h.openai("/chat/completions", map[string]any{"model": OpenAIModel}), http.StatusBadRequest)
	want(t, h.openai("/chat/completions", "{not json"), http.StatusBadRequest)
	r := h.openai("/chat/completions", map[string]any{"model": "other", "messages": []any{user("x")}})
	if path(r.json(), "error", "code") != "model_not_found" {
		t.Fatalf("%s", r.body)
	}
}

func TestChatStructuredOutputAndToolChoice(t *testing.T) {
	h := newHarness(t)
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "age": map[string]any{"type": "integer"}}}
	r := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user("x")},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "p", "schema": schema}}})
	want(t, r, 200)
	if path(r.json(), "choices", 0, "message", "content") != `{"age":42,"name":"fixture"}` {
		t.Fatalf("%s", r.body)
	}
	r = h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user("x")}, "response_format": map[string]any{"type": "json_object"}})
	if path(r.json(), "choices", 0, "message", "content") != `{"result":"fixture"}` {
		t.Fatalf("%s", r.body)
	}
	// A named tool_choice forces the call with schema-derived arguments.
	r = h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user("x")}, "tools": []any{weatherTool},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}})
	if path(r.json(), "choices", 0, "message", "tool_calls", 0, "function", "arguments") != `{"city":"fixture"}` {
		t.Fatalf("%s", r.body)
	}
	// tool_choice none withholds the tools, so a scripted call is refused as undeclared.
	r = h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user(weatherPrompt)}, "tools": []any{weatherTool}, "tool_choice": "none"})
	if c, _ := path(r.json(), "choices", 0, "message", "content").(string); !strings.Contains(c, "not declared") {
		t.Fatalf("%s", r.body)
	}
}

func TestScriptedFailuresUseTheVendorErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	r := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user(`[[olp:fail 429]]`)}})
	want(t, r, http.StatusTooManyRequests)
	if path(r.json(), "error", "type") != "rate_limit_error" {
		t.Fatalf("%s", r.body)
	}
	if got := r.headers.Get("Retry-After"); got != RetryAfterSeconds {
		t.Fatalf("Retry-After %q, want %q", got, RetryAfterSeconds)
	}
	r = h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": []any{user(`[[olp:tool x {bad}]]`)}})
	want(t, r, http.StatusBadRequest)
	if m, _ := path(r.json(), "error", "message").(string); !strings.Contains(m, "malformed") {
		t.Fatalf("%s", r.body)
	}
	rec := h.recorded("")
	if rec[0].Script != "fail" || rec[0].Status != 429 || rec[1].Script != "directive_error" {
		t.Fatalf("records %+v", rec)
	}
}

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

// output reads the output items of a response or, for a stream, the items the
// stream finished, and verifies the stream's own accounting on the way.
func streamedOutput(t *testing.T, body []byte) (items []any, completed map[string]any) {
	t.Helper()
	seq := -1
	for _, f := range frames(t, body) {
		var e map[string]any
		if err := json.Unmarshal([]byte(f.data), &e); err != nil {
			t.Fatal(err)
		}
		if e["type"] != f.event || int(e["sequence_number"].(float64)) != seq+1 {
			t.Fatalf("event %q has type %v and sequence %v after %d", f.event, e["type"], e["sequence_number"], seq)
		}
		seq++
		switch f.event {
		case "response.output_item.done":
			items = append(items, e["item"])
		case "response.completed":
			completed = e["response"].(map[string]any)
		}
	}
	return items, completed
}

func TestResponsesReasoningAndToolCallStreamMatchesUnary(t *testing.T) {
	h := newHarness(t)
	body := func() map[string]any {
		return map[string]any{"input": weatherPrompt, "tools": []any{weatherFunction}, "store": false,
			"reasoning": map[string]any{"effort": "medium", "summary": "auto"}, "include": []any{"reasoning.encrypted_content"}}
	}
	unary := respond(h, body())
	want(t, unary, 200)
	output := unary.json()["output"].([]any)
	if len(output) != 2 || path(output[0], "type") != "reasoning" || path(output[0], "encrypted_content") != "fixture-encrypted-reasoning" ||
		path(output[0], "summary", 0, "type") != "summary_text" || path(output[1], "type") != "function_call" ||
		path(output[1], "call_id") != "call_fx_1_1" || path(output[1], "arguments") != `{"city":"Paris"}` {
		t.Fatalf("unary output %s", unary.body)
	}
	if path(unary.json(), "usage", "output_tokens_details", "reasoning_tokens").(float64) < 1 {
		t.Fatalf("no reasoning tokens: %s", unary.body)
	}

	b := body()
	b["stream"] = true
	stream := respond(h, b)
	want(t, stream, 200)
	items, completed := streamedOutput(t, stream.body)
	if completed == nil || len(items) != 2 {
		t.Fatalf("stream finished %d items", len(items))
	}
	for i := range items {
		// Item identifiers differ between two requests; everything else matches.
		got, want := items[i].(map[string]any), output[i].(map[string]any)
		delete(got, "id")
		delete(want, "id")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("streamed item %d = %v, unary %v", i, got, want)
		}
	}
	if path(completed, "usage") == nil || path(completed, "status") != "completed" {
		t.Fatalf("completed response %v", completed)
	}
	if !strings.Contains(string(stream.body), `"type":"response.function_call_arguments.delta"`) || !strings.Contains(string(stream.body), `"type":"response.reasoning_summary_text.delta"`) {
		t.Fatalf("stream lacks argument or reasoning deltas")
	}
}

func TestResponsesReasoningOnlyWhenRequested(t *testing.T) {
	h := newHarness(t)
	plain := respond(h, map[string]any{"input": "hi", "store": false})
	if n := len(plain.json()["output"].([]any)); n != 1 {
		t.Fatalf("%d output items without a reasoning request", n)
	}
	noEncryption := respond(h, map[string]any{"input": "hi", "store": false, "reasoning": map[string]any{"effort": "low"}})
	if path(noEncryption.json(), "output", 0, "encrypted_content") != nil || path(noEncryption.json(), "output", 0, "type") != "reasoning" {
		t.Fatalf("%s", noEncryption.body)
	}
}

func TestResponsesTextStreamAssemblesToTheUnaryText(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"input": `[[olp:reply "alpha beta gamma"]]`, "store": false}
	unary := respond(h, body)
	body["stream"] = true
	var text string
	for _, f := range frames(t, respond(h, body).body) {
		var e map[string]any
		json.Unmarshal([]byte(f.data), &e)
		if f.event == "response.output_text.delta" {
			text += e["delta"].(string)
		}
		if f.event == "response.output_text.done" && e["text"] != "alpha beta gamma" {
			t.Fatalf("done text %v", e["text"])
		}
	}
	if path(unary.json(), "output", 0, "content", 0, "text") != "alpha beta gamma" || text != "alpha beta gamma" {
		t.Fatalf("unary %s, streamed %q", unary.body, text)
	}
}

func TestResponsesStatelessToolLoopCarriesItemsInTheRequest(t *testing.T) {
	h := newHarness(t)
	input := []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": weatherPrompt}}},
		map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}},
		map[string]any{"type": "function_call", "call_id": "call_fx_1_1", "name": "get_weather", "arguments": `{"city":"Paris"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_fx_1_1", "output": "sunny"},
	}
	r := respond(h, map[string]any{"input": input, "tools": []any{weatherFunction}, "store": false})
	want(t, r, 200)
	if path(r.json(), "output", 0, "content", 0, "text") != "The tools returned: sunny" {
		t.Fatalf("%s", r.body)
	}
	// A call without its output, and an output without its call, are refused as
	// OpenAI refuses them.
	for name, items := range map[string][]any{
		"missing output": {input[0], input[2]},
		"orphan output":  {input[0], input[3]},
	} {
		t.Run(name, func(t *testing.T) {
			want(t, respond(h, map[string]any{"input": items, "store": false}), http.StatusBadRequest)
		})
	}
	want(t, respond(h, map[string]any{"input": []any{}}), http.StatusBadRequest)
}

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

func TestResponsesStructuredOutputAndInputTokens(t *testing.T) {
	h := newHarness(t)
	r := respond(h, map[string]any{"input": "x", "store": false, "text": map[string]any{"format": map[string]any{
		"type": "json_schema", "name": "p", "strict": true,
		"schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
	}}})
	want(t, r, 200)
	if path(r.json(), "output", 0, "content", 0, "text") != `{"ok":true}` || path(r.json(), "text", "format", "name") != "p" {
		t.Fatalf("%s", r.body)
	}
	count := h.openai("/responses/input_tokens", map[string]any{"model": OpenAIModel, "input": "hello"})
	want(t, count, 200)
	if count.json()["object"] != "response.input_tokens" || count.json()["input_tokens"].(float64) < 1 {
		t.Fatalf("%s", count.body)
	}
	want(t, h.openai("/responses/input_tokens", map[string]any{"model": "other", "input": "x"}), http.StatusNotFound)
}

func TestNullInputsAreMissingInputs(t *testing.T) {
	h := newHarness(t)
	want(t, h.openai("/responses", `{"model":"`+OpenAIModel+`","input":null}`), http.StatusBadRequest)
	want(t, h.openai("/embeddings", `{"model":"`+OpenAIModel+`","input":null}`), http.StatusBadRequest)
	want(t, h.openai("/chat/completions", `{"model":"`+OpenAIModel+`","messages":null}`), http.StatusBadRequest)
}

func TestMultibyteTextSurvivesStreamingAndClipping(t *testing.T) {
	for _, s := range []string{`{"c":"日本語"}`, `{"c":"é"}`, `{"e":"🙂🙂🙂"}`, "é", "日", "ab", ""} {
		parts := fragments(s)
		if strings.Join(parts, "") != s {
			t.Errorf("fragments(%q) = %q does not join back", s, parts)
		}
		for _, p := range parts {
			if !utf8.ValidString(p) {
				t.Errorf("fragments(%q) split a character: %q", s, p)
			}
		}
	}
	if got := clip("日本語", 4); got != "日" || !utf8.ValidString(got) {
		t.Errorf("clip split a character: %q", got)
	}
	if clip("abc", 2) != "ab" || clip("abc", 3) != "abc" || clip("🙂", 2) != "" {
		t.Errorf("clip bounds are wrong")
	}

	h := newHarness(t)
	args := `{"c":"日本語の天気"}`
	prompt := `[[olp:tool get_weather ` + args + `]]`
	stream := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "stream": true, "messages": []any{user(prompt)}, "tools": []any{weatherTool}})
	var streamed string
	for _, c := range decodeFrames(t, stream.body) {
		if tc := path(c, "choices", 0, "delta", "tool_calls", 0, "function", "arguments"); tc != nil {
			streamed += tc.(string)
		}
	}
	if streamed != args {
		t.Fatalf("streamed arguments %q, want %q", streamed, args)
	}
	// A long non-ASCII result is clipped to whole characters.
	long := strings.Repeat("天気", 200)
	followup := []any{user(prompt),
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_fx_1_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": args}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_fx_1_1", "content": long}}
	final := h.openai("/chat/completions", map[string]any{"model": OpenAIModel, "messages": followup, "tools": []any{weatherTool}})
	text := path(final.json(), "choices", 0, "message", "content").(string)
	if !utf8.ValidString(text) || !strings.HasPrefix(text, ToolResultsPrefix+"天気") || strings.Contains(text, "�") {
		t.Fatalf("final text is not valid: %q", text)
	}
}
