package protocols

import (
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type streamCall struct {
	id, name  string
	arguments strings.Builder
}

type streamTranslator struct {
	source, target            wire.Format
	route                     string
	limit                     int
	includeUsage              bool
	emit                      wire.Emit
	started                   bool
	id                        string
	text                      strings.Builder
	calls                     map[int]*streamCall
	responseTexts             map[int]*strings.Builder
	responseRefusals          map[int]*strings.Builder
	blocks                    map[int]int
	textBlock                 *int
	refusalBlock              *int
	refusal                   strings.Builder
	refused                   bool
	sequence, nextBlock, size int
	retained                  *int
}

func responseBuffer(buffers map[int]*strings.Builder, index int) *strings.Builder {
	buffer := buffers[index]
	if buffer == nil {
		buffer = &strings.Builder{}
		buffers[index] = buffer
	}
	return buffer
}

func (t *streamTranslator) send(event string, f Object) error {
	if t.target == wire.Responses {
		f["sequence_number"] = raw(t.sequence)
		t.sequence++
	}
	return t.emit(eventFrame(event, f))
}

func (t *streamTranslator) start(id string) error {
	if t.started {
		return nil
	}
	t.started = true
	t.id = id
	if t.id == "" {
		t.id = "olp_stream"
	}
	switch t.target {
	case wire.Chat:
		return t.chat(Object{"role": raw("assistant")}, nil, nil)
	case wire.Anthropic:
		return t.send("message_start", Object{"type": raw("message_start"), "message": raw(map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.route, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}})})
	case wire.Responses:
		return t.send("response.created", Object{"type": raw("response.created"), "response": raw(map[string]any{"id": t.id, "object": "response", "created_at": 0, "status": "in_progress", "model": t.route, "output": []any{}})})
	}
	return nil
}

func (t *streamTranslator) reserve(n int) error {
	used := &t.size
	if t.retained != nil {
		used = t.retained
	}
	*used += n
	if *used > t.limit {
		return wire.ErrEventTooLarge
	}
	return nil
}

func (t *streamTranslator) textDelta(text string) error {
	if text == "" {
		return nil
	}
	if e := t.start(""); e != nil {
		return e
	}
	if t.source == wire.Responses && t.target != wire.Responses {
		if err := t.reserve(len(text)); err != nil {
			return err
		}
		t.text.WriteString(text)
	}
	switch t.target {
	case wire.Chat:
		return t.chat(Object{"content": raw(text)}, nil, nil)
	case wire.Anthropic:
		if t.textBlock == nil {
			index := t.nextBlock
			t.nextBlock++
			t.textBlock = &index
			if e := t.send("content_block_start", Object{"type": raw("content_block_start"), "index": raw(index), "content_block": raw(map[string]string{"type": "text", "text": ""})}); e != nil {
				return e
			}
		}
		return t.send("content_block_delta", Object{"type": raw("content_block_delta"), "index": raw(*t.textBlock), "delta": raw(map[string]string{"type": "text_delta", "text": text})})
	case wire.Responses:
		if e := t.reserve(len(text)); e != nil {
			return e
		}
		if t.textBlock == nil {
			index := t.nextBlock
			t.nextBlock++
			t.textBlock = &index
			if e := t.send("response.output_item.added", Object{"type": raw("response.output_item.added"), "output_index": raw(index), "item": raw(map[string]any{"id": "msg_" + t.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}})}); e != nil {
				return e
			}
			if e := t.send("response.content_part.added", Object{"type": raw("response.content_part.added"), "item_id": raw("msg_" + t.id), "output_index": raw(index), "content_index": raw(0), "part": raw(map[string]any{"type": "output_text", "text": "", "annotations": []any{}})}); e != nil {
				return e
			}
		}
		t.text.WriteString(text)
		return t.send("response.output_text.delta", Object{"type": raw("response.output_text.delta"), "item_id": raw("msg_" + t.id), "output_index": raw(*t.textBlock), "content_index": raw(0), "delta": raw(text)})
	default:
		return t.send("", Object{"responseId": raw(t.id), "modelVersion": raw(t.route), "candidates": raw([]any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": text}}}}})})
	}
}

func (t *streamTranslator) toolDelta(index int, id, name, args string) error {
	if e := t.start(""); e != nil {
		return e
	}
	call := t.calls[index]
	if call == nil {
		if e := t.reserve(64); e != nil {
			return e
		}
		call = &streamCall{}
		t.calls[index] = call
	}
	if id != "" {
		call.id += id
	}
	if name != "" {
		call.name += name
	}
	if e := t.reserve(len(args) + len(id) + len(name)); e != nil {
		return e
	}
	call.arguments.WriteString(args)
	if t.target == wire.Chat {
		fun := Object{}
		if name != "" {
			fun["name"] = raw(name)
		}
		if args != "" {
			fun["arguments"] = raw(args)
		}
		position, ok := t.blocks[index]
		if !ok {
			position = len(t.blocks)
			t.blocks[index] = position
		}
		v := Object{"index": raw(position), "function": raw(fun)}
		if id != "" {
			v["id"] = raw(id)
			v["type"] = raw("function")
		}
		return t.chat(Object{"tool_calls": raw([]Object{v})}, nil, nil)
	}
	if call.id == "" || call.name == "" || call.arguments.Len() == 0 {
		return nil
	}
	block, opened := t.blocks[index]
	partial := args
	if !opened {
		block = t.nextBlock
		t.nextBlock++
		t.blocks[index] = block
		switch t.target {
		case wire.Anthropic:
			if e := t.send("content_block_start", Object{"type": raw("content_block_start"), "index": raw(block), "content_block": raw(map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": map[string]any{}})}); e != nil {
				return e
			}
		case wire.Responses:
			if e := t.send("response.output_item.added", Object{"type": raw("response.output_item.added"), "output_index": raw(block), "item": raw(map[string]any{"type": "function_call", "id": "fc_" + call.id, "call_id": call.id, "name": call.name, "arguments": "", "status": "in_progress"})}); e != nil {
				return e
			}
		}
		partial = call.arguments.String()
	}
	if partial == "" {
		return nil
	}
	switch t.target {
	case wire.Anthropic:
		return t.send("content_block_delta", Object{"type": raw("content_block_delta"), "index": raw(block), "delta": raw(map[string]string{"type": "input_json_delta", "partial_json": partial})})
	case wire.Responses:
		return t.send("response.function_call_arguments.delta", Object{"type": raw("response.function_call_arguments.delta"), "item_id": raw("fc_" + call.id), "output_index": raw(block), "delta": raw(partial)})
	}
	return nil
}

func (t *streamTranslator) chat(delta Object, finish *string, usage *wire.Usage) error {
	choices := []Object{}
	if delta != nil || finish != nil {
		choices = append(choices, Object{"index": raw(0), "delta": raw(delta), "finish_reason": raw(finish)})
	}
	f := Object{"id": raw(t.id), "object": raw("chat.completion.chunk"), "created": raw(0), "model": raw(t.route), "choices": raw(choices)}
	if usage != nil {
		f["usage"] = raw(chatUsage(usage))
	}
	return t.send("", f)
}

func (t *streamTranslator) responseToolSnapshot(index int, item Object) error {
	if str(item["type"]) != "function_call" {
		return nil
	}
	id, name, arguments := str(item["call_id"]), str(item["name"]), str(item["arguments"])
	if id == "" || name == "" {
		return protocolError("incomplete translated tool identity")
	}
	if _, err := object([]byte(arguments)); err != nil {
		return protocolError("incomplete translated tool arguments")
	}
	call := t.calls[index]
	if call == nil {
		return t.toolDelta(index, id, name, arguments)
	}
	if call.id != id || call.name != name || !strings.HasPrefix(arguments, call.arguments.String()) {
		return protocolError("translated tool snapshot disagrees with deltas")
	}
	return t.toolDelta(index, "", "", strings.TrimPrefix(arguments, call.arguments.String()))
}

func (t *streamTranslator) responseTextDelta(index int, text string) error {
	responseBuffer(t.responseTexts, index).WriteString(text)
	return t.textDelta(text)
}

func (t *streamTranslator) responseRefusalDelta(index int, text string) error {
	responseBuffer(t.responseRefusals, index).WriteString(text)
	return t.refusalDelta(text)
}

func (t *streamTranslator) responseMessageSnapshot(index int, item Object) error {
	if str(item["type"]) != "message" {
		return nil
	}
	content := func(kind, field string) string {
		return strings.Join(slices.Collect(func(yield func(string) bool) {
			for _, content := range arr(item["content"]) {
				part, _ := object(content)
				if str(part["type"]) == kind && !yield(str(part[field])) {
					return
				}
			}
		}), "")
	}
	text, refusal := content("output_text", "text"), content("refusal", "refusal")
	texts, refusals := responseBuffer(t.responseTexts, index), responseBuffer(t.responseRefusals, index)
	if !strings.HasPrefix(text, texts.String()) || !strings.HasPrefix(refusal, refusals.String()) {
		return protocolError("Responses message snapshot disagrees with streamed deltas")
	}
	if err := t.responseTextDelta(index, text[texts.Len():]); err != nil {
		return err
	}
	return t.responseRefusalDelta(index, refusal[refusals.Len():])
}

func (t *streamTranslator) summary(c *wire.Completion) (*wire.Completion, error) {
	summary := *c
	summary.UpstreamID = t.id
	summary.OutputText = t.text.String()
	summary.Refusal = t.refusal.String()
	if t.refused && summary.FinishReason == "stop" && t.target.Surface() != "openai" {
		summary.FinishReason = "content_filter"
	}
	summary.ToolCalls = nil
	for _, i := range slices.Sorted(maps.Keys(t.calls)) {
		call := t.calls[i]
		if call.id == "" || call.name == "" {
			return nil, protocolError("incomplete translated tool call")
		}
		if call.arguments.Len() == 0 {
			if err := t.toolDelta(i, "", "", "{}"); err != nil {
				return nil, err
			}
		}
		if _, e := object([]byte(call.arguments.String())); e != nil {
			return nil, protocolError("incomplete translated tool arguments")
		}
		summary.ToolCalls = append(summary.ToolCalls, wire.ToolCall{ID: call.id, Name: call.name, Arguments: call.arguments.String()})
	}
	if len(summary.ToolCalls) > 0 && summary.FinishReason == "stop" {
		summary.FinishReason = "tool_calls"
	}
	return &summary, nil
}

func (t *streamTranslator) finish(c *wire.Completion) error {
	if e := t.start(c.UpstreamID); e != nil {
		return e
	}
	summary, err := t.summary(c)
	if err != nil {
		return err
	}
	switch t.target {
	case wire.Chat:
		if e := t.chat(Object{}, &summary.FinishReason, nil); e != nil {
			return e
		}
		if t.includeUsage && summary.Usage != nil {
			if e := t.chat(nil, nil, summary.Usage); e != nil {
				return e
			}
		}
		return t.emit([]byte("data: [DONE]\n\n"))
	case wire.Anthropic:
		for i := 0; i < t.nextBlock; i++ {
			if e := t.send("content_block_stop", Object{"type": raw("content_block_stop"), "index": raw(i)}); e != nil {
				return e
			}
		}
		reason := "end_turn"
		switch summary.FinishReason {
		case "tool_calls":
			reason = "tool_use"
		case "length":
			reason = "max_tokens"
		case "content_filter":
			reason = "refusal"
		}
		f := Object{"type": raw("message_delta"), "delta": raw(map[string]any{"stop_reason": reason, "stop_sequence": nil})}
		if summary.Usage != nil {
			f["usage"] = raw(map[string]int64{"input_tokens": summary.Usage.InputTokens, "output_tokens": summary.Usage.OutputTokens})
		}
		if e := t.send("message_delta", f); e != nil {
			return e
		}
		return t.send("message_stop", Object{"type": raw("message_stop")})
	case wire.Responses:
		output := make([]Object, t.nextBlock)
		if t.textBlock != nil {
			index := *t.textBlock
			part := Object{"type": raw("output_text"), "text": raw(t.text.String()), "annotations": raw([]any{})}
			if e := t.send("response.output_text.done", Object{"type": raw("response.output_text.done"), "item_id": raw("msg_" + t.id), "output_index": raw(index), "content_index": raw(0), "text": raw(t.text.String())}); e != nil {
				return e
			}
			if e := t.send("response.content_part.done", Object{"type": raw("response.content_part.done"), "item_id": raw("msg_" + t.id), "output_index": raw(index), "content_index": raw(0), "part": raw(part)}); e != nil {
				return e
			}
			output[index] = Object{"id": raw("msg_" + t.id), "type": raw("message"), "role": raw("assistant"), "status": raw("completed"), "content": raw([]Object{part})}
		}
		for _, i := range slices.Sorted(maps.Keys(t.calls)) {
			call := t.calls[i]
			index := t.blocks[i]
			if e := t.send("response.function_call_arguments.done", Object{"type": raw("response.function_call_arguments.done"), "item_id": raw("fc_" + call.id), "output_index": raw(index), "arguments": raw(call.arguments.String())}); e != nil {
				return e
			}
			output[index] = Object{"id": raw("fc_" + call.id), "type": raw("function_call"), "status": raw("completed"), "call_id": raw(call.id), "name": raw(call.name), "arguments": raw(call.arguments.String())}
		}
		if e := t.finishRefusal(output); e != nil {
			return e
		}
		for i, item := range output {
			if e := t.send("response.output_item.done", Object{"type": raw("response.output_item.done"), "output_index": raw(i), "item": raw(item)}); e != nil {
				return e
			}
		}
		f, _ := object(renderCompletion(summary, t.target, t.route))
		f["output"] = raw(output)
		kind := "response." + str(f["status"])
		return t.send(kind, Object{"type": raw(kind), "response": raw(f)})
	default:
		summary.OutputText = ""
		body := renderCompletion(summary, t.target, t.route)
		f, _ := object(body)
		return t.send("", f)
	}
}
