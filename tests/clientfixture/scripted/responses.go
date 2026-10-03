package scripted

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Stored responses make `previous_response_id` continuation behave like the
// real API: the new request sees the whole stored conversation, and an unknown
// identifier is a 404. They are the fixture's own state, never the gateway's.

type storedResponse struct {
	response map[string]any
	// history is the conversation as input items, ending with the output;
	// inputs counts the items before that output.
	history []map[string]any
	inputs  int
}

type responsesRequest struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Reasoning          json.RawMessage `json:"reasoning"`
	Include            []string        `json:"include"`
	Metadata           json.RawMessage `json:"metadata"`
	Text               json.RawMessage `json:"text"`
}

// textFormat is the structured output request of a Responses `text` member.
type textFormat struct {
	Format struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	} `json:"format"`
}

func (f *Fixture) createResponse(x *request) {
	var req responsesRequest
	if !x.decode(&req) || !x.checkOpenAIModel(req.Model) {
		return
	}
	var history []map[string]any
	if req.PreviousResponseID != "" {
		f.mu.Lock()
		prev, ok := f.responses[req.PreviousResponseID]
		if ok {
			history = slices.Clone(prev.history)
		}
		f.mu.Unlock()
		if !ok {
			x.fail(http.StatusNotFound, "previous_response_not_found", fmt.Sprintf("Previous response with id '%s' not found.", req.PreviousResponseID))
			return
		}
	}
	items, err := inputItems(req.Input)
	if err != nil {
		x.fail(http.StatusBadRequest, "invalid_value", err.Error())
		return
	}
	history = append(history, items...)
	if msg := checkToolOutputs(history); msg != "" {
		x.fail(http.StatusBadRequest, "invalid_value", msg)
		return
	}
	t := responsesTurn(&req, history)
	rp := decide(t)
	if x.failScripted(rp, req.Stream) {
		return
	}
	x.note(rp.script, req.Stream)
	u := usageOf(x.body, rp)
	id := f.nextID("resp")
	output := f.responseOutput(rp, slices.Contains(req.Include, "reasoning.encrypted_content"))
	store := req.Store == nil || *req.Store
	envelope := func(status string, output []map[string]any, withUsage bool) map[string]any {
		r := map[string]any{
			"id": id, "object": "response", "created_at": createdAt, "status": status, "background": false,
			"error": nil, "incomplete_details": nil, "instructions": rawOrNil(req.Instructions), "model": OpenAIModel,
			"output": output, "parallel_tool_calls": true, "previous_response_id": nilIfEmpty(req.PreviousResponseID),
			"store": store, "temperature": 1.0, "top_p": 1.0, "truncation": "disabled", "user": nil,
			"tool_choice": rawOr(req.ToolChoice, "auto"), "tools": rawOr(req.Tools, []any{}),
			"reasoning": rawOr(req.Reasoning, map[string]any{"effort": nil, "summary": nil}),
			"text":      map[string]any{"format": map[string]any{"type": "text"}},
			"metadata":  rawOr(req.Metadata, map[string]any{}),
		}
		if len(req.Text) > 0 && string(req.Text) != "null" {
			r["text"] = req.Text
		}
		if withUsage {
			r["usage"] = map[string]any{
				"input_tokens": u.input, "input_tokens_details": map[string]any{"cached_tokens": 0},
				"output_tokens": u.output, "output_tokens_details": map[string]any{"reasoning_tokens": u.reasoning},
				"total_tokens": u.input + u.output,
			}
		}
		return r
	}
	completed := envelope("completed", output, true)
	if store {
		f.mu.Lock()
		f.responses[id] = &storedResponse{response: completed, history: append(slices.Clone(history), output...), inputs: len(history)}
		f.mu.Unlock()
	}
	if !req.Stream {
		writeJSON(x.w, http.StatusOK, completed)
		return
	}
	streamResponse(x, id, envelope, output, completed)
}

// responsesTurn reads the neutral turn from the assembled input items.
func responsesTurn(req *responsesRequest, history []map[string]any) *turn {
	t := newTurn()
	for _, item := range history {
		switch itemType(item) {
		case "message":
			if item["role"] == "user" {
				t.user(itemText(item["content"]))
			}
		case "function_call_output", "custom_tool_call_output":
			t.results(itemText(item["output"]))
		}
	}
	var tools []struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal(req.Tools, &tools) == nil {
		for _, tool := range tools {
			if tool.Type == "function" || tool.Type == "custom" {
				t.declare(tool.Name, tool.Parameters)
			}
		}
	}
	applyToolChoice(t, req.ToolChoice)
	var text textFormat
	_ = json.Unmarshal(req.Text, &text)
	switch text.Format.Type {
	case "json_schema":
		t.schema = text.Format.Schema
		if len(t.schema) == 0 {
			t.schema = json.RawMessage("{}")
		}
	case "json_object":
		t.jsonMode = true
	}
	var reasoning map[string]any
	t.thinking = json.Unmarshal(req.Reasoning, &reasoning) == nil && reasoning != nil
	return t
}

func itemType(item map[string]any) string {
	if t, ok := item["type"].(string); ok {
		return t
	}
	if _, ok := item["role"]; ok {
		return "message"
	}
	return ""
}

// itemText reads a message content or tool output: a string or text parts.
func itemText(v any) string {
	encoded, _ := json.Marshal(v)
	return contentText(encoded)
}

// inputItems normalizes a Responses input: a string is one user message.
func inputItems(raw json.RawMessage) ([]map[string]any, error) {
	var text string
	if !isNull(raw) && json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, errors.New("Invalid 'input': expected a string or a non-empty array of input items.")
	}
	for _, item := range items {
		if _, ok := item["type"]; !ok {
			item["type"] = "message"
		}
	}
	return items, nil
}

// checkToolOutputs is the pairing rule OpenAI enforces: every function call in
// the conversation has exactly its output, and every output a call.
func checkToolOutputs(history []map[string]any) string {
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range history {
		id, _ := item["call_id"].(string)
		switch item["type"] {
		case "function_call":
			calls[id] = true
		case "function_call_output":
			outputs[id] = true
		}
	}
	for _, id := range sortedKeys(outputs) {
		if !calls[id] {
			return fmt.Sprintf("No tool call found for function call output with call_id %s.", id)
		}
	}
	for _, id := range sortedKeys(calls) {
		if !outputs[id] {
			return fmt.Sprintf("No tool output found for function call %s.", id)
		}
	}
	return ""
}

// responseOutput builds the output items of a reply: reasoning, then a message
// or the function calls.
func (f *Fixture) responseOutput(rp reply, encrypted bool) []map[string]any {
	var out []map[string]any
	if rp.reasoning != "" {
		item := map[string]any{
			"id": f.nextID("rs"), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": rp.reasoning}},
		}
		if encrypted {
			item["encrypted_content"] = "fixture-encrypted-reasoning"
		}
		out = append(out, item)
	}
	if len(rp.calls) > 0 {
		for _, c := range rp.calls {
			out = append(out, map[string]any{
				"id": f.nextID("fc"), "type": "function_call", "status": "completed",
				"call_id": c.id("call"), "name": c.name, "arguments": compact(c.args),
			})
		}
		return out
	}
	return append(out, map[string]any{
		"id": f.nextID("msg"), "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": rp.text, "annotations": []any{}, "logprobs": []any{}}},
	})
}

// streamResponse emits the Responses event sequence for the output items.
func streamResponse(x *request, id string, envelope func(string, []map[string]any, bool) map[string]any, output []map[string]any, completed map[string]any) {
	s := newSSE(x.w, "\n\n")
	seq := 0
	emit := func(kind string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = kind, seq
		seq++
		s.event(kind, fields)
	}
	emit("response.created", map[string]any{"response": envelope("in_progress", []map[string]any{}, false)})
	emit("response.in_progress", map[string]any{"response": envelope("in_progress", []map[string]any{}, false)})
	for i, item := range output {
		itemID := item["id"].(string)
		switch item["type"] {
		case "reasoning":
			summary := item["summary"].([]any)[0].(map[string]any)["text"].(string)
			started := map[string]any{"id": itemID, "type": "reasoning", "summary": []any{}}
			emit("response.output_item.added", map[string]any{"output_index": i, "item": started})
			emit("response.reasoning_summary_part.added", map[string]any{"item_id": itemID, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
			for _, w := range words(summary) {
				emit("response.reasoning_summary_text.delta", map[string]any{"item_id": itemID, "output_index": i, "summary_index": 0, "delta": w})
			}
			emit("response.reasoning_summary_text.done", map[string]any{"item_id": itemID, "output_index": i, "summary_index": 0, "text": summary})
			emit("response.reasoning_summary_part.done", map[string]any{"item_id": itemID, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": summary}})
		case "function_call":
			started := map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": item["call_id"], "name": item["name"], "arguments": ""}
			emit("response.output_item.added", map[string]any{"output_index": i, "item": started})
			for _, part := range fragments(item["arguments"].(string)) {
				emit("response.function_call_arguments.delta", map[string]any{"item_id": itemID, "output_index": i, "delta": part})
			}
			emit("response.function_call_arguments.done", map[string]any{"item_id": itemID, "output_index": i, "name": item["name"], "arguments": item["arguments"]})
		default:
			text := item["content"].([]any)[0].(map[string]any)["text"].(string)
			started := map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			emit("response.output_item.added", map[string]any{"output_index": i, "item": started})
			emit("response.content_part.added", map[string]any{"item_id": itemID, "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
			for _, w := range words(text) {
				emit("response.output_text.delta", map[string]any{"item_id": itemID, "output_index": i, "content_index": 0, "delta": w, "logprobs": []any{}})
			}
			emit("response.output_text.done", map[string]any{"item_id": itemID, "output_index": i, "content_index": 0, "text": text, "logprobs": []any{}})
			emit("response.content_part.done", map[string]any{"item_id": itemID, "output_index": i, "content_index": 0, "part": item["content"].([]any)[0]})
		}
		emit("response.output_item.done", map[string]any{"output_index": i, "item": item})
	}
	emit("response.completed", map[string]any{"response": completed})
}

func (f *Fixture) responseInputTokens(x *request) {
	var req responsesRequest
	if !x.decode(&req) || !x.checkOpenAIModel(req.Model) {
		return
	}
	x.note("count_tokens", false)
	writeJSON(x.w, http.StatusOK, map[string]any{"object": "response.input_tokens", "input_tokens": tokens(string(x.body))})
}

// stored looks up the response the path names, answering 404 itself.
func (f *Fixture) stored(x *request) (*storedResponse, bool) {
	id := x.r.PathValue("id")
	f.mu.Lock()
	stored, ok := f.responses[id]
	f.mu.Unlock()
	if !ok {
		x.fail(http.StatusNotFound, "response_not_found", fmt.Sprintf("Response with id '%s' not found.", id))
	}
	return stored, ok
}

func (f *Fixture) getResponse(x *request) {
	if stored, ok := f.stored(x); ok {
		x.note("response_get", false)
		writeJSON(x.w, http.StatusOK, stored.response)
	}
}

func (f *Fixture) deleteResponse(x *request) {
	if _, ok := f.stored(x); !ok {
		return
	}
	id := x.r.PathValue("id")
	f.mu.Lock()
	delete(f.responses, id)
	f.mu.Unlock()
	x.note("response_delete", false)
	writeJSON(x.w, http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true})
}

// responseInputItems lists the conversation that led to the response, without
// its own output.
func (f *Fixture) responseInputItems(x *request) {
	stored, ok := f.stored(x)
	if !ok {
		return
	}
	items := make([]any, 0, stored.inputs)
	for i, item := range stored.history[:stored.inputs] {
		item = maps.Clone(item)
		if _, ok := item["id"]; !ok {
			item["id"] = fmt.Sprintf("item_fx_%d", i+1)
		}
		items = append(items, item)
	}
	x.note("response_input_items", false)
	list := map[string]any{"object": "list", "data": items, "has_more": false, "first_id": nil, "last_id": nil}
	if len(items) > 0 {
		list["first_id"], list["last_id"] = items[0].(map[string]any)["id"], items[len(items)-1].(map[string]any)["id"]
	}
	writeJSON(x.w, http.StatusOK, list)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func rawOrNil(raw json.RawMessage) any { return rawOr(raw, nil) }

// rawOr echoes a request member, or the default when it was absent.
func rawOr(raw json.RawMessage, fallback any) any {
	if len(strings.TrimSpace(string(raw))) == 0 || string(raw) == "null" {
		return fallback
	}
	return raw
}
