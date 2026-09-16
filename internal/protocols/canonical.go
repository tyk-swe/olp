package protocols

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type Part struct{ Text, URL, MIME, Detail string }
type Message struct {
	Role, Name, ToolID string
	Parts              []Part
	Calls              []openai.ToolCall
}
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
}
type Generation struct {
	Messages   []Message
	Tools      []Tool
	Parameters Object
	Extensions []string
	Stream     bool
}

// reader consumes only represented semantics. Everything left over is a source
// extension, including unknown nested content and tool fields.
type reader struct {
	f    Object
	path string
	c    *Generation
}

func read(v json.RawMessage, path string, c *Generation) (*reader, error) {
	f, e := object(v)
	return &reader{f, path, c}, e
}
func (r *reader) take(k string) json.RawMessage { v := r.f[k]; delete(r.f, k); return v }
func (r *reader) finish() {
	for k, v := range r.f {
		if present(v) {
			r.c.Extensions = append(r.c.Extensions, r.path+"/"+k)
		}
	}
}
func (r *reader) parameter(from, to string) {
	if v := r.take(from); present(v) {
		r.c.Parameters[to] = v
	}
}

func decodeCanonical(family openai.Family, fields Object) (*Generation, error) {
	c := &Generation{Parameters: Object{}}
	f := Object{}
	for k, v := range fields {
		f[k] = v
	}
	r := &reader{f: f, c: c}
	r.take("model")
	r.take("stream")
	r.take("stream_options")
	if family.Surface() == "openai" {
		for _, k := range []string{"temperature", "top_p", "stop", "seed", "n", "tool_choice", "parallel_tool_calls", "response_format"} {
			r.parameter(k, k)
		}
		r.parameter("max_tokens", "max_output_tokens")
		r.parameter("max_completion_tokens", "max_output_tokens")
		r.parameter("max_output_tokens", "max_output_tokens")
		if family == openai.FamilyChat {
			for i, v := range arr(r.take("messages")) {
				m, e := decodeChatMessage(v, fmt.Sprintf("/messages/%d", i), c)
				if e != nil {
					return nil, e
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
						return nil, e
					}
					kind := str(item.take("type"))
					switch kind {
					case "", "message":
						item.take("status")
						item.take("id")
						m := Message{Role: str(item.take("role"))}
						m.Parts, e = decodeParts(item.take("content"), "responses", p+"/content", c)
						if e != nil {
							return nil, e
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
							return nil, e
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
					return nil, e
				}
				if format := t.take("format"); present(format) {
					fo, e := object(format)
					if e != nil {
						return nil, e
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
				return nil, e
			}
			if str(t.take("type")) != "function" {
				c.Extensions = append(c.Extensions, p+"/type")
			}
			if family == openai.FamilyChat {
				nested, e := read(t.take("function"), p+"/function", c)
				if e != nil {
					return nil, e
				}
				c.Tools = append(c.Tools, decodeTool(nested))
				nested.finish()
			} else {
				c.Tools = append(c.Tools, decodeTool(t))
			}
			t.finish()
		}
	} else if family.Surface() == "anthropic" {
		for _, k := range []string{"temperature", "top_p"} {
			r.parameter(k, k)
		}
		r.parameter("max_tokens", "max_output_tokens")
		r.parameter("stop_sequences", "stop")
		if v := r.take("system"); present(v) {
			parts, e := decodeParts(v, "anthropic", "/system", c)
			if e != nil {
				return nil, e
			}
			c.Messages = append(c.Messages, Message{Role: "system", Parts: parts})
		}
		for i, v := range arr(r.take("messages")) {
			p := fmt.Sprintf("/messages/%d", i)
			m, e := read(v, p, c)
			if e != nil {
				return nil, e
			}
			role := str(m.take("role"))
			content := m.take("content")
			var text string
			if json.Unmarshal(content, &text) == nil {
				c.Messages = append(c.Messages, Message{Role: role, Parts: []Part{{Text: text}}})
			} else {
				base := Message{Role: role}
				for j, part := range arr(content) {
					path := fmt.Sprintf("%s/content/%d", p, j)
					f, e := read(part, path, c)
					if e != nil {
						return nil, e
					}
					kind := str(f.take("type"))
					switch kind {
					case "tool_use":
						base.Calls = append(base.Calls, openai.ToolCall{ID: str(f.take("id")), Name: str(f.take("name")), Arguments: string(f.take("input"))})
					case "tool_result":
						if len(base.Parts) > 0 || len(base.Calls) > 0 {
							c.Messages = append(c.Messages, base)
							base = Message{Role: role}
						}
						result := Message{Role: "tool", ToolID: str(f.take("tool_use_id"))}
						result.Parts, e = decodeParts(f.take("content"), "anthropic", path+"/content", c)
						if e != nil {
							return nil, e
						}
						c.Messages = append(c.Messages, result)
					default:
						f.f["type"] = raw(kind)
						parts, e := decodeParts(raw([]Object{f.f}), "anthropic", path, c)
						if e != nil {
							return nil, e
						}
						base.Parts = append(base.Parts, parts...)
						f.f = nil
					}
					f.finish()
				}
				if len(base.Parts) > 0 || len(base.Calls) > 0 {
					c.Messages = append(c.Messages, base)
				}
			}
			m.finish()
		}
		for i, v := range arr(r.take("tools")) {
			t, e := read(v, fmt.Sprintf("/tools/%d", i), c)
			if e != nil {
				return nil, e
			}
			c.Tools = append(c.Tools, Tool{Name: str(t.take("name")), Description: str(t.take("description")), Schema: t.take("input_schema")})
			t.finish()
		}
		if v := r.take("tool_choice"); present(v) {
			t, e := read(v, "/tool_choice", c)
			if e != nil {
				return nil, e
			}
			switch str(t.take("type")) {
			case "auto":
				c.Parameters["tool_choice"] = raw("auto")
			case "any":
				c.Parameters["tool_choice"] = raw("required")
			case "none":
				c.Parameters["tool_choice"] = raw("none")
			case "tool":
				c.Parameters["tool_choice"] = raw(map[string]any{"type": "function", "function": map[string]any{"name": str(t.take("name"))}})
			default:
				c.Extensions = append(c.Extensions, "/tool_choice/type")
			}
			if v := t.take("disable_parallel_tool_use"); present(v) {
				var disable bool
				if json.Unmarshal(v, &disable) != nil {
					return nil, unsupported("disable_parallel_tool_use")
				}
				c.Parameters["parallel_tool_calls"] = raw(!disable)
			}
			t.finish()
		}
	} else {
		if v := r.take("generateContentRequest"); present(v) {
			nested, e := object(v)
			if e != nil {
				return nil, e
			}
			nestedC, e := decodeCanonical(openai.FamilyGemini, nested)
			if e != nil {
				return nil, e
			}
			c = nestedC
			r.c = c
		}
		if v := r.take("generationConfig"); present(v) {
			g, e := read(v, "/generationConfig", c)
			if e != nil {
				return nil, e
			}
			for from, to := range map[string]string{"temperature": "temperature", "topP": "top_p", "maxOutputTokens": "max_output_tokens", "stopSequences": "stop", "candidateCount": "n", "seed": "seed"} {
				g.parameter(from, to)
			}
			mime := str(g.take("responseMimeType"))
			schema := g.take("responseSchema")
			if present(schema) && mime != "application/json" {
				if mime == "" {
					return nil, unsupported("responseSchema requires responseMimeType application/json")
				}
				c.Extensions = append(c.Extensions, "/generationConfig/responseSchema")
			}
			if mime == "application/json" {
				if present(schema) {
					c.Parameters["response_format"] = raw(map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": schema}})
				} else {
					c.Parameters["response_format"] = raw(map[string]any{"type": "json_object"})
				}
			} else if mime != "" && mime != "text/plain" {
				c.Extensions = append(c.Extensions, "/generationConfig/responseMimeType")
			}
			g.finish()
		}
		if v := r.take("systemInstruction"); present(v) {
			s, e := read(v, "/systemInstruction", c)
			if e != nil {
				return nil, e
			}
			s.take("role")
			parts, e := decodeParts(s.take("parts"), "gemini", "/systemInstruction/parts", c)
			if e != nil {
				return nil, e
			}
			c.Messages = append(c.Messages, Message{Role: "system", Parts: parts})
			s.finish()
		}
		calls := map[string][]string{}
		for i, v := range arr(r.take("contents")) {
			p := fmt.Sprintf("/contents/%d", i)
			m, e := read(v, p, c)
			if e != nil {
				return nil, e
			}
			role := str(m.take("role"))
			if role == "model" {
				role = "assistant"
			}
			if role == "" {
				role = "user"
			}
			base := Message{Role: role}
			for j, v := range arr(m.take("parts")) {
				path := fmt.Sprintf("%s/parts/%d", p, j)
				part, e := read(v, path, c)
				if e != nil {
					return nil, e
				}
				if fc := part.take("functionCall"); present(fc) {
					f, e := read(fc, path+"/functionCall", c)
					if e != nil {
						return nil, e
					}
					name, id := str(f.take("name")), str(f.take("id"))
					if id == "" {
						id = fmt.Sprintf("call_%d_%d", i, j)
					}
					calls[name] = append(calls[name], id)
					base.Calls = append(base.Calls, openai.ToolCall{ID: id, Name: name, Arguments: string(f.take("args"))})
					f.finish()
				} else if fr := part.take("functionResponse"); present(fr) {
					f, e := read(fr, path+"/functionResponse", c)
					if e != nil {
						return nil, e
					}
					name, id := str(f.take("name")), str(f.take("id"))
					if id == "" && len(calls[name]) > 0 {
						id = calls[name][0]
					}
					for n, pending := range calls[name] {
						if pending == id {
							calls[name] = append(calls[name][:n], calls[name][n+1:]...)
							break
						}
					}
					if id == "" {
						return nil, unsupported(path + "/functionResponse/id")
					}
					c.Messages = append(c.Messages, Message{Role: "tool", ToolID: id, Parts: []Part{{Text: string(f.take("response"))}}})
					f.finish()
				} else {
					parts, e := decodeParts(raw([]Object{part.f}), "gemini", path, c)
					if e != nil {
						return nil, e
					}
					base.Parts = append(base.Parts, parts...)
					part.f = nil
				}
				part.finish()
			}
			if len(base.Parts) > 0 || len(base.Calls) > 0 {
				c.Messages = append(c.Messages, base)
			}
			m.finish()
		}
		for i, v := range arr(r.take("tools")) {
			p := fmt.Sprintf("/tools/%d", i)
			t, e := read(v, p, c)
			if e != nil {
				return nil, e
			}
			for j, v := range arr(t.take("functionDeclarations")) {
				f, e := read(v, fmt.Sprintf("%s/functionDeclarations/%d", p, j), c)
				if e != nil {
					return nil, e
				}
				c.Tools = append(c.Tools, decodeTool(f))
				f.finish()
			}
			t.finish()
		}
		if v := r.take("toolConfig"); present(v) {
			t, e := read(v, "/toolConfig", c)
			if e != nil {
				return nil, e
			}
			fc, e := read(t.take("functionCallingConfig"), "/toolConfig/functionCallingConfig", c)
			if e != nil {
				return nil, e
			}
			switch str(fc.take("mode")) {
			case "AUTO":
				c.Parameters["tool_choice"] = raw("auto")
			case "NONE":
				c.Parameters["tool_choice"] = raw("none")
			case "ANY":
				names := arr(fc.take("allowedFunctionNames"))
				if len(names) == 1 {
					c.Parameters["tool_choice"] = raw(map[string]any{"type": "function", "function": map[string]string{"name": str(names[0])}})
				} else if len(names) == 0 {
					c.Parameters["tool_choice"] = raw("required")
				} else {
					c.Extensions = append(c.Extensions, "/toolConfig/functionCallingConfig/allowedFunctionNames")
				}
			default:
				c.Extensions = append(c.Extensions, "/toolConfig/functionCallingConfig/mode")
			}
			fc.finish()
			t.finish()
		}
	}
	r.finish()
	return c, nil
}
func decodeTool(t *reader) Tool {
	return Tool{Name: str(t.take("name")), Description: str(t.take("description")), Schema: t.take("parameters")}
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
func decodeParts(v json.RawMessage, family, path string, c *Generation) ([]Part, error) {
	if !present(v) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(v, &text) == nil {
		return []Part{{Text: text}}, nil
	}
	values := arr(v)
	if values == nil {
		return nil, unsupported(path)
	}
	parts := []Part{}
	for i, v := range values {
		p := fmt.Sprintf("%s/%d", path, i)
		r, e := read(v, p, c)
		if e != nil {
			return nil, e
		}
		kind := str(r.take("type"))
		part := Part{}
		if family == "gemini" {
			if text := r.take("text"); present(text) {
				part.Text = str(text)
			} else if data := r.take("inlineData"); present(data) {
				s, e := read(data, p+"/inlineData", c)
				if e != nil {
					return nil, e
				}
				part.MIME = str(s.take("mimeType"))
				part.URL = "data:" + part.MIME + ";base64," + str(s.take("data"))
				s.finish()
			} else if data := r.take("fileData"); present(data) {
				s, e := read(data, p+"/fileData", c)
				if e != nil {
					return nil, e
				}
				part.MIME = str(s.take("mimeType"))
				part.URL = str(s.take("fileUri"))
				s.finish()
			} else {
				c.Extensions = append(c.Extensions, p)
			}
		} else {
			switch kind {
			case "text", "input_text", "output_text":
				part.Text = str(r.take("text"))
			case "image_url":
				s, e := read(r.take("image_url"), p+"/image_url", c)
				if e != nil {
					return nil, e
				}
				part.URL = str(s.take("url"))
				part.Detail = str(s.take("detail"))
				s.finish()
			case "input_image":
				part.URL = str(r.take("image_url"))
				part.Detail = str(r.take("detail"))
			case "image":
				s, e := read(r.take("source"), p+"/source", c)
				if e != nil {
					return nil, e
				}
				switch str(s.take("type")) {
				case "base64":
					part.MIME = str(s.take("media_type"))
					part.URL = "data:" + part.MIME + ";base64," + str(s.take("data"))
				case "url":
					part.URL = str(s.take("url"))
				default:
					c.Extensions = append(c.Extensions, p+"/source/type")
				}
				s.finish()
			default:
				c.Extensions = append(c.Extensions, p+"/type")
			}
		}
		if part.MIME == "" && strings.HasPrefix(part.URL, "data:") {
			part.MIME, _, _ = strings.Cut(strings.TrimPrefix(part.URL, "data:"), ";")
		}
		parts = append(parts, part)
		r.finish()
	}
	return parts, nil
}

func encodeCanonical(c *Generation, family openai.Family, model, operation string) (Object, error) {
	if family == "bedrock" || family == "bedrock_count" {
		return encodeBedrock(c, model, operation)
	}
	f := Object{"model": raw(model)}
	count := operation == "token_count"
	switch family {
	case openai.FamilyChat, openai.FamilyInputTokens:
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
			if len(content) == 1 && str(content[0]["type"]) == "text" {
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
		} else {
			f["stream"] = raw(c.Stream)
			if c.Stream {
				f["stream_options"] = raw(map[string]bool{"include_usage": true})
			}
		}
	case openai.FamilyAnthropic, openai.FamilyAnthropicCount:
		for _, k := range []string{"seed", "response_format"} {
			if present(c.Parameters[k]) {
				return nil, unsupported(k)
			}
		}
		if n := c.Parameters["n"]; present(n) && string(n) != "1" {
			return nil, unsupported("n")
		}
		if !count && !present(c.Parameters["max_output_tokens"]) {
			return nil, unsupported("missing max_output_tokens")
		}
		messages := []Object{}
		system := []Object{}
		started := false
		for _, m := range c.Messages {
			if m.Name != "" {
				return nil, unsupported("message name")
			}
			parts, e := encodeParts(m.Parts, "anthropic")
			if e != nil {
				return nil, e
			}
			if m.Role == "system" || m.Role == "developer" {
				if started {
					return nil, unsupported("late system message")
				}
				system = append(system, parts...)
				continue
			}
			started = true
			role := m.Role
			if role == "tool" {
				if m.ToolID == "" || len(m.Calls) > 0 {
					return nil, unsupported("tool result")
				}
				role = "user"
				parts = []Object{{"type": raw("tool_result"), "tool_use_id": raw(m.ToolID), "content": raw(parts)}}
			}
			for _, t := range m.Calls {
				if !json.Valid([]byte(t.Arguments)) {
					return nil, unsupported("tool arguments")
				}
				parts = append(parts, Object{"type": raw("tool_use"), "id": raw(t.ID), "name": raw(t.Name), "input": json.RawMessage(t.Arguments)})
			}
			messages = append(messages, Object{"role": raw(role), "content": raw(parts)})
		}
		f["messages"] = raw(messages)
		if len(system) > 0 {
			f["system"] = raw(system)
		}
		for from, to := range map[string]string{"temperature": "temperature", "top_p": "top_p", "max_output_tokens": "max_tokens", "stop": "stop_sequences"} {
			if v := c.Parameters[from]; present(v) {
				if from == "stop" && str(v) != "" {
					v = raw([]string{str(v)})
				}
				f[to] = v
			}
		}
		if count {
			delete(f, "max_tokens")
			delete(f, "temperature")
			delete(f, "top_p")
			delete(f, "stop_sequences")
		} else {
			f["stream"] = raw(c.Stream)
		}
		if len(c.Tools) > 0 {
			tools := []Object{}
			for _, t := range c.Tools {
				tools = append(tools, Object{"name": raw(t.Name), "description": raw(t.Description), "input_schema": t.Schema})
			}
			f["tools"] = raw(tools)
		}
		if v := c.Parameters["tool_choice"]; present(v) {
			choice := Object{}
			switch str(v) {
			case "auto", "none":
				choice["type"] = v
			case "required":
				choice["type"] = raw("any")
			default:
				o, e := object(v)
				if e != nil {
					return nil, e
				}
				fun, e := object(o["function"])
				if e != nil {
					return nil, e
				}
				choice["type"] = raw("tool")
				choice["name"] = fun["name"]
			}
			if v := c.Parameters["parallel_tool_calls"]; present(v) {
				var parallel bool
				if json.Unmarshal(v, &parallel) != nil {
					return nil, unsupported("parallel_tool_calls")
				}
				choice["disable_parallel_tool_use"] = raw(!parallel)
			}
			f["tool_choice"] = raw(choice)
		} else if v := c.Parameters["parallel_tool_calls"]; present(v) {
			var parallel bool
			if json.Unmarshal(v, &parallel) != nil {
				return nil, unsupported("parallel_tool_calls")
			}
			f["tool_choice"] = raw(map[string]any{"type": "auto", "disable_parallel_tool_use": !parallel})
		}
	case openai.FamilyGemini, openai.FamilyGeminiCount:
		delete(f, "model")
		if present(c.Parameters["parallel_tool_calls"]) {
			return nil, unsupported("parallel_tool_calls")
		}
		contents := []Object{}
		system := []Object{}
		started := false
		names := map[string]string{}
		for _, m := range c.Messages {
			if m.Name != "" {
				return nil, unsupported("message name")
			}
			parts, err := encodeParts(m.Parts, "gemini")
			if err != nil {
				return nil, err
			}
			if m.Role == "system" || m.Role == "developer" {
				if started {
					return nil, unsupported("late system message")
				}
				system = append(system, parts...)
				continue
			}
			started = true
			role := m.Role
			if role == "assistant" {
				role = "model"
			}
			if role == "tool" {
				if m.ToolID == "" || names[m.ToolID] == "" {
					return nil, unsupported("tool result ID")
				}
				for _, p := range m.Parts {
					if p.URL != "" {
						return nil, unsupported("non-text tool result")
					}
				}
				role = "user"
				parts = []Object{{"functionResponse": raw(map[string]any{"name": names[m.ToolID], "id": m.ToolID, "response": map[string]any{"output": joinText(m.Parts)}})}}
			}
			for _, t := range m.Calls {
				if _, err := object([]byte(t.Arguments)); err != nil {
					return nil, unsupported("tool arguments")
				}
				names[t.ID] = t.Name
				parts = append(parts, Object{"functionCall": raw(map[string]any{"name": t.Name, "id": t.ID, "args": json.RawMessage(t.Arguments)})})
			}
			contents = append(contents, Object{"role": raw(role), "parts": raw(parts)})
		}
		f["contents"] = raw(contents)
		if len(system) > 0 {
			f["systemInstruction"] = raw(Object{"parts": raw(system)})
		}
		config := Object{}
		for from, to := range map[string]string{"temperature": "temperature", "top_p": "topP", "max_output_tokens": "maxOutputTokens", "stop": "stopSequences", "n": "candidateCount", "seed": "seed"} {
			if v := c.Parameters[from]; present(v) {
				if from == "stop" && str(v) != "" {
					v = raw([]string{str(v)})
				}
				config[to] = v
			}
		}
		if v := c.Parameters["response_format"]; present(v) {
			format, e := object(v)
			if e != nil {
				return nil, e
			}
			switch str(format["type"]) {
			case "text":
			case "json_object":
				config["responseMimeType"] = raw("application/json")
			case "json_schema":
				schema, e := object(format["json_schema"])
				if e != nil {
					return nil, e
				}
				config["responseMimeType"] = raw("application/json")
				config["responseSchema"] = schema["schema"]
			default:
				return nil, unsupported("response_format")
			}
		}
		if len(config) > 0 {
			f["generationConfig"] = raw(config)
		}
		if len(c.Tools) > 0 {
			tools := []Object{}
			for _, t := range c.Tools {
				tools = append(tools, Object{"name": raw(t.Name), "description": raw(t.Description), "parameters": t.Schema})
			}
			f["tools"] = raw([]Object{{"functionDeclarations": raw(tools)}})
		}
		if v := c.Parameters["tool_choice"]; present(v) {
			choice := Object{}
			switch str(v) {
			case "auto":
				choice["mode"] = raw("AUTO")
			case "none":
				choice["mode"] = raw("NONE")
			case "required":
				choice["mode"] = raw("ANY")
			default:
				o, e := object(v)
				if e != nil {
					return nil, e
				}
				fun, e := object(o["function"])
				if e != nil {
					return nil, e
				}
				choice["mode"] = raw("ANY")
				choice["allowedFunctionNames"] = raw([]string{str(fun["name"])})
			}
			f["toolConfig"] = raw(Object{"functionCallingConfig": raw(choice)})
		}
		if count {
			delete(f, "generationConfig")
			f["model"] = raw("models/" + strings.TrimPrefix(model, "models/"))
			f = Object{"generateContentRequest": raw(f)}
		}
	default:
		return nil, unsupported("operation")
	}
	return f, nil
}
func joinText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
func encodeParts(parts []Part, family string) ([]Object, error) {
	out := []Object{}
	for _, p := range parts {
		if p.Detail != "" {
			return nil, unsupported("image detail")
		}
		if p.URL == "" {
			v := Object{"text": raw(p.Text)}
			if family == "anthropic" {
				v["type"] = raw("text")
			}
			out = append(out, v)
			continue
		}
		if family == "anthropic" {
			source := Object{"type": raw("url"), "url": raw(p.URL)}
			if strings.HasPrefix(p.URL, "data:") {
				_, data, ok := strings.Cut(p.URL, ";base64,")
				if !ok || p.MIME == "" {
					return nil, unsupported("image data")
				}
				source = Object{"type": raw("base64"), "media_type": raw(p.MIME), "data": raw(data)}
			}
			out = append(out, Object{"type": raw("image"), "source": raw(source)})
		} else {
			if p.MIME == "" {
				return nil, unsupported("image MIME type")
			}
			if strings.HasPrefix(p.URL, "data:") {
				_, data, ok := strings.Cut(p.URL, ";base64,")
				if !ok {
					return nil, unsupported("image data")
				}
				out = append(out, Object{"inlineData": raw(map[string]string{"mimeType": p.MIME, "data": data})})
			} else {
				out = append(out, Object{"fileData": raw(map[string]string{"mimeType": p.MIME, "fileUri": p.URL})})
			}
		}
	}
	return out, nil
}
