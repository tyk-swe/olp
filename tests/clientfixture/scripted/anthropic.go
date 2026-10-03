package scripted

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

var anthropic = vendor{
	name: "anthropic",
	authorized: func(r *http.Request, credential string) bool {
		return r.Header.Get("X-Api-Key") == credential
	},
	fail: func(w http.ResponseWriter, status int, kind, message string) {
		writeJSON(w, status, map[string]any{
			"type": "error", "error": map[string]any{"type": anthropicType(status), "message": message},
			"request_id": "req_fixture",
		})
	},
}

func anthropicType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func (f *Fixture) registerAnthropic() {
	f.mux.HandleFunc("POST "+AnthropicPrefix+"/messages", f.handle(anthropic, "anthropic.messages", f.messages))
	f.mux.HandleFunc("POST "+AnthropicPrefix+"/messages/count_tokens", f.handle(anthropic, "anthropic.count_tokens", f.countAnthropicTokens))
}

type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools      []map[string]any `json:"tools"`
	ToolChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
	// Structured output is output_config.format in the stable API and
	// output_format in the beta that preceded it.
	OutputConfig struct {
		Effort string `json:"effort"`
		Format struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
	OutputFormat struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	} `json:"output_format"`
	CacheControl json.RawMessage `json:"cache_control"`
	// Fields that pair with a beta header; see betaPairs.
	ContextManagement json.RawMessage `json:"context_management"`
	Safeguards        json.RawMessage `json:"safeguards"`
}

// betaPairs are the request features the API admits only together with their
// beta header, so a gateway that forwards the body and drops the header, or the
// reverse, is a hard 400 instead of a quiet success. They are the pairs Claude
// Code sends together, which its gateway guide documents; the header values
// carry a date, so they match by prefix.
var betaPairs = []struct {
	prefix string
	used   func(*anthropicRequest) string // what the request uses, if anything
}{
	{"context-management-", func(r *anthropicRequest) string { return present(r.ContextManagement, "context_management") }},
	{"dangerous-tool-use-", func(r *anthropicRequest) string { return present(r.Safeguards, "safeguards") }},
	{"effort-", func(r *anthropicRequest) string {
		if r.OutputConfig.Effort != "" {
			return "output_config.effort"
		}
		return ""
	}},
	{"mid-conversation-system-", func(r *anthropicRequest) string {
		for i, m := range r.Messages {
			if m.Role == "system" {
				return fmt.Sprintf("messages.%d.role", i)
			}
		}
		return ""
	}},
}

func present(raw json.RawMessage, name string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return name
}

// requireBetas answers 400 for a feature whose beta header is missing.
func (x *request) requireBetas(req *anthropicRequest) bool {
	var have []string
	for _, value := range x.r.Header.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(value, ",") {
			have = append(have, strings.TrimSpace(beta))
		}
	}
	for _, pair := range betaPairs {
		field := pair.used(req)
		if field == "" || slices.ContainsFunc(have, func(beta string) bool { return strings.HasPrefix(beta, pair.prefix) }) {
			continue
		}
		x.fail(http.StatusBadRequest, "invalid_request_error", field+": Extra inputs are not permitted without the "+pair.prefix+"* beta")
		return false
	}
	return true
}

// block is one content block, with a string content as a text block.
type block = map[string]any

func blocksOf(raw json.RawMessage) []block {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{"type": "text", "text": text}}
	}
	var blocks []block
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

// parseAnthropic validates the request as the Messages API does and reduces it
// to a turn. It answers 400 itself and returns nil when the request is invalid.
func (x *request) parseAnthropic(count bool) (*anthropicRequest, *turn) {
	var req anthropicRequest
	if !x.decode(&req) {
		return nil, nil
	}
	switch {
	case x.r.Header.Get("Anthropic-Version") == "":
		x.fail(http.StatusBadRequest, "invalid_request_error", "anthropic-version: header is required")
	case req.Model != AnthropicModel:
		x.fail(http.StatusNotFound, "not_found_error", "model: "+req.Model)
	case req.MaxTokens <= 0 && !count:
		x.fail(http.StatusBadRequest, "invalid_request_error", "max_tokens: Field required")
	case len(req.Messages) == 0:
		x.fail(http.StatusBadRequest, "invalid_request_error", "messages: Field required")
	default:
		if !x.requireBetas(&req) {
			return nil, nil
		}
		if t := x.anthropicTurn(&req); t != nil {
			return &req, t
		}
	}
	return nil, nil
}

func (x *request) anthropicTurn(req *anthropicRequest) *turn {
	t := newTurn()
	var uses map[string]bool // tool_use ids of the previous assistant message
	for i, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			x.fail(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages.%d.role: Input should be 'user', 'assistant' or 'system'", i))
			return nil
		}
		blocks := blocksOf(m.Content)
		var text []string
		var results []string
		seen := map[string]bool{}
		for j, b := range blocks {
			switch b["type"] {
			case "text":
				text = append(text, fmt.Sprint(b["text"]))
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				if m.Role != "user" || !uses[id] {
					x.fail(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages.%d.content.%d: unexpected `tool_use_id` found in `tool_result` blocks: %s. Each `tool_result` block must have a corresponding `tool_use` block in the previous message.", i, j, id))
					return nil
				}
				seen[id] = true
				encoded, _ := json.Marshal(b["content"])
				results = append(results, contentText(encoded))
			case "thinking":
				if b["signature"] != ThinkingSignature {
					x.fail(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages.%d.content.%d: Invalid `signature` in `thinking` block", i, j))
					return nil
				}
			}
		}
		if missing := missingKeys(uses, seen); len(missing) > 0 {
			x.fail(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages.%d: `tool_use` ids were found without `tool_result` blocks immediately after: %s. Each `tool_use` block must have a corresponding `tool_result` block in the next message.", i-1, strings.Join(missing, ", ")))
			return nil
		}
		uses = map[string]bool{}
		if m.Role == "assistant" {
			for _, b := range blocks {
				if id, ok := b["id"].(string); ok && b["type"] == "tool_use" {
					uses[id] = true
				}
			}
		}
		if len(results) > 0 {
			t.messages = append(t.messages, turnMessage{results: results})
		} else if len(text) > 0 && m.Role == "user" {
			t.user(strings.Join(text, "\n"))
		}
	}
	if len(uses) > 0 {
		x.fail(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages.%d: `tool_use` ids were found without `tool_result` blocks immediately after: %s. Each `tool_use` block must have a corresponding `tool_result` block in the next message.", len(req.Messages)-1, strings.Join(sortedKeys(uses), ", ")))
		return nil
	}
	for _, tool := range req.Tools {
		name, _ := tool["name"].(string)
		schema, _ := json.Marshal(tool["input_schema"])
		t.declare(name, schema)
	}
	switch req.ToolChoice.Type {
	case "none":
		t.hideTools()
	case "any":
		t.forced = "*"
	case "tool":
		t.forced = req.ToolChoice.Name
	}
	for _, format := range []struct {
		kind   string
		schema json.RawMessage
	}{{req.OutputConfig.Format.Type, req.OutputConfig.Format.Schema}, {req.OutputFormat.Type, req.OutputFormat.Schema}} {
		if format.kind == "json_schema" {
			t.schema = format.schema
			if len(t.schema) == 0 {
				t.schema = json.RawMessage("{}")
			}
		}
	}
	t.thinking = req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive"
	return t
}

func missingKeys(want, have map[string]bool) []string {
	missing := map[string]bool{}
	for id := range want {
		if !have[id] {
			missing[id] = true
		}
	}
	return sortedKeys(missing)
}

// promptBlock is one cacheable unit of the prompt, in the order Anthropic
// caches: tools, then system, then messages.
type promptBlock struct {
	canonical string
	marked    bool
	hour      bool
}

func promptBlocks(req *anthropicRequest) []promptBlock {
	var out []promptBlock
	add := func(b block, role string) {
		b = maps.Clone(b)
		control, marked := b["cache_control"].(map[string]any)
		delete(b, "cache_control")
		if role != "" {
			b = block{"role": role, "block": b}
		}
		encoded, _ := json.Marshal(b)
		out = append(out, promptBlock{canonical: string(encoded), marked: marked, hour: marked && control["ttl"] == "1h"})
	}
	for _, tool := range req.Tools {
		add(tool, "")
	}
	for _, b := range blocksOf(req.System) {
		add(b, "")
	}
	for _, m := range req.Messages {
		for _, b := range blocksOf(m.Content) {
			add(b, m.Role)
		}
	}
	if len(req.CacheControl) > 0 && string(req.CacheControl) != "null" && len(out) > 0 {
		out[len(out)-1].marked = true
	}
	return out
}

type anthropicUsage struct {
	input, output, read, create int
	hour                        bool
}

// cacheUsage prices the prompt against the fixture's prompt cache: a prefix
// that ends at a cache breakpoint is written once and read afterwards, and
// every breakpoint prefix becomes readable.
func (f *Fixture) cacheUsage(blocks []promptBlock) anthropicUsage {
	var all strings.Builder
	var marks []string
	var prefixTokens []int
	var hour bool
	for _, b := range blocks {
		all.WriteString(b.canonical)
		if b.marked {
			sum := sha256.Sum256([]byte(all.String()))
			marks = append(marks, hex.EncodeToString(sum[:]))
			prefixTokens = append(prefixTokens, tokens(all.String()))
			hour = hour || b.hour
		}
	}
	total := tokens(all.String())
	u := anthropicUsage{input: total, hour: hour}
	if len(marks) == 0 {
		return u
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(marks) - 1; i >= 0; i-- {
		if f.cache[marks[i]] {
			u.read = prefixTokens[i]
			break
		}
	}
	last := prefixTokens[len(marks)-1]
	u.create, u.input = last-u.read, total-last
	for _, mark := range marks {
		f.cache[mark] = true
	}
	return u
}

func (u anthropicUsage) wire(final bool) map[string]any {
	cache := map[string]any{"ephemeral_5m_input_tokens": u.create, "ephemeral_1h_input_tokens": 0}
	if u.hour {
		cache["ephemeral_5m_input_tokens"], cache["ephemeral_1h_input_tokens"] = 0, u.create
	}
	out := map[string]any{
		"input_tokens": u.input, "cache_creation_input_tokens": u.create, "cache_read_input_tokens": u.read,
		"cache_creation": cache, "output_tokens": u.output, "service_tier": "standard",
	}
	if !final {
		out["output_tokens"] = 1
	}
	return out
}

func (f *Fixture) messages(x *request) {
	req, t := x.parseAnthropic(false)
	if t == nil {
		return
	}
	// Structured output through a forced tool needs the tool's schema, which
	// the turn already holds.
	rp := decide(t)
	if x.failScripted(rp, req.Stream) {
		return
	}
	x.note(rp.script, req.Stream)
	u := f.cacheUsage(promptBlocks(req))
	u.output = usageOf(x.body, rp).output
	id := f.nextID("msg")
	stop := "end_turn"
	if len(rp.calls) > 0 {
		stop = "tool_use"
	}
	content := anthropicContent(rp)
	if !req.Stream {
		writeJSON(x.w, http.StatusOK, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": AnthropicModel,
			"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": u.wire(true),
		})
		return
	}
	s := newSSE(x.w, "\n\n")
	s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": AnthropicModel, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": u.wire(false),
	}})
	s.event("ping", map[string]any{"type": "ping"})
	for i, b := range content {
		start := block{}
		for k, v := range b {
			start[k] = v
		}
		var deltas []map[string]any
		switch b["type"] {
		case "thinking":
			start["thinking"], start["signature"] = "", ""
			for _, w := range words(b["thinking"].(string)) {
				deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": w})
			}
			deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": b["signature"]})
		case "tool_use":
			start["input"] = map[string]any{}
			for _, part := range fragments(compact(rawOf(b["input"]))) {
				deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": part})
			}
		default:
			start["text"] = ""
			for _, w := range words(b["text"].(string)) {
				deltas = append(deltas, map[string]any{"type": "text_delta", "text": w})
			}
		}
		s.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": start})
		for _, d := range deltas {
			s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": d})
		}
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	s.event("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": u.wire(true),
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func rawOf(v any) json.RawMessage {
	encoded, _ := json.Marshal(v)
	return encoded
}

func anthropicContent(rp reply) []block {
	var out []block
	if rp.reasoning != "" {
		out = append(out, block{"type": "thinking", "thinking": rp.reasoning, "signature": ThinkingSignature})
	}
	if len(rp.calls) > 0 {
		for _, c := range rp.calls {
			out = append(out, block{"type": "tool_use", "id": c.id("toolu"), "name": c.name, "input": json.RawMessage(compact(c.args))})
		}
		return out
	}
	return append(out, block{"type": "text", "text": rp.text})
}

func (f *Fixture) countAnthropicTokens(x *request) {
	req, t := x.parseAnthropic(true)
	if t == nil {
		return
	}
	x.note("count_tokens", false)
	var text strings.Builder
	for _, b := range promptBlocks(req) {
		text.WriteString(b.canonical)
	}
	writeJSON(x.w, http.StatusOK, map[string]any{"input_tokens": tokens(text.String())})
}
