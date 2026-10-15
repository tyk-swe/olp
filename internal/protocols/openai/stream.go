package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// wire.ErrEventTooLarge marks a streamed event over the configured cap.

var errStreamComplete = errors.New("terminal stream event delivered")

// responseProjectionError carries a refusal after native Responses grammar and
// accounting have been validated. Native observers still own that evidence;
// client projection cannot undo provider work.
type responseProjectionError struct{ cause error }

func (e *responseProjectionError) Error() string { return e.cause.Error() }
func (e *responseProjectionError) Unwrap() error { return e.cause }

// IsResponseProjectionError reports a local overlay refusal after native
// Responses grammar and accounting have already been validated.
func IsResponseProjectionError(err error) bool {
	_, found := errors.AsType[*responseProjectionError](err)
	return found
}

// wire.Emit receives one encoded client frame. The first call marks the point of
// no return: callers commit the response to the client there.

// Stream decodes an upstream event stream for the family, validates each event,
// rewrites model names, forwards frames, and reports the completion summary.
// includeUsage controls client-visible chat usage; accounting always retains it.
// On failure, the partial summary preserves usage observed before the error.
func Stream(family wire.Format, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit wire.Emit) (*wire.Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, true, emit, nil)
}

// StreamMetadata forwards the same validated frames as Stream, but retains only
// completion metadata. The gateway must not accumulate output or tool arguments
// for the lifetime of a stream; the unary response cap does not bound streams.
func StreamMetadata(family wire.Format, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit wire.Emit) (*wire.Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, false, emit, nil)
}

func StreamMetadataEvents(family wire.Format, r io.Reader, maxEventBytes int, route string, includeUsage bool, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	return stream(family, r, maxEventBytes, route, includeUsage, false, emit, observe)
}

func stream(family wire.Format, r io.Reader, maxEventBytes int, route string, includeUsage, collect bool, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	var s streamer
	var encoded []byte
	retain := func(frame []byte) error { encoded = frame; return nil }
	switch family {
	case wire.Chat, wire.MistralFIM:
		// Fill-in-the-middle streams chat completion chunks.
		// Bound distinct-choice bookkeeping as well as individual wire events.
		s = &chatStream{route: route, includeUsage: includeUsage, collect: collect, maxChoices: max(1, maxEventBytes/16), emit: retain}
	case wire.Responses:
		s = &responsesStream{route: route, collect: collect, emit: retain}
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
		encoded = nil
		if event.Source().Valid() {
			frame.Data = event.Source().Raw()
		} else {
			frame.Data = event.Control()
		}
		parsed := s.frame(frame, event)
		projection, projectionFailed := errors.AsType[*responseProjectionError](parsed)
		if parsed != nil && !errors.Is(parsed, errStreamComplete) && !projectionFailed {
			return parsed
		}
		if observe != nil {
			if err := observe(event); err != nil {
				return err
			}
		}
		if projectionFailed {
			return projection
		}
		if encoded != nil {
			if err := emit(encoded); err != nil {
				return err
			}
		}
		return parsed
	})
	completion, finishErr := s.finish()
	if err != nil && !errors.Is(err, errStreamComplete) {
		if errors.Is(err, sse.ErrEventTooLarge) {
			return completion, wire.ErrEventTooLarge
		}
		if framing, ok := errors.AsType[*sse.DecodeError](err); ok {
			return completion, &wire.ProtocolError{Detail: framing.Detail}
		}
		return completion, err
	}
	return completion, finishErr
}

type streamer interface {
	frame(sse.Frame, oif.Event) error
	finish() (*wire.Completion, error)
}

type chatStream struct {
	includeUsage bool
	collect      bool
	maxChoices   int
	route        string
	emit         wire.Emit
	done         bool
	finished     map[int64]bool
	text         strings.Builder
	refusal      strings.Builder
	calls        map[int64]*wire.ToolCall
	callOrder    []int64
	c            wire.Completion
}

func (s *chatStream) frame(f sse.Frame, event oif.Event) error {
	if f.Data == "[DONE]" {
		if !s.finished[0] {
			return &wire.ProtocolError{Detail: "[DONE] arrived before the first choice finished", Truncated: true}
		}
		for index, finished := range s.finished {
			if !finished {
				return &wire.ProtocolError{Detail: fmt.Sprintf("[DONE] arrived before choice %d finished", index), Truncated: true}
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
		return &wire.ProtocolError{Detail: "chunk is not a JSON object"}
	}
	if raw, present := fields["error"]; present && !isNull(raw) {
		if upstream := errorObject(raw); upstream != nil {
			return upstream
		}
	}
	if kind, ok := stringField(fields, "object"); ok && kind != "chat.completion.chunk" {
		return &wire.ProtocolError{Detail: "unexpected object " + kind}
	}
	if s.c.UpstreamID == "" {
		s.c.UpstreamID, _ = stringField(fields, "id")
	}
	if s.c.ProviderModel == "" {
		s.c.ProviderModel, _ = stringField(fields, "model")
	}
	choices, ok := arrayField(fields, "choices")
	if !ok {
		return &wire.ProtocolError{Detail: "chunk has no choices array"}
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
		s.c.UsageComplete = true
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
		return &wire.ProtocolError{Detail: "choice is not an object"}
	}
	index, ok := int64Field(choice, "index")
	if _, present := choice["index"]; present && !ok {
		return &wire.ProtocolError{Detail: "choice index is not a non-negative integer"}
	}
	if _, seen := s.finished[index]; !seen && len(s.finished) >= s.maxChoices {
		return &wire.ProtocolError{Detail: "stream exceeds the choice tracking limit"}
	}
	finish, hasFinish := stringField(choice, "finish_reason")
	delta := map[string]json.RawMessage{}
	if raw, present := choice["delta"]; present && !isNull(raw) {
		if delta, err = object(raw); err != nil {
			return &wire.ProtocolError{Detail: "delta is not an object"}
		}
	}
	content, hasContent := stringField(delta, "content")
	refusal, hasRefusal := stringField(delta, "refusal")
	calls, hasCalls := arrayField(delta, "tool_calls")
	if s.finished[index] && (hasFinish || hasContent || hasRefusal || hasCalls) {
		return &wire.ProtocolError{Detail: fmt.Sprintf("choice %d received data after its finish", index)}
	}
	if index == 0 {
		if s.collect {
			s.text.WriteString(content)
			s.refusal.WriteString(refusal)
		}
		for _, raw := range calls {
			call, err := object(raw)
			if err != nil {
				return &wire.ProtocolError{Detail: "tool call delta is not an object"}
			}
			if !s.collect {
				continue
			}
			position, _ := int64Field(call, "index")
			if s.calls == nil {
				s.calls = map[int64]*wire.ToolCall{}
			}
			current := s.calls[position]
			if current == nil {
				current = &wire.ToolCall{}
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

func (s *chatStream) finish() (*wire.Completion, error) {
	s.c.OutputText = s.text.String()
	s.c.Refusal = s.refusal.String()
	for _, position := range s.callOrder {
		s.c.ToolCalls = append(s.c.ToolCalls, *s.calls[position])
	}
	if !s.done {
		return &s.c, &wire.ProtocolError{Detail: "stream ended before [DONE]", Truncated: true}
	}
	return &s.c, nil
}

type responsesStream struct {
	collect     bool
	route       string
	emit        wire.Emit
	done        bool
	terminalErr error
	c           *wire.Completion
	upstreamID  string
}

func (s *responsesStream) frame(f sse.Frame, event oif.Event) error {
	source := event.Source()
	fields := source.Fields()
	if fields == nil {
		return &wire.ProtocolError{Detail: "event is not a JSON object"}
	}
	kind, ok := stringField(fields, "type")
	if !ok || kind == "" || strings.ContainsAny(kind, "\r\n\x00") {
		return &wire.ProtocolError{Detail: "event has an invalid type"}
	}
	if f.Event != nil && *f.Event != kind {
		return &wire.ProtocolError{Detail: "event name disagrees with type"}
	}
	if kind == "error" {
		upstream := errorObject(fields["error"])
		if upstream == nil {
			upstream = &wire.UpstreamError{Message: "upstream reported an error"}
		}
		s.terminalErr, s.done = upstream, true
	}
	observed, err := observeResponseEventUsage(event, fields, kind)
	if err != nil {
		return err
	}
	if observed.lifecycle {
		response := observed.response
		id, present := event.Source().Lookup("/response/id")
		upstreamID, valid := id.Text()
		if s.upstreamID != "" && (!present || !valid || upstreamID != s.upstreamID) {
			return &wire.ProtocolError{Detail: "response identity changed during the stream"}
		}
		if present && valid && upstreamID != "" {
			s.upstreamID = upstreamID
		}
		switch kind {
		case "response.completed", "response.incomplete", "response.failed":
			completion, err := responseSummaryWithUsage(response, observed.usage.Usage)
			if err != nil {
				return err
			}
			if !s.collect {
				completion.OutputText, completion.Refusal, completion.ToolCalls = "", "", nil
			}
			terminalErr := terminalResponseError(response)
			if terminalErr != nil && kind != "response.failed" {
				return terminalErr
			}
			if _, invalid := errors.AsType[*wire.ProtocolError](terminalErr); invalid {
				return terminalErr
			}
			if kind == "response.failed" && terminalErr == nil {
				terminalErr = &wire.UpstreamError{Message: "upstream reported a failed response"}
			}
			completion.UsageComplete = observed.usage.Complete
			completion.Terminal = true
			completion.Failure, _ = errors.AsType[*wire.UpstreamError](terminalErr)
			if completion.Usage == nil && s.c != nil {
				completion.Usage = s.c.Usage
			}
			s.c = completion
			s.terminalErr = terminalErr
			s.done = true
		default:
			if observed.usage.Usage != nil {
				if s.c == nil {
					s.c = &wire.Completion{}
				}
				if !s.c.UsageComplete {
					s.c.Usage = observed.usage.Usage
					s.c.UpstreamID, _ = stringField(response, "id")
					s.c.ProviderModel, _ = stringField(response, "model")
				}
			}
		}
		if _, present := source.Lookup("/response/model"); present {
			bound, marshalErr := json.Marshal(s.route)
			if marshalErr != nil {
				return marshalErr
			}
			source, err = oif.Apply(source, []oif.Change{{Pointer: "/response/model", Value: string(bound), Origin: oif.IdentityBinding, Reason: "published response model"}})
			if err != nil {
				return &responseProjectionError{cause: err}
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

func (s *responsesStream) finish() (*wire.Completion, error) {
	if s.done && s.terminalErr != nil {
		return s.c, s.terminalErr
	}
	if !s.done || s.c == nil {
		return s.c, &wire.ProtocolError{Detail: "stream ended before the terminal response event", Truncated: true}
	}
	return s.c, s.terminalErr
}
