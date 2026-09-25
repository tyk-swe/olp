package protocols

import (
	"encoding/json"
	"maps"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// AnthropicKind identifies the grammar-defined event types of the Anthropic
// Messages streaming dialect (anthropic-messages/wire-v1).
type AnthropicKind uint8

const (
	// AnthropicUnknown marks a valid event whose type the dialect does not
	// recognize; it is admitted for native extensibility.
	AnthropicUnknown AnthropicKind = iota
	AnthropicPing
	AnthropicError
	AnthropicMessageStart
	AnthropicBlockStart
	AnthropicBlockDelta
	AnthropicBlockStop
	AnthropicMessageDelta
	AnthropicMessageStop
)

// AnthropicEffect is the disposition the grammar grants a consumer for one
// admitted transition. It is a ceiling: a qualified projection may permit
// less, but no consumer may emit output the grammar did not admit.
type AnthropicEffect uint8

const (
	// AnthropicForward permits the consumer to forward the admitted event
	// source/framing or project it into a client-facing frame.
	AnthropicForward AnthropicEffect = iota + 1
	// AnthropicDrop admits the event as valid transport traffic that carries
	// no client-visible content; consumers must not emit output for it.
	AnthropicDrop
)

// AnthropicTransition describes one validated native event together with the
// facts the reducer extracted from it. A transition is issued only for events
// the grammar admits; a rejected event produces no transition and no output.
type AnthropicTransition struct {
	// Event is the admitted OIF event. Its source document and SSE framing
	// are untouched and safe to forward on native paths.
	Event oif.Event
	// Kind classifies the payload's declared type.
	Kind AnthropicKind
	// Type is the payload's declared "type" member, validated against the
	// SSE event name when the wire carries one.
	Type string
	// Effect states the maximum disposition the grammar permits.
	Effect AnthropicEffect

	// Message carries the message_start "message" object; absent otherwise.
	Message oif.Value
	// Usage carries the event's own usage member; absent when the event has
	// none. Cumulative usage lives on the trace (see AnthropicTrace.Usage).
	Usage oif.Value

	// Index is the content-block position for AnthropicBlock* events.
	Index int64
	// Block carries the content_block_start "content_block" object.
	Block oif.Value
	// BlockKind is the effective block kind for AnthropicBlock* events:
	// "text", "tool_use", or "" for extension blocks the grammar does not
	// interpret.
	BlockKind string
	// Delta carries the "delta" object of content_block_delta and
	// message_delta; absent or null when the event omits it.
	Delta oif.Value
	// Arguments is the complete validated tool-input text at
	// content_block_stop on a tool_use block; "" otherwise.
	Arguments string
}

// AnthropicTrace is the dialect-owned stateful reducer for one Anthropic
// Messages stream. It is the single authority for event identity, ordering,
// block lifecycle, cumulative usage, and terminal facts for the
// anthropic-messages/wire-v1 dialect. Native forwarding and qualified
// projections both consume its transitions instead of re-deriving grammar.
//
// The grammar accepts the legal stream variants Anthropic emits:
//
//	message_start → content_block_* (indexed lifecycle) → message_delta* → message_stop
//
// Multiple top-level message_delta updates may carry cumulative usage or
// repeated/consistent terminal facts; message_stop, not stop_reason, is the
// terminal boundary. Usage updates omit fields they do not change; omission
// never erases a previously declared count.
type AnthropicTrace struct {
	limit     int
	blocks    map[int64]*contentBlock
	next      int64
	retained  int
	usage     Object
	started   bool
	updating  bool
	done      bool
	reason    string
	hasReason bool
	stopSeq   string
	hasSeq    bool
}

// NewAnthropicTrace opens a reducer for one native Anthropic stream. limit
// bounds the bytes retained for open tool-argument blocks; callers should
// pass their transport or continuation byte bound.
func NewAnthropicTrace(limit int) *AnthropicTrace {
	if limit < 1 {
		limit = 1
	}
	return &AnthropicTrace{
		limit:  limit,
		blocks: map[int64]*contentBlock{},
		usage:  Object{},
	}
}

// Started reports whether message_start has been admitted.
func (t *AnthropicTrace) Started() bool { return t.started }

// Terminal reports whether the native terminal boundary (message_stop) has
// been admitted. Only a terminal trace is a complete trace.
func (t *AnthropicTrace) Terminal() bool { return t.done }

// StopReason returns the declared top-level stop_reason and whether one has
// been declared. A null or absent stop_reason never satisfies the flag.
func (t *AnthropicTrace) StopReason() (string, bool) { return t.reason, t.hasReason }

// StopSequence returns the declared stop_sequence string and whether one has
// been declared; presence is distinct from value.
func (t *AnthropicTrace) StopSequence() (string, bool) { return t.stopSeq, t.hasSeq }

// Usage returns the cumulative native usage document: a merge of every
// admitted usage member where absent fields never erase earlier values.
func (t *AnthropicTrace) Usage() Object { return maps.Clone(t.usage) }

// Accept validates one lifted OIF event against the native stream grammar and
// returns its transition. Rejected events return an error and produce no
// transition, so consumers emit nothing for them; transitions admitted before
// the failure remain delivered. Once message_stop has been admitted every
// later event is a protocol violation.
func (t *AnthropicTrace) Accept(event oif.Event) (*AnthropicTransition, error) {
	f := event.Source().Fields()
	if f == nil {
		return nil, protocolError("invalid Anthropic event")
	}
	typ := str(f["type"])
	if typ == "" {
		return nil, protocolError("missing Anthropic event type")
	}
	if name := event.Name(); name != "" && name != typ {
		return nil, protocolError("event name disagrees with type")
	}
	if t.done {
		return nil, protocolError("event after message_stop")
	}
	tr := &AnthropicTransition{Event: event, Type: typ, Effect: AnthropicForward}
	root := event.Source().Root()
	switch typ {
	case "ping":
		tr.Kind, tr.Effect = AnthropicPing, AnthropicDrop
	case "error":
		tr.Kind, tr.Effect = AnthropicError, AnthropicDrop
	case "message_start":
		if t.started {
			return nil, protocolError("duplicate message start")
		}
		message, e := object(f["message"])
		if e != nil || str(message["role"]) != "assistant" || str(message["type"]) != "message" {
			return nil, protocolError("invalid message start")
		}
		if e := t.mergeUsage(message["usage"]); e != nil {
			return nil, e
		}
		t.started = true
		tr.Kind = AnthropicMessageStart
		tr.Message, _ = root.Lookup("message")
		tr.Usage, _ = tr.Message.Lookup("usage")
	case "content_block_start":
		index, ok := count(f["index"])
		if !t.started || t.updating || !ok || index != t.next {
			return nil, protocolError("invalid content block start sequence")
		}
		if len(t.blocks) >= max(1, t.limit/32) {
			return nil, protocolError("too many active content blocks")
		}
		t.next++
		block, e := object(f["content_block"])
		if e != nil {
			return nil, protocolError("invalid content block")
		}
		kind := str(block["type"])
		if kind == "tool_use" && (str(block["id"]) == "" || str(block["name"]) == "") {
			return nil, protocolError("incomplete tool start")
		}
		state := &contentBlock{kind: kind}
		if kind == "tool_use" {
			input, err := object(block["input"])
			if err != nil {
				return nil, protocolError("invalid tool input")
			}
			if len(input) > 0 {
				state.args = string(block["input"])
				t.retained += len(state.args)
			}
		} else if kind != "text" {
			// Extension blocks stay opaque: they are tracked for
			// lifecycle but their deltas carry no delta-type checks.
			state.kind = ""
		}
		if t.retained > t.limit {
			return nil, openai.ErrEventTooLarge
		}
		t.blocks[index] = state
		tr.Kind = AnthropicBlockStart
		tr.Index = index
		tr.Block, _ = root.Lookup("content_block")
		tr.BlockKind = state.kind
	case "content_block_delta":
		index, ok := count(f["index"])
		block := t.blocks[index]
		if !t.started || t.updating || !ok || block == nil {
			return nil, protocolError("delta outside an active content block")
		}
		delta, e := object(f["delta"])
		if e != nil {
			return nil, protocolError("invalid content delta")
		}
		switch str(delta["type"]) {
		case "text_delta":
			if block.kind != "text" {
				return nil, protocolError("text delta for non-text block")
			}
			if _, ok := rawString(delta["text"]); !ok {
				return nil, protocolError("invalid text delta")
			}
		case "input_json_delta":
			if block.kind != "tool_use" {
				return nil, protocolError("tool delta for non-tool block")
			}
			part, ok := rawString(delta["partial_json"])
			if !ok {
				return nil, protocolError("invalid tool delta")
			}
			if t.retained+len(part) > t.limit {
				return nil, openai.ErrEventTooLarge
			}
			t.retained += len(part)
			block.args += part
		}
		tr.Kind = AnthropicBlockDelta
		tr.Index = index
		tr.BlockKind = block.kind
		tr.Delta, _ = root.Lookup("delta")
	case "content_block_stop":
		index, ok := count(f["index"])
		block := t.blocks[index]
		if !t.started || t.updating || !ok || block == nil {
			return nil, protocolError("stop outside an active content block")
		}
		if block.kind == "tool_use" && block.args != "" {
			if _, e := object([]byte(block.args)); e != nil {
				return nil, protocolError("incomplete tool arguments")
			}
		}
		t.retained -= len(block.args)
		delete(t.blocks, index)
		tr.Kind = AnthropicBlockStop
		tr.Index = index
		tr.BlockKind = block.kind
		tr.Arguments = block.args
	case "message_delta":
		if !t.started || len(t.blocks) > 0 {
			return nil, protocolError("message delta outside the update phase")
		}
		delta, e := optionalObject(f["delta"])
		if e != nil {
			return nil, protocolError("invalid message delta")
		}
		t.updating = true
		if delta != nil {
			if e := t.declareTerminal(delta); e != nil {
				return nil, e
			}
		}
		if e := t.mergeUsage(f["usage"]); e != nil {
			return nil, e
		}
		tr.Kind = AnthropicMessageDelta
		tr.Delta, _ = root.Lookup("delta")
		tr.Usage, _ = root.Lookup("usage")
	case "message_stop":
		if !t.started || len(t.blocks) > 0 {
			return nil, protocolError("message stopped before completion")
		}
		t.done = true
		tr.Kind = AnthropicMessageStop
	default:
		if !t.started {
			return nil, protocolError("event before message start")
		}
		tr.Kind = AnthropicUnknown
	}
	return tr, nil
}

// declareTerminal folds one message_delta object's terminal facts into the
// trace. Anthropic sends null stop_reason/stop_sequence in interim updates, so
// null and absent both mean "not yet declared". Later updates may repeat the
// declared facts but never contradict or unset them.
func (t *AnthropicTrace) declareTerminal(delta Object) error {
	if v := delta["stop_reason"]; present(v) {
		reason, ok := rawString(v)
		if !ok {
			return protocolError("invalid stop reason")
		}
		if t.hasReason && t.reason != reason {
			return protocolError("inconsistent stop reason")
		}
		t.reason, t.hasReason = reason, true
	}
	if v := delta["stop_sequence"]; present(v) {
		seq, ok := rawString(v)
		if !ok {
			return protocolError("invalid stop sequence")
		}
		if t.hasSeq && t.stopSeq != seq {
			return protocolError("inconsistent stop sequence")
		}
		t.stopSeq, t.hasSeq = seq, true
	}
	return nil
}

// mergeUsage folds one event's usage object into the cumulative document.
// Omitted members keep their earlier values. Known count members must be
// non-negative integers when present; malformed counts and contradictory
// cache details reject the event. The merged document is revalidated with the
// same usage contract the response codec applies.
func (t *AnthropicTrace) mergeUsage(v json.RawMessage) error {
	u, e := optionalObject(v)
	if e != nil {
		return protocolError("invalid usage")
	}
	if u == nil {
		return nil
	}
	for name, value := range u {
		switch name {
		case "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens":
			if _, ok := count(value); !ok {
				return protocolError("invalid usage count")
			}
		case "cache_creation":
			if _, e := object(value); e != nil {
				return protocolError("invalid usage count")
			}
		}
	}
	maps.Copy(t.usage, u)
	if _, e := nativeUsage(t.usage, "anthropic"); e != nil {
		return e
	}
	return nil
}
