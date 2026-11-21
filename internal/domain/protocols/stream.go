package protocols

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var errStreamDone = errors.New("terminal native event")

// Stream keeps the existing executor's backpressure and commitment boundary.
// Native streams retain extensions. Translators retain at most maxEvent bytes
// of text/tool state needed for the destination's terminal envelope.
func Stream(encodedWire, target wire.Format, r io.Reader, maxEvent int, route string, emit wire.Emit) (*wire.Completion, error) {
	return StreamWithEvents(encodedWire, target, r, maxEvent, route, true, emit, nil)
}

// StreamWithEvents exposes immutable native events before projection. The
// observer executes synchronously under the existing backpressure/cancellation
// contract; ordinary native streams retain no event history.
// Native accounting is interpreted before the observer can refuse projection;
// structurally valid events remain observable on dialect grammar failures.
// Actionability still requires the admitted projection and durable-state barrier.
// expectedCandidates, when present, is the effective dispatched request's
// candidate count. Without that request proof, Gemini totals await clean EOF.
func StreamWithEvents(encodedWire, target wire.Format, r io.Reader, maxEvent int, route string, includeUsage bool, emit wire.Emit, observe func(oif.Event) error, expectedCandidates ...int64) (*wire.Completion, error) {
	expected := int64(0)
	if len(expectedCandidates) != 0 {
		expected = expectedCandidates[0]
	}
	if encodedWire == wire.Chat && target == wire.Completions {
		return streamCompletionChat(r, maxEvent, route, includeUsage, emit, observe, expected)
	}
	native := encodedWire == target || encodedWire == wire.Gemini && target == wire.GeminiStream
	if encodedWire.Surface() == "native" && !native {
		return nil, protocolError("a native generation dialect is never translated")
	}
	var finish func(*wire.Completion) error
	upstreamEmit := emit
	if !native {
		if supportsCandidates(encodedWire) && supportsCandidates(target) {
			translator := &candidateStream{source: encodedWire, target: target, route: route, limit: maxEvent, includeUsage: includeUsage, emit: emit, children: map[int]*streamTranslator{}, finishes: map[int]string{}}
			upstreamEmit, finish = translator.frame, translator.finish
		} else {
			translator := &streamTranslator{source: encodedWire, target: target, route: route, limit: maxEvent, includeUsage: includeUsage, emit: emit, calls: map[int]*streamCall{}, responseTexts: map[int]*strings.Builder{}, responseRefusals: map[int]*strings.Builder{}, blocks: map[int]int{}}
			upstreamEmit, finish = translator.frame, translator.finish
		}
	}

	var c *wire.Completion
	var err error
	switch encodedWire {
	case wire.Completions, wire.Chat, wire.Responses:
		c, err = openai.StreamMetadataEvents(encodedWire, target, r, maxEvent, route, includeUsage || !native, upstreamEmit, observe, expected)
	case wire.MistralFIM:
		// Fill-in-the-middle streams chat completion chunks.
		c, err = openai.StreamMetadataEvents(wire.MistralFIM, target, r, maxEvent, route, true, upstreamEmit, observe)
	case wire.CohereChat:
		c, err = streamCohereEvents(r, maxEvent, upstreamEmit, observe)
	case wire.Anthropic:
		c, err = streamAnthropicEvents(r, maxEvent, route, upstreamEmit, observe)
	case wire.Gemini:
		c, err = streamGeminiEvents(r, maxEvent, route, upstreamEmit, observe, expected)
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

func AggregateTranslatedResponses(target wire.Format, r io.Reader, maxEvent int, route string, observe func(oif.Event) error) (*wire.Completion, error) {
	translator := &streamTranslator{source: wire.Responses, target: target, route: route, limit: maxEvent, includeUsage: true, emit: func([]byte) error { return nil }, calls: map[int]*streamCall{}, responseTexts: map[int]*strings.Builder{}, responseRefusals: map[int]*strings.Builder{}, blocks: map[int]int{}}
	completion, err := openai.StreamMetadataEvents(wire.Responses, target, r, maxEvent, route, true, translator.frame, observe)
	if err != nil {
		return completion, err
	}
	summary, err := translator.summary(completion)
	if err != nil {
		return nil, err
	}
	return &wire.Completion{Body: renderCompletion(summary, target, route), UpstreamID: summary.UpstreamID, ProviderModel: summary.ProviderModel, FinishReason: summary.FinishReason, OutputText: summary.OutputText, Refusal: summary.Refusal, ToolCalls: summary.ToolCalls, Usage: summary.Usage, UsageComplete: summary.UsageComplete, Terminal: summary.Terminal, Failure: summary.Failure}, nil
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
		return wire.ErrEventTooLarge
	}
	if framing, ok := errors.AsType[*sse.DecodeError](err); ok {
		return protocolError(framing.Detail)
	}
	return err
}

// A native parser retains only its current rebound frame. The observer still
// sees the immutable source before delivery and retains its refusal priority.
func emitObservedEvent(event oif.Event, observe func(oif.Event) error, parsed error, frame []byte, emit wire.Emit) error {
	if observe != nil {
		if err := observe(event); err != nil {
			return err
		}
	}
	if parsed != nil && !errors.Is(parsed, errStreamDone) {
		return parsed
	}
	if frame != nil {
		if err := emit(frame); err != nil {
			return err
		}
	}
	return parsed
}

type contentBlock struct {
	kind string
	args strings.Builder
}

type anthropicStreamState struct {
	c               *wire.Completion
	limit           int
	route           string
	started         bool
	sawMessageDelta bool
	done            bool
	blocks          map[int64]*contentBlock
	next            int64
	usage           Object
	retained        int
}

func (st *anthropicStreamState) apply(kind string, f Object) error {
	switch kind {
	case "message_start":
		return st.applyMessageStart(f)
	case "content_block_start":
		return st.applyBlockStart(f)
	case "content_block_delta":
		return st.applyBlockDelta(f)
	case "content_block_stop":
		index, ok := count(f["index"])
		block := st.blocks[index]
		if !ok || block == nil || st.sawMessageDelta {
			return protocolError("stop outside an active content block")
		}
		if block.kind == "tool_use" && block.args.Len() != 0 {
			if _, e := object([]byte(block.args.String())); e != nil {
				return protocolError("incomplete tool arguments")
			}
		}
		st.retained -= block.args.Len()
		delete(st.blocks, index)
	case "message_delta":
		return st.applyMessageDelta(f)
	case "message_stop":
		if st.c.FinishReason == "" || len(st.blocks) > 0 {
			return protocolError("message stopped before completion")
		}
		st.done = true
		st.c.UsageComplete = st.c.Usage != nil
	default:
		if !st.started {
			return protocolError("event before message start")
		}
	}
	return nil
}

func (st *anthropicStreamState) applyMessageStart(f Object) error {
	if st.started {
		return protocolError("duplicate message start")
	}
	m, e := object(f["message"])
	if e != nil || str(m["role"]) != "assistant" || str(m["type"]) != "message" {
		return protocolError("invalid message start")
	}
	st.started = true
	st.c.UpstreamID = str(m["id"])
	st.c.ProviderModel = str(m["model"])
	if u, e := optionalObject(m["usage"]); e != nil {
		return e
	} else {
		maps.Copy(st.usage, u)
	}
	st.c.Usage, e = nativeUsage(st.usage, "anthropic")
	if e != nil {
		return e
	}
	m["model"] = raw(st.route)
	f["message"] = raw(m)
	return nil
}

func (st *anthropicStreamState) applyBlockStart(f Object) error {
	index, ok := count(f["index"])
	if !st.started || st.sawMessageDelta || !ok || index != st.next {
		return protocolError("invalid content block start sequence")
	}
	if len(st.blocks) >= max(1, st.limit/32) {
		return protocolError("too many active content blocks")
	}
	st.next++
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
			state.args.WriteString(string(block["input"]))
			st.retained += state.args.Len()
		}
	} else if kind != "text" {
		state.kind = ""
	}
	if st.retained > st.limit {
		return wire.ErrEventTooLarge
	}
	st.blocks[index] = state
	return nil
}

func (st *anthropicStreamState) applyBlockDelta(f Object) error {
	index, ok := count(f["index"])
	block := st.blocks[index]
	if !st.started || st.sawMessageDelta || !ok || block == nil {
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
		if st.retained+len(part) > st.limit {
			return wire.ErrEventTooLarge
		}
		st.retained += len(part)
		block.args.WriteString(part)
	}
	return nil
}

func (st *anthropicStreamState) applyMessageDelta(f Object) error {
	if !st.started || len(st.blocks) > 0 {
		return protocolError("message delta before blocks finish")
	}
	delta, e := object(f["delta"])
	if e != nil {
		return protocolError("invalid message delta")
	}
	if reason := str(delta["stop_reason"]); reason != "" {
		st.c.FinishReason = anthropicFinish(reason)
	} else if present(delta["stop_reason"]) {
		return protocolError("invalid stop reason")
	}
	u, e := optionalObject(f["usage"])
	if e != nil {
		return e
	}
	if !st.sawMessageDelta {
		delete(st.usage, "output_tokens")
	}
	st.sawMessageDelta = true
	for _, key := range [...]string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "server_tool_use"} {
		if value, exists := u[key]; exists {
			st.usage[key] = value
		}
	}
	st.c.Usage, e = nativeUsage(st.usage, "anthropic")
	return e
}

func streamAnthropicEvents(r io.Reader, limit int, route string, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	st := &anthropicStreamState{c: &wire.Completion{}, limit: limit, route: route, blocks: map[int64]*contentBlock{}, usage: Object{}}
	sequence := uint64(0)
	err := sse.Decode(r, limit, func(frame sse.Frame) (parsed error) {
		event, e := openai.LiftSSE(wire.Anthropic, frame, sequence, limit)
		if e != nil {
			return e
		}
		sequence++
		var encoded []byte
		defer func() { parsed = emitObservedEvent(event, observe, parsed, encoded, emit) }()
		f := event.Source().Fields()
		if f == nil {
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
		if kind == "ping" {
			return nil
		}
		if err := st.apply(kind, f); err != nil {
			return err
		}
		encoded = eventFrame(kind, f)
		if st.done {
			return errStreamDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStreamDone) {
		return st.c, streamError(err)
	}
	if !st.done {
		return st.c, &wire.ProtocolError{Detail: "stream ended before message_stop", Truncated: true}
	}
	return st.c, nil
}

func streamGeminiEvents(r io.Reader, limit int, route string, emit wire.Emit, observe func(oif.Event) error, expectedCandidates int64) (*wire.Completion, error) {
	c := &wire.Completion{}
	finished := map[int64]bool{}
	hasTools := false
	seen := false
	blocked := false
	usageAtFinish := false
	sequence := uint64(0)
	err := sse.Decode(r, limit, func(frame sse.Frame) (parsed error) {
		event, e := openai.LiftSSE(wire.Gemini, frame, sequence, limit)
		if e != nil {
			return e
		}
		sequence++
		var encoded []byte
		defer func() { parsed = emitObservedEvent(event, observe, parsed, encoded, emit) }()
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
			if _, exists := finished[index]; !exists && expectedCandidates > 0 && int64(len(finished)) >= expectedCandidates {
				return protocolError("more candidates than the dispatched request")
			}
			usageAtFinish, c.UsageComplete = false, false
			finished[index] = str(candidate["finishReason"]) != ""
			seen = true
		}
		if present(f["promptFeedback"]) && part.FinishReason == "content_filter" {
			seen = true
			blocked = true
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
		if present(f["usageMetadata"]) && part.Usage != nil {
			if c.Usage != nil && len(c.Usage.ToolCalls) > 0 {
				// Usage snapshots replace token counts, while metered tool
				// invocations accumulate across the chunks that reported them.
				if part.Usage.ToolCalls == nil {
					part.Usage.ToolCalls = c.Usage.ToolCalls
				} else {
					for tool, invocations := range c.Usage.ToolCalls {
						part.Usage.ToolCalls[tool] += invocations
					}
					// The grounded-prompt marker is a flag, not a count: one
					// grounded prompt no matter how many chunks grounded.
					if _, ok := part.Usage.ToolCalls["google_search_prompt"]; ok {
						part.Usage.ToolCalls["google_search_prompt"] = 1
					}
				}
			}
			c.Usage = part.Usage
			usageAtFinish = seen
			for _, done := range finished {
				usageAtFinish = usageAtFinish && done
			}
			c.UsageComplete = usageAtFinish && (blocked || expectedCandidates > 0 && int64(len(finished)) == expectedCandidates)
		}
		encoded = eventFrame("", f)
		return nil
	})
	if err != nil {
		return c, streamError(err)
	}
	if !seen {
		return c, &wire.ProtocolError{Detail: "empty Gemini stream", Truncated: true}
	}
	if !blocked && expectedCandidates > 0 && int64(len(finished)) != expectedCandidates {
		return c, &wire.ProtocolError{Detail: "stream ended before all requested candidates", Truncated: true}
	}
	for _, done := range finished {
		if !done {
			return c, &wire.ProtocolError{Detail: "stream ended before candidate finish", Truncated: true}
		}
	}
	c.UsageComplete = c.Usage != nil && usageAtFinish
	return c, nil
}
