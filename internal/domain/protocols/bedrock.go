package protocols

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func encodeBedrock(c *Generation, model, operation string) (Object, error) {
	for _, k := range []string{"seed", "parallel_tool_calls"} {
		if present(c.Parameters[k]) {
			return nil, unsupported(k)
		}
	}
	if n := c.Parameters["n"]; present(n) && string(n) != "1" {
		return nil, unsupported("n")
	}
	var schemaFormat Object
	if v := c.Parameters["response_format"]; present(v) {
		var e error
		if schemaFormat, e = jsonSchemaFormat(v); e != nil {
			return nil, e
		}
	}
	inference, err := encodeBedrockInference(c.Parameters)
	if err != nil {
		return nil, err
	}
	messages, system, err := encodeBedrockMessages(c.Messages)
	if err != nil {
		return nil, err
	}
	f := Object{"messages": raw(messages)}
	if len(system) > 0 {
		f["system"] = raw(system)
	}
	if schemaFormat != nil {
		var compact bytes.Buffer
		if e := json.Compact(&compact, schemaFormat["schema"]); e != nil {
			return nil, unsupported("response_format schema")
		}
		jsonSchema := Object{"schema": raw(compact.String()), "name": raw(str(schemaFormat["name"]))}
		if d := schemaFormat["description"]; present(d) {
			jsonSchema["description"] = d
		}
		f["outputConfig"] = raw(Object{"textFormat": raw(Object{"type": raw("json_schema"), "structure": raw(Object{"jsonSchema": raw(jsonSchema)})})})
	}
	if operation != "token_count" && len(inference) > 0 {
		f["inferenceConfig"] = raw(inference)
	}
	toolConfig, err := encodeBedrockToolConfig(c.Tools, c.Parameters["tool_choice"])
	if err != nil {
		return nil, err
	}
	if toolConfig != nil {
		f["toolConfig"] = raw(toolConfig)
	}
	if operation == "token_count" {
		return Object{"input": raw(Object{"converse": raw(f)})}, nil
	}
	return f, nil
}

func encodeBedrockInference(params Object) (Object, error) {
	inference := Object{}
	for from, to := range map[string]string{"max_output_tokens": "maxTokens", "temperature": "temperature", "top_p": "topP", "stop": "stopSequences"} {
		if v := params[from]; present(v) {
			if from == "max_output_tokens" {
				n, ok := count(v)
				if !ok || n == 0 || n > 2147483647 {
					return nil, unsupported("max_output_tokens")
				}
			}
			if from == "stop" && str(v) != "" {
				v = raw([]string{str(v)})
			}
			inference[to] = v
		}
	}
	return inference, nil
}

func encodeBedrockParts(rawParts []Part) ([]Object, error) {
	parts := []Object{}
	for _, p := range rawParts {
		if p.Detail != "" {
			return nil, unsupported("image detail")
		}
		if p.URL == "" {
			// Converse rejects blank text blocks.
			if p.Text != "" {
				parts = append(parts, Object{"text": raw(p.Text)})
			}
			continue
		}
		if !strings.HasPrefix(p.URL, "data:") {
			return nil, unsupported("Bedrock image URL")
		}
		_, data, ok := strings.Cut(p.URL, ";base64,")
		format := strings.TrimPrefix(p.MIME, "image/")
		if !ok || !strings.Contains("|png|jpeg|gif|webp|", "|"+format+"|") {
			return nil, unsupported("Bedrock image format")
		}
		if _, e := base64.StdEncoding.DecodeString(data); e != nil {
			return nil, unsupported("image data")
		}
		parts = append(parts, Object{"image": raw(map[string]any{"format": format, "source": map[string]string{"bytes": data}})})
	}
	return parts, nil
}

func encodeBedrockMessages(input []Message) ([]Object, []Object, error) {
	type turn struct {
		role    string
		content []Object
	}
	turns := []turn{}
	system := []Object{}
	for _, m := range input {
		if m.Name != "" {
			return nil, nil, unsupported("message name")
		}
		if m.Role != "tool" && m.ToolID != "" {
			return nil, nil, unsupported("tool result ID on non-tool message")
		}
		parts, err := encodeBedrockParts(m.Parts)
		if err != nil {
			return nil, nil, err
		}
		if m.Role == "system" || m.Role == "developer" {
			// Converse has one system field, ahead of the conversation. A system
			// message between turns would silently move to the front.
			if len(turns) > 0 {
				return nil, nil, unsupported("late system message")
			}
			if m.ToolID != "" || len(m.Calls) > 0 {
				return nil, nil, unsupported("system tool metadata")
			}
			for _, p := range m.Parts {
				if p.URL != "" {
					return nil, nil, unsupported("system image")
				}
			}
			system = append(system, parts...)
			continue
		}
		role := m.Role
		if role == "tool" {
			if m.ToolID == "" || len(m.Calls) > 0 || joinText(m.Parts) == "" {
				return nil, nil, unsupported("tool result")
			}
			for _, p := range m.Parts {
				if p.URL != "" {
					return nil, nil, unsupported("non-text tool result")
				}
			}
			parts = []Object{{"toolResult": raw(map[string]any{"toolUseId": m.ToolID, "content": parts})}}
			role = "user"
		}
		for _, call := range m.Calls {
			if !toolName.MatchString(call.Name) || call.ID == "" {
				return nil, nil, unsupported("tool call")
			}
			if _, e := object([]byte(call.Arguments)); e != nil {
				return nil, nil, unsupported("tool arguments")
			}
			parts = append(parts, Object{"toolUse": raw(map[string]any{"toolUseId": call.ID, "name": call.Name, "input": json.RawMessage(call.Arguments)})})
		}
		if role != "user" && role != "assistant" {
			return nil, nil, unsupported("message role")
		}
		if len(parts) == 0 {
			return nil, nil, unsupported("empty message content")
		}
		// Converse requires alternating roles, with every toolResult for an
		// assistant turn in the next user message, so adjacent turns merge.
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].content = append(turns[n-1].content, parts...)
		} else {
			turns = append(turns, turn{role, parts})
		}
	}
	messages := make([]Object, 0, len(turns))
	for _, t := range turns {
		messages = append(messages, Object{"role": raw(t.role), "content": raw(t.content)})
	}
	return messages, system, nil
}

func encodeBedrockToolConfig(input []Tool, choice json.RawMessage) (Object, error) {
	if len(input) == 0 {
		if present(choice) && str(choice) != "none" && str(choice) != "auto" {
			return nil, unsupported("tool choice without tools")
		}
		return nil, nil
	}
	tools := []Object{}
	names := map[string]bool{}
	for _, t := range input {
		if !toolName.MatchString(t.Name) || names[t.Name] {
			return nil, unsupported("tool name")
		}
		names[t.Name] = true
		if _, e := object(t.Schema); e != nil {
			return nil, unsupported("tool schema")
		}
		spec := map[string]any{"name": t.Name, "inputSchema": map[string]any{"json": t.Schema}}
		// Converse requires a non-empty description when one is sent.
		if t.Description != "" {
			spec["description"] = t.Description
		}
		tools = append(tools, Object{"toolSpec": raw(spec)})
	}
	cfg := Object{"tools": raw(tools)}
	if present(choice) {
		switch str(choice) {
		case "auto":
			cfg["toolChoice"] = raw(map[string]any{"auto": map[string]any{}})
		case "required":
			cfg["toolChoice"] = raw(map[string]any{"any": map[string]any{}})
		case "none":
			return nil, unsupported("tool_choice none with tools")
		default:
			ch, e := object(choice)
			if e != nil {
				return nil, e
			}
			fn, e := object(ch["function"])
			if e != nil {
				return nil, e
			}
			name := str(fn["name"])
			if !names[name] {
				return nil, unsupported("unknown named tool")
			}
			cfg["toolChoice"] = raw(map[string]any{"tool": map[string]string{"name": name}})
		}
	}
	return cfg, nil
}

func bedrockUsage(v json.RawMessage) (*wire.Usage, error) {
	u, e := optionalObject(v)
	if e != nil {
		return nil, e
	}
	if u == nil {
		return nil, nil
	}
	in, hasIn := count(u["inputTokens"])
	out, hasOut := count(u["outputTokens"])
	if !hasIn || !hasOut {
		return nil, protocolError("invalid Bedrock usage")
	}
	usage := &wire.Usage{InputTokens: in, OutputTokens: out}
	optional := func(key string) (*int64, error) {
		if !present(u[key]) {
			return nil, nil
		}
		n, ok := count(u[key])
		if !ok {
			return nil, protocolError("invalid Bedrock cache usage")
		}
		return &n, nil
	}
	var e2 error
	if usage.CachedInputTokens, e2 = optional("cacheReadInputTokens"); e2 != nil {
		return nil, e2
	}
	if usage.CacheWriteInputTokens, e2 = optional("cacheWriteInputTokens"); e2 != nil {
		return nil, e2
	}
	if present(u["cacheDetails"]) {
		details := arr(u["cacheDetails"])
		if details == nil {
			return nil, protocolError("invalid Bedrock cache details")
		}
		seen := map[string]bool{}
		for _, raw := range details {
			entry, e := object(raw)
			if e != nil {
				return nil, protocolError("invalid Bedrock cache detail")
			}
			ttl := str(entry["ttl"])
			if ttl != "5m" && ttl != "1h" {
				return nil, protocolError("unknown Bedrock cache TTL")
			}
			if seen[ttl] {
				return nil, protocolError("duplicate Bedrock cache TTL")
			}
			seen[ttl] = true
			n, ok := count(entry["inputTokens"])
			if !ok {
				return nil, protocolError("invalid Bedrock cache usage")
			}
			if ttl == "5m" {
				usage.CacheWrite5MInputTokens = &n
			} else {
				usage.CacheWrite1HInputTokens = &n
			}
		}
	}
	value := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	short, long := value(usage.CacheWrite5MInputTokens), value(usage.CacheWrite1HInputTokens)
	if short > math.MaxInt64-long {
		return nil, protocolError("Bedrock cache write detail exceeds total")
	}
	if usage.CacheWriteInputTokens == nil && (usage.CacheWrite5MInputTokens != nil || usage.CacheWrite1HInputTokens != nil) {
		return nil, protocolError("Bedrock cache write detail without total")
	}
	if short+long > value(usage.CacheWriteInputTokens) {
		return nil, protocolError("Bedrock cache write detail exceeds total")
	}
	// Converse reports cache reads and writes beside inputTokens, while the
	// canonical input total includes both cache categories.
	read, write := value(usage.CachedInputTokens), value(usage.CacheWriteInputTokens)
	if read > math.MaxInt64-in || write > math.MaxInt64-in-read {
		return nil, protocolError("usage count overflow")
	}
	usage.InputTokens = in + read + write
	if n, ok := count(u["totalTokens"]); ok {
		usage.TotalTokens = n
	} else if out > math.MaxInt64-usage.InputTokens {
		return nil, protocolError("usage count overflow")
	} else {
		usage.TotalTokens = usage.InputTokens + out
	}
	return usage, nil
}

func decodeBedrock(body []byte, route string) (*wire.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid Converse result")
	}
	output, e := object(f["output"])
	if e != nil {
		return nil, protocolError("missing Converse output")
	}
	message, e := object(output["message"])
	if e != nil || str(message["role"]) != "assistant" {
		return nil, protocolError("invalid Converse message")
	}
	if str(f["stopReason"]) == "" {
		return nil, protocolError("missing Converse stop reason")
	}
	c := &wire.Completion{ProviderModel: route, FinishReason: bedrockFinish(str(f["stopReason"]))}
	for _, v := range arr(message["content"]) {
		part, e := object(v)
		if e != nil {
			return nil, protocolError("invalid Converse content")
		}
		c.OutputText += str(part["text"])
		if present(part["toolUse"]) {
			call, e := object(part["toolUse"])
			if e != nil || str(call["toolUseId"]) == "" || str(call["name"]) == "" {
				return nil, protocolError("invalid Converse tool call")
			}
			if _, e := object(call["input"]); e != nil {
				return nil, protocolError("invalid Converse tool input")
			}
			c.ToolCalls = append(c.ToolCalls, wire.ToolCall{ID: str(call["toolUseId"]), Name: str(call["name"]), Arguments: string(call["input"])})
		}
	}
	c.Usage, e = bedrockUsage(f["usage"])
	return c, e
}

func bedrockFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "content_filtered", "guardrail_intervened":
		return "content_filter"
	default:
		return "stop"
	}
}
