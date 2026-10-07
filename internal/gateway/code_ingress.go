package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/oif"
)

// codeIngress is one client path a code route serves over HTTP. The path
// names the client family, whose identity and protocol it carries; the
// account's adapter decides where upstream it goes.
type codeIngress struct {
	protocol codemode.Protocol
	classify func(body []byte, headers http.Header) (codemode.Request, error)
	observer func() codewire.Observer
}

// codeIngressFor returns the ingress of a POST to path, relative to the route,
// or false when no adapter serves such a path.
func codeIngressFor(method, path string) (codeIngress, bool) {
	if method != http.MethodPost {
		return codeIngress{}, false
	}
	switch path {
	case "responses", "responses/compact":
		return codeIngress{protocol: codemode.ProtocolResponses,
			classify: func(body []byte, headers http.Header) (codemode.Request, error) {
				return codexwire.Classify(body, headers, path, false)
			},
			observer: func() codewire.Observer { return codeResponses{allowance: true} },
		}, true
	case "v1/messages":
		return codeIngress{protocol: codemode.ProtocolMessages, classify: codewire.ClassifyMessages, observer: func() codewire.Observer { return codewire.NewMessages() }}, true
	case "v1/chat/completions":
		return codeIngress{protocol: codemode.ProtocolChat, classify: codewire.ClassifyChat, observer: func() codewire.Observer { return codewire.NewChat() }}, true
	case "v1/responses":
		return codeIngress{protocol: codemode.ProtocolResponses,
			classify: func(body []byte, headers http.Header) (codemode.Request, error) {
				return codexwire.ClassifyWith(body, "responses", false, func(oif.Value) (codemode.Identity, error) { return codewire.ClientIdentity(headers) })
			},
			observer: func() codewire.Observer { return codeResponses{} },
		}, true
	}
	return codeIngress{}, false
}

// codeProbe reports a request Claude Code makes to discover what a gateway
// serves, which a route serving Anthropic Messages answers as not found
// without recording a refusal.
func codeProbe(method, path string) bool {
	return method == http.MethodHead && path == "api/hello" || method == http.MethodPost && path == "v1/messages/count_tokens"
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
