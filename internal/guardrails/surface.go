package guardrails

import (
	"bytes"
	"encoding/json"
	"iter"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// Surface retains exact JSON pointers while detectors see decoded text. The
// source remains immutable unless a declared transformation actually acts.
type Surface struct {
	Segments    []Segment
	unavailable bool
	slots       []surfaceSlot
	document    oif.Document
}
type surfaceSlot struct {
	pointer string
	rawJSON bool
}

type JSONSurface struct {
	document oif.Document
	slots    []surfaceSlot
	name     string
	output   bool
}

func ToolJSONSurface(document oif.Document, name string, output bool) *JSONSurface {
	return &JSONSurface{document: document, slots: slices.Collect(jsonSlots(document.Root(), "")), name: name, output: output}
}

func jsonSlots(value oif.Value, pointer string) iter.Seq[surfaceSlot] {
	return func(yield func(surfaceSlot) bool) {
		for index, item := range value.Elements() {
			for slot := range jsonSlots(item, oif.Pointer(pointer, strconv.Itoa(index))) {
				if !yield(slot) {
					return
				}
			}
		}
		for _, member := range value.Members() {
			for slot := range jsonSlots(member.Value, oif.Pointer(pointer, member.Name)) {
				if !yield(slot) {
					return
				}
			}
		}
		if len(value.Elements()) == 0 && len(value.Members()) == 0 {
			yield(surfaceSlot{pointer: pointer, rawJSON: value.Kind() != oif.String})
		}
	}
}

func (s *JSONSurface) Segments() []Segment {
	return slices.Collect(func(yield func(Segment) bool) {
		for index, slot := range s.slots {
			value, _ := s.document.Lookup(slot.pointer)
			text, isText := value.Text()
			if !isText {
				if !yield(s.segment(index, value.Raw())) {
					return
				}
				continue
			}
			if !yield(s.segment(index, text)) {
				return
			}
		}
	})
}

func (s *JSONSurface) segment(index int, text string) Segment {
	if s.output {
		return Segment{ID: "json" + strconv.Itoa(index), Text: text, Kind: ToolResult, ToolName: s.name, Untrusted: true, toolJSON: s}
	}
	return Segment{ID: "json" + strconv.Itoa(index), Text: text, Kind: ToolCall, ToolName: s.name, toolJSON: s}
}

func (s *JSONSurface) Apply(segments []Segment) (oif.Document, error) {
	surface := Surface{document: s.document, slots: s.slots, Segments: s.Segments()}
	projected := slices.Collect(func(yield func(Segment) bool) {
		for _, segment := range segments {
			if segment.toolJSON == s && !yield(segment) {
				return
			}
		}
	})
	return surface.Apply(projected)
}

func jsonToolSegments(segments []Segment) ([]Segment, error) {
	if !slices.ContainsFunc(segments, func(segment Segment) bool { return segment.toolJSON != nil }) {
		return segments, nil
	}
	for _, segment := range segments {
		if segment.toolJSON == nil || segment.ID != "json0" {
			continue
		}
		if _, err := segment.toolJSON.Apply(segments); err != nil {
			return nil, err
		}
	}
	return slices.Collect(func(yield func(Segment) bool) {
		for _, segment := range segments {
			if segment.toolJSON == nil {
				if !yield(segment) {
					return
				}
				continue
			}
			if segment.ID != "json0" {
				continue
			}
			document, _ := segment.toolJSON.Apply(segments)
			if !yield(segment.withText(document.Raw())) {
				return
			}
		}
	}), nil
}

func NativeSurface(document oif.Document, output bool) (*Surface, error) {
	s := &Surface{document: document}
	fields := []string{"messages", "input", "inputs", "prompt", "contents", "system", "systemInstruction", "instructions", "tools", "functions", "toolConfig", "tool_choice", "response_format", "generationConfig", "inferenceConfig", "text", "query", "documents", "instances", "inputText", "generateContentRequest"}
	if output {
		fields = []string{"choices", "output", "content", "candidates", "output_text", "refusal", "predictions", "results", "message", "generation", "generated_text", "completion", "text", "response"}
	}
	recognized := false
	for _, field := range fields {
		if v, ok := document.Root().Lookup(field); ok {
			if !output && field == "prompt" && v.Kind() != oif.String {
				if v.Kind() != oif.Array {
					return nil, ErrSurface
				}
				for _, item := range v.Elements() {
					if item.Kind() != oif.String {
						return nil, ErrSurface
					}
				}
			}
			recognized = true
			s.walk(v, oif.Pointer("", field), "", Text, "", "")
		}
	}
	if !recognized || s.unavailable {
		return nil, ErrSurface
	}
	if err := validateSegments(s.Segments); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Surface) add(v oif.Value, pointer string, role Role, kind SegmentKind, name, callID string) {
	raw, isText := v.Text()
	text := raw
	if !isText {
		text = v.Raw()
	}
	if !isText || kind == ToolCall {
		text = canonicalJSON(text)
	}
	s.Segments = append(s.Segments, Segment{ID: "s" + strconv.Itoa(len(s.Segments)), Text: text, Role: role, Kind: kind, ToolName: name, ToolCallID: callID, Untrusted: kind == ToolResult && name == ""})
	s.slots = append(s.slots, surfaceSlot{pointer: pointer, rawJSON: !isText})
}

// canonicalJSON decodes escape sequences so text detectors see tool
// arguments as written. UseNumber keeps native integers and decimals.
func canonicalJSON(text string) string {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if !json.Valid([]byte(text)) || decoder.Decode(&value) != nil {
		return text
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return text
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (s *Surface) walk(v oif.Value, pointer string, role Role, kind SegmentKind, name, callID string) {
	switch v.Kind() {
	case oif.String:
		s.add(v, pointer, role, kind, name, callID)
	case oif.Array:
		for i, item := range v.Elements() {
			s.walk(item, oif.Pointer(pointer, strconv.Itoa(i)), role, kind, name, callID)
		}
	case oif.Object:
		objectRole := Role(memberText(v, "role", string(role)))
		objectKind := segmentKindOf(v, objectRole, kind)
		s.walkObject(v, pointer, objectRole, objectKind, memberText(v, "name", name), toolResultCallID(v, objectKind, callID))
	}
}

func (s *Surface) walkObject(v oif.Value, pointer string, role Role, kind SegmentKind, name, callID string) {
	for _, m := range v.Members() {
		p := oif.Pointer(pointer, m.Name)
		switch m.Name {
		case "role", "type", "id", "call_id", "tool_call_id", "tool_use_id", "status", "index", "finish_reason", "stop_reason", "signature", "thoughtSignature", "encrypted_content":
			continue
		case "image_url", "inlineData", "inline_data", "fileData", "input_audio", "redacted_thinking":
			// No canonical text contract exists for these bodies. Refuse at
			// admission instead of pretending they were inspected.
			s.unavailable = true
		case "function":
			s.walk(m.Value, p, role, functionKind(m.Value), name, callID)
		case "function_call", "functionCall", "toolUse":
			s.walk(m.Value, p, role, ToolCall, name, callID)
		case "functionResponse", "toolResult":
			s.walk(m.Value, p, role, ToolResult, name, callID)
		case "arguments", "args":
			s.add(m.Value, p, role, ToolCall, name, callID)
		case "input":
			if kind == ToolCall {
				s.add(m.Value, p, role, kind, name, callID)
				continue
			}
			s.walk(m.Value, p, role, kind, name, callID)
		case "parameters", "input_schema", "inputSchema", "parametersJsonSchema":
			s.add(m.Value, p, role, ToolDefinition, name, callID)
		case "name":
			if kind == Text || kind == ToolDefinition {
				s.add(m.Value, p, role, ToolDefinition, name, callID)
			}
		default:
			s.walk(m.Value, p, role, kind, name, callID)
		}
	}
}

func memberText(v oif.Value, field, inherited string) string {
	member, ok := v.Lookup(field)
	if !ok {
		return inherited
	}
	text, _ := member.Text()
	return text
}

func segmentKindOf(v oif.Value, role Role, inherited SegmentKind) SegmentKind {
	switch memberText(v, "type", "") {
	case "tool_use", "function_call":
		return ToolCall
	case "tool_result", "function_call_output":
		return ToolResult
	}
	if role == Tool {
		return ToolResult
	}
	return inherited
}

// functionKind tells chat declarations from invocations, which share a
// function wrapper. Only invocations carry arguments.
func functionKind(v oif.Value) SegmentKind {
	if _, ok := v.Lookup("arguments"); ok {
		return ToolCall
	}
	return ToolDefinition
}

func toolResultCallID(value oif.Value, kind SegmentKind, inherited string) string {
	if kind != ToolResult {
		return inherited
	}
	for _, field := range []string{"tool_call_id", "tool_use_id", "call_id", "id"} {
		if id := memberText(value, field, ""); id != "" {
			return id
		}
	}
	return inherited
}
func (s *Surface) Apply(segments []Segment) (oif.Document, error) {
	if len(segments) != len(s.Segments) {
		return oif.Document{}, ErrSurface
	}
	var changes []oif.Change
	for i, segment := range segments {
		if segment.ID != s.Segments[i].ID {
			return oif.Document{}, ErrSurface
		}
		if segment.Text == s.Segments[i].Text {
			continue
		}
		value := segment.Text
		if !s.slots[i].rawJSON {
			raw, _ := json.Marshal(value)
			value = string(raw)
		} else if !json.Valid([]byte(value)) {
			return oif.Document{}, ErrSurface
		}
		changes = append(changes, oif.Change{Pointer: s.slots[i].pointer, Value: value, Origin: oif.ExplicitTransform, Reason: "guardrail transformation"})
	}
	return oif.Apply(s.document, changes)
}

func (s *Surface) Replacement(index int, text string) string {
	if s.slots[index].rawJSON {
		value, _ := json.Marshal(text)
		return string(value)
	}
	return text
}

func (s *Surface) TextAt(pointer string) []string {
	return slices.Collect(func(yield func(string) bool) {
		for i, slot := range s.slots {
			if (slot.pointer == pointer || strings.HasPrefix(slot.pointer, pointer+"/")) && !yield(s.Segments[i].Text) {
				return
			}
		}
	})
}
