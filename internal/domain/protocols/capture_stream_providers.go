package protocols

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func (s *CaptureStream) completions(f map[string]json.RawMessage) {
	if present(f["error"]) {
		s.invalid = true
	}
	for _, raw := range s.array(f["choices"]) {
		choice, err := s.object(raw)
		if err != nil {
			s.invalid = true
			return
		}
		id := captureIndex(choice["index"])
		if id == "" {
			s.invalid = true
			return
		}
		text := s.str(choice["text"])
		if s.groups[id] && text != "" {
			s.invalid = true
		}
		if _, ok := s.groups[id]; !ok {
			s.groups[id] = false
		}
		s.add(id+"/text", text, false, false)
		if reason := s.str(choice["finish_reason"]); reason != "" {
			if !captureCompleteReason(wire.Completions, reason) {
				s.invalid = true
			}
			s.groups[id] = true
			s.done(id + "/text")
		}
	}
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
		status, explicit := r["status"]
		state := s.str(status)
		if err != nil || (explicit && (!present(status) || (state != "" && state != "completed"))) {
			s.invalid = true
			return
		}
		for _, item := range s.array(r["output"]) {
			s.responseItem(item, true)
		}
		s.terminal = true
	case "error", "response.failed", "response.incomplete":
		s.invalid = true
	case "response.created", "response.queued", "response.in_progress", "response.content_part.added", "response.content_part.done", "response.output_text.annotation.added", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
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
			if s.tools && present(b["input"]) {
				input, err := s.object(b["input"])
				if err != nil {
					s.invalid = true
					return
				}
				if len(input) != 0 {
					args = string(b["input"])
				}
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
		if p := s.calls[id]; p != nil && p.text.Len() == 0 {
			p.text.WriteString("{}")
		}
		s.done(id)
	case "message_delta":
		d, err := s.object(f["delta"])
		if err != nil {
			s.invalid = true
			return
		}
		reason := d["stop_reason"]
		if !present(reason) {
			return
		}
		if !captureCompleteReason(wire.Anthropic, s.str(reason)) {
			s.invalid = true
			return
		}
		s.anthropicFinished = true
	case "message_stop":
		if !s.anthropicFinished {
			s.invalid = true
			return
		}
		s.terminal = true
	case "message_start", "ping":
	default:
		s.invalid = true
	}
}

func (s *CaptureStream) gemini(f map[string]json.RawMessage) {
	candidates := s.array(f["candidates"])
	feedback, _ := s.object(f["promptFeedback"])
	if present(f["error"]) || (present(f["promptFeedback"]) && (len(candidates) == 0 || s.str(feedback["blockReason"]) != "")) {
		s.invalid = true
	}
	for candidateIndex, raw := range candidates {
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
				s.tool(key, s.str(fn["name"]), string(geminiArguments(fn["args"])), true, true)
			}
			if s.output && (present(p["inlineData"]) || present(p["fileData"]) || present(p["executableCode"]) || present(p["codeExecutionResult"])) {
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
	if typ == "debug" {
		return
	}
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
