package connectors

import (
	"errors"
	"net/url"
	"strings"
)

const VertexLiveMethod = "google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"

func (c Config) DuplexURL(model string) (string, error) {
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	switch c.Hosting() {
	case "vertex-live":
		base, err := c.duplexBase()
		if err != nil {
			return "", err
		}
		ws := websocketScheme(base)
		if ws == "" {
			return "", errors.New("native duplex endpoint requires HTTP(S) hosting")
		}
		if strings.Contains(base, "/v1/projects/") {
			parsed, parseErr := url.Parse(ws)
			if parseErr != nil {
				return "", parseErr
			}
			ws = parsed.Scheme + "://" + parsed.Host
		}
		return ws + "/ws/" + VertexLiveMethod, nil
	case "xai-realtime":
		base, err := c.duplexBase()
		if err != nil {
			return "", err
		}
		query := url.Values{"model": {c.Model(model)}}
		ws := websocketScheme(base)
		if ws == "" {
			return "", errors.New("native duplex endpoint requires HTTP(S) hosting")
		}
		return ws + "/realtime?" + query.Encode(), nil
	}
	return "", errors.New("provider profile does not support native duplex sessions")
}

func (c Config) DuplexBoundModel(model string) (string, error) {
	if c.Hosting() != "vertex-live" {
		return c.Model(model), nil
	}
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	if !cloudIdentifier.MatchString(c.CloudRegion) || !cloudIdentifier.MatchString(c.CloudProject) {
		return "", errors.New("Vertex Live requires a configured project and location")
	}
	bound := c.Model(model)
	prefix := "projects/" + c.CloudProject + "/locations/" + c.CloudRegion + "/publishers/google/models/"
	if strings.HasPrefix(bound, "projects/") {
		if !strings.HasPrefix(bound, prefix) || len(bound) == len(prefix) {
			return "", errors.New("Vertex Live model must stay inside the bound project and location")
		}
		return bound, nil
	}
	if bound == "" || strings.Contains(bound, "/") {
		return "", errors.New("Vertex Live model must be a bare publisher model name")
	}
	return prefix + bound, nil
}

func (c Config) duplexBase() (string, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || c.validateProfileEndpoint(u) != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("native duplex endpoint must be an absolute URL without query or fragment")
	}
	return strings.TrimRight(c.Endpoint, "/"), nil
}

func websocketScheme(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return ""
	}
}
