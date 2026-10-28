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
	Segments []Segment
	slots    []surfaceSlot
	document oif.Document
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
		return Segment{ID: "json" + strconv.Itoa(index), Text: text, Kind: "tool_result", ToolName: s.name, Untrusted: true, toolJSON: s}
	}
	return Segment{ID: "json" + strconv.Itoa(index), Text: text, Kind: "tool_call", ToolName: s.name, toolJSON: s}
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
			s.walk(v, oif.Pointer("", field), "", "", "")
		}
	}
	if !recognized {
		return nil, ErrSurface
	}
	if err := validateSegments(s.Segments); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Surface) add(v oif.Value, pointer, role, kind, name string) {
	text, ok := v.Text()
	if !ok {
		text = v.Raw()
	}
	if !ok || kind == "tool_call" {
		// Decode JSON escape sequences before text detectors see arguments.
		// UseNumber preserves native integers and decimals during inspection;
		// the original document is retained unless a transformation acts.
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var value any
		if json.Valid([]byte(text)) && decoder.Decode(&value) == nil {
			var b bytes.Buffer
			encoder := json.NewEncoder(&b)
			encoder.SetEscapeHTML(false)
			if encoder.Encode(value) == nil {
				text = strings.TrimSuffix(b.String(), "\n")
			}
		}
	}
	s.Segments = append(s.Segments, Segment{ID: "s" + strconv.Itoa(len(s.Segments)), Text: text, Role: role, Kind: kind, ToolName: name, Untrusted: kind == "tool_result" && name == ""})
	s.slots = append(s.slots, surfaceSlot{pointer: pointer, rawJSON: !ok})
}
func (s *Surface) walk(v oif.Value, pointer, role, kind, name string) {
	switch v.Kind() {
	case oif.String:
		s.add(v, pointer, role, kind, name)
	case oif.Array:
		for i, item := range v.Elements() {
			s.walk(item, oif.Pointer(pointer, strconv.Itoa(i)), role, kind, name)
		}
	case oif.Object:
		if member, ok := v.Lookup("role"); ok {
			role, _ = member.Text()
		}
		typeValue, _ := v.Lookup("type")
		typ, _ := typeValue.Text()
		if n, ok := v.Lookup("name"); ok {
			name, _ = n.Text()
		}
		if typ == "tool_result" || typ == "function_call_output" || role == "tool" {
			kind = "tool_result"
		}
		if typ == "tool_use" || typ == "function_call" {
			kind = "tool_call"
		}
		for _, m := range v.Members() {
			p := oif.Pointer(pointer, m.Name)
			switch m.Name {
			case "role", "type", "id", "call_id", "tool_call_id", "tool_use_id", "status", "index", "finish_reason", "stop_reason", "signature", "thoughtSignature", "encrypted_content":
				continue
			case "image_url", "inlineData", "inline_data", "fileData", "input_audio", "redacted_thinking":
				// No canonical text contract exists for these bodies. Refuse at
				// admission instead of pretending they were inspected.
				s.Segments = append(s.Segments, Segment{ID: "!unavailable"})
				continue
			case "function":
				// Chat declarations and invocations both use a function wrapper.
				// Only invocations carry arguments; a name alone declares a tool.
				functionKind := "tool_definition"
				if _, ok := m.Value.Lookup("arguments"); ok {
					functionKind = "tool_call"
				}
				s.walk(m.Value, p, role, functionKind, name)
			case "function_call", "functionCall", "toolUse":
				s.walk(m.Value, p, role, "tool_call", name)
			case "functionResponse", "toolResult":
				s.walk(m.Value, p, role, "tool_result", name)
			case "arguments", "args":
				s.add(m.Value, p, role, "tool_call", name)
			case "input":
				if kind == "tool_call" {
					s.add(m.Value, p, role, kind, name)
				} else {
					s.walk(m.Value, p, role, kind, name)
				}
			case "parameters", "input_schema", "inputSchema", "parametersJsonSchema":
				s.add(m.Value, p, role, "tool_definition", name)
			case "name":
				if kind == "" || kind == "tool_definition" {
					s.add(m.Value, p, role, "tool_definition", name)
				}
			default:
				s.walk(m.Value, p, role, kind, name)
			}
		}
	}
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
