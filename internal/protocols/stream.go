package protocols

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

var streamDone = errors.New("terminal native event")

// Stream keeps the existing executor's backpressure and commitment boundary.
// Native streams retain extensions. Translators retain at most maxEvent bytes
// of text/tool state needed for the destination's terminal envelope.
func Stream(wire, target openai.Family, r io.Reader, maxEvent int, route string, includeUsage bool, emit openai.Emit) (*openai.Completion, error) {
	native := wire == target || wire == openai.FamilyGemini && target == openai.FamilyGeminiStream
	var finish func(*openai.Completion) error
	upstreamEmit := emit
	if !native {
		if supportsCandidates(wire) && supportsCandidates(target) {
			translator := &candidateStream{source: wire, target: target, route: route, limit: maxEvent, includeUsage: includeUsage, emit: emit, children: map[int]*streamTranslator{}, finishes: map[int]string{}}
			upstreamEmit, finish = translator.frame, translator.finish
		} else {
			translator := &streamTranslator{source: wire, target: target, route: route, limit: maxEvent, includeUsage: includeUsage, emit: emit, calls: map[int]*openai.ToolCall{}, blocks: map[int]int{}}
			upstreamEmit, finish = translator.frame, translator.finish
		}
	}

	var c *openai.Completion
	var err error
	switch wire {
	case openai.FamilyChat, openai.FamilyResponses:
		c, err = openai.StreamMetadata(wire, r, maxEvent, route, true, func(frame []byte) error {
			if native && wire == openai.FamilyChat && !includeUsage {
				var f Object
				payload := strings.TrimSpace(strings.TrimPrefix(string(frame), "data: "))
				if json.Unmarshal([]byte(payload), &f) == nil {
					if len(arr(f["choices"])) == 0 && present(f["usage"]) {
						return nil
					}
					delete(f, "usage")
					frame = eventFrame("", f)
				}
			}
			return upstreamEmit(frame)
		})
	case openai.FamilyAnthropic:
		c, err = streamAnthropic(r, maxEvent, route, upstreamEmit)
	case openai.FamilyGemini:
		c, err = streamGemini(r, maxEvent, route, upstreamEmit)
	case "bedrock":
		c, err = streamBedrock(r, maxEvent, route, upstreamEmit)
	default:
		err = protocolError("unknown stream family")
	}
	if err != nil {
		return c, err
	}
	if finish != nil {
		err = finish(c)
	}
	return c, err
}
func eventFrame(event string, v any) []byte {
	prefix := ""
	if event != "" {
		prefix = "event: " + event + "\n"
	}
	return []byte(prefix + "data: " + string(raw(v)) + "\n\n")
}
func streamError(err error) error {
	if errors.Is(err, sse.ErrEventTooLarge) {
		return openai.ErrEventTooLarge
	}
	if framing, ok := errors.AsType[*sse.DecodeError](err); ok {
		return protocolError(framing.Detail)
	}
	return err
}

type contentBlock struct{ kind, args string }

func streamAnthropic(r io.Reader, limit int, route string, emit openai.Emit) (*openai.Completion, error) {
	c := &openai.Completion{}
	started, finished, done := false, false, false
	blocks := map[int64]*contentBlock{}
	next := int64(0)
	usage := Object{}
	retained := 0
	err := sse.Decode(r, limit, func(frame sse.Frame) error {
		f, e := object([]byte(frame.Data))
		if e != nil {
			return protocolError("invalid Anthropic event")
		}
		kind := str(f["type"])
		if frame.Event != nil && *frame.Event != kind {
			return protocolError("event name disagrees with type")
		}
		if kind == "error" {
			if e := openai.ParseErrorBody([]byte(frame.Data)); e != nil {
				return e
			}
			return protocolError("invalid stream error")
		}
		switch kind {
		case "ping":
			return nil
		case "message_start":
			if started {
				return protocolError("duplicate message start")
			}
			m, e := object(f["message"])
			if e != nil || str(m["role"]) != "assistant" || str(m["type"]) != "message" {
				return protocolError("invalid message start")
			}
			started = true
			c.UpstreamID = str(m["id"])
			c.ProviderModel = str(m["model"])
			if u, e := optionalObject(m["usage"]); e != nil {
				return e
			} else {
				maps.Copy(usage, u)
			}
			m["model"] = raw(route)
			f["message"] = raw(m)
		case "content_block_start":
			index, ok := count(f["index"])
			if !started || finished || !ok || index != next {
				return protocolError("invalid content block start sequence")
			}
			if len(blocks) >= max(1, limit/32) {
				return protocolError("too many active content blocks")
			}
			next++
			block, e := object(f["content_block"])
			if e != nil {
				return protocolError("invalid content block")
			}
			kind := str(block["type"])
			if kind == "tool_use" && (str(block["id"]) == "" || str(block["name"]) == "") {
				return protocolError("incomplete tool start")
			}
			state := &contentBlock{kind: kind}
			if kind == "tool_use" {
				input, err := object(block["input"])
				if err != nil {
					return protocolError("invalid tool input")
				}
				if len(input) > 0 {
					state.args = string(block["input"])
					retained += len(state.args)
				}
			} else if kind != "text" {
				state.kind = ""
			}
			if retained > limit {
				return openai.ErrEventTooLarge
			}
			blocks[index] = state
		case "content_block_delta":
			index, ok := count(f["index"])
			block := blocks[index]
			if !started || finished || !ok || block == nil {
				return protocolError("delta outside an active content block")
			}
			delta, e := object(f["delta"])
			if e != nil {
				return protocolError("invalid content delta")
			}
			switch str(delta["type"]) {
			case "text_delta":
				if block.kind != "text" {
					return protocolError("text delta for non-text block")
				}
				var text string
				if json.Unmarshal(delta["text"], &text) != nil {
					return protocolError("invalid text delta")
				}
			case "input_json_delta":
				if block.kind != "tool_use" {
					return protocolError("tool delta for non-tool block")
				}
				var part string
				if json.Unmarshal(delta["partial_json"], &part) != nil {
					return protocolError("invalid tool delta")
				}
				if retained+len(part) > limit {
					return openai.ErrEventTooLarge
				}
				retained += len(part)
				block.args += part
			}
		case "content_block_stop":
			index, ok := count(f["index"])
			block := blocks[index]
			if !ok || block == nil || finished {
				return protocolError("stop outside an active content block")
			}
			if block.kind == "tool_use" && block.args != "" {
				if _, e := object([]byte(block.args)); e != nil {
					return protocolError("incomplete tool arguments")
				}
			}
			retained -= len(block.args)
			delete(blocks, index)
		case "message_delta":
			if !started || finished || len(blocks) > 0 {
				return protocolError("message delta before blocks finish")
			}
			delta, e := object(f["delta"])
			if e != nil {
				return protocolError("invalid message delta")
			}
			reason := str(delta["stop_reason"])
			if reason == "" {
				return protocolError("missing stop reason")
			}
			c.FinishReason = anthropicFinish(reason)
			finished = true
			if u, e := optionalObject(f["usage"]); e != nil {
				return e
			} else {
				delete(usage, "output_tokens")
				maps.Copy(usage, u)
			}
			c.Usage, e = nativeUsage(usage, "anthropic")
			if e != nil {
				return e
			}
		case "message_stop":
			if !finished || len(blocks) > 0 {
				return protocolError("message stopped before completion")
			}
			done = true
		default:
			if !started {
				return protocolError("event before message start")
			}
		}
		if e := emit(eventFrame(kind, f)); e != nil {
			return e
		}
		if done {
			return streamDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, streamDone) {
		return c, streamError(err)
	}
	if !done {
		return c, &openai.ProtocolError{Detail: "stream ended before message_stop", Truncated: true}
	}
	return c, nil
}
func streamGemini(r io.Reader, limit int, route string, emit openai.Emit) (*openai.Completion, error) {
	c := &openai.Completion{}
	finished := map[int64]bool{}
	hasTools := false
	seen := false
	err := sse.Decode(r, limit, func(frame sse.Frame) error {
		part, e := decodeGemini([]byte(frame.Data), route, true)
		if e != nil {
			return e
		}
		f, _ := object(part.Body)
		for i, v := range arr(f["candidates"]) {
			candidate, _ := object(v)
			index, ok := count(candidate["index"])
			if !ok {
				index = int64(i)
			}
			if len(finished) >= max(1, limit/16) {
				if _, ok := finished[index]; !ok {
					return protocolError("too many streamed candidates")
				}
			}
			if finished[index] {
				return protocolError("candidate continued after finish")
			}
			finished[index] = str(candidate["finishReason"]) != ""
			seen = true
		}
		if present(f["promptFeedback"]) && part.FinishReason == "content_filter" {
			seen = true
			finished[0] = true
		}
		if c.UpstreamID == "" {
			c.UpstreamID = part.UpstreamID
			c.ProviderModel = part.ProviderModel
		}
		hasTools = hasTools || len(part.ToolCalls) > 0
		if part.FinishReason != "" {
			c.FinishReason = part.FinishReason
			if c.FinishReason == "stop" && hasTools {
				c.FinishReason = "tool_calls"
			}
		}
		if present(f["usageMetadata"]) {
			c.Usage = part.Usage
		}
		return emit(eventFrame("", f))
	})
	if err != nil {
		return c, streamError(err)
	}
	if !seen {
		return c, &openai.ProtocolError{Detail: "empty Gemini stream", Truncated: true}
	}
	for _, done := range finished {
		if !done {
			return c, &openai.ProtocolError{Detail: "stream ended before candidate finish", Truncated: true}
		}
	}
	return c, nil
}

type streamTranslator struct {
	source, target            openai.Family
	route                     string
	limit                     int
	includeUsage              bool
	emit                      openai.Emit
	started                   bool
	id, text                  string
	calls                     map[int]*openai.ToolCall
	blocks                    map[int]int
	textBlock                 *int
	refusalBlock              *int
	refusal                   string
	refused                   bool
	sequence, nextBlock, size int
	retained                  *int
}

func (t *streamTranslator) send(event string, f Object) error {
	if t.target == openai.FamilyResponses {
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
	case openai.FamilyChat:
		return t.chat(Object{"role": raw("assistant")}, nil, nil)
	case openai.FamilyAnthropic:
		return t.send("message_start", Object{"type": raw("message_start"), "message": raw(map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.route, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}})})
	case openai.FamilyResponses:
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
		return openai.ErrEventTooLarge
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
	switch t.target {
	case openai.FamilyChat:
		return t.chat(Object{"content": raw(text)}, nil, nil)
	case openai.FamilyAnthropic:
		if t.textBlock == nil {
			index := t.nextBlock
			t.nextBlock++
			t.textBlock = &index
			if e := t.send("content_block_start", Object{"type": raw("content_block_start"), "index": raw(index), "content_block": raw(map[string]string{"type": "text", "text": ""})}); e != nil {
				return e
			}
		}
		return t.send("content_block_delta", Object{"type": raw("content_block_delta"), "index": raw(*t.textBlock), "delta": raw(map[string]string{"type": "text_delta", "text": text})})
	case openai.FamilyResponses:
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
		t.text += text
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
	fresh := call == nil
	if fresh {
		if e := t.reserve(64); e != nil {
			return e
		}
		call = &openai.ToolCall{}
		t.calls[index] = call
	}
	if id != "" {
		call.ID += id
	}
	if name != "" {
		call.Name += name
	}
	if e := t.reserve(len(args) + len(id) + len(name)); e != nil {
		return e
	}
	call.Arguments += args
	if t.target == openai.FamilyChat {
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
	if call.ID == "" || call.Name == "" || call.Arguments == "" {
		return nil
	}
	block, opened := t.blocks[index]
	if !opened {
		block = t.nextBlock
		t.nextBlock++
		t.blocks[index] = block
		switch t.target {
		case openai.FamilyAnthropic:
			if e := t.send("content_block_start", Object{"type": raw("content_block_start"), "index": raw(block), "content_block": raw(map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}})}); e != nil {
				return e
			}
		case openai.FamilyResponses:
			if e := t.send("response.output_item.added", Object{"type": raw("response.output_item.added"), "output_index": raw(block), "item": raw(map[string]any{"type": "function_call", "id": "fc_" + call.ID, "call_id": call.ID, "name": call.Name, "arguments": "", "status": "in_progress"})}); e != nil {
				return e
			}
		}
		args = call.Arguments
	}
	if args == "" {
		return nil
	}
	switch t.target {
	case openai.FamilyAnthropic:
		return t.send("content_block_delta", Object{"type": raw("content_block_delta"), "index": raw(block), "delta": raw(map[string]string{"type": "input_json_delta", "partial_json": args})})
	case openai.FamilyResponses:
		return t.send("response.function_call_arguments.delta", Object{"type": raw("response.function_call_arguments.delta"), "item_id": raw("fc_" + call.ID), "output_index": raw(block), "delta": raw(args)})
	}
	return nil
}
func (t *streamTranslator) chat(delta Object, finish *string, usage *openai.Usage) error {
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
func (t *streamTranslator) frame(frame []byte) error {
	return sse.Decode(strings.NewReader(string(frame)), max(t.limit, len(frame)), func(event sse.Frame) error {
		if event.Data == "[DONE]" {
			return nil
		}
		f, e := object([]byte(event.Data))
		if e != nil {
			return protocolError("invalid translated event")
		}
		switch t.source {
		case openai.FamilyChat:
			if e := t.start(str(f["id"])); e != nil {
				return e
			}
			for _, v := range arr(f["choices"]) {
				choice, _ := object(v)
				idx, _ := count(choice["index"])
				if idx != 0 {
					return protocolError("translation requires one candidate")
				}
				delta, _ := optionalObject(choice["delta"])
				if e := t.refusalDelta(str(delta["refusal"])); e != nil {
					return e
				}
				if e := t.textDelta(str(delta["content"])); e != nil {
					return e
				}
				for _, v := range arr(delta["tool_calls"]) {
					call, _ := object(v)
					index, _ := count(call["index"])
					fun, _ := optionalObject(call["function"])
					if e := t.toolDelta(int(index), str(call["id"]), str(fun["name"]), str(fun["arguments"])); e != nil {
						return e
					}
				}
			}
		case openai.FamilyResponses:
			if response, e := optionalObject(f["response"]); e != nil {
				return e
			} else if response != nil {
				if e := t.start(str(response["id"])); e != nil {
					return e
				}
			}
			switch str(f["type"]) {
			case "response.refusal.delta":
				return t.refusalDelta(str(f["delta"]))
			case "response.output_text.delta":
				return t.textDelta(str(f["delta"]))
			case "response.output_item.added":
				item, _ := optionalObject(f["item"])
				if str(item["type"]) == "function_call" {
					index, _ := count(f["output_index"])
					return t.toolDelta(int(index), str(item["call_id"]), str(item["name"]), str(item["arguments"]))
				}
			case "response.function_call_arguments.delta":
				index, _ := count(f["output_index"])
				return t.toolDelta(int(index), "", "", str(f["delta"]))
			}
		case openai.FamilyAnthropic:
			index, _ := count(f["index"])
			switch str(f["type"]) {
			case "message_start":
				m, _ := object(f["message"])
				return t.start(str(m["id"]))
			case "content_block_start":
				b, _ := object(f["content_block"])
				switch str(b["type"]) {
				case "text":
					return t.textDelta(str(b["text"]))
				case "tool_use":
					input := ""
					if value, err := object(b["input"]); err == nil && len(value) > 0 {
						input = string(b["input"])
					}
					return t.toolDelta(int(index), str(b["id"]), str(b["name"]), input)
				}
			case "content_block_delta":
				d, _ := object(f["delta"])
				switch str(d["type"]) {
				case "text_delta":
					return t.textDelta(str(d["text"]))
				case "input_json_delta":
					return t.toolDelta(int(index), "", "", str(d["partial_json"]))
				}
			}
		case openai.FamilyGemini:
			if e := t.start(str(f["responseId"])); e != nil {
				return e
			}
			for _, v := range arr(f["candidates"]) {
				candidate, _ := object(v)
				index, _ := count(candidate["index"])
				if index != 0 {
					return protocolError("translation requires one candidate")
				}
				content, _ := optionalObject(candidate["content"])
				for _, v := range arr(content["parts"]) {
					part, _ := object(v)
					var thought bool
					_ = json.Unmarshal(part["thought"], &thought)
					if !thought {
						if e := t.textDelta(str(part["text"])); e != nil {
							return e
						}
					}
					if present(part["functionCall"]) {
						call, _ := object(part["functionCall"])
						index := len(t.calls)
						id := str(call["id"])
						if id == "" {
							id = "call_" + strconv.Itoa(index)
						}
						if e := t.toolDelta(index, id, str(call["name"]), string(call["args"])); e != nil {
							return e
						}
					}
				}
			}
		case "bedrock":
			return t.bedrockFrame(f)
		}
		return nil
	})
}
func (t *streamTranslator) finish(c *openai.Completion) error {
	if e := t.start(c.UpstreamID); e != nil {
		return e
	}
	summary := *c
	summary.UpstreamID = t.id
	summary.OutputText = t.text
	summary.Refusal = t.refusal
	if t.refused && summary.FinishReason == "stop" && t.target.Surface() != "openai" {
		summary.FinishReason = "content_filter"
	}
	summary.ToolCalls = nil
	indices := []int{}
	for i := range t.calls {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	for _, i := range indices {
		call := t.calls[i]
		if call.ID == "" || call.Name == "" {
			return protocolError("incomplete translated tool call")
		}
		if call.Arguments == "" {
			if err := t.toolDelta(i, "", "", "{}"); err != nil {
				return err
			}
		}
		if _, e := object([]byte(call.Arguments)); e != nil {
			return protocolError("incomplete translated tool arguments")
		}
		summary.ToolCalls = append(summary.ToolCalls, *call)
	}
	if len(summary.ToolCalls) > 0 && summary.FinishReason == "stop" {
		summary.FinishReason = "tool_calls"
	}
	switch t.target {
	case openai.FamilyChat:
		if e := t.chat(Object{}, &summary.FinishReason, nil); e != nil {
			return e
		}
		if t.includeUsage && summary.Usage != nil {
			if e := t.chat(nil, nil, summary.Usage); e != nil {
				return e
			}
		}
		return t.emit([]byte("data: [DONE]\n\n"))
	case openai.FamilyAnthropic:
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
	case openai.FamilyResponses:
		output := make([]Object, t.nextBlock)
		if t.textBlock != nil {
			index := *t.textBlock
			part := Object{"type": raw("output_text"), "text": raw(t.text), "annotations": raw([]any{})}
			if e := t.send("response.output_text.done", Object{"type": raw("response.output_text.done"), "item_id": raw("msg_" + t.id), "output_index": raw(index), "content_index": raw(0), "text": raw(t.text)}); e != nil {
				return e
			}
			if e := t.send("response.content_part.done", Object{"type": raw("response.content_part.done"), "item_id": raw("msg_" + t.id), "output_index": raw(index), "content_index": raw(0), "part": raw(part)}); e != nil {
				return e
			}
			output[index] = Object{"id": raw("msg_" + t.id), "type": raw("message"), "role": raw("assistant"), "status": raw("completed"), "content": raw([]Object{part})}
		}
		for _, i := range indices {
			call := t.calls[i]
			index := t.blocks[i]
			if e := t.send("response.function_call_arguments.done", Object{"type": raw("response.function_call_arguments.done"), "item_id": raw("fc_" + call.ID), "output_index": raw(index), "arguments": raw(call.Arguments)}); e != nil {
				return e
			}
			output[index] = Object{"id": raw("fc_" + call.ID), "type": raw("function_call"), "status": raw("completed"), "call_id": raw(call.ID), "name": raw(call.Name), "arguments": raw(call.Arguments)}
		}
		if e := t.finishRefusal(output); e != nil {
			return e
		}
		for i, item := range output {
			if e := t.send("response.output_item.done", Object{"type": raw("response.output_item.done"), "output_index": raw(i), "item": raw(item)}); e != nil {
				return e
			}
		}
		f, _ := object(renderCompletion(&summary, t.target, t.route))
		f["output"] = raw(output)
		kind := "response." + str(f["status"])
		return t.send(kind, Object{"type": raw(kind), "response": raw(f)})
	default:
		summary.OutputText = ""
		body := renderCompletion(&summary, t.target, t.route)
		f, _ := object(body)
		return t.send("", f)
	}
}
