package connectors

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// GenerationURL resolves the invocation address from a registered generation
// dialect's declared addressing contract rather than a wire-family table. A
// dialect with a registered relative path appends it to the connector endpoint
// (direct hosting only); a checked-in legacy path delegates to the existing
// hosting address table, which remains the only place cloud/SSE addressing is
// spelled out.
func (c Config) GenerationURL(dialect generation.Dialect, model string, stream bool) (string, error) {
	if dialect.Operation != generation.Contract() {
		return "", errors.New("address requires a registered generation dialect")
	}
	if c.ProfileID != "" {
		if err := c.ValidateProfile(); err != nil {
			return "", err
		}
		p, _ := profileView(c.ProfileID, c.ProfileRevision)
		if !slices.Contains(p.Operations, "generation") || p.OperationDialect("generation") != dialect.Label {
			return "", errors.New("generation dialect does not match the selected provider profile")
		}
	}
	if dialect.Address.RelativePath != "" {
		if c.Endpoint == "" {
			return "", errors.New("connector endpoint is required for relative addressing")
		}
		if hosting := c.Hosting(); hosting != "" && !strings.HasPrefix(hosting, "direct-") {
			return "", errors.New("relative generation addressing requires a direct hosting contract")
		}
		if !ModelValid(c.Kind, model) {
			return "", errors.New("invalid upstream model identifier")
		}
		path := strings.TrimSuffix(c.Endpoint, "/") + "/" + dialect.Address.RelativePath
		if stream && dialect.DeliveryKey != "" {
			path += "?" + dialect.DeliveryKey + "=" + url.QueryEscape(dialect.DeliveryValue)
		}
		return path, nil
	}
	if dialect.Address.LegacyPath == "" {
		return "", errors.New("generation dialect declares no addressing contract")
	}
	return c.URL(openai.Family(dialect.Address.LegacyPath), model, stream)
}

// SupportsGenerationTarget reports whether the configured profile's registered
// generation dialect can serve the requested surface and delivery mode. When
// the profile's dialect has no registration the legacy capability matrix still
// answers, so pre-registration deployments keep their behavior.
func (c Config) SupportsGenerationTarget(surface, mode string) (bool, bool) {
	if c.ProfileID == "" {
		return false, false
	}
	p, err := profileView(c.ProfileID, c.ProfileRevision)
	if err != nil || !slices.Contains(p.Operations, "generation") {
		return false, false
	}
	dialect, ok := operationregistry.Generation.DialectLabel(p.OperationDialect("generation"))
	if !ok {
		return false, false
	}
	return operationregistry.Generation.SupportsTarget(dialect.Identity, surface, mode), true
}

// WrapBodyDialect applies the reviewed hosting wrapper for a generation
// dialect identity. Cloud Anthropic hostings admit only the registered
// Messages dialect; all other dialects pass through.
func (c Config) WrapBodyDialect(body []byte, dialect oif.Identity) ([]byte, error) {
	hosting := c.Hosting()
	if hosting != "vertex-anthropic" && hosting != "bedrock-anthropic-invoke" {
		return body, nil
	}
	if dialect.ID != "anthropic-messages" {
		return nil, errors.New("Anthropic cloud profile requires the Messages dialect")
	}
	return c.wrapAnthropicCloud(body)
}
