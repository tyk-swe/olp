package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/oif"
)

// codeIngress is one request path a code route's adapter serves over HTTP.
type codeIngress struct {
	protocol codemode.Protocol
	// upstream extends the connection's endpoint.
	upstream string
	classify func(body []byte, headers http.Header) (codemode.Request, error)
	observer func() codewire.Observer
}

// codeIngressFor returns the ingress of a POST to path, relative to the route,
// or false when the route's adapter serves no such path.
func codeIngressFor(adapter codemode.Adapter, method, path string) (codeIngress, bool) {
	if method != http.MethodPost {
		return codeIngress{}, false
	}
	messages := codeIngress{protocol: codemode.ProtocolMessages, classify: codewire.ClassifyMessages, observer: func() codewire.Observer { return codewire.NewMessages() }}
	chat := codeIngress{protocol: codemode.ProtocolChat, classify: codewire.ClassifyChat, observer: func() codewire.Observer { return codewire.NewChat() }}
	switch adapter {
	case codemode.AdapterCodex:
		if path == "responses" || path == "responses/compact" {
			return codeIngress{protocol: codemode.ProtocolResponses, upstream: path,
				classify: func(body []byte, headers http.Header) (codemode.Request, error) {
					return codexwire.Classify(body, headers, path, false)
				},
				observer: func() codewire.Observer { return codeResponses{allowance: true} },
			}, true
		}
	case codemode.AdapterOpenCodeGo:
		switch path {
		case "v1/chat/completions":
			chat.upstream = "chat/completions"
			return chat, true
		case "v1/messages":
			messages.upstream = "messages"
			return messages, true
		case "v1/responses":
			return codeIngress{protocol: codemode.ProtocolResponses, upstream: "responses",
				classify: func(body []byte, headers http.Header) (codemode.Request, error) {
					return codexwire.ClassifyWith(body, "responses", false, func(oif.Value) (codemode.Identity, error) { return codewire.ClientIdentity(headers) })
				},
				observer: func() codewire.Observer { return codeResponses{} },
			}, true
		}
	case codemode.AdapterZAICoding:
		switch path {
		case "v1/messages":
			messages.upstream = "anthropic/v1/messages"
			return messages, true
		case "v1/chat/completions":
			chat.upstream = "coding/paas/v4/chat/completions"
			return chat, true
		}
	}
	return codeIngress{}, false
}

// codeProbe reports a request Claude Code makes to discover what a gateway
// serves, which a code route answers as not found without recording a refusal.
func codeProbe(adapter codemode.Adapter, method, path string) bool {
	return adapter != codemode.AdapterCodex && (method == http.MethodHead && path == "api/hello" || method == http.MethodPost && path == "v1/messages/count_tokens")
}

// codeResponses observes a Responses API stream. Only Codex reports
// subscription allowance in it.
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
