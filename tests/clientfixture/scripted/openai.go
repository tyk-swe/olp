package scripted

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
)

// createdAt keeps every response byte-identical from run to run.
const createdAt = 1767225600

var openAI = vendor{
	name: "openai",
	authorized: func(r *http.Request, credential string) bool {
		return r.Header.Get("Authorization") == "Bearer "+credential
	},
	fail: func(w http.ResponseWriter, status int, kind, message string) {
		writeJSON(w, status, map[string]any{"error": map[string]any{
			"message": message, "type": openAIType(status), "param": nil, "code": kind,
		}})
	},
}

func openAIType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "server_error"
	}
	return "invalid_request_error"
}

func (f *Fixture) registerOpenAI() {
	f.mux.HandleFunc("POST "+OpenAIPrefix+"/chat/completions", f.handle(openAI, "openai.chat", f.chatCompletions))
	f.mux.HandleFunc("POST "+OpenAIPrefix+"/responses", f.handle(openAI, "openai.responses", f.createResponse))
	f.mux.HandleFunc("POST "+OpenAIPrefix+"/responses/input_tokens", f.handle(openAI, "openai.responses.input_tokens", f.responseInputTokens))
	f.mux.HandleFunc("GET "+OpenAIPrefix+"/responses/{id}", f.handle(openAI, "openai.responses.get", f.getResponse))
	f.mux.HandleFunc("DELETE "+OpenAIPrefix+"/responses/{id}", f.handle(openAI, "openai.responses.delete", f.deleteResponse))
	f.mux.HandleFunc("GET "+OpenAIPrefix+"/responses/{id}/input_items", f.handle(openAI, "openai.responses.input_items", f.responseInputItems))
	f.mux.HandleFunc("POST "+OpenAIPrefix+"/embeddings", f.handle(openAI, "openai.embeddings", f.openAIEmbeddings))
}

// checkOpenAIModel answers 404 as OpenAI does for a model it does not serve.
func (x *request) checkOpenAIModel(model string) bool {
	if model != OpenAIModel {
		x.fail(http.StatusNotFound, "model_not_found", fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", model))
		return false
	}
	return true
}

// failScripted answers a scripted failure or directive error.
func (x *request) failScripted(rp reply, stream bool) bool {
	if rp.failStatus == 0 {
		return false
	}
	x.note(rp.script, stream)
	x.fail(rp.failStatus, "scripted_error", rp.failMessage)
	return true
}

// usage is the token accounting every dialect reports.
type usage struct {
	input, output, reasoning int
}

func usageOf(body []byte, rp reply) usage {
	out := tokens(rp.text)
	for _, c := range rp.calls {
		out += tokens(string(c.args)) + tokens(c.name)
	}
	u := usage{input: tokens(string(body)), output: out}
	if rp.reasoning != "" {
		u.reasoning = tokens(rp.reasoning)
		u.output += u.reasoning
	}
	return u
}

// text joins the text of a content value: a string, or an array of parts that
// carry a text member.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Text != "" && (p.Type == "" || strings.HasSuffix(p.Type, "text")) {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// --- Chat Completions ---

type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	ResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

func (f *Fixture) chatCompletions(x *request) {
	var req chatRequest
	if !x.decode(&req) || !x.checkOpenAIModel(req.Model) {
		return
	}
	if len(req.Messages) == 0 {
		x.fail(http.StatusBadRequest, "missing_required_parameter", "Missing required parameter: 'messages'.")
		return
	}
	t := newTurn()
	// OpenAI refuses a tool message that answers nothing, and an assistant
	// tool call that nothing answers.
	pending := map[string]bool{}
	for i, m := range req.Messages {
		if m.Role != "tool" && len(pending) > 0 {
			x.fail(http.StatusBadRequest, "invalid_value", fmt.Sprintf("An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'. The following tool_call_ids did not have response messages: %s", strings.Join(sortedKeys(pending), ", ")))
			return
		}
		switch m.Role {
		case "user":
			t.user(contentText(m.Content))
		case "tool":
			if !pending[m.ToolCallID] {
				x.fail(http.StatusBadRequest, "invalid_value", fmt.Sprintf("Invalid parameter: messages with role 'tool' must be a response to a preceeding message with 'tool_calls'. (messages[%d])", i))
				return
			}
			delete(pending, m.ToolCallID)
			t.results(contentText(m.Content))
		case "assistant":
			for _, c := range m.ToolCalls {
				pending[c.ID] = true
			}
		}
	}
	if len(pending) > 0 {
		x.fail(http.StatusBadRequest, "invalid_value", fmt.Sprintf("An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'. The following tool_call_ids did not have response messages: %s", strings.Join(sortedKeys(pending), ", ")))
		return
	}
	for _, tool := range req.Tools {
		if tool.Type == "function" {
			t.declare(tool.Function.Name, tool.Function.Parameters)
		}
	}
	applyToolChoice(t, req.ToolChoice)
	switch req.ResponseFormat.Type {
	case "json_schema":
		t.schema = req.ResponseFormat.JSONSchema.Schema
		if len(t.schema) == 0 {
			t.schema = json.RawMessage("{}")
		}
	case "json_object":
		t.jsonMode = true
	}
	rp := decide(t)
	if x.failScripted(rp, req.Stream) {
		return
	}
	x.note(rp.script, req.Stream)
	u := usageOf(x.body, rp)
	id := f.nextID("chatcmpl")
	usageBody := map[string]any{
		"prompt_tokens": u.input, "completion_tokens": u.output, "total_tokens": u.input + u.output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 0, "audio_tokens": 0},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 0, "audio_tokens": 0, "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0},
	}
	finish := "stop"
	if len(rp.calls) > 0 {
		finish = "tool_calls"
	}
	if !req.Stream {
		message := map[string]any{"role": "assistant", "content": rp.text, "refusal": nil, "annotations": []any{}}
		if len(rp.calls) > 0 {
			message["content"] = nil
			message["tool_calls"] = chatToolCalls(rp.calls)
		}
		writeJSON(x.w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": createdAt, "model": OpenAIModel,
			"choices":            []any{map[string]any{"index": 0, "message": message, "finish_reason": finish, "logprobs": nil}},
			"usage":              usageBody,
			"service_tier":       "default",
			"system_fingerprint": "fp_fixture",
		})
		return
	}
	s := newSSE(x.w, "\n\n")
	chunk := func(delta map[string]any, finish any) map[string]any {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": createdAt, "model": OpenAIModel,
			"service_tier": "default", "system_fingerprint": "fp_fixture",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}},
		}
		if req.StreamOptions.IncludeUsage {
			c["usage"] = nil
		}
		return c
	}
	s.event("", chunk(map[string]any{"role": "assistant", "content": "", "refusal": nil}, nil))
	for _, w := range words(rp.text) {
		s.event("", chunk(map[string]any{"content": w}, nil))
	}
	for i, c := range rp.calls {
		s.event("", chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": c.id("call"), "type": "function", "function": map[string]any{"name": c.name, "arguments": ""},
		}}}, nil))
		for _, part := range fragments(compact(c.args)) {
			s.event("", chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "function": map[string]any{"arguments": part},
			}}}, nil))
		}
	}
	s.event("", chunk(map[string]any{}, finish))
	if req.StreamOptions.IncludeUsage {
		s.event("", map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": createdAt, "model": OpenAIModel,
			"service_tier": "default", "system_fingerprint": "fp_fixture", "choices": []any{}, "usage": usageBody,
		})
	}
	s.event("", "[DONE]")
}

// isNull reports an absent or null JSON value, which no string decoding may
// mistake for an empty string.
func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func chatToolCalls(calls []invocation) []any {
	out := make([]any, len(calls))
	for i, c := range calls {
		out[i] = map[string]any{"id": c.id("call"), "type": "function", "function": map[string]any{"name": c.name, "arguments": compact(c.args)}}
	}
	return out
}

// applyToolChoice reads the tool_choice shared by Chat Completions and
// Responses: a mode string, or an object naming the function.
func applyToolChoice(t *turn, raw json.RawMessage) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none":
			t.hideTools()
		case "required":
			t.forced = "*"
		}
		return
	}
	var named struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) == nil && named.Type == "function" {
		t.forced = named.Name + named.Function.Name
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// --- Embeddings ---

func (f *Fixture) openAIEmbeddings(x *request) {
	var req struct {
		Model          string          `json:"model"`
		Input          json.RawMessage `json:"input"`
		Dimensions     int             `json:"dimensions"`
		EncodingFormat string          `json:"encoding_format"`
	}
	if !x.decode(&req) || !x.checkOpenAIModel(req.Model) {
		return
	}
	inputs := embeddingInputs(req.Input)
	if len(inputs) == 0 {
		x.fail(http.StatusBadRequest, "missing_required_parameter", "Missing required parameter: 'input'.")
		return
	}
	if req.EncodingFormat != "" && req.EncodingFormat != "float" && req.EncodingFormat != "base64" {
		x.fail(http.StatusBadRequest, "invalid_value", "Invalid value for 'encoding_format': expected float or base64.")
		return
	}
	x.note("embeddings", false)
	data, total := make([]any, len(inputs)), 0
	for i, in := range inputs {
		var vector any = embedding(in, req.Dimensions)
		if req.EncodingFormat == "base64" {
			vector = encodeVector(embedding(in, req.Dimensions))
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
		total += tokens(in)
	}
	writeJSON(x.w, http.StatusOK, map[string]any{
		"object": "list", "data": data, "model": OpenAIModel,
		"usage": map[string]any{"prompt_tokens": total, "total_tokens": total},
	})
}

// embeddingInputs flattens the accepted input shapes: a string, an array of
// strings, a token array, or an array of token arrays.
func embeddingInputs(raw json.RawMessage) []string {
	if isNull(raw) {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var ids []int
	if json.Unmarshal(raw, &ids) == nil {
		return []string{fmt.Sprint(ids)}
	}
	var batches [][]int
	if json.Unmarshal(raw, &batches) == nil {
		out := make([]string, len(batches))
		for i, b := range batches {
			out[i] = fmt.Sprint(b)
		}
		return out
	}
	return nil
}

const defaultDimensions = 16

// embedding is a deterministic unit vector derived from the text, so equal
// inputs embed equally and different inputs differ.
func embedding(text string, dimensions int) []float32 {
	if dimensions <= 0 {
		dimensions = defaultDimensions
	}
	dimensions = min(dimensions, 4096)
	out := make([]float32, 0, dimensions)
	for block := 0; len(out) < dimensions; block++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s", block, text))
		for i := 0; i+1 < len(sum) && len(out) < dimensions; i += 2 {
			out = append(out, float32(binary.BigEndian.Uint16(sum[i:]))/32767.5-1)
		}
	}
	var norm float64
	for _, v := range out {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out
}

// encodeVector is the base64 form of little-endian float32 values.
func encodeVector(v []float32) string {
	raw := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(raw)
}
