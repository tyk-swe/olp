package routes

import (
	"github.com/tyk-swe/olp/internal/domain/oif"
)

const (
	maxInspectedTurns = 64
	maxInspectedParts = 16
	maxLinkedCalls    = 32
)

type structureInspector struct {
	turns    []inspectedTurn
	omitted  int
	calls    map[string]int
	nextCall int
}

func (si *structureInspector) add(turn inspectedTurn) {
	if len(si.turns) == maxInspectedTurns {
		si.omitted++
		return
	}
	si.turns = append(si.turns, turn)
}

func (si *structureInspector) appendPart(turn *inspectedTurn, kind, callID string, call bool) {
	if len(turn.Parts) == maxInspectedParts {
		turn.OmittedParts++
		return
	}
	part := inspectedPart{Kind: kind}
	if callID != "" {
		if call {
			if _, known := si.calls[callID]; !known && si.nextCall <= maxLinkedCalls {
				si.calls[callID] = si.nextCall
				si.nextCall++
			}
		}
		if ordinal, known := si.calls[callID]; known {
			part.CallOrdinal = &ordinal
		}
	}
	turn.Parts = append(turn.Parts, part)
}

func (si *structureInspector) appendContent(turn *inspectedTurn, content oif.Value) {
	switch content.Kind() {
	case oif.String:
		si.appendPart(turn, "text", "", false)
	case oif.Array:
		for _, block := range content.Elements() {
			if block.Kind() != oif.Object {
				si.appendPart(turn, "opaque", "", false)
				continue
			}
			kind := inspectedBlockKind(block)
			callID := ""
			if kind == "tool_call" {
				callID = inspectionString(block, "id")
			} else if kind == "tool_result" {
				callID = inspectionString(block, "tool_use_id")
				if callID == "" {
					callID = inspectionString(block, "call_id")
				}
			}
			si.appendPart(turn, kind, callID, kind == "tool_call")
		}
	case oif.Object:
		si.appendPart(turn, inspectedBlockKind(content), "", false)
	case oif.Null:
		si.appendPart(turn, "null", "", false)
	default:
		si.appendPart(turn, "opaque", "", false)
	}
}

func (si *structureInspector) inspectItem(scope string, index int, item oif.Value) {
	turn := inspectedTurn{Scope: scope, Index: index, Role: "unspecified", Parts: []inspectedPart{}}
	if item.Kind() != oif.Object {
		si.appendContent(&turn, item)
		si.add(turn)
		return
	}
	turn.Role = inspectedRole(inspectionString(item, "role"))
	switch inspectionString(item, "type") {
	case "function_call":
		si.appendPart(&turn, "tool_call", inspectionString(item, "call_id"), true)
	case "function_call_output":
		si.appendPart(&turn, "tool_result", inspectionString(item, "call_id"), false)
	default:
		if id := inspectionString(item, "tool_call_id"); turn.Role == "tool" && id != "" {
			// A Chat tool message's content is the result, not a
			// separate text block preceding the result.
			si.appendPart(&turn, "tool_result", id, false)
		} else {
			if value, ok := item.Lookup("content"); ok {
				si.appendContent(&turn, value)
			}
			if value, ok := item.Lookup("parts"); ok {
				si.appendContent(&turn, value)
			}
			if calls, ok := item.Lookup("tool_calls"); ok {
				for _, call := range calls.Elements() {
					si.appendPart(&turn, "tool_call", inspectionString(call, "id"), true)
				}
			}
		}
	}
	si.add(turn)
}

// inspectStructure deliberately walks only known native input containers.
// Every emitted string is a constant from this file; source strings are used
// solely to choose an allowlisted role/kind or correlate an ordinal tool call.
func inspectStructure(root oif.Value) ([]inspectedTurn, int) {
	si := &structureInspector{turns: []inspectedTurn{}, calls: make(map[string]int), nextCall: 1}
	for _, name := range []string{"system", "instructions", "systemInstruction"} {
		if content, present := root.Lookup(name); present {
			turn := inspectedTurn{Scope: name, Index: 0, Role: name, Parts: []inspectedPart{}}
			if name == "systemInstruction" {
				turn.Scope, turn.Role = "system_instruction", "system"
			}
			if name == "instructions" {
				turn.Role = "instruction"
			}
			if parts, ok := content.Lookup("parts"); ok {
				si.appendContent(&turn, parts)
			} else {
				si.appendContent(&turn, content)
			}
			si.add(turn)
		}
	}
	for _, scope := range []string{"messages", "input", "contents"} {
		content, present := root.Lookup(scope)
		if !present {
			continue
		}
		items := content.Elements()
		if content.Kind() != oif.Array {
			items = []oif.Value{content}
		}
		for index, item := range items {
			if len(si.turns) == maxInspectedTurns {
				si.omitted += len(items) - index
				break
			}
			si.inspectItem(scope, index, item)
		}
	}
	return si.turns, si.omitted
}

func inspectionString(value oif.Value, name string) string {
	field, ok := value.Lookup(name)
	if !ok {
		return ""
	}
	result, _ := field.Text()
	return result
}

func inspectedRole(source string) string {
	switch source {
	case "system", "developer", "user", "assistant", "tool", "model", "function":
		return source
	case "":
		return "unspecified"
	default:
		return "other"
	}
}

func inspectedBlockKind(block oif.Value) string {
	switch inspectionString(block, "type") {
	case "text", "input_text", "output_text":
		return "text"
	case "image", "image_url", "input_image":
		return "image"
	case "audio", "input_audio", "output_audio":
		return "audio"
	case "video":
		return "video"
	case "file", "document", "input_file":
		return "document"
	case "tool_use", "function_call":
		return "tool_call"
	case "tool_result", "function_call_output":
		return "tool_result"
	case "thinking", "redacted_thinking", "reasoning":
		return "reasoning"
	case "refusal":
		return "refusal"
	}
	for _, field := range []struct{ name, kind string }{
		{"text", "text"}, {"inlineData", "media"}, {"fileData", "document"},
		{"functionCall", "tool_call"}, {"functionResponse", "tool_result"},
	} {
		if _, present := block.Lookup(field.name); present {
			return field.kind
		}
	}
	return "opaque"
}
