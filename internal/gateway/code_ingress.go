package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/oif"
)

type codeIngress struct {
	path      string
	protocol  codemode.Protocol
	operation string
	identity  codeadapter.IdentityPolicy
}

func codeIngressFor(path string) (codeIngress, bool) {
	endpoint, ok := codeadapter.ForPath(path)
	if !ok {
		return codeIngress{}, false
	}
	return codeIngress{path: path, protocol: endpoint.Protocol, operation: endpoint.Operation, identity: endpoint.Identity}, true
}

func classifyCode(ingress codeIngress, body []byte, headers http.Header, websocket bool) (codemode.Request, error) {
	switch ingress.protocol {
	case codemode.ProtocolResponses:
		if ingress.identity == codeadapter.IdentityCodex {
			return codexwire.Classify(body, headers, ingress.operation, websocket)
		}
		return codexwire.ClassifyWith(body, ingress.operation, websocket, func(oif.Value) (codemode.Identity, error) { return codewire.ClientIdentity(headers) })
	case codemode.ProtocolMessages:
		return codewire.ClassifyMessages(body, headers)
	case codemode.ProtocolChat:
		return codewire.ClassifyChat(body, headers)
	}
	return codemode.Request{}, codemode.Refuse(400, "code_operation_unsupported")
}

func codeConnectionIdentity(ingress codeIngress, headers http.Header) (codemode.Identity, error) {
	if ingress.identity == codeadapter.IdentityCodex {
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
