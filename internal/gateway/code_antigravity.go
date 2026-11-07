package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeAntigravityProtocol(r *http.Request) wire.Format {
	if strings.HasSuffix(r.URL.Path, "/responses") {
		return wire.Responses
	}
	if strings.HasSuffix(r.URL.Path, "/messages") {
		return wire.Anthropic
	}
	return wire.Chat
}

func codeTranslateAntigravity(body []byte, r *http.Request, model, project string) (codePayload, error) {
	return codeTranslateAntigravityProtocol(body, r, model, project, codeAntigravityProtocol(r))
}

func codeTranslateAntigravityProtocol(body []byte, r *http.Request, model, project string, family wire.Format, safetyMapping ...bool) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if family != wire.Anthropic && r.URL.RawQuery != "" || len(r.Trailer) != 0 || project == "" {
		return codePayload{}, refused
	}
	request, err := protocols.ParseForModel(family, body, model)
	if err != nil {
		return codePayload{}, refused
	}
	if family == wire.Anthropic {
		if err := codeValidateAntigravityMessages(request, r); err != nil {
			return codePayload{}, err
		}
	}
	if family == wire.Responses {
		if err := codeValidateAntigravityResponses(request, r); err != nil {
			return codePayload{}, err
		}
	}
	if family == wire.Chat {
		messages, _ := request.OIF().Document().Root().Lookup("messages")
		for _, message := range messages.Elements() {
			if !codeTextContent(message, "content") {
				content, present := message.Lookup("content")
				if !present || content.Kind() != oif.Null {
					return codePayload{}, refused
				}
			}
		}
	}
	identifier, identityErr := func() (string, error) {
		if family != wire.Anthropic {
			return "", nil
		}
		return codeAntigravityMessagesIdentity(request, len(safetyMapping) == 1 && safetyMapping[0])
	}()
	if identityErr != nil {
		return codePayload{}, identityErr
	}
	prepared, prepareErr := func() (*wire.Request, error) {
		if identifier == "" {
			return request, nil
		}
		return request.WithFields(request.OIF().Document().FieldsWithout("metadata"), oif.TransformedMapping)
	}()
	if prepareErr != nil {
		return codePayload{}, refused
	}
	for _, field := range []string{"reasoning_effort", "reasoning", "thinking", "audio", "modalities", "prediction", "response_format", "service_tier", "n", "parallel_tool_calls"} {
		if request.Field(field) != nil {
			return codePayload{}, refused
		}
	}
	encoded, _, err := protocols.EncodeTarget(prepared, wire.Gemini, "gemini", "google", model, nil)
	if err != nil {
		return codePayload{}, refused
	}
	labelled, labelErr := func() ([]byte, error) {
		if identifier == "" {
			return encoded, nil
		}
		document, err := oif.ParseJSON(encoded, oif.Limits{})
		if err != nil {
			return nil, err
		}
		fields := document.Fields()
		labels, err := json.Marshal(struct {
			Identifier string `json:"safety_identifier"`
		}{identifier})
		if err != nil {
			return nil, err
		}
		return json.Marshal(maps.Collect(func(yield func(string, json.RawMessage) bool) {
			for name, value := range fields {
				if !yield(name, value) {
					return
				}
			}
			yield("labels", labels)
		}))
	}()
	if labelErr != nil {
		return codePayload{}, refused
	}
	wrapped, err := json.Marshal(struct {
		Project string          `json:"project"`
		Model   string          `json:"model"`
		Request json.RawMessage `json:"request"`
	}{Project: project, Model: model, Request: labelled})
	if err != nil {
		return codePayload{}, refused
	}
	query := func() string {
		if request.Stream {
			return "alt=sse"
		}
		return ""
	}()
	return codePayload{body: wrapped, query: query, stream: request.Stream, includeUsage: request.IncludeUsage, target: codeadapter.TargetAntigravity}, nil
}

func (a *codeAttempt) antigravityResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, payload codePayload) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		codeWriteError(w, r, codemode.Refuse(response.StatusCode, "code_upstream_rejected"))
		return
	}
	family := codeAntigravityProtocol(r)
	writer := &streamWriter{w: w, family: family}
	transform := func() func([]byte) ([]byte, error) {
		if !payload.signedContinuation {
			return antigravityResult
		}
		return func(body []byte) ([]byte, error) { return a.signedAntigravityResult(ctx, body, a.model) }
	}()
	completion, err := func() (*wire.Completion, error) {
		if payload.stream {
			if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
				return nil, codemode.Refuse(502, "code_upstream_invalid")
			}
			reader := antigravityStreamWithResult(response.Body, model, transform)
			defer reader.Close()
			return protocols.StreamWithEvents(wire.Gemini, family, reader, codewire.MaxBody, model, payload.includeUsage, writer.emit, nil, 1)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
		if err != nil || len(body) > codewire.MaxBody {
			return nil, codemode.Refuse(502, "code_upstream_invalid")
		}
		unwrapped, stateErr := transform(body)
		completion, err := protocols.Decode(wire.Gemini, family, unwrapped, model, "")
		if err != nil {
			return nil, err
		}
		return completion, stateErr
	}()
	if unsupported, ok := errors.AsType[*antigravityStateError](err); ok {
		a.observe(antigravityObservation(unsupported.completion, true, false))
	}
	a.observe(antigravityObservation(completion, err == nil || !payload.stream && completion != nil, err == nil))
	if err != nil {
		code := func() string {
			if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && (refusal.Code == "code_upstream_unsupported_state" || refusal.Code == "code_continuation_unavailable") {
				return refusal.Code
			}
			return "code_upstream_invalid"
		}()
		writer.finish(&outcome{err: &Error{Status: 502, Code: code, Type: "api_error", Message: code}, cancelled: ctx.Err() != nil})
		return
	}
	if payload.stream {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(completion.Body)
}

type antigravityStateError struct {
	*codemode.Refusal
	completion *wire.Completion
}

func (e *antigravityStateError) Unwrap() error { return e.Refusal }

func antigravityStream(source io.Reader, model string) io.ReadCloser {
	return antigravityStreamWithResult(source, model, antigravityResult)
}

func antigravityStreamWithResult(source io.Reader, model string, result func([]byte) ([]byte, error)) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		err := sse.Decode(source, codewire.MaxBody, func(frame sse.Frame) error {
			if frame.Control {
				_, err := writer.Write(frame.Encode())
				return err
			}
			body, err := result([]byte(frame.Data))
			if err != nil {
				if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && (refusal.Code == "code_upstream_unsupported_state" || refusal.Code == "code_continuation_unavailable") {
					completion, _ := protocols.Decode(wire.Gemini, wire.Gemini, body, model, "")
					return &antigravityStateError{Refusal: refusal, completion: completion}
				}
				return err
			}
			_, writeErr := writer.Write((sse.Frame{Data: string(body)}).Encode())
			return writeErr
		})
		writer.CloseWithError(err)
	}()
	return reader
}

func antigravityResult(body []byte) ([]byte, error) {
	refused := codemode.Refuse(502, "code_upstream_invalid")
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return nil, refused
	}
	result, found := document.Root().Lookup("response")
	if !found || result.Kind() != oif.Object {
		return nil, refused
	}
	candidates, _ := result.Lookup("candidates")
	for _, candidate := range candidates.Elements() {
		if _, present := candidate.Lookup("groundingMetadata"); present {
			return result.Bytes(), codemode.Refuse(502, "code_upstream_unsupported_state")
		}
		content, _ := candidate.Lookup("content")
		parts, _ := content.Lookup("parts")
		for _, part := range parts.Elements() {
			if part.Kind() != oif.Object || len(part.Members()) != 1 || !slices.Contains([]string{"text", "functionCall"}, part.Members()[0].Name) {
				return result.Bytes(), codemode.Refuse(502, "code_upstream_unsupported_state")
			}
			call, found := part.Lookup("functionCall")
			if found && slices.ContainsFunc(call.Members(), func(member oif.Member) bool { return !slices.Contains([]string{"id", "name", "args"}, member.Name) }) {
				return result.Bytes(), codemode.Refuse(502, "code_upstream_unsupported_state")
			}
		}
	}
	return result.Bytes(), nil
}

func antigravityObservation(completion *wire.Completion, complete, successful bool) codemode.Observation {
	if completion == nil || !complete && !completion.UsageComplete {
		return codemode.Observation{}
	}
	usage := func() codemode.Usage {
		if completion.Usage == nil {
			return codemode.Usage{}
		}
		return codemode.Usage{Total: new(completion.Usage.TotalTokens), Input: new(completion.Usage.InputTokens), Output: new(completion.Usage.OutputTokens), Cached: completion.Usage.CachedInputTokens, Reasoning: completion.Usage.ReasoningTokens}
	}()
	kind := func() string {
		if completion.FinishReason == "length" || completion.Failure != nil {
			return "incomplete"
		}
		return "completed"
	}()
	return codemode.Observation{Terminal: true, Successful: complete && successful && kind == "completed", Usage: usage, Outcome: &codemode.Outcome{Origin: "upstream", Kind: kind, ObservedAt: time.Now().UTC()}}
}
