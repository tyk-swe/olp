package protocols

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

func candidateField(family openai.Family) string {
	if family == openai.FamilyChat {
		return "choices"
	}
	return "candidates"
}
func supportsCandidates(family openai.Family) bool {
	return family == openai.FamilyChat || family == openai.FamilyGemini || family == openai.FamilyGeminiStream
}

func translateCandidates(wire, target openai.Family, body []byte, route string) ([]byte, error) {
	source, err := object(body)
	if err != nil {
		return nil, err
	}
	values := arr(source[candidateField(wire)])
	if len(values) < 2 {
		return nil, nil
	}
	if !supportsCandidates(target) {
		return nil, protocolError("destination requires exactly one candidate")
	}
	var result Object
	translated := []json.RawMessage{}
	for i, value := range values {
		item, _ := object(value)
		index, ok := count(item["index"])
		if !ok {
			index = int64(i)
		}
		item["index"] = raw(0)
		source[candidateField(wire)] = raw([]Object{item})
		var c *openai.Completion
		if wire == openai.FamilyChat {
			c, err = openai.DecodeChat(raw(source), route)
		} else {
			c, err = decodeGemini(raw(source), route, false)
		}
		if err != nil {
			return nil, err
		}
		// Every candidate, not only the first, must render as a valid call.
		if err := validateTranslatedCalls(c.ToolCalls); err != nil {
			return nil, err
		}
		out, _ := object(renderCompletion(c, target, route))
		items := arr(out[candidateField(target)])
		if len(items) != 1 {
			return nil, protocolError("candidate could not be translated")
		}
		entry, _ := object(items[0])
		entry["index"] = raw(index)
		translated = append(translated, raw(entry))
		if result == nil {
			result = out
		}
	}
	result[candidateField(target)] = raw(translated)
	return raw(result), nil
}

type candidateStream struct {
	source, target  openai.Family
	route           string
	limit, retained int
	includeUsage    bool
	emit            openai.Emit
	children        map[int]*streamTranslator
	finishes        map[int]string
}

func (m *candidateStream) child(index int) *streamTranslator {
	if t := m.children[index]; t != nil {
		return t
	}
	t := &streamTranslator{source: m.source, target: m.target, route: m.route, limit: m.limit, retained: &m.retained, calls: map[int]*openai.ToolCall{}, blocks: map[int]int{}}
	t.emit = func(frame []byte) error {
		if strings.TrimSpace(string(frame)) == "data: [DONE]" {
			return nil
		}
		return sse.Decode(strings.NewReader(string(frame)), max(m.limit, len(frame)), func(event sse.Frame) error {
			f, err := object([]byte(event.Data))
			if err != nil {
				return err
			}
			field := candidateField(m.target)
			values := arr(f[field])
			for i, value := range values {
				v, _ := object(value)
				v["index"] = raw(index)
				values[i] = raw(v)
			}
			if values != nil {
				f[field] = raw(values)
			}
			return m.emit(eventFrame("", f))
		})
	}
	m.children[index] = t
	m.retained += 128
	return t
}
func (m *candidateStream) frame(frame []byte) error {
	return sse.Decode(strings.NewReader(string(frame)), max(m.limit, len(frame)), func(event sse.Frame) error {
		if event.Data == "[DONE]" {
			return nil
		}
		f, err := object([]byte(event.Data))
		if err != nil {
			return err
		}
		field := candidateField(m.source)
		for i, value := range arr(f[field]) {
			item, _ := object(value)
			index, ok := count(item["index"])
			if !ok {
				index = int64(i)
			}
			child := m.child(int(index))
			if m.retained > m.limit {
				return openai.ErrEventTooLarge
			}
			reason := str(item["finish_reason"])
			if m.source == openai.FamilyGemini {
				if r := str(item["finishReason"]); r != "" {
					reason = geminiFinish(r)
				}
			}
			if reason != "" {
				m.finishes[int(index)] = reason
			}
			item["index"] = raw(0)
			single := Object{}
			maps.Copy(single, f)
			single[field] = raw([]Object{item})
			if err := child.frame(eventFrame("", single)); err != nil {
				return err
			}
		}
		return nil
	})
}
func (m *candidateStream) finish(c *openai.Completion) error {
	if len(m.children) == 0 {
		m.child(0)
	}
	indices := make([]int, 0, len(m.children))
	for i := range m.children {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	for i, index := range indices {
		summary := *c
		summary.Usage = nil
		if reason := m.finishes[index]; reason != "" {
			summary.FinishReason = reason
		}
		if m.target != openai.FamilyChat && i == len(indices)-1 {
			summary.Usage = c.Usage
		}
		if err := m.children[index].finish(&summary); err != nil {
			return err
		}
	}
	if m.target == openai.FamilyChat {
		if m.includeUsage && c.Usage != nil {
			if err := m.children[indices[0]].chat(nil, nil, c.Usage); err != nil {
				return err
			}
		}
		return m.emit([]byte("data: [DONE]\n\n"))
	}
	return nil
}
func geminiFinish(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	}
	return "stop"
}
