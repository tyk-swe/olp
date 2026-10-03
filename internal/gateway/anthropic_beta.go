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
	return cfg.DeclaresSemanticHeader("Anthropic-Beta")
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

// sendsAnthropicBeta is whether the caller sent an Anthropic-Beta header at all,
// which is all a request that sent none has to learn: its providers' own betas
// are sent as the provider publishes them.
func (x *execution) sendsAnthropicBeta() bool {
	return len(x.semanticHeaders.Values("Anthropic-Beta")) > 0
}

// anthropicBetaFor is the Anthropic-Beta list a provider that takes the header
// is sent for the caller's: the betas the provider publishes, which the operator
// chose to send with everything, and then the caller's own, which the fields of
// its request need. Each beta is sent once. A provider that publishes none is
// sent the caller's list as it was written. It is not ok when the joined list
// is longer than one a strict route's profile binding allows.
func anthropicBetaFor(cfg connectors.Config, caller string) (beta string, ok bool) {
	published, has := cfg.SemanticHeader("Anthropic-Beta")
	if !has || strings.TrimSpace(published) == "" {
		return caller, true
	}
	var joined []string
	for _, list := range [...]string{published, caller} {
		for entry := range strings.SplitSeq(list, ",") {
			if entry = strings.TrimSpace(entry); entry != "" && !slices.Contains(joined, entry) {
				joined = append(joined, entry)
			}
		}
	}
	beta = strings.Join(joined, ",")
	return beta, len(beta) <= maxAnthropicBeta
}

// checkAnthropicBeta refuses, before any provider is called, an Anthropic
// request whose Anthropic-Beta header cannot be forwarded to a provider that
// takes it, which a transformed route may send the request to: a header that is
// not a valid value, and one that is too long once it is joined with the betas
// a provider publishes. Dropping it would send the beta fields of the body
// without their header, which the upstream refuses. A strict route validates the
// header when its profile binds it.
func (x *execution) checkAnthropicBeta() *Error {
	if x.strict() || !carriesAnthropicSource(x) || !x.sendsAnthropicBeta() || !x.servesAnthropicBeta() {
		return nil
	}
	caller, ok := anthropicBeta(x.semanticHeaders)
	if !ok {
		param := "anthropic-beta"
		return invalidRequest("invalid_request", "The Anthropic-Beta header must be a valid header value of at most 2048 bytes.", &param)
	}
	providers := x.snapshot().Providers
	for _, target := range x.route.Targets {
		provider := providers[target.ProviderID]
		cfg := provider.Connector()
		if !takesAnthropicBeta(cfg) {
			continue
		}
		if _, ok := anthropicBetaFor(cfg, caller); !ok {
			param := "anthropic-beta"
			return invalidRequest("invalid_request", "The Anthropic-Beta header, with the betas a provider of the route publishes, must be at most 2048 bytes.", &param)
		}
	}
	return nil
}

// forwardAnthropicBeta passes the caller's Anthropic-Beta header to a provider
// that takes it when a transformed route hands it the caller's own request, and
// returns the connector the request is then hosted by. The codec keeps the
// native fields of the body, and the fields of a beta travel with its header:
// the upstream refuses a field whose beta it was not sent. A request translated
// from another dialect carries no Anthropic semantics, so it gets none, and
// neither does a provider that takes the betas some other way. A strict route
// binds the header through its profile instead.
//
// A provider whose profile publishes betas of its own has them applied when the
// request is hosted, which would replace a header set here, so the list to send
// is published in the connector the request is hosted by instead.
func forwardAnthropicBeta(header http.Header, x *execution, wire openai.Family, cfg connectors.Config) connectors.Config {
	if wire != x.parsed.Family || !carriesAnthropicSource(x) || !x.sendsAnthropicBeta() || !takesAnthropicBeta(cfg) {
		return cfg
	}
	caller, ok := anthropicBeta(x.semanticHeaders)
	if !ok || caller == "" {
		return cfg
	}
	beta, ok := anthropicBetaFor(cfg, caller)
	if !ok {
		return cfg
	}
	if cfg.ProfileID == "" {
		header.Set("Anthropic-Beta", beta)
		return cfg
	}
	return cfg.WithSemanticHeader("Anthropic-Beta", beta)
}
