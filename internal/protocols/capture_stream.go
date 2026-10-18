package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var errCaptureStream = errors.New("capture stream unavailable or incomplete")

type capturePart struct {
	text, name string
	done       bool
}

// CaptureStream assembles complete fields by native identity. It never holds
// response frames and never modifies bytes delivered to the inference caller.
// Bounds apply to selected content, identities and the number of parts.
type CaptureStream struct {
	family               wire.Format
	limit                int
	output, tools        bool
	text, calls          map[string]*capturePart
	textOrder, callOrder []string
	groups               map[string]bool
	terminal             bool
	geminiCallSequence   uint64
	captureReader
}

func NewCaptureStream(f wire.Format, limit int, output, tools bool) *CaptureStream {
	return &CaptureStream{family: f, limit: limit, output: output, tools: tools, text: map[string]*capturePart{}, calls: map[string]*capturePart{}, groups: map[string]bool{}}
}
func (s *CaptureStream) part(id string, tool bool) *capturePart {
	parts, order := s.text, &s.textOrder
	if tool {
		parts, order = s.calls, &s.callOrder
	}
	if p := parts[id]; p != nil {
		return p
	}
	if len(id) > 256 || len(s.text)+len(s.calls) >= 2048 {
		s.invalid = true
		return &capturePart{}
	}
	p := &capturePart{}
	parts[id] = p
	*order = append(*order, id)
	return p
}
func (s *CaptureStream) add(id, value string, tool, snapshot bool) {
	if s.invalid || (tool && !s.tools) || (!tool && !s.output) {
		return
	}
	p := s.part(id, tool)
	if len(value) > s.limit {
		s.invalid = true
		return
	}
	if snapshot {
		// A terminal snapshot may supply content absent from deltas, but must agree
		// with a previously assembled value. Repeated snapshots are not appended.
		if p.text != "" && p.text != value {
			s.invalid = true
			return
		}
		p.text = value
	} else {
		if p.done && value != "" {
			s.invalid = true
			return
		}
		if len(p.text)+len(value) > s.limit {
			s.invalid = true
			return
		}
		p.text += value
	}
	_ = s.bounds()
}
func (s *CaptureStream) tool(id, name, args string, snapshot, done bool) {
	if s.invalid || !s.tools {
		return
	}
	p := s.part(id, true)
	if name != "" {
		if !snapshot && (s.family == wire.Chat || s.family == wire.MistralFIM) {
			p.name += name
		} else if p.name != "" && p.name != name {
			s.invalid = true
		} else {
			p.name = name
		}
	}
	s.add(id, args, true, snapshot)
	p.done = p.done || done
}
func (s *CaptureStream) done(id string) {
	if p := s.text[id]; p != nil {
		p.done = true
	}
	if p := s.calls[id]; p != nil {
		p.done = true
	}
}
func (s *CaptureStream) bounds() error {
	textSize, toolSize := 0, 0
	for id, p := range s.text {
		textSize += len(p.text) + len(id)
	}
	for id, p := range s.calls {
		toolSize += len(p.text) + len(p.name) + len(id)
	}
	if s.invalid || textSize > s.limit || toolSize > s.limit || len(s.groups) > 2048 {
		s.invalid = true
		return errCaptureStream
	}
	return nil
}
func (s *CaptureStream) Push(frame []byte) error {
	if s.invalid {
		return errCaptureStream
	}
	// Native frame decoders already impose their own bounds. Keep a second
	// ceiling here for direct users and avoid allocating oversized JSON trees.
	if len(frame) > 8<<20 || !utf8.Valid(frame) && s.family != wire.Bedrock {
		s.invalid = true
		return errCaptureStream
	}
	if s.family == wire.Bedrock {
		m, err := ReadBedrockEvent(bytes.NewReader(frame), len(frame))
		if err != nil {
			s.invalid = true
			return errCaptureStream
		}
		typ := m.Headers.Get(":message-type")
		kind := m.Headers.Get(":event-type")
		if typ == nil || typ.String() != "event" || kind == nil {
			s.invalid = true
			return errCaptureStream
		}
		if !validCaptureJSON(m.Payload) {
			s.invalid = true
			return errCaptureStream
		}
		f, err := s.object(m.Payload)
		if err != nil {
			s.invalid = true
			return errCaptureStream
		}
		s.bedrock(kind.String(), f)
	} else {
		// SSE data lines form one JSON event. Multiple events can share a write.
		for _, event := range bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
			var lines [][]byte
			for line := range bytes.SplitSeq(event, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) {
					lines = append(lines, bytes.TrimSpace(line[5:]))
				}
			}
			if len(lines) == 0 {
				continue
			}
			data := bytes.Join(lines, []byte("\n"))
			if bytes.Equal(data, []byte("[DONE]")) {
				if s.family == wire.Chat || s.family == wire.MistralFIM {
					s.terminal = true
					continue
				}
				s.invalid = true
				break
			}
			if !validCaptureJSON(data) {
				s.invalid = true
				break
			}
			f, err := s.object(data)
			if err != nil {
				s.invalid = true
				break
			}
			switch s.family {
			case wire.Chat, wire.MistralFIM:
				s.chat(f)
			case wire.Responses:
				s.responses(f)
			case wire.Anthropic:
				s.anthropic(f)
			case wire.Gemini, wire.GeminiStream:
				s.gemini(f)
			case wire.CohereChat:
				s.cohere(f)
			default:
				s.invalid = true
			}
		}
	}
	return s.bounds()
}
func captureIndex(raw json.RawMessage) string {
	var n int
	if !present(raw) || json.Unmarshal(raw, &n) != nil || n < 0 {
		return ""
	}
	return fmt.Sprint(n)
}
func (s *CaptureStream) chat(f map[string]json.RawMessage) {
	if present(f["error"]) {
		s.invalid = true
	}
	for _, raw := range s.array(f["choices"]) {
		c, err := s.object(raw)
		if err != nil {
			s.invalid = true
			return
		}
		id := captureIndex(c["index"])
		if id == "" {
			s.invalid = true
			return
		}
		if _, ok := s.groups[id]; !ok {
			s.groups[id] = false
		}
		d, _ := s.object(c["delta"])
		if s.output && (present(d["audio"]) || present(d["images"])) {
			s.invalid = true
		}
		if s.groups[id] && (s.str(d["content"]) != "" || s.str(d["refusal"]) != "" || len(s.array(d["tool_calls"])) > 0) {
			s.invalid = true
		}
		s.add(id+"/text", s.str(d["content"]), false, false)
		s.add(id+"/refusal", s.str(d["refusal"]), false, false)
		for _, raw := range s.array(d["tool_calls"]) {
			call, _ := s.object(raw)
			ix := captureIndex(call["index"])
			if ix == "" {
				s.invalid = true
				continue
			}
			fn, _ := s.object(call["function"])
			s.tool(id+"/"+ix, s.str(fn["name"]), s.str(fn["arguments"]), false, false)
		}
		if fn, err := s.object(d["function_call"]); err == nil {
			s.tool(id+"/legacy", s.str(fn["name"]), s.str(fn["arguments"]), false, false)
		}
		if present(c["finish_reason"]) {
			reason := s.str(c["finish_reason"])
			if reason != "stop" && reason != "tool_calls" && reason != "function_call" {
				s.invalid = true
			}
			s.groups[id] = true
			for key := range s.calls {
				if strings.HasPrefix(key, id+"/") {
					s.done(key)
				}
			}
		}
	}
}
func (s *CaptureStream) responseItem(raw json.RawMessage, done bool) {
	item, err := s.object(raw)
	if err != nil {
		s.invalid = true
		return
	}
	id := s.str(item["id"])
	if id == "" {
		s.invalid = true
		return
	}
	switch s.str(item["type"]) {
	case "function_call":
		s.tool(id, s.str(item["name"]), s.str(item["arguments"]), true, done)
	case "message":
		if done {
			for i, raw := range s.array(item["content"]) {
				p, _ := s.object(raw)
				key := fmt.Sprintf("%s/%d", id, i)
				switch s.str(p["type"]) {
				case "output_text":
					s.add(key, s.str(p["text"]), false, true)
				case "refusal":
					s.add(key, s.str(p["refusal"]), false, true)
				default:
					s.invalid = true
				}
				s.done(key)
			}
		}
	case "reasoning": // Excluded from capture, as in the existing native adapter.
	default:
		s.invalid = true
	}
}
func (s *CaptureStream) responses(f map[string]json.RawMessage) {
	typ := s.str(f["type"])
	id := s.str(f["item_id"])
	switch typ {
	case "response.output_text.delta", "response.refusal.delta":
		ix := captureIndex(f["content_index"])
		if id == "" || ix == "" {
			s.invalid = true
			return
		}
		s.add(id+"/"+ix, s.str(f["delta"]), false, false)
	case "response.output_text.done", "response.refusal.done":
		ix := captureIndex(f["content_index"])
		if id == "" || ix == "" {
			s.invalid = true
			return
		}
		value := s.str(f["text"])
		if typ == "response.refusal.done" {
			value = s.str(f["refusal"])
		}
		s.add(id+"/"+ix, value, false, true)
		s.done(id + "/" + ix)
	case "response.function_call_arguments.delta":
		if id == "" {
			s.invalid = true
			return
		}
		s.tool(id, "", s.str(f["delta"]), false, false)
	case "response.function_call_arguments.done":
		if id == "" {
			s.invalid = true
			return
		}
		s.tool(id, "", s.str(f["arguments"]), true, true)
	case "response.output_item.added":
		s.responseItem(f["item"], false)
	case "response.output_item.done":
		s.responseItem(f["item"], true)
	case "response.completed":
		r, err := s.object(f["response"])
		if err != nil || s.str(r["status"]) != "completed" {
			s.invalid = true
			return
		}
		for _, item := range s.array(r["output"]) {
			s.responseItem(item, true)
		}
		s.terminal = true
	case "error", "response.failed", "response.incomplete":
		s.invalid = true
	case "response.created", "response.in_progress", "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
	default:
		s.invalid = true
	}
}
func (s *CaptureStream) anthropic(f map[string]json.RawMessage) {
	typ := s.str(f["type"])
	id := captureIndex(f["index"])
	switch typ {
	case "content_block_start":
		if id == "" {
			s.invalid = true
			return
		}
		s.groups[id] = false
		b, _ := s.object(f["content_block"])
		switch s.str(b["type"]) {
		case "text":
			s.add(id, s.str(b["text"]), false, false)
		case "tool_use":
			args := ""
			if present(b["input"]) && string(b["input"]) != "{}" {
				args = string(b["input"])
			}
			s.tool(id, s.str(b["name"]), args, false, false)
		case "thinking", "redacted_thinking":
		default:
			s.invalid = true
		}
	case "content_block_delta":
		if done, ok := s.groups[id]; !ok || done {
			s.invalid = true
			return
		}
		d, _ := s.object(f["delta"])
		switch s.str(d["type"]) {
		case "text_delta":
			s.add(id, s.str(d["text"]), false, false)
		case "input_json_delta":
			s.tool(id, "", s.str(d["partial_json"]), false, false)
		case "thinking_delta", "signature_delta":
		default:
			s.invalid = true
		}
	case "content_block_stop":
		if _, ok := s.groups[id]; !ok {
			s.invalid = true
		}
		s.groups[id] = true
		if p := s.calls[id]; p != nil && p.text == "" {
			p.text = "{}"
		}
		s.done(id)
	case "message_delta":
		d, _ := s.object(f["delta"])
		reason := s.str(d["stop_reason"])
		if reason != "end_turn" && reason != "tool_use" && reason != "stop_sequence" {
			s.invalid = true
		}
	case "message_stop":
		s.terminal = true
	case "message_start", "ping":
	default:
		s.invalid = true
	}
}
func (s *CaptureStream) gemini(f map[string]json.RawMessage) {
	if present(f["error"]) || present(f["promptFeedback"]) {
		s.invalid = true
	}
	for candidateIndex, raw := range s.array(f["candidates"]) {
		c, _ := s.object(raw)
		id := captureIndex(c["index"])
		if _, explicit := c["index"]; !explicit {
			id = fmt.Sprint(candidateIndex)
		}
		if id == "" {
			s.invalid = true
			return
		}
		if done := s.groups[id]; done {
			s.invalid = true
			return
		}
		s.groups[id] = false
		content, _ := s.object(c["content"])
		for _, raw := range s.array(content["parts"]) {
			p, _ := s.object(raw)
			if string(p["thought"]) == "true" {
				continue
			}
			if present(p["text"]) {
				s.add(id+"/text", s.str(p["text"]), false, false)
			}
			if fn, err := s.object(p["functionCall"]); err == nil {
				key := id + "/id/" + s.str(fn["id"])
				if s.str(fn["id"]) == "" {
					key = fmt.Sprintf("%s/anonymous/%d", id, s.geminiCallSequence)
					s.geminiCallSequence++
				}
				// Gemini emits complete functionCall objects. Repeated identities must
				// agree; partial/streaming argument extensions are unsupported.
				if present(fn["partialArgs"]) || present(fn["willContinue"]) {
					s.invalid = true
				}
				s.tool(key, s.str(fn["name"]), string(fn["args"]), true, true)
			}
			if present(p["inlineData"]) || present(p["fileData"]) || present(p["executableCode"]) || present(p["codeExecutionResult"]) {
				s.invalid = true
			}
		}
		if present(c["finishReason"]) {
			if s.str(c["finishReason"]) != "STOP" {
				s.invalid = true
			}
			s.groups[id] = true
		}
	}
}
func (s *CaptureStream) bedrock(kind string, f map[string]json.RawMessage) {
	id := captureIndex(f["contentBlockIndex"])
	switch kind {
	case "contentBlockStart":
		if id == "" {
			s.invalid = true
			return
		}
		s.groups[id] = false
		start, _ := s.object(f["start"])
		use, _ := s.object(start["toolUse"])
		if use != nil {
			s.tool(id, s.str(use["name"]), "", false, false)
		}
	case "contentBlockDelta":
		if id == "" || s.groups[id] {
			s.invalid = true
			return
		}
		s.groups[id] = false
		d, _ := s.object(f["delta"])
		if present(d["text"]) {
			s.add(id, s.str(d["text"]), false, false)
		}
		if use, err := s.object(d["toolUse"]); err == nil {
			s.tool(id, "", s.str(use["input"]), false, false)
		}
		if !present(d["text"]) && !present(d["toolUse"]) && !present(d["reasoningContent"]) {
			s.invalid = true
		}
	case "contentBlockStop":
		if _, ok := s.groups[id]; !ok {
			s.invalid = true
		}
		s.groups[id] = true
		s.done(id)
	case "messageStop":
		reason := s.str(f["stopReason"])
		if reason != "end_turn" && reason != "tool_use" && reason != "stop_sequence" {
			s.invalid = true
		}
		s.terminal = true
	case "messageStart", "metadata":
	default:
		s.invalid = true
	}
}
func (s *CaptureStream) cohere(f map[string]json.RawMessage) {
	typ := s.str(f["type"])
	id := captureIndex(f["index"])
	d, _ := s.object(f["delta"])
	m, _ := s.object(d["message"])
	switch typ {
	case "content-start":
		if id == "" {
			s.invalid = true
			return
		}
		s.groups["text/"+id] = false
	case "content-delta":
		if id == "" || s.groups["text/"+id] {
			s.invalid = true
			return
		}
		s.groups["text/"+id] = false
		c, _ := s.object(m["content"])
		s.add("text/"+id, s.str(c["text"]), false, false)
	case "content-end":
		s.groups["text/"+id] = true
		s.done("text/" + id)
	case "tool-call-start", "tool-call-delta":
		if id == "" || s.groups["tool/"+id] {
			s.invalid = true
			return
		}
		s.groups["tool/"+id] = false
		call, _ := s.object(m["tool_calls"])
		fn, _ := s.object(call["function"])
		s.tool("tool/"+id, s.str(fn["name"]), s.str(fn["arguments"]), false, false)
	case "tool-call-end":
		s.groups["tool/"+id] = true
		s.done("tool/" + id)
	case "message-end":
		reason := s.str(d["finish_reason"])
		if reason != "COMPLETE" && reason != "TOOL_CALL" && reason != "STOP_SEQUENCE" {
			s.invalid = true
		}
		s.terminal = true
	case "message-start", "citation-start", "citation-end", "tool-plan-delta":
	default:
		s.invalid = true
	}
}
func (s *CaptureStream) Finish() (string, []json.RawMessage, error) {
	if s.bounds() != nil {
		return "", nil, errCaptureStream
	}
	for _, done := range s.groups {
		if !done {
			return "", nil, errCaptureStream
		}
	}
	if s.family == wire.Chat || s.family == wire.MistralFIM || s.family == wire.Gemini || s.family == wire.GeminiStream {
		if len(s.groups) == 0 {
			return "", nil, errCaptureStream
		}
	} else if !s.terminal {
		return "", nil, errCaptureStream
	}
	var texts []string
	for _, id := range s.textOrder {
		p := s.text[id]
		if !utf8.ValidString(p.text) || (s.family == wire.Responses && !p.done) {
			return "", nil, errCaptureStream
		}
		if p.text != "" {
			texts = append(texts, p.text)
		}
	}
	var calls []json.RawMessage
	size := 0
	for _, id := range s.callOrder {
		p := s.calls[id]
		if !p.done || p.name == "" || !utf8.ValidString(p.text) || !validCaptureJSON([]byte(p.text)) {
			return "", nil, errCaptureStream
		}
		raw, err := json.Marshal(map[string]any{"id": id, "name": p.name, "arguments": json.RawMessage(p.text)})
		size += len(raw)
		if err != nil || size > s.limit {
			return "", nil, errCaptureStream
		}
		calls = append(calls, raw)
	}
	output := strings.Join(texts, "\n")
	if len(output) > s.limit {
		return "", nil, errCaptureStream
	}
	return output, calls, nil
}
