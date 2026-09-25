package interaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// NativeTerminal is the bounded terminal observation the negotiated carrier
// retains for one accepted native result. It records the dialect reducer's
// admitted terminal facts verbatim beside the compatible client-protocol
// finish reason; it is never reconstructed from the narrowed Chat output.
type NativeTerminal struct {
	// Reason is the declared native stop_reason at the terminal boundary.
	Reason string `json:"stop_reason"`
	// Sequence carries the matched stop_sequence exactly as the native
	// contract defines it: a string when the terminal declared a match, an
	// explicit null when it declared none, and an absent member when no
	// admitted update carried the member at all.
	Sequence json.RawMessage `json:"stop_sequence,omitempty"`
	// Finish is the compatible client-protocol finish reason emitted beside
	// this record.
	Finish string `json:"finish_reason"`
}

// Valid reports whether a decoded record satisfies the admitted carrier
// grammar: a qualified native reason, the compatible finish it projects to,
// and the matched-sequence correspondence the native contract defines. A
// stored record that fails this shape was never committed by this contract.
func (t *NativeTerminal) Valid() bool {
	if t == nil {
		return false
	}
	switch t.Reason {
	case "end_turn", "tool_use", "max_tokens", "stop_sequence":
	default:
		return false
	}
	switch t.Finish {
	case "stop", "tool_calls", "length":
	default:
		return false
	}
	matched := len(t.Sequence) != 0 && string(t.Sequence) != "null"
	if matched {
		var sequence string
		if json.Unmarshal(t.Sequence, &sequence) != nil || sequence == "" {
			return false
		}
	}
	return (t.Reason == "stop_sequence") == matched
}

// ContinuationActions is the committed explicit actionability claim of one
// ready delivery, independent of the ready handle itself. The handle means the
// dependency and its recorded delivery are recoverable; this member states
// which next actions the admitted native outcome actually permits. A tool call
// identity is listed only when the native terminal yielded tool use, the call
// carried complete validated arguments, the ordered dependencies were
// retained, and the whole-turn durability barrier committed before any
// actionable byte was published. A partial or non-tool outcome is recoverable
// while claiming no tool action; a delivery committed before this member
// existed decodes with a nil Actions and reads as explicitly unavailable.
type ContinuationActions struct {
	// ToolCalls lists the ordered call identities the qualified client may
	// answer with tool results. An empty member is an explicit "no tool
	// action" claim; it never means "run these tools" by implication.
	ToolCalls []string `json:"tool_calls"`
}

// UnmarshalJSON enforces the committed carrier grammar at the member level:
// exactly the tool_calls member, so a stored claim carrying anything else —
// or omitting it — was never committed by this contract and fails decode.
func (a *ContinuationActions) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	if len(members) != 1 {
		return errors.New("continuation actions carry members outside the carrier")
	}
	var calls []string
	if err := json.Unmarshal(members["tool_calls"], &calls); err != nil {
		return err
	}
	a.ToolCalls = calls
	return nil
}

// Valid reports whether a decoded claim satisfies the committed carrier
// grammar: an explicit tool_calls member of bounded nonempty identities. A
// stored claim that fails this shape was never committed by this contract.
func (a *ContinuationActions) Valid() bool {
	if a == nil || a.ToolCalls == nil || len(a.ToolCalls) > 1024 {
		return false
	}
	for _, id := range a.ToolCalls {
		if id == "" {
			return false
		}
	}
	return true
}

// Delivery is already projected client data. The gateway commits it with the
// complete native dependency before publishing any queued actionable frame.
// Replaying this value never invokes a provider or claims new token usage.
// Terminal and Actions are additive on the carrier: deliveries committed
// before those records existed decode with nil members and read as explicitly
// unavailable rather than upgrading to a tool yield on replay.
type Delivery struct {
	Stream   bool                 `json:"stream"`
	Frames   []json.RawMessage    `json:"frames,omitempty"`
	Body     json.RawMessage      `json:"body,omitempty"`
	Terminal *NativeTerminal      `json:"native_terminal,omitempty"`
	Actions  *ContinuationActions `json:"actions,omitempty"`
}
type ToolProjection struct {
	plan  *Plan
	trace *protocols.AnthropicTrace
	limit int
	// Separately bounded resources (A05): dependency is the retained native
	// state materialized into the persisted Continuation; delivery is
	// projected client output retained for replay; transient is live
	// in-flight event memory released as soon as it is no longer held;
	// eventWork is the aggregate admitted-event work counter that also
	// covers events the projection discards (pings, native error envelopes).
	dependency, delivery, transient, eventWork                                  streamBudget
	blocksCharged, contentCharged, callsCharged, usageCharged, observationsHeld int
	blocks                                                                      []json.RawMessage
	active                                                                      map[int]*toolBlock
	frames                                                                      []json.RawMessage
	observations                                                                []json.RawMessage
	nativeUsage                                                                 map[string]json.RawMessage
	blocked                                                                     bool
	id, model                                                                   string
	toolCount                                                                   int
	content                                                                     strings.Builder
	calls                                                                       []map[string]any
}
type toolBlock struct {
	fields                map[string]json.RawMessage
	kind                  string
	text, signature, args strings.Builder
	held                  int // transient bytes currently charged to this open block
}

// streamBudget is one separately bounded resource of a projection. Retained
// budgets (dependency, delivery, eventWork) only grow; the transient budget
// is a gauge — credit is released as soon as the held representation is
// dropped or materializes into a retained budget.
type streamBudget struct {
	resource string
	category LimitCategory
	limit    int
	used     int
}

func (b *streamBudget) charge(n int) error {
	if n < 0 || n > b.limit-b.used {
		return &Exhaustion{Resource: b.resource, Category: b.category, Limit: b.limit}
	}
	b.used += n
	return nil
}

func (b *streamBudget) release(n int) {
	if n >= b.used {
		b.used = 0
		return
	}
	b.used -= n
}

// Accounting reports the projection's separated resource counters so callers
// and tests can distinguish retained dependency state, projected replay
// delivery, live transient memory and aggregate admitted event work.
type Accounting struct {
	RetainedDependencyBytes int
	ProjectedDeliveryBytes  int
	TransientEventBytes     int
	AdmittedEventWorkBytes  int
}

func (p *ToolProjection) Accounting() Accounting {
	return Accounting{
		RetainedDependencyBytes: p.dependency.used,
		ProjectedDeliveryBytes:  p.delivery.used,
		TransientEventBytes:     p.transient.used,
		AdmittedEventWorkBytes:  p.eventWork.used,
	}
}

func (p *Plan) ToolContinuation() bool {
	return p != nil && p.receipt.Obligations.Continuation == ContinuationV1
}
func (p *Plan) NewToolProjection(limit int) (*ToolProjection, error) {
	if !p.ToolContinuation() || limit < 1 || limit > 4<<20 {
		return nil, continuationFailure("/client_contract", "bounded_tool_projection")
	}
	return &ToolProjection{
		plan: p, trace: protocols.NewAnthropicTrace(limit), limit: limit, active: map[int]*toolBlock{},
		dependency: streamBudget{resource: "retained_dependency_bytes", category: LimitBytes, limit: limit},
		delivery:   streamBudget{resource: "projected_delivery_bytes", category: LimitBytes, limit: limit},
		transient:  streamBudget{resource: "transient_event_bytes", category: LimitBytes, limit: limit},
		eventWork:  streamBudget{resource: "admitted_event_work_bytes", category: LimitEventWork, limit: limit},
	}, nil
}
func (p *ToolProjection) chunk(delta any, observation map[string]any) ([]byte, error) {
	extension := map[string]any{"version": ContinuationV1, "observation": observation}
	frame, err := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, "olp": extension})
	if err != nil {
		return nil, err
	}
	observationRaw, _ := json.Marshal(observation)
	// The projected frame is delivery retained for replay — it is not
	// retained dependency state. The aggregate observations array is held
	// bookkeeping memory: unary delivery embeds it in the body (charged when
	// Complete serializes that body) while streaming drops it at Complete.
	if err = p.delivery.charge(len(frame)); err != nil {
		return nil, err
	}
	if err = p.transient.charge(len(observationRaw)); err != nil {
		return nil, err
	}
	p.observationsHeld += len(observationRaw)
	p.frames = append(p.frames, json.RawMessage(frame))
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
		raw := field.Value.Bytes()
		// The retained usage map is in-flight memory until Complete
		// materializes it into the terminal delivery; replacement members
		// release their transient credit.
		if old, present := p.nativeUsage[field.Name]; present {
			p.transient.release(len(old))
			p.usageCharged -= len(old)
		}
		if err := p.transient.charge(len(raw)); err != nil {
			return err
		}
		p.usageCharged += len(raw)
		p.nativeUsage[field.Name] = raw
	}
	return nil
}

// Observe consumes one reducer-admitted native event synchronously. Event
// identity, ordering, block lifecycle and cumulative usage come from the
// dialect-owned AnthropicTrace; this type only applies the qualified
// projection contract on top. Before a tool starts, non-actionable
// text/reasoning observations are incremental. A tool turn's complete
// assistant history (including later blocks/signatures) is required to
// reconstruct the next native request, so that dependency gates its tool
// bytes.
func (p *ToolProjection) Observe(event oif.Event) ([][]byte, error) {
	if err := p.plan.ValidateEvent(event); err != nil {
		return nil, err
	}
	tr, err := p.trace.Accept(event)
	if err != nil {
		// The reducer's own bound guards live partial tool arguments — the
		// transient event memory it must hold while a block stays open.
		if errors.Is(err, openai.ErrEventTooLarge) {
			return nil, &Exhaustion{Resource: "transient_event_bytes", Category: LimitBytes, Limit: p.limit}
		}
		return nil, err
	}
	// Every admitted event spends aggregate event work — transport, parse and
	// projection effort proportional to its source — including events the
	// grammar drops. The source envelope itself is transient credit that
	// releases when Observe returns because nothing here retains it.
	sourceBytes := event.Source().Len()
	if err := p.eventWork.charge(sourceBytes); err != nil {
		return nil, err
	}
	if err := p.transient.charge(sourceBytes); err != nil {
		return nil, err
	}
	defer p.transient.release(sourceBytes)
	root := event.Source().Root()
	var frame []byte
	switch tr.Kind {
	case protocols.AnthropicPing, protocols.AnthropicError:
		// Admitted transport traffic with no projected output; the stream
		// codec reports a native error event's upstream detail itself. The
		// source consumed event work and held a transient envelope, but
		// retains no dependency or delivery state.
		return nil, nil
	case protocols.AnthropicMessageStart:
		message := tr.Message
		if !onlyMembers(root, "type message") || !onlyMembers(message, "id type role model content stop_reason stop_sequence usage") || len(member(message, "content").Elements()) != 0 {
			return nil, guardFailure("/events", "message_start_contract")
		}
		p.id = valueText(member(message, "id"))
		p.model = valueText(member(message, "model"))
		if usage := tr.Usage; usage.Kind() == oif.Object {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
	case protocols.AnthropicBlockStart:
		if !onlyMembers(root, "type index content_block") {
			return nil, guardFailure("/events", "block_start_contract")
		}
		index, e := toolIndex(member(root, "index"))
		if e != nil {
			return nil, e
		}
		if index != len(p.blocks) {
			return nil, guardFailure("/events/index", "ordered_block_start")
		}
		block := tr.Block
		b := &toolBlock{fields: map[string]json.RawMessage{}, kind: valueText(member(block, "type"))}
		held := 0
		for _, f := range block.Members() {
			raw := f.Value.Bytes()
			held += len(raw)
			b.fields[f.Name] = raw
		}
		switch b.kind {
		case "text":
			if !onlyMembers(block, "type text") || member(block, "text").Kind() != oif.String {
				return nil, guardFailure("/content", "text_block")
			}
			text := valueText(member(block, "text"))
			b.text.WriteString(text)
			held += len(text)
		case "thinking":
			if !onlyMembers(block, "type thinking signature") || member(block, "thinking").Kind() != oif.String || member(block, "signature").Kind() != oif.String {
				return nil, guardFailure("/content", "thinking_block")
			}
			thinking, signature := valueText(member(block, "thinking")), valueText(member(block, "signature"))
			b.text.WriteString(thinking)
			b.signature.WriteString(signature)
			held += len(thinking) + len(signature)
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
				args := member(block, "input").Raw()
				b.args.WriteString(args)
				held += len(args)
			}
		default:
			return nil, guardFailure("/content", "qualified_native_block")
		}
		// The open block's member copies and accumulating builders are
		// transient memory for the rest of the block's lifetime.
		if err := p.transient.charge(held); err != nil {
			return nil, err
		}
		b.held = held
		p.blocks = append(p.blocks, nil)
		p.active[index] = b
		change := map[string]any{}
		observation := map[string]any{"index": index, "type": b.kind, "phase": "start"}
		if b.text.Len() > 0 {
			observation["text"] = b.text.String()
			if b.kind == "text" {
				change["content"] = b.text.String()
				if err := p.transient.charge(b.text.Len()); err != nil {
					return nil, err
				}
				p.content.WriteString(b.text.String())
				p.contentCharged += b.text.Len()
			}
		}
		frame, err = p.chunk(change, observation)
	case protocols.AnthropicBlockDelta:
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
		delta := tr.Delta
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
			if err := p.transient.charge(len(text)); err != nil {
				return nil, err
			}
			b.text.WriteString(text)
			b.held += len(text)
			change := map[string]any{}
			if b.kind == "text" {
				change["content"] = text
				if err := p.transient.charge(len(text)); err != nil {
					return nil, err
				}
				p.content.WriteString(text)
				p.contentCharged += len(text)
			}
			frame, err = p.chunk(change, map[string]any{"index": index, "type": b.kind, "phase": "delta", "text": text})
		case "signature_delta":
			if b.kind != "thinking" || !onlyMembers(delta, "type signature") || member(delta, "signature").Kind() != oif.String {
				return nil, guardFailure("/events/delta", "native_signature_delta")
			}
			signature := valueText(member(delta, "signature"))
			if err := p.transient.charge(len(signature)); err != nil {
				return nil, err
			}
			b.signature.WriteString(signature)
			b.held += len(signature)
		case "input_json_delta":
			if b.kind != "tool_use" || !onlyMembers(delta, "type partial_json") || member(delta, "partial_json").Kind() != oif.String {
				return nil, guardFailure("/events/delta", "native_tool_arguments")
			}
			part := valueText(member(delta, "partial_json"))
			if err := p.transient.charge(len(part)); err != nil {
				return nil, err
			}
			b.args.WriteString(part)
			b.held += len(part)
		default:
			return nil, guardFailure("/events/delta", "qualified_native_delta")
		}
	case protocols.AnthropicBlockStop:
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
		hold := func(raw json.RawMessage) error {
			if e := p.transient.charge(len(raw)); e != nil {
				return e
			}
			b.held += len(raw)
			return nil
		}
		switch b.kind {
		case "text":
			raw, _ := json.Marshal(b.text.String())
			if e := hold(raw); e != nil {
				return nil, e
			}
			b.fields["text"] = raw
		case "thinking":
			if b.signature.Len() == 0 {
				return nil, guardFailure("/content", "complete_native_signature")
			}
			thinking, _ := json.Marshal(b.text.String())
			signature, _ := json.Marshal(b.signature.String())
			if e := hold(thinking); e != nil {
				return nil, e
			}
			if e := hold(signature); e != nil {
				return nil, e
			}
			b.fields["thinking"] = thinking
			b.fields["signature"] = signature
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
			if e := hold(document.Bytes()); e != nil {
				return nil, e
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
			callBytes, _ := json.Marshal(call)
			// The recorded call entry is held until Complete builds the
			// assistant representation.
			if e := p.transient.charge(len(callBytes)); e != nil {
				return nil, e
			}
			p.callsCharged += len(callBytes)
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
		// The closed block's marshal is the actual retained dependency
		// representation; the open-block copies and builders release their
		// transient credit.
		if err = p.dependency.charge(len(p.blocks[index])); err != nil {
			return nil, err
		}
		p.blocksCharged += len(p.blocks[index])
		p.transient.release(b.held)
		delete(p.active, index)
		frame, err = p.chunk(delta, observation)
	case protocols.AnthropicMessageDelta:
		if !onlyMembers(root, "type delta usage") || len(p.active) > 0 {
			return nil, guardFailure("/events", "message_delta_contract")
		}
		if usage := tr.Usage; usage.Kind() == oif.Object {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
		if delta := tr.Delta; delta.Kind() == oif.Object && !onlyMembers(delta, "stop_reason stop_sequence") {
			return nil, guardFailure("/events", "terminal_metadata")
		}
		// Interim updates may carry no terminal declaration; once declared,
		// the same native fact is revalidated on every later update.
		if reason, declared := p.trace.StopReason(); declared {
			if reason != "end_turn" && reason != "tool_use" && reason != "max_tokens" && reason != "stop_sequence" {
				return nil, guardFailure("/events/stop_reason", "known_terminal")
			}
			if (reason == "tool_use") != (p.toolCount > 0) {
				return nil, guardFailure("/events/stop_reason", "tool_terminal_correspondence")
			}
			// A matched sequence belongs only to a stop_sequence terminal.
			if _, matched := p.trace.StopSequence(); matched && reason != "stop_sequence" {
				return nil, guardFailure("/events/stop_sequence", "terminal_sequence_correspondence")
			}
		}
	case protocols.AnthropicMessageStop:
		reason, declared := p.trace.StopReason()
		sequence, matched := p.trace.StopSequence()
		if !onlyMembers(root, "type") || !declared || len(p.active) > 0 {
			return nil, guardFailure("/events", "complete_native_terminal")
		}
		// The native contract defines stop_sequence as the matched sequence
		// that caused the stop: a stop_sequence reason without a declared
		// match — or a declared match under any other reason — contradicts
		// the accepted terminal. An empty match is no match at all and would
		// produce a record the committed carrier itself rejects.
		if (reason == "stop_sequence") != matched || (matched && sequence == "") {
			return nil, guardFailure("/events/stop_sequence", "terminal_sequence_correspondence")
		}
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

// Complete runs only after the authoritative native reducer has accepted the
// terminal boundary. It builds a bounded delivery; the caller still must commit
// it and its dependency state before emitting the queued frames or handle.
func (p *ToolProjection) Complete(completion *openai.Completion, handle string) (*Continuation, Delivery, error) {
	reason, declared := p.trace.StopReason()
	sequence, matched := p.trace.StopSequence()
	// The terminal/content correspondence is revalidated here so a record can
	// never be completed from a trace whose declared reason and matched
	// sequence contradict each other.
	if !p.trace.Terminal() || completion == nil || completion.Usage == nil || len(p.nativeUsage) == 0 || handle == "" || !declared || (reason == "stop_sequence") != matched || (matched && sequence == "") {
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
	// The terminal observation comes only from the reducer's admitted native
	// facts — the declared reason, the matched-sequence member presence — with
	// the compatible client finish reason recorded beside it, never derived
	// back from the narrowed projection.
	finish := completion.FinishReason
	if reason == "tool_use" {
		finish = "tool_calls"
	}
	terminalRecord := &NativeTerminal{Reason: reason, Finish: finish}
	switch p.trace.StopSequencePresence() {
	case oif.Present:
		sequence, _ := p.trace.StopSequence()
		terminalRecord.Sequence, _ = json.Marshal(sequence)
	case oif.ExplicitNull:
		terminalRecord.Sequence = json.RawMessage("null")
	}
	recordRaw, _ := json.Marshal(terminalRecord)
	// The explicit actionability claim is computed from the admitted native
	// outcome: a tool action exists only when the native terminal yielded
	// tool use and every call's complete arguments and ordered dependencies
	// already validated. Any other terminal — output limit, end of turn or a
	// matched stop sequence — commits ready with an empty claim, never with
	// an implied one.
	actions := &ContinuationActions{ToolCalls: []string{}}
	if reason == "tool_use" {
		for _, call := range p.calls {
			id, _ := call["id"].(string)
			if id == "" {
				return nil, Delivery{}, guardFailure("/result", "complete_recoverable_result")
			}
			actions.ToolCalls = append(actions.ToolCalls, id)
		}
	}
	actionsRaw, _ := json.Marshal(actions)
	extension := map[string]any{"version": ContinuationV1, "handle": handle, "ready": true, "native_usage": p.nativeUsage, "native_terminal": terminalRecord, "actions": actions}
	terminal, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage, "olp": extension})
	// Retained dependency: the persisted interaction representation — source,
	// native request, serialized assistant and the block array's own
	// serialization overhead (member bytes were charged as blocks closed).
	if err := p.dependency.charge(len(assistantRaw) + len(state.Source) + len(state.NativeRequest) + len(blocks) - p.blocksCharged); err != nil {
		return nil, Delivery{}, err
	}
	// The terminal record and action claim are retained delivery state
	// committed on the carrier in both modes; their standalone serializations
	// charge the delivery budget once here while the copies embedded in the
	// terminal frame or unary body are charged with those serializations.
	if err := p.delivery.charge(len(recordRaw) + len(actionsRaw)); err != nil {
		return nil, Delivery{}, err
	}
	delivery := Delivery{Stream: p.plan.stream, Terminal: terminalRecord, Actions: actions}
	extension["observations"] = p.observations
	if p.plan.stream {
		// The terminal frame is replayed delivery; its serialization is
		// charged where it is actually retained.
		if err := p.delivery.charge(len(terminal)); err != nil {
			return nil, Delivery{}, err
		}
		frames := append([]json.RawMessage{}, p.frames...)
		frames = append(frames, terminal)
		delivery.Frames = frames
	} else {
		body, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion", "created": 0, "model": p.plan.route, "choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": finish}}, "usage": usage, "olp": extension})
		if err := p.delivery.charge(len(body)); err != nil {
			return nil, Delivery{}, err
		}
		delivery.Body = body
	}
	// The in-flight content, call, usage and aggregate-observation
	// representations have materialized into the retained state and delivery
	// (or are dropped, for the streaming aggregate); their transient credit
	// releases.
	p.transient.release(p.contentCharged + p.callsCharged + p.usageCharged + p.observationsHeld)
	p.contentCharged, p.callsCharged, p.usageCharged, p.observationsHeld = 0, 0, 0, 0
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
	// The synthesized events are admitted through the same authoritative
	// reducer as wire traffic — no parallel grammar runs for unary results.
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
	// Anthropic message_start always opens with both terminal members null;
	// the result's terminal facts enter through the synthesized delta below.
	fields["stop_sequence"] = json.RawMessage(`null`)
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
	// The synthesized update preserves the result's terminal members exactly:
	// an absent stop_sequence stays absent rather than collapsing into null.
	delta := map[string]json.RawMessage{"stop_reason": member(root, "stop_reason").Bytes()}
	if sequence, present := root.Lookup("stop_sequence"); present {
		delta["stop_sequence"] = sequence.Bytes()
	}
	if err = observe("message_delta", map[string]any{"type": "message_delta", "delta": delta, "usage": json.RawMessage(member(root, "usage").Bytes())}); err != nil {
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
