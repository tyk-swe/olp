package protocols

import (
	"encoding/json"
	"io"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type WorkflowResult struct {
	CallID  string
	Content string
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
	field := func() string {
		if family.Surface() == "gemini" {
			return "contents"
		}
		return "messages"
	}()
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
