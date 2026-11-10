package wire

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/oif"
)

type Observer interface {
	Event(data []byte) code.Observation
	Unary(body []byte) code.Observation
	Finish() (code.Observation, bool)
}

func NewObserver(endpoint vendor.Endpoint) Observer {
	switch endpoint.Protocol {
	case code.ProtocolResponses:
		return NewResponses(endpoint.Observation == vendor.ObserveCodex)
	case code.ProtocolMessages:
		return NewMessages()
	case code.ProtocolChat:
		return NewChat()
	}
	return nil
}

func ClassifyRequest(endpoint vendor.Endpoint, body []byte, headers http.Header, websocket bool) (code.Request, error) {
	switch endpoint.Protocol {
	case code.ProtocolResponses:
		return ClassifyResponses(body, headers, endpoint.Operation, endpoint.Identity, websocket)
	case code.ProtocolMessages:
		return classifyClientRequest(body, headers, string(code.ProtocolMessages), "max_tokens")
	case code.ProtocolChat:
		return classifyClientRequest(body, headers, string(code.ProtocolChat), "max_completion_tokens", "max_tokens")
	}
	return code.Request{}, code.Refuse(400, "code_operation_unsupported")
}

func classifyClientRequest(body []byte, headers http.Header, operation string, caps ...string) (code.Request, error) {
	var request code.Request
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return request, code.Refuse(400, "code_body_invalid")
	}
	root := document.Root()
	model, ok := field(root, "model").Text()
	if !ok || code.ValidateModels([]string{model}) != nil {
		return request, code.Refuse(400, "code_model_invalid")
	}
	identity, err := ClientIdentity(headers, root)
	if err != nil {
		return request, err
	}
	request.Operation = code.Operation{Name: operation, Model: model, Identity: identity}
	if err := ValidateGenericRequest(root, headers, request); err != nil {
		return request, err
	}
	request.Estimate = estimateTokens(body, root, caps...)
	return request, nil
}

func failedObservation(status int, body []byte) code.Observation {
	var upstreamStatus *int
	if status != 0 {
		upstreamStatus = &status
	}
	outcome := code.UpstreamTerminalOutcome(code.OutcomeFailed, upstreamStatus, time.Now().UTC())
	return code.Observation{Terminal: true, Status: status, Outcome: &outcome, RejectionBody: RejectionBody(body)}
}
