package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type codePayload struct {
	body         []byte
	query        string
	stream       bool
	translated   bool
	includeUsage bool
	target       codeadapter.Target
}

func codeTranslateMessages(body []byte, r *http.Request, model string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil || (len(query) != 0 && (len(query) != 1 || !slices.Equal(query["beta"], []string{"true"}))) || r.Header.Get("Anthropic-Beta") != "" || len(r.Trailer) != 0 {
		return codePayload{}, refused
	}
	request, requestErr := protocols.ParseForModel(wire.Anthropic, body, model)
	if requestErr != nil {
		return codePayload{}, refused
	}
	if request.Field("stream_options") != nil {
		return codePayload{}, refused
	}
	root := request.OIF().Document().Root()
	if !codeTextContent(root, "system") {
		return codePayload{}, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !codeMessagesContent(message) {
			return codePayload{}, refused
		}
	}
	return codeTranslateResponses(request)
}

func codeTranslateChat(body []byte, r *http.Request, model string) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.URL.RawQuery != "" || len(r.Trailer) != 0 || len(r.Header.Values("X-Codex-Turn-State")) != 0 || len(r.Header.Values("OpenAI-Beta")) != 0 {
		return codePayload{}, refused
	}
	request, err := protocols.ParseForModel(wire.Chat, body, model)
	if err != nil {
		return codePayload{}, refused
	}
	root := request.OIF().Document().Root()
	options, _ := root.Lookup("stream_options")
	if options.Kind() != oif.Absent && options.Kind() != oif.Null {
		if options.Kind() != oif.Object {
			return codePayload{}, refused
		}
		for _, member := range options.Members() {
			if member.Name != "include_usage" || member.Value.Kind() != oif.Boolean {
				return codePayload{}, refused
			}
		}
	}
	effort, _ := root.Lookup("reasoning_effort")
	if effort.Kind() != oif.Absent && effort.Kind() != oif.Null && !slices.Contains([]string{"low", "medium", "high"}, stringValue(effort)) {
		return codePayload{}, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		role, _ := message.Lookup("role")
		name, _ := message.Lookup("name")
		content, _ := message.Lookup("content")
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, stringValue(role)) || name.Kind() != oif.Absent && name.Kind() != oif.Null || content.Kind() != oif.Null && !codeTextContent(message, "content") {
			return codePayload{}, refused
		}
	}
	return codeTranslateResponses(request)
}

func stringValue(value oif.Value) string {
	text, _ := value.Text()
	return text
}

func codeTranslateResponses(request *wire.Request) (codePayload, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	encoded, _, encodeErr := protocols.EncodeTarget(request, wire.Responses, "openai", "openai", request.Route, nil)
	if encodeErr != nil {
		return codePayload{}, refused
	}
	target, targetErr := protocols.ParseForModel(wire.Responses, encoded, request.Route)
	if targetErr != nil {
		return codePayload{}, refused
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
		return codePayload{}, refused
	}
	return codePayload{body: prepared.Document().Bytes(), stream: request.Stream, translated: true, includeUsage: request.IncludeUsage}, nil
}

func codeMessagesContent(message oif.Value) bool {
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
			if !codeTextContent(block, "content") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func codeTextContent(parent oif.Value, key string) bool {
	value, present := parent.Lookup(key)
	if !present || value.Kind() == oif.String {
		return true
	}
	if value.Kind() != oif.Array {
		return false
	}
	for _, block := range value.Elements() {
		kind, _ := block.Lookup("type")
		name, _ := kind.Text()
		if name != "text" {
			return false
		}
	}
	return true
}

func (a *codeAttempt) translateResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, stream bool, family wire.Format, includeUsage bool) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		codeWriteError(w, r, codemode.Refuse(response.StatusCode, "code_upstream_rejected"))
		return
	}
	writer := &streamWriter{w: w, family: family}
	completion, err := a.translatedCompletion(response, model, stream, family, includeUsage, writer.emit)
	if err != nil {
		if !a.terminal {
			a.outcome(ctx, codemode.Outcome{Origin: "upstream", Kind: "incomplete"})
		}
		writer.finish(&outcome{err: &Error{Status: 502, Code: "code_upstream_invalid", Type: "api_error", Message: "code_upstream_invalid"}, cancelled: ctx.Err() != nil})
		return
	}
	if stream {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(completion.Body)
}

func (a *codeAttempt) translatedCompletion(response *http.Response, model string, stream bool, family wire.Format, includeUsage bool, emit func([]byte) error) (*wire.Completion, error) {
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		observe := func(event oif.Event) error {
			a.observe(a.observer.Event(event.Source().Bytes()))
			return nil
		}
		if stream {
			return protocols.StreamWithEvents(wire.Responses, family, response.Body, codewire.MaxBody, model, includeUsage, emit, observe)
		}
		return protocols.AggregateTranslatedResponses(family, response.Body, codewire.MaxBody, model, observe)
	}
	if stream {
		return nil, codemode.Refuse(502, "code_upstream_invalid")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
	if err != nil || len(body) > codewire.MaxBody {
		return nil, codemode.Refuse(502, "code_upstream_invalid")
	}
	a.observe(a.observer.Unary(body))
	return protocols.Decode(wire.Responses, family, body, model, "")
}
