package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func codeTranslateAntigravity(body []byte, r *http.Request, model, project string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || project == "" {
		return codePayload{}, refused
	}
	request, err := protocols.ParseForModel(wire.Chat, body, model)
	if err != nil {
		return codePayload{}, refused
	}
	root := request.OIF().Document().Root()
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !codeTextContent(message, "content") {
			content, present := message.Lookup("content")
			if !present || content.Kind() != oif.Null {
				return codePayload{}, refused
			}
		}
	}
	for _, field := range []string{"reasoning_effort", "reasoning", "thinking", "audio", "modalities", "prediction", "response_format", "service_tier", "n", "parallel_tool_calls"} {
		if request.Field(field) != nil {
			return codePayload{}, refused
		}
	}
	encoded, _, err := protocols.EncodeTarget(request, wire.Gemini, "gemini", "google", model, nil)
	if err != nil {
		return codePayload{}, refused
	}
	wrapped, err := json.Marshal(struct {
		Project string          `json:"project"`
		Model   string          `json:"model"`
		Request json.RawMessage `json:"request"`
	}{Project: project, Model: model, Request: encoded})
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
	writer := &streamWriter{w: w, family: wire.Chat}
	completion, err := func() (*wire.Completion, error) {
		if payload.stream {
			if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
				return nil, codemode.Refuse(502, "code_upstream_invalid")
			}
			reader := antigravityStream(response.Body, model)
			defer reader.Close()
			return protocols.StreamWithEvents(wire.Gemini, wire.Chat, reader, codewire.MaxBody, model, payload.includeUsage, writer.emit, nil, 1)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
		if err != nil || len(body) > codewire.MaxBody {
			return nil, codemode.Refuse(502, "code_upstream_invalid")
		}
		unwrapped, stateErr := antigravityResult(body)
		completion, err := protocols.Decode(wire.Gemini, wire.Chat, unwrapped, model, "")
		if err != nil {
			return nil, err
		}
		return completion, stateErr
	}()
	if unsupported, ok := errors.AsType[*antigravityStateError](err); ok {
		a.observe(antigravityObservation(unsupported.completion, true))
	}
	a.observe(antigravityObservation(completion, err == nil || !payload.stream && completion != nil))
	if err != nil {
		code := func() string {
			if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && refusal.Code == "code_upstream_unsupported_state" {
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
	reader, writer := io.Pipe()
	go func() {
		err := sse.Decode(source, codewire.MaxBody, func(frame sse.Frame) error {
			if frame.Control {
				_, err := writer.Write(frame.Encode())
				return err
			}
			body, err := antigravityResult([]byte(frame.Data))
			if err != nil {
				if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && refusal.Code == "code_upstream_unsupported_state" {
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

func antigravityObservation(completion *wire.Completion, complete bool) codemode.Observation {
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
	return codemode.Observation{Terminal: true, Successful: complete && kind == "completed", Usage: usage, Outcome: &codemode.Outcome{Origin: "upstream", Kind: kind, ObservedAt: time.Now().UTC()}}
}
