package interaction

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Delivery is already projected client data. The gateway commits it with the
// complete native dependency before publishing any queued actionable frame.
// Replaying this value never invokes a provider or claims new token usage.
type Delivery struct {
	Stream bool              `json:"stream"`
	Frames []json.RawMessage `json:"frames,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}
type ToolProjection struct {
	plan              *Plan
	limit, retained   int
	blocks            []json.RawMessage
	active            map[int]*toolBlock
	frames            []json.RawMessage
	observations      []json.RawMessage
	nativeUsage       map[string]json.RawMessage
	blocked           bool
	started, terminal bool
	finish            string
	id, model         string
	toolCount         int
	content           strings.Builder
	calls             []map[string]any
}
type toolBlock struct {
	fields                map[string]json.RawMessage
	kind                  string
	text, signature, args strings.Builder
}

func (p *Plan) ToolContinuation() bool {
	return p != nil && p.receipt.Obligations.Continuation == ContinuationV1
}
func (p *Plan) NewToolProjection(limit int) (*ToolProjection, error) {
	if !p.ToolContinuation() || limit < 1 || limit > 4<<20 {
		return nil, continuationFailure("/client_contract", "bounded_tool_projection")
	}
	return &ToolProjection{plan: p, limit: limit, active: map[int]*toolBlock{}}, nil
}
func (p *ToolProjection) retain(n int) error {
	if n < 0 || n > p.limit-p.retained {
		return guardFailure("/continuation", "bounded_dependency_state")
	}
	p.retained += n
	return nil
}
func (p *ToolProjection) chunk(delta any, observation map[string]any) ([]byte, error) {
	extension := map[string]any{"version": ContinuationV1, "observation": observation}
	frame, err := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, "olp": extension})
	if err != nil {
		return nil, err
	}
	if err = p.retain(len(frame)); err != nil {
		return nil, err
	}
	p.frames = append(p.frames, json.RawMessage(frame))
	observationRaw, _ := json.Marshal(observation)
	p.observations = append(p.observations, observationRaw)
	if p.blocked {
		return nil, nil
	}
	return frame, nil
}

// Preserve exact provider usage categories in the negotiated extension. The
// ordinary Chat usage object has no fields for Anthropic cache-write TTLs.
// Unknown categories fail closed before a ready result can be published.
func (p *ToolProjection) recordNativeUsage(usage oif.Value) error {
	if !onlyMembers(usage, "input_tokens output_tokens cache_read_input_tokens cache_creation_input_tokens cache_creation") {
		return guardFailure("/usage", "qualified_native_usage_categories")
	}
	if p.nativeUsage == nil {
		p.nativeUsage = map[string]json.RawMessage{}
	}
	for _, field := range usage.Members() {
		if field.Name == "cache_creation" {
			if !onlyMembers(field.Value, "ephemeral_5m_input_tokens ephemeral_1h_input_tokens") {
				return guardFailure("/usage/cache_creation", "qualified_native_cache_ttls")
			}
			for _, detail := range field.Value.Members() {
				if !nonnegativeInteger(detail.Value) {
					return guardFailure("/usage/cache_creation", "native_cache_token_count")
				}
			}
		} else if !nonnegativeInteger(field.Value) {
			return guardFailure("/usage/"+field.Name, "native_token_count")
		}
		p.nativeUsage[field.Name] = field.Value.Bytes()
	}
	return nil
}

// Observe processes one native event synchronously. Before a tool starts,
// non-actionable text/reasoning observations are incremental. A tool turn's
// complete assistant history (including later blocks/signatures) is required to
// reconstruct the next native request, so that dependency gates its tool bytes.
func (p *ToolProjection) Observe(event oif.Event) ([][]byte, error) {
	if err := p.plan.ValidateEvent(event); err != nil {
		return nil, err
	}
	root := event.Source().Root()
	kind := valueText(member(root, "type"))
	if err := p.retain(event.Source().Len()); err != nil {
		return nil, err
	}
	var frame []byte
	var err error
	switch kind {
	case "ping":
		return nil, nil
	case "message_start":
		message := member(root, "message")
		if p.started || !onlyMembers(root, "type message") || !onlyMembers(message, "id type role model content stop_reason stop_sequence usage") || len(member(message, "content").Elements()) != 0 {
			return nil, guardFailure("/events", "message_start_contract")
		}
		p.started = true
		p.id = valueText(member(message, "id"))
		p.model = valueText(member(message, "model"))
		if usage, present := message.Lookup("usage"); present {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
	case "content_block_start":
		if !p.started || p.terminal || !onlyMembers(root, "type index content_block") {
			return nil, guardFailure("/events", "block_start_contract")
		}
		index, e := toolIndex(member(root, "index"))
		if e != nil {
			return nil, e
		}
		if index != len(p.blocks) {
			return nil, guardFailure("/events/index", "ordered_block_start")
		}
		block := member(root, "content_block")
		b := &toolBlock{fields: map[string]json.RawMessage{}, kind: valueText(member(block, "type"))}
		for _, f := range block.Members() {
			b.fields[f.Name] = f.Value.Bytes()
		}
		switch b.kind {
		case "text":
			if !onlyMembers(block, "type text") || member(block, "text").Kind() != oif.String {
				return nil, guardFailure("/content", "text_block")
			}
			b.text.WriteString(valueText(member(block, "text")))
		case "thinking":
			if !onlyMembers(block, "type thinking signature") || member(block, "thinking").Kind() != oif.String || member(block, "signature").Kind() != oif.String {
				return nil, guardFailure("/content", "thinking_block")
			}
			b.text.WriteString(valueText(member(block, "thinking")))
			b.signature.WriteString(valueText(member(block, "signature")))
		case "redacted_thinking":
			if !onlyMembers(block, "type data") || member(block, "data").Kind() != oif.String {
				return nil, guardFailure("/content", "opaque_reasoning_block")
			}
		case "tool_use":
			if !onlyMembers(block, "type id name input") || valueText(member(block, "id")) == "" || valueText(member(block, "name")) == "" || member(block, "input").Kind() != oif.Object {
				return nil, guardFailure("/content", "tool_block")
			}
			// Even the name or partial arguments could trigger an ordinary SDK caller.
			p.blocked = true
			if len(member(block, "input").Members()) != 0 {
				b.args.WriteString(member(block, "input").Raw())
			}
		default:
			return nil, guardFailure("/content", "qualified_native_block")
		}
		p.blocks = append(p.blocks, nil)
		p.active[index] = b
		change := map[string]any{}
		observation := map[string]any{"index": index, "type": b.kind, "phase": "start"}
		if b.text.Len() > 0 {
			observation["text"] = b.text.String()
			if b.kind == "text" {
				change["content"] = b.text.String()
				p.content.WriteString(b.text.String())
			}
		}
		frame, err = p.chunk(change, observation)
	case "content_block_delta":
		if !onlyMembers(root, "type index delta") {
			return nil, guardFailure("/events", "delta_contract")
		}
		index, e := toolIndex(member(root, "index"))
		if e != nil {
			return nil, e
		}
		b := p.active[index]
		if b == nil {
			return nil, guardFailure("/events", "active_block")
		}
		delta := member(root, "delta")
		deltaKind := valueText(member(delta, "type"))
		switch deltaKind {
		case "text_delta", "thinking_delta":
			field, expected := "text", "text"
			if deltaKind == "thinking_delta" {
				field, expected = "thinking", "thinking"
			}
			if b.kind != expected || !onlyMembers(delta, "type "+field) || member(delta, field).Kind() != oif.String {
				return nil, guardFailure("/events/delta", "typed_text_delta")
			}
			text := valueText(member(delta, field))
			b.text.WriteString(text)
			change := map[string]any{}
			if b.kind == "text" {
				change["content"] = text
				p.content.WriteString(text)
			}
			frame, err = p.chunk(change, map[string]any{"index": index, "type": b.kind, "phase": "delta", "text": text})
		case "signature_delta":
			if b.kind != "thinking" || !onlyMembers(delta, "type signature") || member(delta, "signature").Kind() != oif.String {
				return nil, guardFailure("/events/delta", "native_signature_delta")
			}
			b.signature.WriteString(valueText(member(delta, "signature")))
		case "input_json_delta":
			if b.kind != "tool_use" || !onlyMembers(delta, "type partial_json") || member(delta, "partial_json").Kind() != oif.String {
				return nil, guardFailure("/events/delta", "native_tool_arguments")
			}
			b.args.WriteString(valueText(member(delta, "partial_json")))
		default:
			return nil, guardFailure("/events/delta", "qualified_native_delta")
		}
	case "content_block_stop":
		if !onlyMembers(root, "type index") {
			return nil, guardFailure("/events", "block_stop_contract")
		}
		index, e := toolIndex(member(root, "index"))
		if e != nil {
			return nil, e
		}
		b := p.active[index]
		if b == nil {
			return nil, guardFailure("/events", "active_block")
		}
		delta := map[string]any{}
		observation := map[string]any{"index": index, "type": b.kind, "phase": "end"}
		switch b.kind {
		case "text":
			b.fields["text"], _ = json.Marshal(b.text.String())
		case "thinking":
			if b.signature.Len() == 0 {
				return nil, guardFailure("/content", "complete_native_signature")
			}
			b.fields["thinking"], _ = json.Marshal(b.text.String())
			b.fields["signature"], _ = json.Marshal(b.signature.String())
			observation["opaque_state"] = true
		case "redacted_thinking":
			observation["opaque_state"] = true
		case "tool_use":
			args := b.args.String()
			if args == "" {
				args = "{}"
			}
			document, e := oif.ParseJSON([]byte(args), oif.Limits{MaxBytes: p.limit})
			if e != nil || document.Root().Kind() != oif.Object {
				return nil, guardFailure("/content/input", "complete_native_tool_arguments")
			}
			b.fields["input"] = document.Bytes()
			id, name := stringField(b.fields, "id"), stringField(b.fields, "name")
			if !p.plan.declaresTool(name) {
				return nil, guardFailure("/content/name", "declared_tool_correspondence")
			}
			for _, call := range p.calls {
				if call["id"] == id {
					return nil, guardFailure("/content/id", "unique_tool_identity")
				}
			}
			fn := map[string]any{"name": name, "arguments": args}
			call := map[string]any{"id": id, "type": "function", "function": fn}
			p.calls = append(p.calls, call)
			delta["tool_calls"] = []any{map[string]any{"index": p.toolCount, "id": id, "type": "function", "function": fn}}
			observation["call_id"] = id
			observation["name"] = name
			p.toolCount++
		}
		p.blocks[index], err = json.Marshal(b.fields)
		if err != nil {
			return nil, err
		}
		delete(p.active, index)
		frame, err = p.chunk(delta, observation)
	case "message_delta":
		if !onlyMembers(root, "type delta usage") || len(p.active) > 0 {
			return nil, guardFailure("/events", "message_delta_contract")
		}
		if usage, present := root.Lookup("usage"); present {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
		delta := member(root, "delta")
		if !onlyMembers(delta, "stop_reason stop_sequence") {
			return nil, guardFailure("/events", "terminal_metadata")
		}
		p.finish = valueText(member(delta, "stop_reason"))
		if p.finish != "end_turn" && p.finish != "tool_use" && p.finish != "max_tokens" && p.finish != "stop_sequence" {
			return nil, guardFailure("/events/stop_reason", "known_terminal")
		}
		if p.finish == "tool_use" && p.toolCount == 0 || p.finish != "tool_use" && p.toolCount > 0 {
			return nil, guardFailure("/events/stop_reason", "tool_terminal_correspondence")
		}
	case "message_stop":
		if !onlyMembers(root, "type") || p.finish == "" || len(p.active) > 0 {
			return nil, guardFailure("/events", "complete_native_terminal")
		}
		p.terminal = true
	default:
		return nil, guardFailure("/events", "qualified_native_event")
	}
	if err != nil {
		return nil, err
	}
	if len(frame) == 0 {
		return nil, nil
	}
	return [][]byte{frame}, nil
}
func stringField(fields map[string]json.RawMessage, name string) string {
	var out string
	_ = json.Unmarshal(fields[name], &out)
	return out
}
func (p *Plan) declaresTool(name string) bool {
	for _, tool := range member(p.effective.Root(), "tools").Elements() {
		if valueText(member(tool, "name")) == name {
			return true
		}
	}
	return false
}

// Complete runs only after the existing native stream grammar has accepted a
// terminal result. It builds a bounded delivery; the caller still must commit
// it and its dependency state before emitting the queued frames or handle.
func (p *ToolProjection) Complete(completion *openai.Completion, handle string) (*Continuation, Delivery, error) {
	if !p.terminal || completion == nil || completion.Usage == nil || len(p.nativeUsage) == 0 || handle == "" {
		return nil, Delivery{}, guardFailure("/result", "complete_recoverable_result")
	}
	assistant := map[string]any{"role": "assistant", "content": p.content.String()}
	if len(p.calls) > 0 {
		assistant["tool_calls"] = p.calls
	}
	assistantRaw, _ := json.Marshal(assistant)
	blocks, _ := json.Marshal(p.blocks)
	state := &Continuation{Version: ContinuationV1, Source: p.plan.prepared.Request().Document().Bytes(), NativeRequest: p.plan.Body(), Blocks: blocks, Assistant: assistantRaw}
	usage := map[string]any{"prompt_tokens": completion.Usage.InputTokens, "completion_tokens": completion.Usage.OutputTokens, "total_tokens": completion.Usage.TotalTokens}
	if completion.Usage.CachedInputTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *completion.Usage.CachedInputTokens}
	}
	extension := map[string]any{"version": ContinuationV1, "handle": handle, "ready": true, "native_usage": p.nativeUsage}
	finish := completion.FinishReason
	if p.finish == "tool_use" {
		finish = "tool_calls"
	}
	terminal, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage, "olp": extension})
	if err := p.retain(len(terminal) + len(assistantRaw) + len(blocks) + len(state.Source) + len(state.NativeRequest)); err != nil {
		return nil, Delivery{}, err
	}
	frames := append([]json.RawMessage{}, p.frames...)
	frames = append(frames, terminal)
	extension["observations"] = p.observations
	body, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": finish}}, "usage": usage, "olp": extension})
	delivery := Delivery{Stream: p.plan.stream}
	if p.plan.stream {
		delivery.Frames = frames
	} else {
		delivery.Body = body
	}
	return state, delivery, nil
}

// ProjectUnary uses the same native block/correspondence projection as streaming,
// after the normal native result codec has established its grammar and usage.
func (p *Plan) ProjectUnary(completion *openai.Completion, handle string, limit int) (*Continuation, Delivery, error) {
	projection, err := p.NewToolProjection(limit)
	if err != nil {
		return nil, Delivery{}, err
	}
	document := completion.Native.Source()
	root := document.Root()
	if !onlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
		return nil, Delivery{}, guardFailure("/result", "qualified_native_result")
	}
	// Reuse the incremental block guards without passing through a legacy codec.
	projection.plan = &Plan{template: p.template, config: p.config, prepared: p.prepared, effective: p.effective, sourceFamily: p.sourceFamily, stream: true, route: p.route, receipt: p.receipt}
	seq := uint64(0)
	observe := func(name string, body any) error {
		raw, _ := json.Marshal(body)
		doc, e := oif.ParseJSON(raw, oif.Limits{MaxBytes: limit})
		if e != nil {
			return e
		}
		event, e := oif.NewEvent(openai.Descriptor(openai.FamilyAnthropic, true), doc, name, seq)
		seq++
		if e != nil {
			return e
		}
		_, e = projection.Observe(event)
		return e
	}
	fields := document.Fields()
	fields["content"] = json.RawMessage(`[]`)
	fields["stop_reason"] = json.RawMessage(`null`)
	if err = observe("message_start", map[string]any{"type": "message_start", "message": fields}); err != nil {
		return nil, Delivery{}, err
	}
	for i, block := range member(root, "content").Elements() {
		if err = observe("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": json.RawMessage(block.Bytes())}); err != nil {
			return nil, Delivery{}, err
		}

		if err = observe("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}); err != nil {
			return nil, Delivery{}, err
		}
	}
	if err = observe("message_delta", map[string]any{"type": "message_delta", "delta": map[string]json.RawMessage{"stop_reason": member(root, "stop_reason").Bytes(), "stop_sequence": member(root, "stop_sequence").Bytes()}, "usage": json.RawMessage(member(root, "usage").Bytes())}); err != nil {
		return nil, Delivery{}, err
	}
	if err = observe("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return nil, Delivery{}, err
	}
	projection.plan = p
	return projection.Complete(completion, handle)
}

// Frame wraps projected SDK JSON as SSE without decoding its native numbers.
func ContinuationFrame(raw []byte) []byte {
	return bytes.Join([][]byte{[]byte("data: "), raw, []byte("\n\n")}, nil)
}

func ContinuationActionable(frame []byte) bool {
	document, err := oif.ParseJSON(frame, oif.Limits{})
	if err != nil {
		return false
	}
	for _, choice := range member(document.Root(), "choices").Elements() {
		if len(member(member(choice, "delta"), "tool_calls").Elements()) > 0 {
			return true
		}
	}
	return false
}
