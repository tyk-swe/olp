package protocols

import (
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func CaptureDocumentToolCalls(family wire.Format, body []byte) []json.RawMessage {
	_, calls, err := CaptureResponse(family, body, false, true)
	if err != nil {
		return nil
	}
	return calls
}

// CaptureResponse extracts only complete, inspectable selected surfaces. Unlike
// serving rewrites it is read-only, including on strict native routes.
func CaptureResponse(family wire.Format, body []byte, output, tools bool) (string, []json.RawMessage, error) {
	r := &captureReader{}
	var calls []json.RawMessage
	f, err := r.object(body)
	if err != nil || !validCaptureJSON(body) {
		return "", nil, errCaptureStream
	}
	var texts []string
	add := func(raw json.RawMessage) {
		if !output || !present(raw) {
			return
		}
		values, e := captureResponseText(raw)
		if e != nil {
			err = e
		} else {
			texts = append(texts, values...)
		}
	}
	switch family {
	case wire.Chat, wire.MistralFIM:
		if len(r.array(f["choices"])) == 0 {
			err = errCaptureStream
		}
		for _, raw := range r.array(f["choices"]) {
			c, _ := r.object(raw)
			if !captureCompleteReason(family, r.str(c["finish_reason"])) {
				err = errCaptureStream
			}
			m, _ := r.object(c["message"])
			if m == nil {
				err = errCaptureStream
			}
			if tools {
				for _, call := range r.array(m["tool_calls"]) {
					calls = append(calls, call)
				}
				if present(m["function_call"]) {
					calls = append(calls, rawJSONFunction(m["function_call"]))
				}
			}
			add(m["content"])
			add(m["refusal"])
			if output && present(m["audio"]) {
				err = errCaptureStream
			}
		}
	case wire.Responses:
		if !present(f["output"]) || (present(f["status"]) && r.str(f["status"]) != "completed") {
			err = errCaptureStream
		}
		for _, raw := range r.array(f["output"]) {
			item, _ := r.object(raw)
			switch r.str(item["type"]) {
			case "message":
				add(item["content"])
			case "function_call":
				if tools {
					calls = append(calls, raw)
				}
			case "reasoning":
			default:
				err = errCaptureStream
			}
		}
	case wire.Anthropic:
		if !present(f["content"]) || !captureCompleteReason(family, r.str(f["stop_reason"])) {
			err = errCaptureStream
		}
		add(f["content"])
		if tools {
			for _, raw := range r.array(f["content"]) {
				block, _ := r.object(raw)
				if r.str(block["type"]) == "tool_use" {
					calls = append(calls, raw)
				}
			}
		}
	case wire.Gemini, wire.GeminiStream:
		if len(r.array(f["candidates"])) == 0 {
			err = errCaptureStream
		}
		for _, raw := range r.array(f["candidates"]) {
			c, _ := r.object(raw)
			if !captureCompleteReason(family, r.str(c["finishReason"])) {
				err = errCaptureStream
			}
			content, _ := r.object(c["content"])
			for _, raw := range r.array(content["parts"]) {
				p, _ := r.object(raw)
				if tools && present(p["functionCall"]) {
					calls = append(calls, p["functionCall"])
				}
				if string(p["thought"]) == "true" {
					continue
				}
				if present(p["text"]) {
					add(p["text"])
				} else if !present(p["functionCall"]) && output {
					err = errCaptureStream
				}
			}
		}
	case wire.Bedrock:
		if !captureCompleteReason(family, r.str(f["stopReason"])) {
			err = errCaptureStream
		}
		out, _ := r.object(f["output"])
		m, _ := r.object(out["message"])
		if m == nil {
			err = errCaptureStream
		}
		for _, raw := range r.array(m["content"]) {
			p, _ := r.object(raw)
			if tools && present(p["toolUse"]) {
				calls = append(calls, p["toolUse"])
			}
			if present(p["text"]) {
				add(p["text"])
			} else if !present(p["toolUse"]) && !present(p["reasoningContent"]) && output {
				err = errCaptureStream
			}
		}
	case wire.CohereChat:
		if !captureCompleteReason(family, r.str(f["finish_reason"])) {
			err = errCaptureStream
		}
		m, _ := r.object(f["message"])
		if tools {
			calls = append(calls, r.array(m["tool_calls"])...)
		}
		if m == nil {
			err = errCaptureStream
		}
		add(m["content"])
	default:
		err = errCaptureStream
	}
	if r.invalid {
		err = errCaptureStream
	}
	for _, call := range calls {
		if !validCaptureCall(family, call) {
			err = errCaptureStream
		}
	}
	return strings.Join(texts, "\n"), calls, err
}

func captureCompleteReason(family wire.Format, reason string) bool {
	switch family {
	case wire.Chat, wire.MistralFIM:
		return reason == "stop" || reason == "tool_calls" || reason == "function_call"
	case wire.Anthropic, wire.Bedrock:
		return reason == "end_turn" || reason == "tool_use" || reason == "stop_sequence"
	case wire.Gemini, wire.GeminiStream:
		return reason == "STOP"
	case wire.CohereChat:
		return reason == "COMPLETE" || reason == "TOOL_CALL" || reason == "STOP_SEQUENCE"
	default:
		return false
	}
}

func captureResponseText(raw json.RawMessage) ([]string, error) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, errCaptureStream
	}
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return []string{text}, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, errCaptureStream
	}
	var texts []string
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			return nil, errCaptureStream
		}
		switch p["type"] {
		case "text", "output_text":
			text, ok := p["text"].(string)
			if !ok {
				return nil, errCaptureStream
			}
			texts = append(texts, text)
		case "refusal":
			text, ok := p["refusal"].(string)
			if !ok {
				return nil, errCaptureStream
			}
			texts = append(texts, text)
		case "tool_use", "thinking", "redacted_thinking":
		default:
			return nil, errCaptureStream
		}
	}
	return texts, nil
}

func rawJSONFunction(fn json.RawMessage) json.RawMessage {
	raw, _ := json.Marshal(map[string]json.RawMessage{"function": fn})
	return raw
}

func validCaptureCall(family wire.Format, raw json.RawMessage) bool {
	r := &captureReader{}
	call, err := r.object(raw)
	if err != nil {
		return false
	}
	var name string
	var args json.RawMessage
	switch family {
	case wire.Chat, wire.MistralFIM, wire.CohereChat:
		fn, _ := r.object(call["function"])
		name = r.str(fn["name"])
		args = []byte(r.str(fn["arguments"]))
	case wire.Responses:
		name = r.str(call["name"])
		args = []byte(r.str(call["arguments"]))
	case wire.Anthropic, wire.Bedrock:
		name = r.str(call["name"])
		args = call["input"]
	case wire.Gemini, wire.GeminiStream:
		name = r.str(call["name"])
		args = call["args"]
	default:
		return false
	}
	return !r.invalid && name != "" && validCaptureJSON(args)
}
