package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var errCaptureStream = errors.New("capture stream unavailable or incomplete")

type capturePart struct {
	text, name strings.Builder
	done       bool
}

// CaptureStream assembles complete fields by native identity. It never holds
// response frames and never modifies bytes delivered to the inference caller.
// Bounds apply to selected content, identities and the number of parts.
type CaptureStream struct {
	family               wire.Format
	limit                int
	output, tools        bool
	text, calls          map[string]*capturePart
	textOrder, callOrder []string
	groups               map[string]bool
	terminal             bool
	anthropicFinished    bool
	geminiCallSequence   uint64
	captureReader
}

func NewCaptureStream(f wire.Format, limit int, output, tools bool) *CaptureStream {
	return &CaptureStream{family: f, limit: limit, output: output, tools: tools, text: map[string]*capturePart{}, calls: map[string]*capturePart{}, groups: map[string]bool{}}
}

func (s *CaptureStream) part(id string, tool bool) *capturePart {
	parts, order := s.text, &s.textOrder
	if tool {
		parts, order = s.calls, &s.callOrder
	}
	if p := parts[id]; p != nil {
		return p
	}
	if len(id) > 256 || len(s.text)+len(s.calls) >= 2048 {
		s.invalid = true
		return &capturePart{}
	}
	p := &capturePart{}
	parts[id] = p
	*order = append(*order, id)
	return p
}

func (s *CaptureStream) add(id, value string, tool, snapshot bool) {
	if s.invalid || (tool && !s.tools) || (!tool && !s.output) {
		return
	}
	p := s.part(id, tool)
	if len(value) > s.limit {
		s.invalid = true
		return
	}
	if snapshot {
		// A terminal snapshot may supply content absent from deltas, but must agree
		// with a previously assembled value. Repeated snapshots are not appended.
		if p.text.Len() != 0 && p.text.String() != value {
			s.invalid = true
			return
		}
		if p.text.Len() == 0 {
			p.text.WriteString(value)
		}
	} else {
		if p.done && value != "" {
			s.invalid = true
			return
		}
		if p.text.Len()+len(value) > s.limit {
			s.invalid = true
			return
		}
		p.text.WriteString(value)
	}
	_ = s.bounds()
}

func (s *CaptureStream) tool(id, name, args string, snapshot, done bool) {
	if s.invalid || !s.tools {
		return
	}
	p := s.part(id, true)
	if name != "" {
		if !snapshot && (s.family == wire.Chat || s.family == wire.MistralFIM) {
			p.name.WriteString(name)
		} else if p.name.Len() != 0 && p.name.String() != name {
			s.invalid = true
		} else if p.name.Len() == 0 {
			p.name.WriteString(name)
		}
	}
	s.add(id, args, true, snapshot)
	p.done = p.done || done
}

func (s *CaptureStream) done(id string) {
	if p := s.text[id]; p != nil {
		p.done = true
	}
	if p := s.calls[id]; p != nil {
		p.done = true
	}
}

func (s *CaptureStream) bounds() error {
	textSize, toolSize := 0, 0
	for id, p := range s.text {
		textSize += p.text.Len() + len(id)
	}
	for id, p := range s.calls {
		toolSize += p.text.Len() + p.name.Len() + len(id)
	}
	if s.invalid || textSize > s.limit || toolSize > s.limit || len(s.groups) > 2048 {
		s.invalid = true
		return errCaptureStream
	}
	return nil
}

func (s *CaptureStream) Push(frame []byte) error {
	if s.invalid {
		return errCaptureStream
	}
	// Native frame decoders already impose their own bounds. Keep a second
	// ceiling here for direct users and avoid allocating oversized JSON trees.
	if len(frame) > 8<<20 || !utf8.Valid(frame) && s.family != wire.Bedrock {
		s.invalid = true
		return errCaptureStream
	}
	if s.family == wire.Bedrock {
		m, err := ReadBedrockEvent(bytes.NewReader(frame), len(frame))
		if err != nil {
			s.invalid = true
			return errCaptureStream
		}
		typ := m.Headers.Get(":message-type")
		kind := m.Headers.Get(":event-type")
		if typ == nil || typ.String() != "event" || kind == nil {
			s.invalid = true
			return errCaptureStream
		}
		if !validCaptureJSON(m.Payload) {
			s.invalid = true
			return errCaptureStream
		}
		f, err := s.object(m.Payload)
		if err != nil {
			s.invalid = true
			return errCaptureStream
		}
		s.bedrock(kind.String(), f)
	} else {
		// SSE data lines form one JSON event. Multiple events can share a write.
		for _, event := range bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
			var lines [][]byte
			for line := range bytes.SplitSeq(event, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) {
					lines = append(lines, bytes.TrimSpace(line[5:]))
				}
			}
			if len(lines) == 0 {
				continue
			}
			data := bytes.Join(lines, []byte("\n"))
			if bytes.Equal(data, []byte("[DONE]")) {
				if s.family == wire.Completions || s.family == wire.Chat || s.family == wire.MistralFIM {
					if len(s.groups) == 0 {
						s.invalid = true
					}
					for _, done := range s.groups {
						if !done {
							s.invalid = true
						}
					}
					s.terminal = true
					continue
				}
				s.invalid = true
				break
			}
			if !validCaptureJSON(data) {
				s.invalid = true
				break
			}
			f, err := s.object(data)
			if err != nil {
				s.invalid = true
				break
			}
			switch s.family {
			case wire.Completions:
				s.completions(f)
			case wire.Chat, wire.MistralFIM:
				s.chat(f)
			case wire.Responses:
				s.responses(f)
			case wire.Anthropic:
				s.anthropic(f)
			case wire.Gemini, wire.GeminiStream:
				s.gemini(f)
			case wire.CohereChat:
				s.cohere(f)
			default:
				s.invalid = true
			}
		}
	}
	return s.bounds()
}

func captureIndex(raw json.RawMessage) string {
	var n int
	if !present(raw) || json.Unmarshal(raw, &n) != nil || n < 0 {
		return ""
	}
	return fmt.Sprint(n)
}

func (s *CaptureStream) Finish() (string, []json.RawMessage, error) {
	if s.bounds() != nil {
		return "", nil, errCaptureStream
	}
	for _, done := range s.groups {
		if !done {
			return "", nil, errCaptureStream
		}
	}
	if s.family == wire.Completions || s.family == wire.Chat || s.family == wire.MistralFIM || s.family == wire.Gemini || s.family == wire.GeminiStream {
		if len(s.groups) == 0 {
			return "", nil, errCaptureStream
		}
	}
	if s.family != wire.Gemini && s.family != wire.GeminiStream && !s.terminal {
		return "", nil, errCaptureStream
	}
	var texts []string
	for _, id := range s.textOrder {
		p := s.text[id]
		if !utf8.ValidString(p.text.String()) || (s.family == wire.Responses && !p.done) {
			return "", nil, errCaptureStream
		}
		if p.text.Len() != 0 {
			texts = append(texts, p.text.String())
		}
	}
	var calls []json.RawMessage
	size := 0
	for _, id := range s.callOrder {
		p := s.calls[id]
		if !p.done || p.name.Len() == 0 || !utf8.ValidString(p.text.String()) || !validCaptureJSON([]byte(p.text.String())) {
			return "", nil, errCaptureStream
		}
		raw, err := json.Marshal(map[string]any{"id": id, "name": p.name.String(), "arguments": json.RawMessage(p.text.String())})
		size += len(raw)
		if err != nil || size > s.limit {
			return "", nil, errCaptureStream
		}
		calls = append(calls, raw)
	}
	output := strings.Join(texts, "\n")
	if len(output) > s.limit {
		return "", nil, errCaptureStream
	}
	return output, calls, nil
}
