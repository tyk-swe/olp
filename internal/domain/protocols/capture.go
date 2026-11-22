package protocols

import (
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type responseCapturer struct {
	r      *captureReader
	output bool
	tools  bool
	texts  []string
	calls  []json.RawMessage
	err    error
}

func (c *responseCapturer) add(raw json.RawMessage) {
	if !c.output || !present(raw) {
		return
	}
	values, e := captureResponseText(raw)
	if e != nil {
		c.err = e
		return
	}
	c.texts = append(c.texts, values...)
}

// CaptureResponse extracts only complete, inspectable selected surfaces. Unlike
// serving rewrites it is read-only, including on strict native routes.
func CaptureResponse(family wire.Format, body []byte, output, tools bool) (string, []json.RawMessage, error) {
	c := &responseCapturer{r: &captureReader{}, output: output, tools: tools}
	f, err := c.r.object(body)
	if err != nil || !validCaptureJSON(body) {
		return "", nil, errCaptureStream
	}
	c.captureFamily(family, f)
	if c.r.invalid {
		c.err = errCaptureStream
	}
	for _, call := range c.calls {
		if !validCaptureCall(family, call) {
			c.err = errCaptureStream
		}
	}
	return strings.Join(c.texts, "\n"), c.calls, c.err
}

func (c *responseCapturer) captureFamily(family wire.Format, f map[string]json.RawMessage) {
	switch family {
	case wire.Completions:
		c.captureCompletions(family, f)
	case wire.Chat, wire.MistralFIM:
		c.captureChat(family, f)
	case wire.Responses:
		c.captureResponses(f)
	case wire.Anthropic:
		c.captureAnthropic(family, f)
	case wire.Gemini, wire.GeminiStream:
		c.captureGemini(family, f)
	case wire.Bedrock:
		c.captureBedrock(family, f)
	case wire.CohereChat:
		if !captureCompleteReason(family, c.r.str(f["finish_reason"])) {
			c.err = errCaptureStream
		}
		m, _ := c.r.object(f["message"])
		if c.tools {
			c.calls = append(c.calls, c.r.array(m["tool_calls"])...)
		}
		if m == nil {
			c.err = errCaptureStream
		}
		c.add(m["content"])
	default:
		c.err = errCaptureStream
	}
}

func (c *responseCapturer) captureCompletions(family wire.Format, f map[string]json.RawMessage) {
	choices := c.r.array(f["choices"])
	if len(choices) == 0 {
		c.err = errCaptureStream
	}
	for _, raw := range choices {
		choice, _ := c.r.object(raw)
		if !captureCompleteReason(family, c.r.str(choice["finish_reason"])) {
			c.err = errCaptureStream
		}
		text := c.r.str(choice["text"])
		if !present(choice["text"]) {
			c.err = errCaptureStream
		}
		if c.output {
			c.texts = append(c.texts, text)
		}
	}
}

func (c *responseCapturer) captureChat(family wire.Format, f map[string]json.RawMessage) {
	if len(c.r.array(f["choices"])) == 0 {
		c.err = errCaptureStream
	}
	for _, raw := range c.r.array(f["choices"]) {
		choice, _ := c.r.object(raw)
		if !captureCompleteReason(family, c.r.str(choice["finish_reason"])) {
			c.err = errCaptureStream
		}
		m, _ := c.r.object(choice["message"])
		if m == nil {
			c.err = errCaptureStream
		}
		if c.tools {
			c.calls = append(c.calls, c.r.array(m["tool_calls"])...)
			if present(m["function_call"]) {
				c.calls = append(c.calls, rawJSONFunction(m["function_call"]))
			}
		}
		c.add(m["content"])
		c.add(m["refusal"])
		if c.output && present(m["audio"]) {
			c.err = errCaptureStream
		}
	}
}

func (c *responseCapturer) captureResponses(f map[string]json.RawMessage) {
	if !present(f["output"]) || (present(f["status"]) && c.r.str(f["status"]) != "completed") {
		c.err = errCaptureStream
	}
	for _, raw := range c.r.array(f["output"]) {
		item, _ := c.r.object(raw)
		switch c.r.str(item["type"]) {
		case "message":
			c.add(item["content"])
		case "function_call":
			if c.tools {
				c.calls = append(c.calls, raw)
			}
		case "reasoning":
		default:
			c.err = errCaptureStream
		}
	}
}

func (c *responseCapturer) captureAnthropic(family wire.Format, f map[string]json.RawMessage) {
	if !present(f["content"]) || !captureCompleteReason(family, c.r.str(f["stop_reason"])) {
		c.err = errCaptureStream
	}
	c.add(f["content"])
	if c.tools {
		for _, raw := range c.r.array(f["content"]) {
			block, _ := c.r.object(raw)
			if c.r.str(block["type"]) == "tool_use" {
				c.calls = append(c.calls, raw)
			}
		}
	}
}

func (c *responseCapturer) captureGemini(family wire.Format, f map[string]json.RawMessage) {
	if len(c.r.array(f["candidates"])) == 0 {
		c.err = errCaptureStream
	}
	for _, raw := range c.r.array(f["candidates"]) {
		cand, _ := c.r.object(raw)
		if !captureCompleteReason(family, c.r.str(cand["finishReason"])) {
			c.err = errCaptureStream
		}
		content, _ := c.r.object(cand["content"])
		for _, raw := range c.r.array(content["parts"]) {
			p, _ := c.r.object(raw)
			if c.tools && present(p["functionCall"]) {
				c.calls = append(c.calls, p["functionCall"])
			}
			if string(p["thought"]) == "true" {
				continue
			}
			if present(p["text"]) {
				c.add(p["text"])
			} else if !present(p["functionCall"]) && c.output {
				c.err = errCaptureStream
			}
		}
	}
}

func (c *responseCapturer) captureBedrock(family wire.Format, f map[string]json.RawMessage) {
	if !captureCompleteReason(family, c.r.str(f["stopReason"])) {
		c.err = errCaptureStream
	}
	out, _ := c.r.object(f["output"])
	m, _ := c.r.object(out["message"])
	if m == nil {
		c.err = errCaptureStream
	}
	for _, raw := range c.r.array(m["content"]) {
		p, _ := c.r.object(raw)
		if c.tools && present(p["toolUse"]) {
			c.calls = append(c.calls, p["toolUse"])
		}
		if present(p["text"]) {
			c.add(p["text"])
		} else if !present(p["toolUse"]) && !present(p["reasoningContent"]) && c.output {
			c.err = errCaptureStream
		}
	}
}

func captureCompleteReason(family wire.Format, reason string) bool {
	switch family {
	case wire.Completions:
		return reason == "stop" || reason == "eos_token" || reason == "stop_sequence"
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
		if present(call["partialArgs"]) || present(call["willContinue"]) {
			return false
		}
		args = geminiArguments(call["args"])
	default:
		return false
	}
	return !r.invalid && name != "" && validCaptureJSON(args)
}
