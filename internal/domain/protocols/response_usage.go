package protocols

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func count(v json.RawMessage) (int64, bool) {
	if !present(v) {
		return 0, false
	}
	n, e := strconv.ParseInt(string(v), 10, 64)
	return n, e == nil && n >= 0
}

func position(v json.RawMessage) (int, bool) {
	if !present(v) {
		return 0, false
	}
	n, e := strconv.Atoi(string(v))
	return n, e == nil && n >= 0
}

func nativeUsage(f Object, family string) (*wire.Usage, error) {
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
	if family == "gemini" && hasIn && !present(f[out]) {
		// proto3 JSON omits zero counts: a reply with no visible output (for
		// example, thinking exhausted maxOutputTokens) has no candidatesTokenCount.
		hasOut = true
	}
	if !hasIn || !hasOut {
		return nil, nil
	}
	if i > math.MaxInt64-o {
		return nil, protocolError("usage count overflow")
	}
	u := &wire.Usage{InputTokens: i, OutputTokens: o, TotalTokens: i + o}
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
		if err := applyAnthropicUsageExtras(u, f); err != nil {
			return nil, err
		}
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
	if family == "gemini" {
		if e := geminiUsageDetails(u, f); e != nil {
			return nil, e
		}
	}
	return u, nil
}

func applyAnthropicUsageExtras(u *wire.Usage, f Object) error {
	n, hasCreation := count(f["cache_creation_input_tokens"])
	creation, e := optionalObject(f["cache_creation"])
	if e != nil {
		return e
	}
	if creation != nil {
		detail, hasDetail := int64(0), false
		for field, slot := range map[string]**int64{
			"ephemeral_5m_input_tokens": &u.CacheWrite5MInputTokens,
			"ephemeral_1h_input_tokens": &u.CacheWrite1HInputTokens,
		} {
			if v, ok := count(creation[field]); ok {
				if v > math.MaxInt64-detail {
					return protocolError("usage count overflow")
				}
				*slot = &v
				detail += v
				hasDetail = true
			} else if present(creation[field]) {
				return protocolError("invalid usage count")
			}
		}
		if hasDetail && !hasCreation {
			return protocolError("cache write detail without total")
		}
		if detail > n {
			return protocolError("cache write detail exceeds total")
		}
	}
	if hasCreation {
		u.CacheWriteInputTokens = &n
	}
	if n > math.MaxInt64-u.InputTokens-u.OutputTokens {
		return protocolError("usage count overflow")
	}
	u.InputTokens += n
	u.TotalTokens = u.InputTokens + u.OutputTokens
	tools, e := optionalObject(f["server_tool_use"])
	if e != nil {
		return e
	}
	if tools != nil {
		// Only request-counted tools carry a per-call fee; tools billed
		// by time, such as code execution, have no key here.
		for key, name := range map[string]string{
			"web_search_requests": "web_search",
			"web_fetch_requests":  "web_fetch",
		} {
			if v, ok := count(tools[key]); ok {
				if v > 0 {
					if u.ToolCalls == nil {
						u.ToolCalls = map[string]int64{}
					}
					u.ToolCalls[name] = v
				}
			} else if present(tools[key]) {
				return protocolError("invalid usage count")
			}
		}
	}
	return nil
}

// geminiTokenDetail is one modality bucket in a Gemini usage detail array.
type geminiTokenDetail struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

// geminiDetailCounts folds one detail array into the modality share it
// describes. Negative counts mark the observation inconsistent so the caller
// drops the detail rather than bill audio at a text rate.
func geminiDetailCounts(detail []geminiTokenDetail) (audio, image int64, err error) {
	for _, entry := range detail {
		switch entry.Modality {
		case "AUDIO":
			if entry.TokenCount > math.MaxInt64-audio {
				return 0, 0, protocolError("usage count overflow")
			}
			audio += entry.TokenCount
		case "IMAGE", "VIDEO":
			if entry.TokenCount > math.MaxInt64-image {
				return 0, 0, protocolError("usage count overflow")
			}
			image += entry.TokenCount
		}
	}
	return audio, image, nil
}

// geminiUsageDetails reads the modality detail arrays on a usageMetadata or
// equivalent usage object: prompt shares by modality, cache shares by modality,
// and output shares by modality. Live calls its output detail response.
func geminiUsageDetails(u *wire.Usage, f Object) error {
	details := []struct {
		name         string
		audio, image **int64
	}{
		{"promptTokensDetails", &u.AudioInputTokens, &u.ImageInputTokens},
		{"cacheTokensDetails", &u.CachedAudioInputTokens, &u.CachedImageInputTokens},
		{"candidatesTokensDetails", &u.AudioOutputTokens, &u.ImageOutputTokens},
		{"responseTokensDetails", &u.AudioOutputTokens, &u.ImageOutputTokens},
	}
	for _, slot := range details {
		if !present(f[slot.name]) {
			continue
		}
		var detail []geminiTokenDetail
		if err := json.Unmarshal(f[slot.name], &detail); err != nil {
			return protocolError("invalid usage modality detail")
		}
		for _, entry := range detail {
			if entry.TokenCount < 0 {
				return protocolError("invalid usage count")
			}
		}
		audio, image, countErr := geminiDetailCounts(detail)
		if countErr != nil {
			return countErr
		}
		if audio > 0 {
			*slot.audio = &audio
		}
		if image > 0 {
			*slot.image = &image
		}
	}
	return nil
}

// geminiGrounding counts the search queries grounded candidates made. Both
// keys are emitted together so a price can bill per query or per grounded
// prompt and set the other fee to zero.
func geminiGrounding(u *wire.Usage, f Object) error {
	if u == nil || !present(f["candidates"]) || !bytes.Contains(f["candidates"], []byte("webSearchQueries")) {
		return nil
	}
	var candidates []struct {
		Metadata *struct {
			Queries []json.RawMessage `json:"webSearchQueries"`
		} `json:"groundingMetadata"`
	}
	if err := json.Unmarshal(f["candidates"], &candidates); err != nil {
		return protocolError("invalid candidates")
	}
	queries := int64(0)
	for _, candidate := range candidates {
		if candidate.Metadata != nil {
			queries += int64(len(candidate.Metadata.Queries))
		}
	}
	if queries > 0 {
		if u.ToolCalls == nil {
			u.ToolCalls = map[string]int64{}
		}
		u.ToolCalls["google_search_query"] = queries
		u.ToolCalls["google_search_prompt"] = 1
	}
	return nil
}

func renderCompletion(c *wire.Completion, family wire.Format, route string) []byte {
	id := c.UpstreamID
	if id == "" {
		id = "olp_completion"
	}
	switch family {
	case wire.Chat:
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
	case wire.Responses:
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
	case wire.Anthropic:
		content := []any{}
		if text := c.OutputText + c.Refusal; text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, t := range c.ToolCalls {
			content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": json.RawMessage(t.Arguments)})
		}
		reason := func() string {
			switch c.FinishReason {
			case "tool_calls":
				return "tool_use"
			case "length":
				return "max_tokens"
			case "content_filter":
				return "refusal"
			}
			if c.Refusal != "" {
				return "refusal"
			}
			return "end_turn"
		}()
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

func chatUsage(u *wire.Usage) Object {
	f := Object{"prompt_tokens": raw(u.InputTokens), "completion_tokens": raw(u.OutputTokens), "total_tokens": raw(u.TotalTokens)}
	type details struct {
		Cached     *int64 `json:"cached_tokens,omitempty"`
		CacheWrite *int64 `json:"cache_write_tokens,omitempty"`
		Audio      *int64 `json:"audio_tokens,omitempty"`
		Reasoning  *int64 `json:"reasoning_tokens,omitempty"`
	}
	if u.CachedInputTokens != nil || u.CacheWriteInputTokens != nil || u.AudioInputTokens != nil {
		f["prompt_tokens_details"] = raw(details{Cached: u.CachedInputTokens, CacheWrite: u.CacheWriteInputTokens, Audio: u.AudioInputTokens})
	}
	if u.ReasoningTokens != nil || u.AudioOutputTokens != nil {
		f["completion_tokens_details"] = raw(details{Audio: u.AudioOutputTokens, Reasoning: u.ReasoningTokens})
	}
	return f
}

func responseUsage(u *wire.Usage) Object {
	f := Object{"input_tokens": raw(u.InputTokens), "output_tokens": raw(u.OutputTokens), "total_tokens": raw(u.TotalTokens)}
	if u.CachedInputTokens != nil {
		f["input_tokens_details"] = raw(map[string]any{"cached_tokens": *u.CachedInputTokens})
	}
	if u.ReasoningTokens != nil {
		f["output_tokens_details"] = raw(map[string]any{"reasoning_tokens": *u.ReasoningTokens})
	}
	return f
}

func geminiUsage(u *wire.Usage) Object {
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
