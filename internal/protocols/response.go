package protocols

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Decode validates a bounded upstream result. Native extensions survive model
// rewriting; translated results contain only the shared, represented fields.
func Decode(wire, target openai.Family, body []byte, route, encoding string) (*openai.Completion, error) {
	return decode(wire, target, body, route, encoding, nil)
}

func DecodeRequest(wire, target openai.Family, body []byte, route, encoding string, request *openai.Request) (*openai.Completion, error) {
	return decode(wire, target, body, route, encoding, request)
}

func decode(wire, target openai.Family, body []byte, route, encoding string, request *openai.Request) (*openai.Completion, error) {
	var c *openai.Completion
	var err error
	switch wire {
	case openai.FamilyChat:
		c, err = openai.DecodeChat(body, route)
	case openai.FamilyResponses:
		var background bool
		if request != nil && target == openai.FamilyResponses {
			_ = json.Unmarshal(request.Field("background"), &background)
		}
		if background {
			c, err = openai.DecodeBackgroundResponse(body, route)
		} else {
			c, err = openai.DecodeResponse(body, route)
		}
	case openai.FamilyAnthropic:
		c, err = decodeAnthropic(body, route)
	case openai.FamilyGemini:
		c, err = decodeGemini(body, route, false)
	case openai.FamilyInputTokens, openai.FamilyAnthropicCount, openai.FamilyGeminiCount, "bedrock_count":
		f, e := object(body)
		if e != nil {
			return nil, protocolError("invalid count result")
		}
		key := "input_tokens"
		if wire == openai.FamilyGeminiCount {
			key = "totalTokens"
		}
		if wire == "bedrock_count" {
			key = "inputTokens"
		}
		n, ok := count(f[key])
		if !ok {
			return nil, protocolError("missing token count")
		}
		out := Object{"input_tokens": raw(n), "object": raw("response.input_tokens")}
		if target == openai.FamilyAnthropicCount {
			out = Object{"input_tokens": raw(n)}
		}
		if target == openai.FamilyGeminiCount {
			out = Object{"totalTokens": raw(n)}
		}
		if wire == target {
			out = f
		}
		c = &openai.Completion{Body: raw(out), FinishReason: "stop", Usage: &openai.Usage{InputTokens: n, TotalTokens: n}}
	case openai.FamilyEmbeddings:
		c, err = decodeEmbeddings(body, route, encoding)
	case openai.FamilyGeminiEmbeddings:
		c, err = decodeGeminiEmbedding(body, route, encoding)
	case openai.FamilyGeminiEmbeddingsBatch:
		c, err = decodeGeminiEmbeddingBatch(body, route, encoding)
	case openai.FamilyVertexEmbeddings:
		c, err = decodeVertexEmbeddings(body, route, encoding)
	case openai.FamilyBedrockEmbeddings:
		c, err = decodeBedrockEmbedding(body, route, encoding)
	case openai.FamilyRerank:
		c, err = decodeRerank(body, route, request)
	case openai.FamilyModeration:
		f, e := object(body)
		if e != nil {
			return nil, protocolError("invalid moderation result")
		}
		if len(arr(f["results"])) == 0 {
			return nil, protocolError("missing moderation results")
		}
		for _, v := range arr(f["results"]) {
			r, e := object(v)
			if e != nil {
				return nil, protocolError("invalid moderation result")
			}
			var flagged bool
			if json.Unmarshal(r["flagged"], &flagged) != nil {
				return nil, protocolError("missing moderation decision")
			}
			categories, e := object(r["categories"])
			if e != nil {
				return nil, protocolError("missing moderation categories")
			}
			for _, v := range categories {
				var decision bool
				if json.Unmarshal(v, &decision) != nil {
					return nil, protocolError("invalid moderation category")
				}
			}
			scores, e := object(r["category_scores"])
			if e != nil {
				return nil, protocolError("missing moderation scores")
			}
			for _, v := range scores {
				var score float64
				if json.Unmarshal(v, &score) != nil || math.IsInf(score, 0) || math.IsNaN(score) {
					return nil, protocolError("invalid moderation score")
				}
			}
		}
		if _, ok := f["model"]; ok {
			f["model"] = raw(route)
		}
		c = &openai.Completion{Body: raw(f), FinishReason: "stop"}
	case "bedrock":
		c, err = decodeBedrock(body, route)
	default:
		return nil, protocolError("unknown response family")
	}
	if err != nil {
		return c, err
	}
	if wire != target && target.Operation() == "generation" {
		for _, call := range c.ToolCalls {
			if call.ID == "" || call.Name == "" {
				return c, protocolError("incomplete translated tool identity")
			}
			if _, err := object([]byte(call.Arguments)); err != nil {
				return c, protocolError("translated tool arguments must be a JSON object")
			}
		}
		if supportsCandidates(wire) {
			translated, e := translateCandidates(wire, target, body, route)
			if e != nil {
				return c, e
			}
			if translated != nil {
				c.Body = translated
				return c, nil
			}
		}
		c.Body = renderCompletion(c, target, route)
	}
	return c, nil
}
func count(v json.RawMessage) (int64, bool) {
	if !present(v) {
		return 0, false
	}
	n, e := strconv.ParseInt(string(v), 10, 64)
	return n, e == nil && n >= 0
}
func nativeUsage(f Object, family string) (*openai.Usage, error) {
	if f == nil {
		return nil, nil
	}
	in, out, total, cached, reason := "input_tokens", "output_tokens", "", "cache_read_input_tokens", ""
	if family == "gemini" {
		in, out, total, cached, reason = "promptTokenCount", "candidatesTokenCount", "totalTokenCount", "cachedContentTokenCount", "thoughtsTokenCount"
	}
	for _, k := range []string{in, out, total, cached, reason, "cache_creation_input_tokens"} {
		if k != "" && present(f[k]) {
			if _, ok := count(f[k]); !ok {
				return nil, protocolError("invalid usage count")
			}
		}
	}
	i, hasIn := count(f[in])
	o, hasOut := count(f[out])
	if !hasIn || !hasOut {
		return nil, nil
	}
	if i > math.MaxInt64-o {
		return nil, protocolError("usage count overflow")
	}
	u := &openai.Usage{InputTokens: i, OutputTokens: o, TotalTokens: i + o}
	if n, ok := count(f[cached]); ok {
		u.CachedInputTokens = &n
		if family == "anthropic" {
			if n > math.MaxInt64-u.InputTokens-u.OutputTokens {
				return nil, protocolError("usage count overflow")
			}
			u.InputTokens += n
		}
	}
	if family == "anthropic" {
		n, hasCreation := count(f["cache_creation_input_tokens"])
		if hasCreation {
			u.CacheWriteInputTokens = &n
		}
		if n > math.MaxInt64-u.InputTokens-u.OutputTokens {
			return nil, protocolError("usage count overflow")
		}
		u.InputTokens += n
		creation, e := optionalObject(f["cache_creation"])
		if e != nil {
			return nil, e
		}
		if creation != nil {
			detail := int64(0)
			for field, slot := range map[string]**int64{
				"ephemeral_5m_input_tokens": &u.CacheWrite5MInputTokens,
				"ephemeral_1h_input_tokens": &u.CacheWrite1HInputTokens,
			} {
				if v, ok := count(creation[field]); ok {
					*slot = &v
					detail += v
				} else if present(creation[field]) {
					return nil, protocolError("invalid usage count")
				}
			}
			if detail > n {
				return nil, protocolError("cache write detail exceeds total")
			}
		}
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if n, ok := count(f[reason]); ok {
		u.ReasoningTokens = &n
		if n > math.MaxInt64-u.InputTokens-u.OutputTokens {
			return nil, protocolError("usage count overflow")
		}
		u.OutputTokens += n
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if n, ok := count(f[total]); ok {
		u.TotalTokens = n
	}
	return u, nil
}
func decodeAnthropic(body []byte, route string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("message is not an object")
	}
	if str(f["type"]) == "error" {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	if str(f["type"]) != "message" || str(f["role"]) != "assistant" {
		return nil, protocolError("invalid Anthropic message")
	}
	reason := str(f["stop_reason"])
	if reason == "" {
		return nil, protocolError("message has no stop reason")
	}
	c := &openai.Completion{UpstreamID: str(f["id"]), ProviderModel: str(f["model"]), FinishReason: anthropicFinish(reason)}
	if err := anthropicContent(c, f["content"]); err != nil {
		return nil, err
	}
	usage, e := optionalObject(f["usage"])
	if e != nil {
		return nil, e
	}
	c.Usage, e = nativeUsage(usage, "anthropic")
	if e != nil {
		return nil, e
	}
	f["model"] = raw(route)
	c.Body = raw(f)
	return c, nil
}
func optionalObject(v json.RawMessage) (Object, error) {
	if !present(v) {
		return nil, nil
	}
	f, e := object(v)
	if e != nil {
		return nil, protocolError("expected response object")
	}
	return f, nil
}
func anthropicContent(c *openai.Completion, v json.RawMessage) error {
	parts := arr(v)
	if parts == nil {
		return protocolError("message content is not an array")
	}
	for _, v := range parts {
		p, e := object(v)
		if e != nil {
			return protocolError("invalid message content")
		}
		switch str(p["type"]) {
		case "text":
			var text string
			if json.Unmarshal(p["text"], &text) != nil {
				return protocolError("invalid text block")
			}
			c.OutputText += text
		case "tool_use":
			if str(p["id"]) == "" || str(p["name"]) == "" {
				return protocolError("incomplete tool call")
			}
			if _, e := object(p["input"]); e != nil {
				return protocolError("invalid tool input")
			}
			c.ToolCalls = append(c.ToolCalls, openai.ToolCall{ID: str(p["id"]), Name: str(p["name"]), Arguments: string(p["input"])})
		}
	}
	return nil
}
func anthropicFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}
func decodeGemini(body []byte, route string, partial bool) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("generation is not an object")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	candidates := arr(f["candidates"])
	if len(candidates) == 0 && !present(f["promptFeedback"]) && !partial {
		return nil, protocolError("generation has no candidates")
	}
	c := &openai.Completion{UpstreamID: str(f["responseId"]), ProviderModel: str(f["modelVersion"])}
	seen := map[int64]bool{}
	for i, v := range candidates {
		candidate, e := object(v)
		if e != nil {
			return nil, protocolError("invalid candidate")
		}
		idx, ok := count(candidate["index"])
		if !ok {
			if present(candidate["index"]) {
				return nil, protocolError("invalid candidate index")
			}
			idx = int64(i)
		}
		if seen[idx] {
			return nil, protocolError("duplicate candidate index")
		}
		seen[idx] = true
		reason := str(candidate["finishReason"])
		if !partial && reason == "" {
			return nil, protocolError("candidate has no finish reason")
		}
		first := c
		if i > 0 {
			first = &openai.Completion{}
		}
		c := first
		content, e := optionalObject(candidate["content"])
		if e != nil {
			return nil, e
		}
		for j, v := range arr(content["parts"]) {
			p, e := object(v)
			if e != nil {
				return nil, protocolError("invalid candidate part")
			}
			if present(p["text"]) {
				var text string
				if json.Unmarshal(p["text"], &text) != nil {
					return nil, protocolError("invalid candidate text")
				}
				var thought bool
				_ = json.Unmarshal(p["thought"], &thought)
				if !thought {
					c.OutputText += text
				}
			}
			if present(p["functionCall"]) {
				call, e := object(p["functionCall"])
				if e != nil {
					return nil, protocolError("invalid function call")
				}
				id := str(call["id"])
				if id == "" {
					id = "call_" + strconv.Itoa(j)
				}
				if str(call["name"]) == "" {
					return nil, protocolError("function call has no name")
				}
				if _, e := object(call["args"]); e != nil {
					return nil, protocolError("invalid function arguments")
				}
				c.ToolCalls = append(c.ToolCalls, openai.ToolCall{ID: id, Name: str(call["name"]), Arguments: string(call["args"])})
			}
		}
		if reason != "" {
			c.FinishReason = "stop"
			switch reason {
			case "MAX_TOKENS":
				c.FinishReason = "length"
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
				c.FinishReason = "content_filter"
			}
			if c.FinishReason == "stop" && len(c.ToolCalls) > 0 {
				c.FinishReason = "tool_calls"
			}
		}
	}
	if len(candidates) == 0 && present(f["promptFeedback"]) {
		c.FinishReason = "content_filter"
	}
	usage, e := optionalObject(f["usageMetadata"])
	if e != nil {
		return nil, e
	}
	c.Usage, e = nativeUsage(usage, "gemini")
	if e != nil {
		return nil, e
	}
	if _, ok := f["modelVersion"]; ok {
		f["modelVersion"] = raw(route)
	}
	c.Body = raw(f)
	return c, nil
}
func renderCompletion(c *openai.Completion, family openai.Family, route string) []byte {
	id := c.UpstreamID
	if id == "" {
		id = "olp_completion"
	}
	switch family {
	case openai.FamilyChat:
		msg := map[string]any{"role": "assistant", "content": c.OutputText}
		if c.Refusal != "" {
			msg["refusal"] = c.Refusal
		}
		if len(c.ToolCalls) > 0 {
			calls := []any{}
			for _, t := range c.ToolCalls {
				calls = append(calls, map[string]any{"id": t.ID, "type": "function", "function": map[string]any{"name": t.Name, "arguments": t.Arguments}})
			}
			msg["tool_calls"] = calls
		}
		f := map[string]any{"id": id, "object": "chat.completion", "created": 0, "model": route, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": c.FinishReason}}}
		if c.Usage != nil {
			f["usage"] = chatUsage(c.Usage)
		}
		return raw(f)
	case openai.FamilyResponses:
		output := []any{}
		content := []any{}
		if c.OutputText != "" {
			content = append(content, map[string]any{"type": "output_text", "text": c.OutputText, "annotations": []any{}})
		}
		if c.Refusal != "" {
			content = append(content, map[string]any{"type": "refusal", "refusal": c.Refusal})
		}
		if len(content) > 0 {
			output = append(output, map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "status": "completed", "content": content})
		}
		for _, t := range c.ToolCalls {
			output = append(output, map[string]any{"id": "fc_" + t.ID, "type": "function_call", "status": "completed", "call_id": t.ID, "name": t.Name, "arguments": t.Arguments})
		}
		f := map[string]any{"id": id, "object": "response", "created_at": 0, "status": "completed", "model": route, "output": output, "error": nil, "incomplete_details": nil}
		if c.FinishReason == "length" || c.FinishReason == "content_filter" {
			f["status"] = "incomplete"
			reason := "max_output_tokens"
			if c.FinishReason == "content_filter" {
				reason = "content_filter"
			}
			f["incomplete_details"] = map[string]string{"reason": reason}
		}
		if c.Usage != nil {
			f["usage"] = responseUsage(c.Usage)
		}
		return raw(f)
	case openai.FamilyAnthropic:
		content := []any{}
		if text := c.OutputText + c.Refusal; text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, t := range c.ToolCalls {
			content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": json.RawMessage(t.Arguments)})
		}
		reason := "end_turn"
		switch c.FinishReason {
		case "tool_calls":
			reason = "tool_use"
		case "length":
			reason = "max_tokens"
		case "content_filter":
			reason = "refusal"
		}
		f := map[string]any{"id": id, "type": "message", "role": "assistant", "model": route, "content": content, "stop_reason": reason, "stop_sequence": nil}
		if c.Usage != nil {
			f["usage"] = map[string]any{"input_tokens": c.Usage.InputTokens, "output_tokens": c.Usage.OutputTokens}
		}
		return raw(f)
	default:
		parts := []any{}
		if text := c.OutputText + c.Refusal; text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, t := range c.ToolCalls {
			parts = append(parts, map[string]any{"functionCall": map[string]any{"id": t.ID, "name": t.Name, "args": json.RawMessage(t.Arguments)}})
		}
		reason := "STOP"
		switch c.FinishReason {
		case "length":
			reason = "MAX_TOKENS"
		case "content_filter":
			reason = "SAFETY"
		}
		f := map[string]any{"responseId": id, "modelVersion": route, "candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": reason}}}
		if c.Usage != nil {
			f["usageMetadata"] = geminiUsage(c.Usage)
		}
		return raw(f)
	}
}
func chatUsage(u *openai.Usage) Object {
	f := Object{"prompt_tokens": raw(u.InputTokens), "completion_tokens": raw(u.OutputTokens), "total_tokens": raw(u.TotalTokens)}
	if u.CachedInputTokens != nil {
		f["prompt_tokens_details"] = raw(map[string]any{"cached_tokens": *u.CachedInputTokens})
	}
	if u.ReasoningTokens != nil {
		f["completion_tokens_details"] = raw(map[string]any{"reasoning_tokens": *u.ReasoningTokens})
	}
	return f
}
func responseUsage(u *openai.Usage) Object {
	f := Object{"input_tokens": raw(u.InputTokens), "output_tokens": raw(u.OutputTokens), "total_tokens": raw(u.TotalTokens)}
	if u.CachedInputTokens != nil {
		f["input_tokens_details"] = raw(map[string]any{"cached_tokens": *u.CachedInputTokens})
	}
	if u.ReasoningTokens != nil {
		f["output_tokens_details"] = raw(map[string]any{"reasoning_tokens": *u.ReasoningTokens})
	}
	return f
}
func geminiUsage(u *openai.Usage) Object {
	f := Object{"promptTokenCount": raw(u.InputTokens), "candidatesTokenCount": raw(u.OutputTokens), "totalTokenCount": raw(u.TotalTokens)}
	if u.CachedInputTokens != nil {
		f["cachedContentTokenCount"] = raw(*u.CachedInputTokens)
	}
	if u.ReasoningTokens != nil {
		f["thoughtsTokenCount"] = raw(*u.ReasoningTokens)
		f["candidatesTokenCount"] = raw(max(0, u.OutputTokens-*u.ReasoningTokens))
	}
	return f
}

func embeddingItems(data []json.RawMessage, encoding string) ([]json.RawMessage, error) {
	seen := map[int64]bool{}
	for i, v := range data {
		item, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		index, ok := count(item["index"])
		if !ok || seen[index] {
			return nil, protocolError("invalid embedding index")
		}
		seen[index] = true
		var values []float64
		var encoded string
		if json.Unmarshal(item["embedding"], &encoded) == nil {
			b, e := base64.StdEncoding.DecodeString(encoded)
			if e != nil || len(b) == 0 || len(b)%4 != 0 {
				return nil, protocolError("invalid base64 embedding")
			}
			for pos := 0; pos < len(b); pos += 4 {
				v := math.Float32frombits(binary.LittleEndian.Uint32(b[pos:]))
				if math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) {
					return nil, protocolError("non-finite embedding")
				}
				values = append(values, float64(v))
			}
		} else if json.Unmarshal(item["embedding"], &values) != nil || len(values) == 0 {
			return nil, protocolError("invalid embedding vector")
		}
		for _, value := range values {
			if math.IsInf(value, 0) || math.IsNaN(value) {
				return nil, protocolError("non-finite embedding")
			}
		}
		if encoding == "float" {
			item["embedding"] = raw(values)
		}
		if encoding == "base64" && encoded == "" {
			bytes := make([]byte, 4*len(values))
			for i, value := range values {
				converted := float32(value)
				if math.IsInf(float64(converted), 0) {
					return nil, protocolError("embedding exceeds float32 range")
				}
				binary.LittleEndian.PutUint32(bytes[i*4:], math.Float32bits(converted))
			}
			item["embedding"] = raw(base64.StdEncoding.EncodeToString(bytes))
		}
		item["object"] = raw("embedding")
		data[i] = raw(item)
	}
	return data, nil
}

func nativeEmbeddingsCompletion(values [][]float64, route, encoding string, usage *openai.Usage) (*openai.Completion, error) {
	data := make([]json.RawMessage, len(values))
	for i, vector := range values {
		data[i] = raw(Object{"index": raw(i), "embedding": raw(vector)})
	}
	checked, err := embeddingItems(data, encoding)
	if err != nil {
		return nil, err
	}
	f := Object{"data": raw(checked), "model": raw(route), "object": raw("list")}
	if usage != nil {
		f["usage"] = raw(Object{"prompt_tokens": raw(usage.InputTokens), "total_tokens": raw(usage.TotalTokens)})
	}
	c := &openai.Completion{Body: raw(f), FinishReason: "stop", Usage: usage}
	return c, nil
}

func floatVector(raw json.RawMessage) ([]float64, error) {
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return nil, protocolError("invalid embedding vector")
	}
	return values, nil
}

func decodeGeminiEmbedding(body []byte, route, encoding string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	embedding, e := optionalObject(f["embedding"])
	if e != nil {
		return nil, e
	}
	values, err := floatVector(embedding["values"])
	if err != nil {
		return nil, err
	}
	return nativeEmbeddingsCompletion([][]float64{values}, route, encoding, nil)
}

func decodeGeminiEmbeddingBatch(body []byte, route, encoding string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	embeddings := arr(f["embeddings"])
	if len(embeddings) == 0 {
		return nil, protocolError("missing embeddings")
	}
	values := make([][]float64, len(embeddings))
	for i, v := range embeddings {
		item, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		vector, err := floatVector(item["values"])
		if err != nil {
			return nil, err
		}
		values[i] = vector
	}
	return nativeEmbeddingsCompletion(values, route, encoding, nil)
}

func decodeVertexEmbeddings(body []byte, route, encoding string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["error"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	predictions := arr(f["predictions"])
	if len(predictions) == 0 {
		return nil, protocolError("missing embeddings")
	}
	values := make([][]float64, len(predictions))
	var tokens int64
	var usage *openai.Usage
	for i, v := range predictions {
		prediction, e := object(v)
		if e != nil {
			return nil, protocolError("invalid embedding")
		}
		embedding, e := optionalObject(prediction["embeddings"])
		if e != nil {
			return nil, e
		}
		vector, err := floatVector(embedding["values"])
		if err != nil {
			return nil, err
		}
		values[i] = vector
		if statistics, e := optionalObject(embedding["statistics"]); e != nil {
			return nil, e
		} else if present(statistics["token_count"]) {
			n, ok := count(statistics["token_count"])
			if !ok {
				return nil, protocolError("invalid embeddings usage")
			}
			tokens += n
			usage = &openai.Usage{InputTokens: tokens, TotalTokens: tokens}
		}
	}
	return nativeEmbeddingsCompletion(values, route, encoding, usage)
}

func decodeBedrockEmbedding(body []byte, route, encoding string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	if present(f["message"]) && !present(f["embedding"]) {
		if upstream := openai.ParseErrorBody(body); upstream != nil {
			return nil, upstream
		}
		return nil, protocolError("invalid native error envelope")
	}
	values, err := floatVector(f["embedding"])
	if err != nil {
		return nil, err
	}
	n, ok := count(f["inputTextTokenCount"])
	if !ok {
		return nil, protocolError("invalid embeddings usage")
	}
	usage := &openai.Usage{InputTokens: n, TotalTokens: n}
	return nativeEmbeddingsCompletion([][]float64{values}, route, encoding, usage)
}

func decodeEmbeddings(body []byte, route, encoding string) (*openai.Completion, error) {
	f, e := object(body)
	if e != nil {
		return nil, protocolError("invalid embeddings result")
	}
	data := arr(f["data"])
	if len(data) == 0 {
		return nil, protocolError("missing embeddings")
	}
	data, err := embeddingItems(data, encoding)
	if err != nil {
		return nil, err
	}
	f["data"] = raw(data)
	f["model"] = raw(route)
	f["object"] = raw("list")
	c := &openai.Completion{Body: raw(f), FinishReason: "stop"}
	usage, e := optionalObject(f["usage"])
	if e != nil {
		return nil, e
	}
	if usage != nil {
		n, ok := count(usage["prompt_tokens"])
		if !ok {
			n, ok = count(usage["total_tokens"])
		}
		if !ok {
			return nil, protocolError("invalid embeddings usage")
		}
		c.Usage = &openai.Usage{InputTokens: n, TotalTokens: n}
	}
	return c, nil
}

// Redact removes credential values from any upstream-owned diagnostic text.
func Redact(message string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}
