package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type WorkflowResult struct {
	CallID  string
	Content string
}

func AddChatWorkflowUsage(total, turn *wire.Usage) *wire.Usage {
	if total == nil || turn == nil ||
		turn.InputTokens > math.MaxInt64-total.InputTokens ||
		turn.OutputTokens > math.MaxInt64-total.OutputTokens ||
		turn.TotalTokens > math.MaxInt64-total.TotalTokens {
		return nil
	}
	cached, cachedOK := addReportedTokens(total.CachedInputTokens, turn.CachedInputTokens)
	cacheWrite, cacheWriteOK := addReportedTokens(total.CacheWriteInputTokens, turn.CacheWriteInputTokens)
	audioIn, audioInOK := addReportedTokens(total.AudioInputTokens, turn.AudioInputTokens)
	audioOut, audioOutOK := addReportedTokens(total.AudioOutputTokens, turn.AudioOutputTokens)
	reasoning, reasoningOK := addReportedTokens(total.ReasoningTokens, turn.ReasoningTokens)
	if !cachedOK || !cacheWriteOK || !audioInOK || !audioOutOK || !reasoningOK {
		return nil
	}
	return &wire.Usage{
		InputTokens:           total.InputTokens + turn.InputTokens,
		OutputTokens:          total.OutputTokens + turn.OutputTokens,
		TotalTokens:           total.TotalTokens + turn.TotalTokens,
		CachedInputTokens:     cached,
		CacheWriteInputTokens: cacheWrite,
		AudioInputTokens:      audioIn,
		AudioOutputTokens:     audioOut,
		ReasoningTokens:       reasoning,
	}
}

func addReportedTokens(total, turn *int64) (*int64, bool) {
	switch {
	case total == nil:
		return turn, true
	case turn == nil:
		return total, true
	case *turn > math.MaxInt64-*total:
		return nil, false
	default:
		return new(*total + *turn), true
	}
}

func ProjectChatWorkflowUsage(body []byte, usage *wire.Usage, maxBytes int) ([]byte, error) {
	if usage == nil {
		return body, nil
	}
	fields, err := object(body)
	if err != nil {
		return nil, err
	}
	if !present(fields["usage"]) {
		return body, nil
	}
	return replaceChatWorkflowUsage(fields, chatUsage(usage), maxBytes)
}

func ProjectChatWorkflowStreamUsage(frames []byte, usage *wire.Usage, maxEvent, maxBytes int) ([]byte, error) {
	if usage == nil {
		return frames, nil
	}
	replacement := chatUsage(usage)
	decoder := sse.NewRawDecoder(bytes.NewReader(frames), maxEvent)
	var out bytes.Buffer
	for {
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		projected, err := projectChatWorkflowFrame(frame, replacement, maxEvent)
		if err != nil {
			return nil, err
		}
		if len(projected) > maxBytes-out.Len() {
			return nil, wire.ErrAggregateTooLarge
		}
		out.Write(projected)
	}
}

func projectChatWorkflowFrame(frame sse.RawFrame, usage Object, maxEvent int) ([]byte, error) {
	if !frame.HasData || bytes.Equal(frame.Data, []byte("[DONE]")) {
		return frame.Bytes, nil
	}
	fields, err := object(frame.Data)
	if err != nil {
		return nil, err
	}
	if !present(fields["usage"]) {
		return frame.Bytes, nil
	}
	body, err := replaceChatWorkflowUsage(fields, usage, maxEvent)
	if err != nil {
		return nil, err
	}
	projected, err := frame.ReplaceData(0, len(frame.Data), body)
	if len(projected) > maxEvent {
		return nil, wire.ErrEventTooLarge
	}
	return projected, err
}

func replaceChatWorkflowUsage(fields, usage Object, maxBytes int) ([]byte, error) {
	replacement, err := mergeChatWorkflowUsage(fields["usage"], usage)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(overlayChatWorkflowFields(fields, Object{"usage": raw(replacement)}))
	if len(body) > maxBytes {
		return nil, wire.ErrAggregateTooLarge
	}
	return body, err
}

func mergeChatWorkflowUsage(final json.RawMessage, usage Object) (Object, error) {
	fields, err := object(final)
	if err != nil {
		return nil, err
	}
	updates := maps.Collect(func(yield func(string, json.RawMessage) bool) {
		for key, value := range usage {
			if !yield(key, mergeChatWorkflowDetail(key, fields[key], value)) {
				return
			}
		}
	})
	return overlayChatWorkflowFields(fields, updates), nil
}

func mergeChatWorkflowDetail(key string, final, projected json.RawMessage) json.RawMessage {
	if key != "prompt_tokens_details" && key != "completion_tokens_details" || !present(final) {
		return projected
	}
	details, err := object(final)
	if err != nil {
		return projected
	}
	counters, _ := object(projected)
	return raw(overlayChatWorkflowFields(details, counters))
}

func overlayChatWorkflowFields(base, updates Object) Object {
	return maps.Collect(func(yield func(string, json.RawMessage) bool) {
		for key, value := range base {
			if _, replaced := updates[key]; !replaced && !yield(key, value) {
				return
			}
		}
		maps.All(updates)(yield)
	})
}

func WorkflowSingleCandidate(body []byte) bool {
	fields, err := object(body)
	return err == nil && len(arr(fields["candidates"])) <= 1
}

func WorkflowStreamBody(family wire.Format, frames io.Reader, maxEvent int) (json.RawMessage, error) {
	switch family.Surface() {
	case "anthropic":
		return workflowAnthropicStreamBody(frames, maxEvent)
	case "gemini":
		return workflowGeminiStreamBody(frames, maxEvent)
	default:
		return nil, unsupported("workflow stream dialect")
	}
}

// SSE callbacks accumulate bounded deltas without repeatedly copying earlier events.
func workflowGeminiStreamBody(frames io.Reader, maxEvent int) (json.RawMessage, error) {
	parts := []json.RawMessage{}
	err := sse.Decode(frames, maxEvent, func(frame sse.Frame) error {
		fields, err := object([]byte(frame.Data))
		if err != nil {
			return err
		}
		candidates := arr(fields["candidates"])
		if len(candidates) > 1 {
			return unsupported("workflow candidates")
		}
		if len(candidates) == 0 {
			return nil
		}
		candidate, err := object(candidates[0])
		if err != nil {
			return err
		}
		content, err := optionalObject(candidate["content"])
		if err != nil {
			return err
		}
		parts = append(parts, arr(content["parts"])...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return raw(Object{"candidates": raw([]Object{{"content": raw(Object{"role": raw("model"), "parts": raw(parts)})}})}), nil
}

type workflowAnthropicBlock struct {
	fields    Object
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	input     strings.Builder
	citations []json.RawMessage
}

// Anthropic sends indexed block deltas, which must be accumulated to preserve signed content.
func workflowAnthropicStreamBody(frames io.Reader, maxEvent int) (json.RawMessage, error) {
	blocks := []*workflowAnthropicBlock{}
	err := sse.Decode(frames, maxEvent, func(frame sse.Frame) error {
		event, err := object([]byte(frame.Data))
		if err != nil {
			return err
		}
		switch str(event["type"]) {
		case "content_block_start":
			fields, err := object(event["content_block"])
			if err != nil {
				return err
			}
			block := &workflowAnthropicBlock{fields: fields, citations: arr(fields["citations"])}
			block.text.WriteString(str(fields["text"]))
			block.thinking.WriteString(str(fields["thinking"]))
			block.signature.WriteString(str(fields["signature"]))
			blocks = append(blocks, block)
		case "content_block_delta":
			index, valid := count(event["index"])
			if !valid || index < 0 || index >= int64(len(blocks)) {
				return unsupported("workflow stream block")
			}
			delta, err := object(event["delta"])
			if err != nil {
				return err
			}
			block := blocks[index]
			switch str(delta["type"]) {
			case "text_delta":
				block.text.WriteString(str(delta["text"]))
			case "thinking_delta":
				block.thinking.WriteString(str(delta["thinking"]))
			case "signature_delta":
				block.signature.WriteString(str(delta["signature"]))
			case "input_json_delta":
				block.input.WriteString(str(delta["partial_json"]))
			case "citations_delta":
				block.citations = append(block.citations, delta["citation"])
			default:
				return unsupported("workflow stream delta")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	content := make([]Object, len(blocks))
	for i, block := range blocks {
		if present(block.fields["text"]) {
			block.fields["text"] = raw(block.text.String())
		}
		if present(block.fields["thinking"]) {
			block.fields["thinking"] = raw(block.thinking.String())
		}
		if present(block.fields["signature"]) || block.signature.Len() > 0 {
			block.fields["signature"] = raw(block.signature.String())
		}
		if block.input.Len() > 0 {
			if _, err := object([]byte(block.input.String())); err != nil {
				return nil, err
			}
			block.fields["input"] = json.RawMessage(block.input.String())
		}
		if block.citations != nil {
			block.fields["citations"] = raw(block.citations)
		}
		content[i] = block.fields
	}
	return raw(Object{"content": raw(content)}), nil
}

func WorkflowDefinitions(family wire.Format, tools []Tool) (json.RawMessage, error) {
	generation := &Generation{Tools: tools, Parameters: Object{}}
	fields, err := workflowEncoding(family, generation)
	if err != nil {
		return nil, err
	}
	return fields["tools"], nil
}

func WorkflowTurns(family wire.Format, completion *wire.Completion, results []WorkflowResult) ([]json.RawMessage, error) {
	if family == wire.Responses {
		fields, err := object(completion.Body)
		if err != nil || len(arr(fields["output"])) == 0 {
			return nil, unsupported("workflow turns")
		}
		turns := slices.Collect(func(yield func(json.RawMessage) bool) {
			for _, result := range results {
				if !yield(raw(Object{"type": raw("function_call_output"), "call_id": raw(result.CallID), "output": raw(result.Content)})) {
					return
				}
			}
		})
		return slices.Concat(arr(fields["output"]), turns), nil
	}
	if family.Surface() == "gemini" && len(completion.Body) > 0 {
		return workflowGeminiTurns(completion, results)
	}
	messages := slices.Collect(func(yield func(Message) bool) {
		if !yield(Message{Role: "assistant", Parts: []Part{{Text: completion.OutputText}}, Calls: completion.ToolCalls}) {
			return
		}
		for _, result := range results {
			if !yield(Message{Role: "tool", ToolID: result.CallID, Parts: []Part{{Text: result.Content}}}) {
				return
			}
		}
	})
	fields, err := workflowEncoding(family, &Generation{Messages: messages, Parameters: Object{}})
	if err != nil {
		return nil, err
	}
	field := family.WorkflowTranscriptField()
	var turns []json.RawMessage
	if err := json.Unmarshal(fields[field], &turns); err != nil || len(turns) == 0 {
		return nil, unsupported("workflow turns")
	}
	if len(completion.Body) == 0 {
		return turns, nil
	}
	assistant, err := workflowAssistant(family, completion.Body)
	if err != nil {
		return nil, err
	}
	return slices.Concat([]json.RawMessage{assistant}, turns[1:]), nil
}

func workflowGeminiTurns(completion *wire.Completion, results []WorkflowResult) ([]json.RawMessage, error) {
	assistant, err := workflowAssistant(wire.Gemini, completion.Body)
	if err != nil {
		return nil, err
	}
	content, err := object(assistant)
	if err != nil || len(completion.ToolCalls) != len(results) {
		return nil, unsupported("workflow Gemini tool results")
	}
	responses := make([]Object, 0, len(results))
	for _, value := range arr(content["parts"]) {
		part, err := object(value)
		if err != nil {
			return nil, err
		}
		if !present(part["functionCall"]) {
			continue
		}
		callIndex := len(responses)
		if callIndex >= len(results) {
			return nil, unsupported("workflow Gemini tool results")
		}
		call, result := completion.ToolCalls[callIndex], results[callIndex]
		nativeID, name := workflowGeminiID(part["functionCall"])
		if call.ID == "" || result.CallID != call.ID || name == "" || name != call.Name || nativeID != "" && nativeID != call.ID {
			return nil, unsupported("workflow Gemini tool results")
		}
		responses = append(responses, geminiFunctionResponse(name, nativeID, result.Content))
	}
	if len(responses) != len(results) {
		return nil, unsupported("workflow Gemini tool results")
	}
	if len(responses) == 0 {
		return []json.RawMessage{assistant}, nil
	}
	return []json.RawMessage{assistant, raw(Object{"role": raw("user"), "parts": raw(responses)})}, nil
}

func workflowEncoding(family wire.Format, generation *Generation) (Object, error) {
	switch family.Surface() {
	case "anthropic":
		return encodeAnthropicGeneration(generation, "", true)
	case "gemini":
		return encodeGeminiGeneration(generation, "", false)
	default:
		return nil, unsupported("workflow dialect")
	}
}

func workflowAssistant(family wire.Format, body []byte) (json.RawMessage, error) {
	fields, err := object(body)
	if err != nil {
		return nil, err
	}
	switch family.Surface() {
	case "anthropic":
		if !present(fields["content"]) {
			return nil, unsupported("workflow assistant content")
		}
		return raw(Object{"role": raw("assistant"), "content": fields["content"]}), nil
	case "gemini":
		candidates := arr(fields["candidates"])
		if len(candidates) != 1 {
			return nil, unsupported("workflow candidates")
		}
		candidate, err := object(candidates[0])
		if err != nil || !present(candidate["content"]) {
			return nil, unsupported("workflow assistant content")
		}
		return candidate["content"], nil
	default:
		return nil, unsupported("workflow dialect")
	}
}
