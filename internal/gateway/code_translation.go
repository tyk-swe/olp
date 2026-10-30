package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func codeTranslateMessages(body []byte, r *http.Request) ([]byte, bool, error) {
	refused := codemode.Refuse(422, "code_translation_unsupported_semantics")
	if r.Header.Get("Anthropic-Beta") != "" || r.URL.RawQuery != "" || len(r.Trailer) != 0 {
		return nil, false, refused
	}
	request, err := protocols.Parse(wire.Anthropic, body, "")
	if err != nil {
		return nil, false, refused
	}
	for _, name := range []string{"tools", "tool_choice"} {
		if request.Field(name) != nil {
			return nil, false, refused
		}
	}
	root := request.OIF().Document().Root()
	if !codeTextContent(root, "system") {
		return nil, false, refused
	}
	messages, _ := root.Lookup("messages")
	for _, message := range messages.Elements() {
		if !codeTextContent(message, "content") {
			return nil, false, refused
		}
	}
	encoded, _, err := protocols.EncodeTarget(request, wire.Responses, "openai", "openai", request.Route, nil)
	if err != nil {
		return nil, false, refused
	}
	target, err := protocols.Parse(wire.Responses, encoded, request.Route)
	if err != nil {
		return nil, false, refused
	}
	prepared, err := target.OIF().WithChanges(
		oif.Change{Pointer: "/stream", Value: "true", Origin: oif.TransportOption, Reason: "Codex subscription streaming transport"},
		oif.Change{Pointer: "/store", Value: "false", Origin: oif.TransportOption, Reason: "Stateless Messages translation"},
	)
	if err != nil {
		return nil, false, refused
	}
	return prepared.Document().Bytes(), request.Stream, nil
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

func (a *codeAttempt) translateResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, response *http.Response, model string, stream bool) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		codeWriteError(w, r, codemode.Refuse(response.StatusCode, "code_upstream_rejected"))
		return
	}
	var completion *wire.Completion
	var err error
	writer := &streamWriter{w: w, family: wire.Anthropic}
	emit := func(frame []byte) error {
		if !stream {
			return nil
		}
		return writer.emit(frame)
	}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		observe := func(event oif.Event) error {
			a.observe(a.observer.Event(event.Source().Bytes()))
			return nil
		}
		if stream {
			completion, err = protocols.StreamWithEvents(wire.Responses, wire.Anthropic, response.Body, codewire.MaxBody, model, true, emit, observe)
		} else {
			aggregated, aggregateErr := openai.AggregateEvents(wire.Responses, response.Body, codewire.MaxBody, codewire.MaxBody, observe)
			err = aggregateErr
			if aggregateErr == nil {
				completion, err = protocols.Decode(wire.Responses, wire.Anthropic, aggregated.Body, model, "")
			}
		}
	} else if stream {
		err = codemode.Refuse(502, "code_upstream_invalid")
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
		if readErr != nil || len(body) > codewire.MaxBody {
			err = codemode.Refuse(502, "code_upstream_invalid")
		} else {
			a.observe(a.observer.Unary(body))
			completion, err = protocols.Decode(wire.Responses, wire.Anthropic, body, model, "")
		}
	}
	if err != nil {
		if !a.terminal {
			a.outcome(ctx, codemode.Outcome{Origin: "upstream", Kind: "incomplete"})
		}
		writer.finish(&outcome{err: &Error{Status: 502, Code: "code_upstream_invalid", Type: "api_error", Message: "code_upstream_invalid"}, cancelled: ctx.Err() != nil})
		return
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(protocols.RenderCompletion(completion, wire.Anthropic, model))
	}
}
