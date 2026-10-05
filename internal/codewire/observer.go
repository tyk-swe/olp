package codewire

import (
	"net/http"
	"strconv"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

// Observer reads one generation's upstream response from copies of its
// bytes. Event takes each SSE event's data and Unary a whole body; Finish
// reports the outcome a stream implied without its final event.
type Observer interface {
	Event(data []byte) codemode.Observation
	Unary(body []byte) codemode.Observation
	Finish() (codemode.Observation, bool)
}

// ClassifyMessages reads an Anthropic Messages request.
func ClassifyMessages(body []byte, headers http.Header) (codemode.Request, error) {
	return classify(body, headers, string(codemode.ProtocolMessages), "max_tokens")
}

// ClassifyChat reads a Chat Completions request.
func ClassifyChat(body []byte, headers http.Header) (codemode.Request, error) {
	return classify(body, headers, string(codemode.ProtocolChat), "max_completion_tokens", "max_tokens")
}

func classify(body []byte, headers http.Header, operation string, caps ...string) (codemode.Request, error) {
	var request codemode.Request
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return request, codemode.Refuse(400, "code_body_invalid")
	}
	root := document.Root()
	model, ok := field(root, "model").Text()
	if !ok || codemode.ValidateModels([]string{model}) != nil {
		return request, codemode.Refuse(400, "code_model_invalid")
	}
	identity, err := ClientIdentity(headers)
	if err != nil {
		return request, err
	}
	request.Operation = codemode.Operation{Name: operation, Model: model, Identity: identity}
	// This is a rate estimate only; prompt tokens have no proven bound.
	request.Estimate = max(1, int64(len(body))/4)
	for _, name := range caps {
		if cap, err := strconv.ParseInt(field(root, name).Raw(), 10, 64); err == nil && cap > 0 && cap <= 1<<32 {
			request.Estimate += cap
			break
		}
	}
	return request, nil
}

func count(value oif.Value) *int64 {
	if value.Kind() != oif.Number {
		return nil
	}
	n, err := strconv.ParseInt(value.Raw(), 10, 64)
	if err != nil || n < 0 || n > 1<<53-1 {
		return nil
	}
	return &n
}
