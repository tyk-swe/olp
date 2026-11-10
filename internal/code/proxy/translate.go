package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
)

type translatedPayload struct {
	body         []byte
	query        string
	stream       bool
	translated   bool
	includeUsage bool
	target       vendor.Target
}

func translateMessagesToCodex(body []byte, r *http.Request, model string) (translatedPayload, error) {
	refused := code.Refuse(422, "code_translation_unsupported_semantics")
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil || (len(query) != 0 && (len(query) != 1 || !slices.Equal(query["beta"], []string{"true"}))) || r.Header.Get("Anthropic-Beta") != "" || len(r.Trailer) != 0 {
		return translatedPayload{}, refused
	}
	request, requestErr := protocols.ParseForModel(protocolwire.Anthropic, body, model)
	if requestErr != nil || request.Field("stream_options") != nil {
		return translatedPayload{}, refused
	}
	root := request.OIF().Document().Root()
	if !isTextContent(root, "system") {
		return translatedPayload{}, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !isMessagesContent(message) {
			return translatedPayload{}, refused
		}
	}
	return encodeCodexResponses(request)
}

func translateChatToCodex(body []byte, r *http.Request, model string) (translatedPayload, error) {
	refused := code.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || len(r.Header.Values("X-Codex-Turn-State")) != 0 || len(r.Header.Values("OpenAI-Beta")) != 0 {
		return translatedPayload{}, refused
	}
	request, err := protocols.ParseForModel(protocolwire.Chat, body, model)
	if err != nil {
		return translatedPayload{}, refused
	}
	root := request.OIF().Document().Root()
	options, _ := root.Lookup("stream_options")
	if options.Kind() != oif.Absent && options.Kind() != oif.Null {
		if options.Kind() != oif.Object {
			return translatedPayload{}, refused
		}
		for _, member := range options.Members() {
			if member.Name != "include_usage" || member.Value.Kind() != oif.Boolean {
				return translatedPayload{}, refused
			}
		}
	}
	effort, _ := root.Lookup("reasoning_effort")
	if effort.Kind() != oif.Absent && effort.Kind() != oif.Null && !slices.Contains([]string{"low", "medium", "high"}, textValue(effort)) {
		return translatedPayload{}, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		role, _ := message.Lookup("role")
		name, _ := message.Lookup("name")
		content, _ := message.Lookup("content")
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, textValue(role)) || name.Kind() != oif.Absent && name.Kind() != oif.Null || content.Kind() != oif.Null && !isTextContent(message, "content") {
			return translatedPayload{}, refused
		}
	}
	return encodeCodexResponses(request)
}

func textValue(value oif.Value) string {
	text, _ := value.Text()
	return text
}

func encodeCodexResponses(request *protocolwire.Request) (translatedPayload, error) {
	refused := code.Refuse(422, "code_translation_unsupported_semantics")
	encoded, _, encodeErr := protocols.EncodeTarget(request, protocolwire.Responses, "openai", "openai", request.Route, nil)
	if encodeErr != nil {
		return translatedPayload{}, refused
	}
	target, targetErr := protocols.ParseForModel(protocolwire.Responses, encoded, request.Route)
	if targetErr != nil {
		return translatedPayload{}, refused
	}
	input, _ := target.OIF().Document().Root().Lookup("input")
	roles := slices.Collect(func(yield func(oif.Change) bool) {
		for index, item := range input.Elements() {
			role, _ := item.Lookup("role")
			if text, _ := role.Text(); text == "system" && !yield(oif.Change{Pointer: fmt.Sprintf("/input/%d/role", index), Value: `"developer"`, Origin: oif.TransformedMapping, Reason: "Codex system instruction representation"}) {
				return
			}
		}
	})
	changes := slices.Concat([]oif.Change{
		{Pointer: "/stream", Value: "true", Origin: oif.TransportOption, Reason: "Codex subscription streaming transport"},
		{Pointer: "/store", Value: "false", Origin: oif.TransportOption, Reason: "Stateless code-mode translation"},
		{Pointer: "/instructions", Value: `""`, Origin: oif.TransportOption, Reason: "Codex required empty transport instructions"},
	}, roles)
	prepared, preparedErr := target.OIF().WithChanges(changes...)
	if preparedErr != nil {
		return translatedPayload{}, refused
	}
	return translatedPayload{body: prepared.Document().Bytes(), stream: request.Stream, translated: true, includeUsage: request.IncludeUsage}, nil
}

func isMessagesContent(message oif.Value) bool {
	content, _ := message.Lookup("content")
	if content.Kind() == oif.String {
		return true
	}
	if content.Kind() != oif.Array {
		return false
	}
	for _, block := range content.Elements() {
		kind, _ := block.Lookup("type")
		switch name, _ := kind.Text(); name {
		case "text", "tool_use":
		case "tool_result":
			if !isTextContent(block, "content") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isTextContent(parent oif.Value, key string) bool {
	value, present := parent.Lookup(key)
	if !present || value.Kind() == oif.String {
		return true
	}
	if value.Kind() != oif.Array {
		return false
	}
	for _, block := range value.Elements() {
		kind, _ := block.Lookup("type")
		if name, _ := kind.Text(); name != "text" {
			return false
		}
	}
	return true
}

func (a *subscriptionAttempt) translateResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, stream bool, family protocolwire.Format, includeUsage bool) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal := RefusalFromError(code.Refuse(response.StatusCode, "code_upstream_rejected"))
		a.core.server.Host.WriteTranslatedRejection(w, r, refusal)
		_ = http.NewResponseController(w).Flush()
		if response.StatusCode == http.StatusTooManyRequests {
			if body, err := readTranslatedRejection(ctx, response.Body, wire.MaxBody+1); err == nil {
				a.observeEncodedUnary(body, response.Header.Get("Content-Encoding"))
			}
		}
		a.core.terminal = true
		return
	}
	writer := a.core.server.Host.NewStreamWriter(w, family)
	completion, err := a.translatedCompletion(response, model, stream, family, includeUsage, writer.Emit)
	if err != nil {
		if !a.core.terminal {
			a.core.recordOutcome(ctx, code.UpstreamTerminalOutcome(code.OutcomeIncomplete, nil, a.core.server.Host.Now().UTC()))
		}
		writer.Fail(502, "code_upstream_invalid", ctx.Err() != nil)
		return
	}
	if stream {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(completion.Body)
}

func (a *subscriptionAttempt) translatedCompletion(response *http.Response, model string, stream bool, family protocolwire.Format, includeUsage bool, emit func([]byte) error) (*protocolwire.Completion, error) {
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		observe := func(event oif.Event) error {
			a.observe(a.core.observer.Event(event.Source().Bytes()))
			return nil
		}
		if stream {
			return protocols.StreamWithEvents(protocolwire.Responses, family, response.Body, wire.MaxBody, model, includeUsage, emit, observe)
		}
		return protocols.AggregateTranslatedResponses(family, response.Body, wire.MaxBody, model, observe)
	}
	if stream {
		return nil, code.Refuse(502, "code_upstream_invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxBody+1))
	if err != nil || len(body) > wire.MaxBody {
		return nil, code.Refuse(502, "code_upstream_invalid")
	}
	a.observeUnary(body)
	return protocols.Decode(protocolwire.Responses, family, body, model, "")
}

func translateAntigravity(body []byte, r *http.Request, model, project string) (translatedPayload, error) {
	refused := code.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || project == "" {
		return translatedPayload{}, refused
	}
	request, err := protocols.ParseForModel(protocolwire.Chat, body, model)
	if err != nil {
		return translatedPayload{}, refused
	}
	root := request.OIF().Document().Root()
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !isTextContent(message, "content") {
			content, present := message.Lookup("content")
			if !present || content.Kind() != oif.Null {
				return translatedPayload{}, refused
			}
		}
	}
	for _, field := range []string{"reasoning_effort", "reasoning", "thinking", "audio", "modalities", "prediction", "response_format", "service_tier", "n", "parallel_tool_calls"} {
		if request.Field(field) != nil {
			return translatedPayload{}, refused
		}
	}
	encoded, _, err := protocols.EncodeTarget(request, protocolwire.Gemini, "gemini", "google", model, nil)
	if err != nil {
		return translatedPayload{}, refused
	}
	wrapped, err := json.Marshal(struct {
		Project string          `json:"project"`
		Model   string          `json:"model"`
		Request json.RawMessage `json:"request"`
	}{Project: project, Model: model, Request: encoded})
	if err != nil {
		return translatedPayload{}, refused
	}
	query := ""
	if request.Stream {
		query = "alt=sse"
	}
	return translatedPayload{body: wrapped, query: query, stream: request.Stream, includeUsage: request.IncludeUsage, target: vendor.TargetAntigravity}, nil
}

func (a *subscriptionAttempt) antigravityResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, payload translatedPayload) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.core.server.writeError(w, r, code.Refuse(response.StatusCode, "code_upstream_rejected"))
		return
	}
	writer := a.core.server.Host.NewStreamWriter(w, protocolwire.Chat)
	completion, err := func() (*protocolwire.Completion, error) {
		if payload.stream {
			if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
				return nil, code.Refuse(502, "code_upstream_invalid")
			}
			reader := antigravityStream(response.Body, model)
			defer reader.Close()
			return protocols.StreamWithEvents(protocolwire.Gemini, protocolwire.Chat, reader, wire.MaxBody, model, payload.includeUsage, writer.Emit, nil, 1)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxBody+1))
		if err != nil || len(body) > wire.MaxBody {
			return nil, code.Refuse(502, "code_upstream_invalid")
		}
		unwrapped, stateErr := antigravityResult(body)
		completion, err := protocols.Decode(protocolwire.Gemini, protocolwire.Chat, unwrapped, model, "")
		if err != nil {
			return nil, err
		}
		return completion, stateErr
	}()
	if unsupported, ok := errors.AsType[*antigravityStateError](err); ok {
		a.observe(antigravityObservation(unsupported.completion, true, a.core.server.Host.Now().UTC()))
	}
	a.observe(antigravityObservation(completion, err == nil || !payload.stream && completion != nil, a.core.server.Host.Now().UTC()))
	if err != nil {
		errCode := "code_upstream_invalid"
		if refusal, ok := errors.AsType[*code.Refusal](err); ok && refusal.Code == "code_upstream_unsupported_state" {
			errCode = refusal.Code
		}
		writer.Fail(502, errCode, ctx.Err() != nil)
		return
	}
	if payload.stream {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(completion.Body)
}

type antigravityStateError struct {
	*code.Refusal
	completion *protocolwire.Completion
}

func (e *antigravityStateError) Unwrap() error { return e.Refusal }

func antigravityStream(source io.Reader, model string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		err := sse.Decode(source, wire.MaxBody, func(frame sse.Frame) error {
			if frame.Control {
				_, err := writer.Write(frame.Encode())
				return err
			}
			body, err := antigravityResult([]byte(frame.Data))
			if err != nil {
				if refusal, ok := errors.AsType[*code.Refusal](err); ok && refusal.Code == "code_upstream_unsupported_state" {
					completion, _ := protocols.Decode(protocolwire.Gemini, protocolwire.Gemini, body, model, "")
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
	refused := code.Refuse(502, "code_upstream_invalid")
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
			return result.Bytes(), code.Refuse(502, "code_upstream_unsupported_state")
		}
		content, _ := candidate.Lookup("content")
		parts, _ := content.Lookup("parts")
		for _, part := range parts.Elements() {
			if part.Kind() != oif.Object || len(part.Members()) != 1 || !slices.Contains([]string{"text", "functionCall"}, part.Members()[0].Name) {
				return result.Bytes(), code.Refuse(502, "code_upstream_unsupported_state")
			}
			call, found := part.Lookup("functionCall")
			if found && slices.ContainsFunc(call.Members(), func(member oif.Member) bool { return !slices.Contains([]string{"id", "name", "args"}, member.Name) }) {
				return result.Bytes(), code.Refuse(502, "code_upstream_unsupported_state")
			}
		}
	}
	return result.Bytes(), nil
}

func antigravityObservation(completion *protocolwire.Completion, complete bool, now time.Time) code.Observation {
	if completion == nil || !complete && !completion.UsageComplete {
		return code.Observation{}
	}
	var usage code.Usage
	if completion.Usage != nil {
		usage = code.Usage{Total: new(completion.Usage.TotalTokens), Input: new(completion.Usage.InputTokens), Output: new(completion.Usage.OutputTokens), Cached: completion.Usage.CachedInputTokens, Reasoning: completion.Usage.ReasoningTokens}
	}
	kind := code.OutcomeCompleted
	if completion.FinishReason == "length" || completion.Failure != nil {
		kind = code.OutcomeIncomplete
	}
	outcome := code.UpstreamTerminalOutcome(kind, nil, now)
	return code.Observation{Terminal: true, Successful: complete && kind == code.OutcomeCompleted, Usage: usage, Outcome: &outcome}
}
