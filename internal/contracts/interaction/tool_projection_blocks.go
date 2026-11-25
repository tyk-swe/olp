package interaction

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func (p *ToolProjection) startBlock(root oif.Value) ([]byte, error) {
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
	return p.chunk(change, observation)
}

func (p *ToolProjection) deltaBlock(root oif.Value) ([]byte, error) {
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
		return p.chunk(change, map[string]any{"index": index, "type": b.kind, "phase": "delta", "text": text})
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
	return nil, nil
}

func (p *ToolProjection) stopBlock(root oif.Value) ([]byte, error) {
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
	var err error
	p.blocks[index], err = json.Marshal(b.fields)
	if err != nil {
		return nil, err
	}
	delete(p.active, index)
	return p.chunk(delta, observation)
}
