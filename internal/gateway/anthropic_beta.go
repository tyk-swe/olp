package gateway

import (
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// maxAnthropicBeta is the longest Anthropic-Beta list a request may carry, as
// a strict route's profile binding allows.
const maxAnthropicBeta = 2048

// anthropicBeta is the caller's Anthropic-Beta list: the header lines of one
// name are one comma-separated list. It is empty when the caller sent none, and
// not ok when what it sent cannot go upstream.
func anthropicBeta(header http.Header) (beta string, ok bool) {
	beta = strings.Join(header.Values("Anthropic-Beta"), ",")
	return beta, len(beta) <= maxAnthropicBeta && httpguts.ValidHeaderFieldValue(beta)
}

// carriesAnthropicSource is whether the caller's request is in the Anthropic
// Messages dialect, so that its Anthropic-Beta header has meaning.
func carriesAnthropicSource(x *execution) bool {
	return x.parsed.Family == openai.FamilyAnthropic || x.parsed.Family == openai.FamilyAnthropicCount
}

// takesAnthropicBeta is whether a provider is sent the Anthropic-Beta header:
// an automatic Anthropic provider, or one whose profile declares the header
// among its semantic headers, as the Vertex AI hosting of Claude does. The
// profile of Bedrock InvokeModel declares none, so it never receives it.
func takesAnthropicBeta(cfg connectors.Config) bool {
	if cfg.ProfileID == "" {
		return cfg.Kind == "anthropic"
	}
	profile, err := cfg.Profile()
	return err == nil && slices.Contains(profile.SemanticHeaders, "Anthropic-Beta")
}

// servesAnthropicBeta is whether a target of the route is a provider that
// takes the header.
func (x *execution) servesAnthropicBeta() bool {
	providers := x.snapshot().Providers
	return slices.ContainsFunc(x.route.Targets, func(t runtime.Target) bool {
		provider := providers[t.ProviderID]
		return takesAnthropicBeta(provider.Connector())
	})
}

// checkAnthropicBeta refuses, before any provider is called, an Anthropic
// request whose Anthropic-Beta header cannot be forwarded to a provider that
// takes it, which a transformed route may send the request to. Dropping it
// would send the beta fields of the body without their header, which the
// upstream refuses. A strict route validates the header when its profile binds
// it.
func (x *execution) checkAnthropicBeta() *Error {
	if x.strict() || !carriesAnthropicSource(x) || !x.servesAnthropicBeta() {
		return nil
	}
	if _, ok := anthropicBeta(x.semanticHeaders); !ok {
		param := "anthropic-beta"
		return invalidRequest("invalid_request", "The Anthropic-Beta header must be a valid header value of at most 2048 bytes.", &param)
	}
	return nil
}

// forwardAnthropicBeta passes the caller's Anthropic-Beta header to a provider
// that takes it when a transformed route hands it the caller's own request.
// The codec keeps the native fields of the body, and the fields of a beta
// travel with its header: the upstream refuses a field whose beta it was not
// sent. A request translated from another dialect carries no Anthropic
// semantics, so it gets none, and neither does a provider that takes the
// betas some other way. A strict route binds the header through its profile
// instead.
func forwardAnthropicBeta(header http.Header, x *execution, wire openai.Family, cfg connectors.Config) {
	if wire != x.parsed.Family || !carriesAnthropicSource(x) || !takesAnthropicBeta(cfg) {
		return
	}
	if beta, ok := anthropicBeta(x.semanticHeaders); ok && beta != "" {
		header.Set("Anthropic-Beta", beta)
	}
}
