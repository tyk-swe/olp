package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/oif"
)

// codeIngressFor resolves shared request semantics from the static registry.
// Forwarding and observation use the endpoint of the account selected later.
func codeIngressFor(method, path string) (codeadapter.Endpoint, bool) {
	if method != http.MethodPost {
		return codeadapter.Endpoint{}, false
	}
	return codeadapter.ForPath(path)
}

func classifyCode(endpoint codeadapter.Endpoint, body []byte, headers http.Header, websocket bool) (codemode.Request, error) {
	switch endpoint.Protocol {
	case codemode.ProtocolResponses:
		if endpoint.Identity == codeadapter.IdentityCodex {
			return codexwire.Classify(body, headers, endpoint.Operation, websocket)
		}
		return codexwire.ClassifyWith(body, endpoint.Operation, websocket, func(oif.Value) (codemode.Identity, error) { return codewire.ClientIdentity(headers) })
	case codemode.ProtocolMessages:
		return codewire.ClassifyMessages(body, headers)
	case codemode.ProtocolChat:
		return codewire.ClassifyChat(body, headers)
	}
	return codemode.Request{}, codemode.Refuse(400, "code_operation_unsupported")
}

func codeConnectionIdentity(endpoint codeadapter.Endpoint, headers http.Header) (codemode.Identity, error) {
	if endpoint.Identity == codeadapter.IdentityCodex {
		return codexwire.ConnectionIdentity(headers)
	}
	return codewire.ClientIdentity(headers)
}

func codeObserver(endpoint codeadapter.Endpoint) codewire.Observer {
	switch endpoint.Protocol {
	case codemode.ProtocolResponses:
		return codeResponses{allowance: endpoint.Observation == codeadapter.ObserveCodex}
	case codemode.ProtocolMessages:
		return codewire.NewMessages()
	case codemode.ProtocolChat:
		return codewire.NewChat()
	}
	return nil // Declarations reject protocols without a native handler.
}

// codeProbe reports a request Claude Code makes to discover what a gateway
// serves, which a route serving Anthropic Messages answers as not found
// without recording a refusal.
func codeProbe(method, path string) bool {
	return method == http.MethodHead && path == "api/hello" || method == http.MethodPost && path == "v1/messages/count_tokens"
}

// codeResponses observes a Responses API stream, including subscription
// allowance only when the selected endpoint declares it.
type codeResponses struct{ allowance bool }

func (o codeResponses) Event(data []byte) codemode.Observation {
	return o.strip(codexwire.Observe(data, false))
}

func (o codeResponses) Unary(body []byte) codemode.Observation {
	return o.strip(codexwire.Observe(body, true))
}

func (codeResponses) Finish() (codemode.Observation, bool) { return codemode.Observation{}, false }

func (o codeResponses) strip(observation codemode.Observation) codemode.Observation {
	if !o.allowance {
		observation.Allowance = nil
	}
	return observation
}
