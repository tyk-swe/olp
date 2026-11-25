package anthropicbeta

import (
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

const maxBetaBytes = 2048

func parseHeader(header http.Header) (beta string, ok bool) {
	beta = strings.Join(header.Values("Anthropic-Beta"), ",")
	return beta, len(beta) <= maxBetaBytes && httpguts.ValidHeaderFieldValue(beta)
}

func isAnthropicSource(family wire.Format) bool {
	return family == wire.Anthropic || family == wire.AnthropicCount
}

func mergeFor(cfg connectors.Config, caller string) (beta string, ok bool) {
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
	return beta, len(beta) <= maxBetaBytes
}

// Check validates a caller's Anthropic-Beta header against all route targets
// that accept it before any upstream attempt is made.
func Check(strict bool, sourceFamily wire.Format, semanticHeaders http.Header, targets []connectors.Config) *fault.Error {
	if strict || !isAnthropicSource(sourceFamily) || len(semanticHeaders.Values("Anthropic-Beta")) == 0 {
		return nil
	}
	if !slices.ContainsFunc(targets, func(cfg connectors.Config) bool { return cfg.TakesAnthropicBeta() }) {
		return nil
	}
	caller, ok := parseHeader(semanticHeaders)
	if !ok {
		param := "anthropic-beta"
		return fault.InvalidRequest("invalid_request", "The Anthropic-Beta header must be a valid header value of at most 2048 bytes.", &param)
	}
	for _, cfg := range targets {
		if !cfg.TakesAnthropicBeta() {
			continue
		}
		if _, ok := mergeFor(cfg, caller); !ok {
			param := "anthropic-beta"
			return fault.InvalidRequest("invalid_request", "The Anthropic-Beta header, with the betas a provider of the route publishes, must be at most 2048 bytes.", &param)
		}
	}
	return nil
}

// Forward applies a caller's Anthropic-Beta header to a compatible upstream
// target, merging it with any provider-published betas on profiled connectors.
func Forward(header http.Header, sourceFamily, encodedWire wire.Format, semanticHeaders http.Header, cfg connectors.Config) connectors.Config {
	if encodedWire != sourceFamily || !isAnthropicSource(sourceFamily) || len(semanticHeaders.Values("Anthropic-Beta")) == 0 || !cfg.TakesAnthropicBeta() {
		return cfg
	}
	caller, ok := parseHeader(semanticHeaders)
	if !ok || caller == "" {
		return cfg
	}
	beta, ok := mergeFor(cfg, caller)
	if !ok {
		return cfg
	}
	if cfg.ProfileID == "" {
		header.Set("Anthropic-Beta", beta)
		return cfg
	}
	return cfg.WithSemanticHeader("Anthropic-Beta", beta)
}
