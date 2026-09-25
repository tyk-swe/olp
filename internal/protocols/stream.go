package protocols

import (
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/oif"
	"io"
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
	return StreamWithEvents(wire, target, r, maxEvent, route, includeUsage, emit, nil)
}

// StreamWithEvents exposes immutable native events before projection. The
// observer executes synchronously under the existing backpressure/cancellation
// contract; ordinary native streams retain no event history.
// It observes structurally valid JSON before dialect state-grammar validation.
// Actionability still requires the admitted projection and durable-state barrier.
func StreamWithEvents(wire, target openai.Family, r io.Reader, maxEvent int, route string, includeUsage bool, emit openai.Emit, observe func(oif.Event) error) (*openai.Completion, error) {
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
		c, err = openai.StreamMetadataEvents(wire, r, maxEvent, route, true, func(frame []byte) error {
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
		}, observe)
	case openai.FamilyAnthropic:
		c, err = streamAnthropicEvents(r, maxEvent, route, upstreamEmit, observe)
	case openai.FamilyGemini:
		c, err = streamGeminiEvents(r, maxEvent, route, upstreamEmit, observe)
	case "bedrock":
		c, err = streamBedrockEvents(r, maxEvent, route, upstreamEmit, observe, native)
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

// framedEventFrame prepends the admitted SSE id/retry framing so a native
// forward keeps the provider's wire framing instead of silently dropping it.
func framedEventFrame(f oif.Framing, event, data string) []byte {
	var b strings.Builder
	if f.HasID {
		b.WriteString("id: " + f.ID + "\n")
	}
	if f.HasRetry {
		b.WriteString("retry: " + strconv.FormatUint(f.RetryMillis, 10) + "\n")
	}
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	// A payload may span multiple data lines; preserve them verbatim.
	b.WriteString("data: " + strings.ReplaceAll(data, "\n", "\ndata: ") + "\n\n")
	return []byte(b.String())
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
	return streamAnthropicEvents(r, limit, route, emit, nil)
}

// streamAnthropicEvents is a thin transport driver: decode SSE, lift each
// frame to OIF, admit it through the dialect-owned AnthropicTrace reducer, and
// forward the admitted source/framing. Native facts for the completion summary
// come only from the trace; no Chat types or materialized response are
// involved on the native path.
func streamAnthropicEvents(r io.Reader, limit int, route string, emit openai.Emit, observe func(oif.Event) error) (*openai.Completion, error) {
	c := &openai.Completion{}
	trace := NewAnthropicTrace(limit)
	sequence := uint64(0)
	err := sse.Decode(r, limit, func(frame sse.Frame) error {
		event, e := openai.LiftSSE(openai.FamilyAnthropic, frame, sequence, limit)
		if e != nil {
			return e
		}
		sequence++
		if observe != nil {
			if e := observe(event); e != nil {
				return e
			}
		}
		tr, e := trace.Accept(event)
		if e != nil {
			return e
		}
		switch tr.Kind {
		case AnthropicPing:
			return nil
		case AnthropicError:
			if e := openai.ParseErrorBody(event.Source().Bytes()); e != nil {
				return e
			}
			return protocolError("invalid stream error")
		}
		// Forward the admitted source bytes verbatim; message_start is the
		// single authorized rewrite (the routed model replaces wire identity).
		data := event.Source().Raw()
		if tr.Kind == AnthropicMessageStart {
			f := event.Source().Fields()
			m, _ := object(f["message"])
			c.UpstreamID = str(m["id"])
			c.ProviderModel = str(m["model"])
			m["model"] = raw(route)
			f["message"] = raw(m)
			data = string(raw(f))
		}
		if tr.Usage.Kind() == oif.Object {
			usage, e := nativeUsage(trace.Usage(), "anthropic")
			if e != nil {
				return e
			}
			if usage != nil {
				c.Usage = usage
			}
		}
		if reason, declared := trace.StopReason(); declared {
			c.FinishReason = anthropicFinish(reason)
		}
		if e := emit(framedEventFrame(event.Framing(), tr.Type, data)); e != nil {
			return e
		}
		if tr.Kind == AnthropicMessageStop {
			return streamDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, streamDone) {
		return c, streamError(err)
	}
	if !trace.Terminal() {
		return c, &openai.ProtocolError{Detail: "stream ended before message_stop", Truncated: true}
	}
	return c, nil
}
func streamGemini(r io.Reader, limit int, route string, emit openai.Emit) (*openai.Completion, error) {
	return streamGeminiEvents(r, limit, route, emit, nil)
}
func streamGeminiEvents(r io.Reader, limit int, route string, emit openai.Emit, observe func(oif.Event) error) (*openai.Completion, error) {
	c := &openai.Completion{}
	finished := map[int64]bool{}
	hasTools := false
	seen := false
	sequence := uint64(0)
	err := sse.Decode(r, limit, func(frame sse.Frame) error {
		event, e := openai.LiftSSE(openai.FamilyGemini, frame, sequence, limit)
		if e != nil {
			return e
		}
		sequence++
		if observe != nil {
			if e := observe(event); e != nil {
				return e
			}
		}
		part, e := decodeGemini(event.Source().Bytes(), route, true)
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
	trace                     *AnthropicTrace
	lifted                    uint64
}

func (t *streamTranslator) send(event string, f Object) error {
	if t.target == openai.FamilyResponses {
		f["sequence_number"] = raw(t.sequence)
		t.sequence++
	}
	if t.target == openai.FamilyAnthropic {
		// Generated Anthropic output is admitted through the same
		// authoritative reducer as provider streams: a translator defect
		// fails closed instead of emitting an invalid native trace.
		if t.trace == nil {
			t.trace = NewAnthropicTrace(t.limit)
		}
		lifted, e := openai.LiftEvent(openai.FamilyAnthropic, string(raw(f)), event, t.lifted, t.limit)
		if e != nil {
			return e
		}
		t.lifted++
		if _, e := t.trace.Accept(lifted); e != nil {
			return e
		}
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
				default:
					return protocolError("untranslatable Anthropic block")
				}
			case "content_block_delta":
				d, _ := object(f["delta"])
				switch str(d["type"]) {
				case "text_delta":
					return t.textDelta(str(d["text"]))
				case "input_json_delta":
					return t.toolDelta(int(index), "", "", str(d["partial_json"]))
				default:
					return protocolError("untranslatable Anthropic delta")
				}
			case "content_block_stop", "message_delta", "message_stop", "ping":
				// The source reducer already validated these events and
				// collected their native facts; the destination terminal is
				// emitted by finish, never silently dropped here.
				return nil
			default:
				return protocolError("untranslatable Anthropic event")
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
