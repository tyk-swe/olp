package protocols

import (
	"encoding/json"
	"fmt"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func decodeOpenAIGeneration(r *reader, family openai.Family) error {
	c := r.c
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "n", "tool_choice", "parallel_tool_calls", "response_format"} {
		r.parameter(k, k)
	}
	r.parameter("max_tokens", "max_output_tokens")
	r.parameter("max_completion_tokens", "max_output_tokens")
	r.parameter("max_output_tokens", "max_output_tokens")
	if choice, e := object(c.Parameters["tool_choice"]); family != openai.FamilyChat && e == nil &&
		len(choice) == 2 && str(choice["type"]) == "function" && str(choice["name"]) != "" {
		// Responses names a forced function at the top level; the shared form is Chat's.
		c.Parameters["tool_choice"] = raw(map[string]any{"type": "function", "function": map[string]json.RawMessage{"name": choice["name"]}})
	}
	if family == openai.FamilyChat {
		for i, v := range arr(r.take("messages")) {
			m, e := decodeChatMessage(v, fmt.Sprintf("/messages/%d", i), c)
			if e != nil {
				return e
			}
			c.Messages = append(c.Messages, m)
		}
	} else {
		if instructions := r.take("instructions"); present(instructions) {
			c.Messages = append(c.Messages, Message{Role: "system", Parts: []Part{{Text: str(instructions)}}})
		}
		input := r.take("input")
		var text string
		if json.Unmarshal(input, &text) == nil {
			c.Messages = append(c.Messages, Message{Role: "user", Parts: []Part{{Text: text}}})
		} else {
			for i, v := range arr(input) {
				p := fmt.Sprintf("/input/%d", i)
				item, e := read(v, p, c)
				if e != nil {
					return e
				}
				kind := str(item.take("type"))
				switch kind {
				case "", "message":
					item.take("status")
					item.take("id")
					m := Message{Role: str(item.take("role"))}
					m.Parts, e = decodeParts(item.take("content"), "responses", p+"/content", c)
					if e != nil {
						return e
					}
					c.Messages = append(c.Messages, m)
				case "function_call":
					item.take("id")
					item.take("status")
					tc := openai.ToolCall{ID: str(item.take("call_id")), Name: str(item.take("name")), Arguments: str(item.take("arguments"))}
					c.Messages = append(c.Messages, Message{Role: "assistant", Calls: []openai.ToolCall{tc}})
				case "function_call_output":
					item.take("id")
					m := Message{Role: "tool", ToolID: str(item.take("call_id"))}
					m.Parts, e = decodeParts(item.take("output"), "responses", p+"/output", c)
					if e != nil {
						return e
					}
					c.Messages = append(c.Messages, m)
				default:
					c.Extensions = append(c.Extensions, p+"/type")
				}
				item.finish()
			}
		}
		if text := r.take("text"); present(text) {
			t, e := read(text, "/text", c)
			if e != nil {
				return e
			}
			if format := t.take("format"); present(format) {
				fo, e := object(format)
				if e != nil {
					return e
				}
				if str(fo["type"]) == "json_schema" {
					c.Parameters["response_format"] = raw(Object{"type": raw("json_schema"), "json_schema": raw(fo)})
				} else {
					c.Parameters["response_format"] = format
				}
			}
			t.finish()
		}
	}
	for i, v := range arr(r.take("tools")) {
		p := fmt.Sprintf("/tools/%d", i)
		t, e := read(v, p, c)
		if e != nil {
			return e
		}
		if str(t.take("type")) != "function" {
			c.Extensions = append(c.Extensions, p+"/type")
		}
		if family == openai.FamilyChat {
			nested, e := read(t.take("function"), p+"/function", c)
			if e != nil {
				return e
			}
			c.Tools = append(c.Tools, decodeTool(nested))
			nested.finish()
		} else {
			c.Tools = append(c.Tools, decodeTool(t))
		}
		t.finish()
	}
	return nil
}

func encodeOpenAIGeneration(c *Generation, model string, count bool) (Object, error) {
	f := Object{"model": raw(model)}
	messages := []Object{}
	for _, m := range c.Messages {
		v := Object{"role": raw(m.Role)}
		if m.Name != "" {
			v["name"] = raw(m.Name)
		}
		if m.ToolID != "" {
			v["tool_call_id"] = raw(m.ToolID)
		}
		content := []Object{}
		for _, p := range m.Parts {
			if p.URL != "" {
				image := Object{"url": raw(p.URL)}
				if p.Detail != "" {
					image["detail"] = raw(p.Detail)
				}
				content = append(content, Object{"type": raw("image_url"), "image_url": raw(image)})
			} else {
				content = append(content, Object{"type": raw("text"), "text": raw(p.Text)})
			}
		}
		if len(content) == 0 && len(m.Calls) > 0 {
			// Chat rejects an empty content array; a tool-call turn uses null.
			v["content"] = raw(nil)
		} else if len(content) == 1 && str(content[0]["type"]) == "text" {
			v["content"] = content[0]["text"]
		} else {
			v["content"] = raw(content)
		}
		if len(m.Calls) > 0 {
			calls := []Object{}
			for _, t := range m.Calls {
				calls = append(calls, Object{"id": raw(t.ID), "type": raw("function"), "function": raw(map[string]any{"name": t.Name, "arguments": t.Arguments})})
			}
			v["tool_calls"] = raw(calls)
		}
		messages = append(messages, v)
	}
	f["messages"] = raw(messages)
	for k, v := range c.Parameters {
		if k == "max_output_tokens" {
			k = "max_completion_tokens"
		}
		f[k] = v
	}
	if len(c.Tools) > 0 {
		tools := []Object{}
		for _, t := range c.Tools {
			tools = append(tools, Object{"type": raw("function"), "function": raw(map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema})})
		}
		f["tools"] = raw(tools)
	}
	if count {
		input := []Object{}
		for _, m := range c.Messages {
			if len(m.Calls) > 0 {
				for _, call := range m.Calls {
					input = append(input, Object{"type": raw("function_call"), "call_id": raw(call.ID), "name": raw(call.Name), "arguments": raw(call.Arguments)})
				}
			}
			if m.Role == "tool" {
				input = append(input, Object{"type": raw("function_call_output"), "call_id": raw(m.ToolID), "output": raw(joinText(m.Parts))})
			} else if len(m.Parts) > 0 {
				parts := []Object{}
				for _, p := range m.Parts {
					if p.URL != "" {
						parts = append(parts, Object{"type": raw("input_image"), "image_url": raw(p.URL)})
					} else {
						parts = append(parts, Object{"type": raw("input_text"), "text": raw(p.Text)})
					}
				}
				input = append(input, Object{"role": raw(m.Role), "content": raw(parts)})
			}
		}
		delete(f, "messages")
		f["input"] = raw(input)
		if len(c.Tools) > 0 {
			tools := []Object{}
			for _, t := range c.Tools {
				tools = append(tools, Object{"type": raw("function"), "name": raw(t.Name), "description": raw(t.Description), "parameters": t.Schema})
			}
			f["tools"] = raw(tools)
		}
		// Responses-shaped counting names a forced function at the top level.
		if choice, e := object(f["tool_choice"]); e == nil && len(choice) == 2 && str(choice["type"]) == "function" {
			if fn, e := object(choice["function"]); e == nil && len(fn) == 1 && str(fn["name"]) != "" {
				f["tool_choice"] = raw(map[string]json.RawMessage{"type": choice["type"], "name": fn["name"]})
			}
		}
	} else {
		f["stream"] = raw(c.Stream)
		if c.Stream {
			f["stream_options"] = raw(map[string]bool{"include_usage": true})
		}
	}
	return f, nil
}

func decodeChatMessage(v json.RawMessage, path string, c *Generation) (Message, error) {
	r, e := read(v, path, c)
	if e != nil {
		return Message{}, e
	}
	m := Message{Role: str(r.take("role")), Name: str(r.take("name")), ToolID: str(r.take("tool_call_id"))}
	m.Parts, e = decodeParts(r.take("content"), "chat", path+"/content", c)
	if e != nil {
		return m, e
	}
	for i, v := range arr(r.take("tool_calls")) {
		t, e := read(v, fmt.Sprintf("%s/tool_calls/%d", path, i), c)
		if e != nil {
			return m, e
		}
		kind := str(t.take("type"))
		if kind != "function" {
			c.Extensions = append(c.Extensions, t.path+"/type")
		}
		id := str(t.take("id"))
		f, e := read(t.take("function"), t.path+"/function", c)
		if e != nil {
			return m, e
		}
		m.Calls = append(m.Calls, openai.ToolCall{ID: id, Name: str(f.take("name")), Arguments: str(f.take("arguments"))})
		f.finish()
		t.finish()
	}
	r.finish()
	return m, nil
}
