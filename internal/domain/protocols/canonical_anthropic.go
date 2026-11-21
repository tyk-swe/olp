package protocols

import (
	"encoding/json"
	"fmt"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func decodeAnthropicGeneration(r *reader) error {
	c := r.c
	for _, k := range []string{"temperature", "top_p"} {
		r.parameter(k, k)
	}
	r.parameter("max_tokens", "max_output_tokens")
	r.parameter("stop_sequences", "stop")
	if err := decodeAnthropicReasoning(r, c); err != nil {
		return err
	}
	if v := r.take("system"); present(v) {
		parts, e := decodeParts(v, "anthropic", "/system", c)
		if e != nil {
			return e
		}
		c.Messages = append(c.Messages, Message{Role: "system", Parts: parts})
	}
	for i, v := range arr(r.take("messages")) {
		if err := decodeAnthropicMessage(v, fmt.Sprintf("/messages/%d", i), c); err != nil {
			return err
		}
	}
	for i, v := range arr(r.take("tools")) {
		t, e := read(v, fmt.Sprintf("/tools/%d", i), c)
		if e != nil {
			return e
		}
		c.Tools = append(c.Tools, Tool{Name: str(t.take("name")), Description: str(t.take("description")), Schema: t.take("input_schema")})
		// Eager input streaming only asks Anthropic to send the arguments of a
		// call as soon as they are generated, like stream_options a transport
		// option with no meaning to any other provider.
		t.take("eager_input_streaming")
		t.finish()
	}
	return decodeAnthropicToolChoice(r.take("tool_choice"), c)
}

func decodeAnthropicReasoning(r *reader, c *Generation) error {
	if v := r.take("output_config"); present(v) {
		config, err := read(v, "/output_config", c)
		if err != nil {
			return err
		}
		if effort := config.take("effort"); present(effort) {
			switch str(effort) {
			case "low", "medium", "high":
				c.Parameters["reasoning_effort"] = effort
			default:
				return unsupported("output_config.effort")
			}
		}
		config.finish()
	}
	if v := r.take("thinking"); present(v) {
		thinking, err := read(v, "/thinking", c)
		if err != nil {
			return err
		}
		if str(thinking.take("type")) != "adaptive" || !present(c.Parameters["reasoning_effort"]) {
			return unsupported("thinking")
		}
		thinking.finish()
	}
	return nil
}

func decodeAnthropicMessage(v json.RawMessage, p string, c *Generation) error {
	m, e := read(v, p, c)
	if e != nil {
		return e
	}
	role := str(m.take("role"))
	content := m.take("content")
	var text string
	if json.Unmarshal(content, &text) == nil {
		c.Messages = append(c.Messages, Message{Role: role, Parts: []Part{{Text: text}}})
		m.finish()
		return nil
	}
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
			base.Calls = append(base.Calls, wire.ToolCall{ID: str(f.take("id")), Name: str(f.take("name")), Arguments: string(f.take("input"))})
		case "tool_result":
			if value, present := f.f["is_error"]; present && string(value) == "false" {
				f.take("is_error")
			}
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
	m.finish()
	return nil
}

func decodeAnthropicToolChoice(v json.RawMessage, c *Generation) error {
	if !present(v) {
		return nil
	}
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
	messages, system, err := encodeAnthropicMessages(c.Messages)
	if err != nil {
		return nil, err
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
			schema := t.Schema
			if !present(schema) {
				// Anthropic requires an object schema, even for zero-argument tools.
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, Object{"name": raw(t.Name), "description": raw(t.Description), "input_schema": schema})
		}
		f["tools"] = raw(tools)
	}
	choice, err := encodeAnthropicToolChoice(c.Parameters)
	if err != nil {
		return nil, err
	}
	if choice != nil {
		f["tool_choice"] = raw(choice)
	}
	return f, nil
}

func encodeAnthropicMessages(input []Message) ([]Object, []Object, error) {
	messages := []Object{}
	system := []Object{}
	started := false
	for _, m := range input {
		if m.Name != "" {
			return nil, nil, unsupported("message name")
		}
		parts, e := encodeParts(m.Parts, "anthropic")
		if e != nil {
			return nil, nil, e
		}
		if m.Role == "system" || m.Role == "developer" {
			if started {
				return nil, nil, unsupported("late system message")
			}
			system = append(system, parts...)
			continue
		}
		started = true
		role := m.Role
		if role == "tool" {
			if m.ToolID == "" || len(m.Calls) > 0 {
				return nil, nil, unsupported("tool result")
			}
			role = "user"
			result := Object{"type": raw("tool_result"), "tool_use_id": raw(m.ToolID)}
			if len(parts) > 0 {
				result["content"] = raw(parts)
			}
			parts = []Object{result}
		}
		for _, t := range m.Calls {
			if !json.Valid([]byte(t.Arguments)) {
				return nil, nil, unsupported("tool arguments")
			}
			parts = append(parts, Object{"type": raw("tool_use"), "id": raw(t.ID), "name": raw(t.Name), "input": json.RawMessage(t.Arguments)})
		}
		messages = append(messages, Object{"role": raw(role), "content": raw(parts)})
	}
	return messages, system, nil
}

func encodeAnthropicToolChoice(params Object) (Object, error) {
	if v := params["tool_choice"]; present(v) {
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
		if v := params["parallel_tool_calls"]; present(v) {
			var parallel bool
			if json.Unmarshal(v, &parallel) != nil {
				return nil, unsupported("parallel_tool_calls")
			}
			choice["disable_parallel_tool_use"] = raw(!parallel)
		}
		return choice, nil
	}
	if v := params["parallel_tool_calls"]; present(v) {
		var parallel bool
		if json.Unmarshal(v, &parallel) != nil {
			return nil, unsupported("parallel_tool_calls")
		}
		return Object{"type": raw("auto"), "disable_parallel_tool_use": raw(!parallel)}, nil
	}
	return nil, nil
}
