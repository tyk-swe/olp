package protocols

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/oif"
	"io"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/protocols/awsframe"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
	inference := Object{}
	for from, to := range map[string]string{"max_output_tokens": "maxTokens", "temperature": "temperature", "top_p": "topP", "stop": "stopSequences"} {
		if v := c.Parameters[from]; present(v) {
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
	messages := []Object{}
	system := []Object{}
	for _, m := range c.Messages {
		if m.Name != "" {
			return nil, unsupported("message name")
		}
		if m.Role != "tool" && m.ToolID != "" {
			return nil, unsupported("tool result ID on non-tool message")
		}
		parts := []Object{}
		for _, p := range m.Parts {
			if p.Detail != "" {
				return nil, unsupported("image detail")
			}
			if p.URL == "" {
				parts = append(parts, Object{"text": raw(p.Text)})
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
		if m.Role == "system" || m.Role == "developer" {
			if m.ToolID != "" || len(m.Calls) > 0 {
				return nil, unsupported("system tool metadata")
			}
			for _, p := range m.Parts {
				if p.URL != "" {
					return nil, unsupported("system image")
				}
			}
			system = append(system, parts...)
			continue
		}
		role := m.Role
		if role == "tool" {
			if m.ToolID == "" || len(m.Calls) > 0 || joinText(m.Parts) == "" {
				return nil, unsupported("tool result")
			}
			for _, p := range m.Parts {
				if p.URL != "" {
					return nil, unsupported("non-text tool result")
				}
			}
			parts = []Object{{"toolResult": raw(map[string]any{"toolUseId": m.ToolID, "content": parts})}}
			role = "user"
		}
		for _, call := range m.Calls {
			if !toolName.MatchString(call.Name) || call.ID == "" {
				return nil, unsupported("tool call")
			}
			if _, e := object([]byte(call.Arguments)); e != nil {
				return nil, unsupported("tool arguments")
			}
			parts = append(parts, Object{"toolUse": raw(map[string]any{"toolUseId": call.ID, "name": call.Name, "input": json.RawMessage(call.Arguments)})})
		}
		if role != "user" && role != "assistant" {
			return nil, unsupported("message role")
		}
		messages = append(messages, Object{"role": raw(role), "content": raw(parts)})
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
	if len(c.Tools) > 0 {
		tools := []Object{}
		names := map[string]bool{}
		for _, t := range c.Tools {
			if !toolName.MatchString(t.Name) || names[t.Name] {
				return nil, unsupported("tool name")
			}
			names[t.Name] = true
			if _, e := object(t.Schema); e != nil {
				return nil, unsupported("tool schema")
			}
			tools = append(tools, Object{"toolSpec": raw(map[string]any{"name": t.Name, "description": t.Description, "inputSchema": map[string]any{"json": t.Schema}})})
		}
		cfg := Object{"tools": raw(tools)}
		choice := c.Parameters["tool_choice"]
		if present(choice) {
			kind := str(choice)
			switch kind {
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
		f["toolConfig"] = raw(cfg)
	} else if v := c.Parameters["tool_choice"]; present(v) && str(v) != "none" && str(v) != "auto" {
		return nil, unsupported("tool choice without tools")
	}
	if operation == "token_count" {
		return Object{"input": raw(Object{"converse": raw(f)})}, nil
	}
	return f, nil
}
func bedrockUsage(v json.RawMessage) (*openai.Usage, error) {
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
	usage := &openai.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
	if n, ok := count(u["totalTokens"]); ok {
		usage.TotalTokens = n
	}
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
	if value(usage.CacheWrite5MInputTokens)+value(usage.CacheWrite1HInputTokens) > value(usage.CacheWriteInputTokens) {
		return nil, protocolError("Bedrock cache write detail exceeds total")
	}
	if value(usage.CachedInputTokens)+value(usage.CacheWriteInputTokens) > in {
		return nil, protocolError("Bedrock cache usage exceeds input")
	}
	return usage, nil
}
func decodeBedrock(body []byte, route string) (*openai.Completion, error) {
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
	c := &openai.Completion{ProviderModel: route, FinishReason: bedrockFinish(str(f["stopReason"]))}
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
			c.ToolCalls = append(c.ToolCalls, openai.ToolCall{ID: str(call["toolUseId"]), Name: str(call["name"]), Arguments: string(call["input"])})
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

// ReadBedrockEvent checks the advertised length before the maintained AWS
// decoder allocates headers or payload, and lets that decoder verify both CRCs.
func ReadBedrockEvent(r io.Reader, limit int) (eventstream.Message, error) {
	var prelude [12]byte
	if _, e := io.ReadFull(r, prelude[:]); e != nil {
		return eventstream.Message{}, e
	}
	size := binary.BigEndian.Uint32(prelude[:4])
	if size > uint32(limit) {
		return eventstream.Message{}, openai.ErrEventTooLarge
	}
	if size < 16 {
		return eventstream.Message{}, protocolError("invalid AWS event length")
	}
	headerBytes := binary.BigEndian.Uint32(prelude[4:8])
	if headerBytes > size-16 {
		return eventstream.Message{}, protocolError("invalid AWS header length")
	}
	frame := make([]byte, int(size))
	copy(frame, prelude[:])
	if _, err := io.ReadFull(r, frame[12:]); err != nil {
		return eventstream.Message{}, err
	}
	if err := awsframe.ValidateHeaders(frame[12 : 12+headerBytes]); err != nil {
		return eventstream.Message{}, err
	}
	return eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
}
func streamBedrock(r io.Reader, limit int, route string, emit openai.Emit) (*openai.Completion, error) {
	return streamBedrockEvents(r, limit, route, emit, nil, false)
}
func streamBedrockEvents(r io.Reader, limit int, route string, emit openai.Emit, observe func(oif.Event) error, native bool) (*openai.Completion, error) {
	c := &openai.Completion{ProviderModel: route}
	started, stopped := false, false
	blocks := map[int64]*contentBlock{}
	retained := 0
	sequence := uint64(0)
	for {
		message, e := ReadBedrockEvent(r, limit)
		if errors.Is(e, io.EOF) {
			if stopped {
				return c, nil
			}
			return c, &openai.ProtocolError{Detail: "Converse stream ended before messageStop", Truncated: true}
		}
		if e != nil {
			if errors.Is(e, openai.ErrEventTooLarge) {
				return c, e
			}
			if errors.Is(e, io.ErrUnexpectedEOF) {
				return c, &openai.ProtocolError{Detail: "truncated AWS event", Truncated: true}
			}
			return c, protocolError("invalid bounded AWS event stream")
		}
		header := func(name string) string {
			if v := message.Headers.Get(name); v != nil {
				return v.String()
			}
			return ""
		}
		if header(":message-type") != "event" {
			return c, &openai.UpstreamError{Code: header(":exception-type"), Message: "Bedrock stream failed"}
		}
		kind := header(":event-type")
		event, e := openai.LiftEvent(openai.FamilyBedrock, string(message.Payload), kind, sequence, limit)
		if e != nil {
			return c, e
		}
		sequence++
		if observe != nil {
			if e := observe(event); e != nil {
				return c, e
			}
		}
		f := event.Source().Fields()
		if f == nil {
			return c, protocolError("invalid AWS event payload")
		}
		switch kind {
		case "messageStart":
			if started || str(f["role"]) != "assistant" {
				return c, protocolError("invalid Converse start")
			}
			started = true
		case "contentBlockStart":
			index, ok := count(f["contentBlockIndex"])
			if !started || stopped || !ok || blocks[index] != nil || len(blocks) >= max(1, limit/32) {
				return c, protocolError("invalid Converse block start")
			}
			blocks[index] = &contentBlock{}
		case "contentBlockDelta":
			index, ok := count(f["contentBlockIndex"])
			if !started || stopped || !ok {
				return c, protocolError("invalid Converse delta")
			}
			block := blocks[index]
			if block == nil {
				if len(blocks) >= max(1, limit/32) {
					return c, protocolError("too many Converse blocks")
				}
				block = &contentBlock{}
				blocks[index] = block
			}
			delta, _ := optionalObject(f["delta"])
			if call, e := optionalObject(delta["toolUse"]); e != nil {
				return c, e
			} else if call != nil {
				part := str(call["input"])
				if retained+len(part) > limit {
					return c, openai.ErrEventTooLarge
				}
				retained += len(part)
				block.args += part
			}
		case "contentBlockStop":
			index, ok := count(f["contentBlockIndex"])
			block := blocks[index]
			if !ok || block == nil || stopped {
				return c, protocolError("invalid Converse block stop")
			}
			if block.args != "" {
				if _, e := object([]byte(block.args)); e != nil {
					return c, protocolError("incomplete Converse tool arguments")
				}
			}
			retained -= len(block.args)
			delete(blocks, index)
		case "messageStop":
			if !started || stopped || len(blocks) > 0 || str(f["stopReason"]) == "" {
				return c, protocolError("invalid Converse stop")
			}
			stopped = true
			c.FinishReason = bedrockFinish(str(f["stopReason"]))
		case "metadata":
			if !stopped {
				return c, protocolError("Converse metadata before stop")
			}
			c.Usage, e = bedrockUsage(f["usage"])
			if e != nil {
				return c, e
			}
		default:
			if strings.HasSuffix(kind, "Exception") {
				return c, &openai.UpstreamError{Code: kind, Message: "Bedrock stream failed"}
			}
		}
		var frame []byte
		if native {
			var encoded bytes.Buffer
			if err := eventstream.NewEncoder().Encode(&encoded, message); err != nil {
				return c, err
			}
			frame = encoded.Bytes()
		} else {
			f["type"] = raw(kind)
			frame = eventFrame(kind, f)
		}
		if e := emit(frame); e != nil {
			return c, e
		}
	}
}
func (t *streamTranslator) bedrockFrame(f Object) error {
	index, _ := count(f["contentBlockIndex"])
	switch str(f["type"]) {
	case "messageStart":
		return t.start("")
	case "contentBlockStart":
		start, _ := optionalObject(f["start"])
		call, _ := optionalObject(start["toolUse"])
		if call != nil {
			return t.toolDelta(int(index), str(call["toolUseId"]), str(call["name"]), "")
		}
	case "contentBlockDelta":
		delta, _ := optionalObject(f["delta"])
		if e := t.textDelta(str(delta["text"])); e != nil {
			return e
		}
		call, _ := optionalObject(delta["toolUse"])
		if call != nil {
			return t.toolDelta(int(index), "", "", str(call["input"]))
		}
	}
	return nil
}
