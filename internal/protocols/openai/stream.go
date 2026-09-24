package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tyk-swe/olp/internal/oif"
	"io"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// ErrEventTooLarge marks a streamed event over the configured cap.
var ErrEventTooLarge = errors.New("upstream event exceeds the configured size limit")

var errStreamComplete = errors.New("terminal stream event delivered")

// Emit receives one encoded client frame. The first call marks the point of
// no return: callers commit the response to the client there.
type Emit func(frame []byte) error

// Stream decodes an upstream event stream for the family, validates each event,
// rewrites model names, forwards frames, and reports the completion summary.
// includeUsage controls client-visible chat usage; accounting always retains it.
// On failure, the partial summary preserves usage observed before the error.
func Stream(family Family, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit Emit) (*Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, true, emit, nil)
}

// StreamMetadata forwards the same validated frames as Stream, but retains only
// completion metadata. The gateway must not accumulate output or tool arguments
// for the lifetime of a stream; the unary response cap does not bound streams.
func StreamMetadata(family Family, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit Emit) (*Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, false, emit, nil)
}

func StreamMetadataEvents(family Family, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit Emit, observe func(oif.Event) error) (*Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, false, emit, observe)
}

func stream(family Family, r io.Reader, maxEventBytes int, route string, includeUsage, collect bool, emit Emit, observe func(oif.Event) error) (*Completion, error) {
	var s streamer
	switch family {
	case FamilyChat:
		// Bound distinct-choice bookkeeping as well as individual wire events.
		s = &chatStream{route: route, includeUsage: includeUsage, collect: collect, maxChoices: max(1, maxEventBytes/16), emit: emit}
	case FamilyResponses:
		s = &responsesStream{route: route, collect: collect, emit: emit}
	default:
		return nil, errors.New("unknown request family")
	}
	sequence := uint64(0)
	err := sse.Decode(r, maxEventBytes, func(frame sse.Frame) error {
		event, err := LiftSSE(family, frame, sequence, maxEventBytes)
		if err != nil {
			return err
		}
		sequence++
		if observe != nil {
			if err := observe(event); err != nil {
				return err
			}
		}
		if event.Source().Valid() {
			frame.Data = event.Source().Raw()
		} else {
			frame.Data = event.Control()
		}
		return s.frame(frame, event)
	})
	completion, finishErr := s.finish()
	if err != nil && !errors.Is(err, errStreamComplete) {
		if errors.Is(err, sse.ErrEventTooLarge) {
			return completion, ErrEventTooLarge
		}
		if framing, ok := errors.AsType[*sse.DecodeError](err); ok {
			return completion, &ProtocolError{Detail: framing.Detail}
		}
		return completion, err
	}
	return completion, finishErr
}

type streamer interface {
	frame(sse.Frame, oif.Event) error
	finish() (*Completion, error)
}

type chatStream struct {
	includeUsage bool
	collect      bool
	maxChoices   int
	route        string
	emit         Emit
	done         bool
	finished     map[int64]bool
	text         strings.Builder
	refusal      strings.Builder
	calls        map[int64]*ToolCall
	callOrder    []int64
	c            Completion
}

func (s *chatStream) frame(f sse.Frame, event oif.Event) error {
	if f.Data == "[DONE]" {
		if !s.finished[0] {
			return &ProtocolError{Detail: "[DONE] arrived before the first choice finished", Truncated: true}
		}
		for index, finished := range s.finished {
			if !finished {
				return &ProtocolError{Detail: fmt.Sprintf("[DONE] arrived before choice %d finished", index), Truncated: true}
			}
		}
		s.done = true
		if err := s.emit([]byte("data: [DONE]\n\n")); err != nil {
			return err
		}
		return errStreamComplete
	}
	fields := event.Source().Fields()
	if fields == nil {
		return &ProtocolError{Detail: "chunk is not a JSON object"}
	}
	if raw, present := fields["error"]; present && !isNull(raw) {
		if upstream := errorObject(raw); upstream != nil {
			return upstream
		}
	}
	if kind, ok := stringField(fields, "object"); ok && kind != "chat.completion.chunk" {
		return &ProtocolError{Detail: "unexpected object " + kind}
	}
	if s.c.UpstreamID == "" {
		s.c.UpstreamID, _ = stringField(fields, "id")
	}
	if s.c.ProviderModel == "" {
		s.c.ProviderModel, _ = stringField(fields, "model")
	}
	choices, ok := arrayField(fields, "choices")
	if !ok {
		return &ProtocolError{Detail: "chunk has no choices array"}
	}
	for _, raw := range choices {
		if err := s.choice(raw); err != nil {
			return err
		}
	}
	usage, err := chatUsage(fields)
	if err != nil {
		return err
	}
	if usage != nil {
		s.c.Usage = usage
	}
	if !s.includeUsage {
		if usage != nil && len(choices) == 0 {
			return nil
		}
		delete(fields, "usage")
	}
	if err := rewriteModel(fields, s.route); err != nil {
		return err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return s.emit(append(append([]byte("data: "), encoded...), '\n', '\n'))
}

func (s *chatStream) choice(raw json.RawMessage) error {
	choice, err := object(raw)
	if err != nil {
		return &ProtocolError{Detail: "choice is not an object"}
	}
	index, ok := int64Field(choice, "index")
	if _, present := choice["index"]; present && !ok {
		return &ProtocolError{Detail: "choice index is not a non-negative integer"}
	}
	if _, seen := s.finished[index]; !seen && len(s.finished) >= s.maxChoices {
		return &ProtocolError{Detail: "stream exceeds the choice tracking limit"}
	}
	finish, hasFinish := stringField(choice, "finish_reason")
	delta := map[string]json.RawMessage{}
	if raw, present := choice["delta"]; present && !isNull(raw) {
		if delta, err = object(raw); err != nil {
			return &ProtocolError{Detail: "delta is not an object"}
		}
	}
	content, hasContent := stringField(delta, "content")
	refusal, hasRefusal := stringField(delta, "refusal")
	calls, hasCalls := arrayField(delta, "tool_calls")
	if s.finished[index] && (hasFinish || hasContent || hasRefusal || hasCalls) {
		return &ProtocolError{Detail: fmt.Sprintf("choice %d received data after its finish", index)}
	}
	if index == 0 {
		if s.collect {
			s.text.WriteString(content)
			s.refusal.WriteString(refusal)
		}
		for _, raw := range calls {
			call, err := object(raw)
			if err != nil {
				return &ProtocolError{Detail: "tool call delta is not an object"}
			}
			if !s.collect {
				continue
			}
			position, _ := int64Field(call, "index")
			if s.calls == nil {
				s.calls = map[int64]*ToolCall{}
			}
			current := s.calls[position]
			if current == nil {
				current = &ToolCall{}
				s.calls[position] = current
				s.callOrder = append(s.callOrder, position)
			}
			partial := chatToolCall(call)
			if partial.ID != "" {
				current.ID = partial.ID
			}
			if partial.Name != "" {
				current.Name = partial.Name
			}
			current.Arguments += partial.Arguments
		}
	}
	if s.finished == nil {
		s.finished = map[int64]bool{}
	}
	s.finished[index] = s.finished[index] || hasFinish
	if hasFinish && index == 0 {
		s.c.FinishReason = finish
	}
	return nil
}

func (s *chatStream) finish() (*Completion, error) {
	s.c.OutputText = s.text.String()
	s.c.Refusal = s.refusal.String()
	for _, position := range s.callOrder {
		s.c.ToolCalls = append(s.c.ToolCalls, *s.calls[position])
	}
	if !s.done {
		return &s.c, &ProtocolError{Detail: "stream ended before [DONE]", Truncated: true}
	}
	return &s.c, nil
}

type responsesStream struct {
	collect     bool
	route       string
	emit        Emit
	done        bool
	terminalErr error
	c           *Completion
}

func (s *responsesStream) frame(f sse.Frame, event oif.Event) error {
	source := event.Source()
	fields := source.Fields()
	if fields == nil {
		return &ProtocolError{Detail: "event is not a JSON object"}
	}
	kind, ok := stringField(fields, "type")
	if !ok || kind == "" || strings.ContainsAny(kind, "\r\n\x00") {
		return &ProtocolError{Detail: "event has an invalid type"}
	}
	if f.Event != nil && *f.Event != kind {
		return &ProtocolError{Detail: "event name disagrees with type"}
	}
	terminal := kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed"
	if raw := fields["response"]; terminal && (len(raw) == 0 || isNull(raw)) {
		return &ProtocolError{Detail: "terminal event has no response"}
	}
	if kind == "error" {
		upstream := errorObject([]byte(f.Data))
		if upstream == nil {
			upstream = &UpstreamError{Message: "upstream reported an error"}
		}
		return upstream
	}
	if raw, present := fields["response"]; present && !isNull(raw) {
		response, err := object(raw)
		if err != nil {
			return &ProtocolError{Detail: "response is not an object"}
		}
		switch kind {
		case "response.completed", "response.incomplete", "response.failed":
			if s.c, err = responseSummary(response); err != nil {
				return err
			}
			if !s.collect {
				s.c.OutputText, s.c.Refusal, s.c.ToolCalls = "", "", nil
			}
			terminalErr := terminalResponseError(response)
			if terminalErr != nil && kind != "response.failed" {
				return terminalErr
			}
			if kind == "response.failed" && terminalErr == nil {
				terminalErr = &UpstreamError{Message: "upstream reported a failed response"}
			}
			if status, present := stringField(response, "status"); present && status != strings.TrimPrefix(kind, "response.") {
				return &ProtocolError{Detail: "terminal event disagrees with response status"}
			}
			s.terminalErr = terminalErr
			s.done = true
		}
		if _, present := source.Lookup("/response/model"); present {
			bound, marshalErr := json.Marshal(s.route)
			if marshalErr != nil {
				return marshalErr
			}
			source, err = oif.Apply(source, []oif.Change{{Pointer: "/response/model", Value: string(bound), Origin: oif.IdentityBinding, Reason: "published response model"}})
			if err != nil {
				return err
			}
		}
	}
	encoded := source.Bytes()
	frame := make([]byte, 0, len(kind)+len(encoded)+48)
	if f.ID != nil {
		frame = append(frame, "id: "...)
		frame = append(frame, *f.ID...)
		frame = append(frame, '\n')
	}
	if f.RetryMS != nil {
		frame = append(frame, "retry: "...)
		frame = strconv.AppendUint(frame, *f.RetryMS, 10)
		frame = append(frame, '\n')
	}
	frame = append(frame, "event: "...)
	frame = append(frame, kind...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, encoded...)
	if err := s.emit(append(frame, '\n', '\n')); err != nil {
		return err
	}
	if s.done {
		return errStreamComplete
	}
	return nil
}

func (s *responsesStream) finish() (*Completion, error) {
	if !s.done || s.c == nil {
		return s.c, &ProtocolError{Detail: "stream ended before the terminal response event", Truncated: true}
	}
	return s.c, s.terminalErr
}
