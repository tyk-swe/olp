package protocols

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func decodeGeminiGeneration(r *reader) error {
	c := r.c
	r.ignoreDefault("safetySettings", "[]")
	if v := r.take("generateContentRequest"); present(v) {
		nested, e := object(v)
		if e != nil {
			return e
		}
		nestedC, e := decodeCanonical(openai.FamilyGemini, nested)
		if e != nil {
			return e
		}
		c = nestedC
		r.c = c
	}
	if v := r.take("generationConfig"); present(v) {
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
	calls := map[string][]string{}
	for i, v := range arr(r.take("contents")) {
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
				base.Calls = append(base.Calls, openai.ToolCall{ID: id, Name: name, Arguments: string(f.take("args"))})
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
	if v := r.take("toolConfig"); present(v) {
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
	}
	return nil
}

func encodeGeminiGeneration(c *Generation, model string, count bool) (Object, error) {
	f := Object{}
	if present(c.Parameters["parallel_tool_calls"]) {
		return nil, unsupported("parallel_tool_calls")
	}
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
			response := Object{"functionResponse": raw(map[string]any{"name": names[m.ToolID], "id": m.ToolID, "response": map[string]any{"output": joinText(m.Parts)}})}
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
				return nil, unsupported("tool arguments")
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
			config[geminiSchemaField(schema["schema"], "responseSchema", "responseJsonSchema")] = schema["schema"]
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
			tool := Object{"name": raw(t.Name), "description": raw(t.Description)}
			if present(t.Schema) {
				tool[geminiSchemaField(t.Schema, "parameters", "parametersJsonSchema")] = t.Schema
			}
			tools = append(tools, tool)
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
	return f, nil
}
