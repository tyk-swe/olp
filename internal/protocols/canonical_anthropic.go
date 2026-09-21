package protocols

import (
	"encoding/json"
	"fmt"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func decodeAnthropicGeneration(r *reader) error {
	c := r.c
	for _, k := range []string{"temperature", "top_p"} {
		r.parameter(k, k)
	}
	r.parameter("max_tokens", "max_output_tokens")
	r.parameter("stop_sequences", "stop")
	if v := r.take("system"); present(v) {
		parts, e := decodeParts(v, "anthropic", "/system", c)
		if e != nil {
			return e
		}
		c.Messages = append(c.Messages, Message{Role: "system", Parts: parts})
	}
	for i, v := range arr(r.take("messages")) {
		p := fmt.Sprintf("/messages/%d", i)
		m, e := read(v, p, c)
		if e != nil {
			return e
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
					return e
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
						return e
					}
					c.Messages = append(c.Messages, result)
				default:
					f.f["type"] = raw(kind)
					parts, e := decodeParts(raw([]Object{f.f}), "anthropic", path, c)
					if e != nil {
						return e
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
			return e
		}
		c.Tools = append(c.Tools, Tool{Name: str(t.take("name")), Description: str(t.take("description")), Schema: t.take("input_schema")})
		t.finish()
	}
	if v := r.take("tool_choice"); present(v) {
		t, e := read(v, "/tool_choice", c)
		if e != nil {
			return e
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
				return unsupported("disable_parallel_tool_use")
			}
			c.Parameters["parallel_tool_calls"] = raw(!disable)
		}
		t.finish()
	}
	return nil
}

func encodeAnthropicGeneration(c *Generation, model string, count bool) (Object, error) {
	f := Object{"model": raw(model)}
	for _, k := range []string{"seed"} {
		if present(c.Parameters[k]) {
			return nil, unsupported(k)
		}
	}
	if v := c.Parameters["response_format"]; present(v) {
		spec, e := jsonSchemaFormat(v)
		if e != nil {
			return nil, e
		}
		if spec != nil {
			f["output_config"] = raw(Object{"format": raw(Object{"type": raw("json_schema"), "schema": spec["schema"]})})
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
	return f, nil
}
