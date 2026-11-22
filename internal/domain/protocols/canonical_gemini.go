package protocols

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func decodeGeminiGeneration(r *reader) error {
	c := r.c
	r.ignoreDefault("safetySettings", "[]")
	if v := r.take("generateContentRequest"); present(v) {
		nested, e := object(v)
		if e != nil {
			return e
		}
		nestedC, e := decodeCanonical(wire.Gemini, nested)
		if e != nil {
			return e
		}
		c = nestedC
		r.c = c
	}
	if err := decodeGeminiConfig(r.take("generationConfig"), c); err != nil {
		return err
	}
	if v := r.take("systemInstruction"); present(v) {
		s, e := read(v, "/systemInstruction", c)
		if e != nil {
			return e
		}
		s.take("role")
		parts, e := decodeParts(s.take("parts"), "gemini", "/systemInstruction/parts", c)
		if e != nil {
			return e
		}
		c.Messages = append(c.Messages, Message{Role: "system", Parts: parts})
		s.finish()
	}
	if err := decodeGeminiContents(arr(r.take("contents")), c); err != nil {
		return err
	}
	if err := decodeGeminiTools(arr(r.take("tools")), c); err != nil {
		return err
	}
	return decodeGeminiToolChoice(r.take("toolConfig"), c)
}

func decodeGeminiConfig(v json.RawMessage, c *Generation) error {
	if !present(v) {
		return nil
	}
	g, e := read(v, "/generationConfig", c)
	if e != nil {
		return e
	}
	for from, to := range map[string]string{"temperature": "temperature", "topP": "top_p", "maxOutputTokens": "max_output_tokens", "stopSequences": "stop", "candidateCount": "n", "seed": "seed"} {
		g.parameter(from, to)
	}
	mime := str(g.take("responseMimeType"))
	schema := jsonSchemaTypes(g.take("responseSchema"))
	// responseJsonSchema carries a standard JSON Schema where responseSchema
	// carries Gemini's OpenAPI subset; the canonical form is JSON Schema.
	if jsonSchema := g.take("responseJsonSchema"); present(jsonSchema) {
		if present(schema) {
			return unsupported("/generationConfig/responseJsonSchema")
		}
		schema = jsonSchema
	}
	if present(schema) && mime != "application/json" {
		if mime == "" {
			return unsupported("responseSchema requires responseMimeType application/json")
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
	return nil
}

func decodeGeminiContents(contents []json.RawMessage, c *Generation) error {
	calls := map[string][]string{}
	for i, v := range contents {
		p := fmt.Sprintf("/contents/%d", i)
		m, e := read(v, p, c)
		if e != nil {
			return e
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
			if err := decodeGeminiPart(v, path, i, j, c, calls, &base); err != nil {
				return err
			}
		}
		if len(base.Parts) > 0 || len(base.Calls) > 0 {
			c.Messages = append(c.Messages, base)
		}
		m.finish()
	}
	return nil
}

func decodeGeminiPart(v json.RawMessage, path string, i, j int, c *Generation, calls map[string][]string, base *Message) error {
	part, e := read(v, path, c)
	if e != nil {
		return e
	}
	if fc := part.take("functionCall"); present(fc) {
		f, e := read(fc, path+"/functionCall", c)
		if e != nil {
			return e
		}
		name, id := str(f.take("name")), str(f.take("id"))
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", i, j)
		}
		calls[name] = append(calls[name], id)
		base.Calls = append(base.Calls, wire.ToolCall{ID: id, Name: name, Arguments: string(geminiArguments(f.take("args")))})
		f.finish()
	} else if fr := part.take("functionResponse"); present(fr) {
		f, e := read(fr, path+"/functionResponse", c)
		if e != nil {
			return e
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
			return unsupported(path + "/functionResponse/id")
		}
		c.Messages = append(c.Messages, Message{Role: "tool", ToolID: id, Parts: []Part{{Text: string(f.take("response"))}}})
		f.finish()
	} else {
		parts, e := decodeParts(raw([]Object{part.f}), "gemini", path, c)
		if e != nil {
			return e
		}
		base.Parts = append(base.Parts, parts...)
		part.f = nil
	}
	part.finish()
	return nil
}

func decodeGeminiTools(tools []json.RawMessage, c *Generation) error {
	for i, v := range tools {
		p := fmt.Sprintf("/tools/%d", i)
		t, e := read(v, p, c)
		if e != nil {
			return e
		}
		for j, v := range arr(t.take("functionDeclarations")) {
			f, e := read(v, fmt.Sprintf("%s/functionDeclarations/%d", p, j), c)
			if e != nil {
				return e
			}
			tool := decodeTool(f)
			tool.Schema = jsonSchemaTypes(tool.Schema)
			// parametersJsonSchema is the same schema as parameters in standard
			// JSON Schema; current Google SDKs send it for every tool.
			if schema := f.take("parametersJsonSchema"); present(schema) {
				if present(tool.Schema) {
					return unsupported(fmt.Sprintf("%s/functionDeclarations/%d/parametersJsonSchema", p, j))
				}
				tool.Schema = schema
			}
			c.Tools = append(c.Tools, tool)
			f.finish()
		}
		t.finish()
	}
	return nil
}

func decodeGeminiToolChoice(v json.RawMessage, c *Generation) error {
	if !present(v) {
		return nil
	}
	t, e := read(v, "/toolConfig", c)
	if e != nil {
		return e
	}
	fc, e := read(t.take("functionCallingConfig"), "/toolConfig/functionCallingConfig", c)
	if e != nil {
		return e
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
	return nil
}

func encodeGeminiGeneration(c *Generation, model string, count bool) (Object, error) {
	if present(c.Parameters["parallel_tool_calls"]) {
		return nil, unsupported("parallel_tool_calls")
	}
	contents, system, err := encodeGeminiContents(c.Messages)
	if err != nil {
		return nil, err
	}
	f := Object{"contents": raw(contents)}
	if len(system) > 0 {
		f["systemInstruction"] = raw(Object{"parts": raw(system)})
	}
	config, err := encodeGeminiConfig(c.Parameters)
	if err != nil {
		return nil, err
	}
	if len(config) > 0 {
		f["generationConfig"] = raw(config)
	}
	if len(c.Tools) > 0 {
		tools := []Object{}
		for _, t := range c.Tools {
			tool := Object{"name": raw(t.Name), "description": raw(t.Description)}
			if present(t.Schema) {
				tool[geminiSchemaField(t.Schema, "parameters", "parametersJsonSchema")] = t.Schema
			}
			tools = append(tools, tool)
		}
		f["tools"] = raw([]Object{{"functionDeclarations": raw(tools)}})
	}
	if v := c.Parameters["tool_choice"]; present(v) {
		choice, err := encodeGeminiToolChoice(v)
		if err != nil {
			return nil, err
		}
		f["toolConfig"] = raw(Object{"functionCallingConfig": raw(choice)})
	}
	if count {
		delete(f, "generationConfig")
		f["model"] = raw("models/" + strings.TrimPrefix(model, "models/"))
		f = Object{"generateContentRequest": raw(f)}
	}
	return f, nil
}

func encodeGeminiContents(messages []Message) ([]Object, []Object, error) {
	// Gemini requires every functionResponse answering one function-call turn
	// in a single content, so adjacent tool results share one user turn.
	type geminiTurn struct {
		role  string
		parts []Object
		tool  bool
	}
	turns := []geminiTurn{}
	system := []Object{}
	started := false
	names := map[string]string{}
	for _, m := range messages {
		if m.Name != "" {
			return nil, nil, unsupported("message name")
		}
		parts, err := encodeParts(m.Parts, "gemini")
		if err != nil {
			return nil, nil, err
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
		if role == "assistant" {
			role = "model"
		}
		if role == "tool" {
			if m.ToolID == "" || names[m.ToolID] == "" {
				return nil, nil, unsupported("tool result ID")
			}
			for _, p := range m.Parts {
				if p.URL != "" {
					return nil, nil, unsupported("non-text tool result")
				}
			}
			response := geminiFunctionResponse(names[m.ToolID], m.ToolID, joinText(m.Parts))
			if len(m.Calls) == 0 && len(turns) > 0 && turns[len(turns)-1].tool {
				last := &turns[len(turns)-1]
				last.parts = append(last.parts, response)
				continue
			}
			role = "user"
			parts = []Object{response}
		}
		for _, t := range m.Calls {
			if _, err := object([]byte(t.Arguments)); err != nil {
				return nil, nil, unsupported("tool arguments")
			}
			names[t.ID] = t.Name
			parts = append(parts, Object{"functionCall": raw(map[string]any{"name": t.Name, "id": t.ID, "args": json.RawMessage(t.Arguments)})})
		}
		turns = append(turns, geminiTurn{role: role, parts: parts, tool: m.Role == "tool" && len(m.Calls) == 0})
	}
	contents := make([]Object, 0, len(turns))
	for _, t := range turns {
		contents = append(contents, Object{"role": raw(t.role), "parts": raw(t.parts)})
	}
	return contents, system, nil
}

func encodeGeminiConfig(params Object) (Object, error) {
	config := Object{}
	for from, to := range map[string]string{"temperature": "temperature", "top_p": "topP", "max_output_tokens": "maxOutputTokens", "stop": "stopSequences", "n": "candidateCount", "seed": "seed"} {
		if v := params[from]; present(v) {
			if from == "stop" && str(v) != "" {
				v = raw([]string{str(v)})
			}
			config[to] = v
		}
	}
	if v := params["response_format"]; present(v) {
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
			config[geminiSchemaField(schema["schema"], "responseSchema", "responseJsonSchema")] = schema["schema"]
		default:
			return nil, unsupported("response_format")
		}
	}
	return config, nil
}

func encodeGeminiToolChoice(v json.RawMessage) (Object, error) {
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
	return choice, nil
}

func geminiFunctionResponse(name, id, output string) Object {
	response := struct {
		Name     string            `json:"name"`
		ID       string            `json:"id,omitempty"`
		Response map[string]string `json:"response"`
	}{Name: name, ID: id, Response: map[string]string{"output": output}}
	return Object{"functionResponse": raw(response)}
}

func geminiArguments(args json.RawMessage) json.RawMessage {
	if !present(args) {
		return json.RawMessage(`{}`)
	}
	return args
}
