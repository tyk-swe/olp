package connectors

import (
	"errors"
	"net/url"
	"strings"
)

const vertexLiveMethod = "google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"

func (c Config) DuplexURL(model string) (string, error) {
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	hosting := c.Hosting()
	if hosting != "vertex-live" && hosting != "xai-realtime" {
		return "", errors.New("provider profile does not support native duplex sessions")
	}
	base, ok := c.bareEndpoint()
	if !ok {
		return "", errors.New("native duplex endpoint must be an absolute URL without query or fragment")
	}
	socket, ok := websocketURL(base)
	if !ok {
		return "", errors.New("native duplex endpoint requires HTTP(S) hosting")
	}
	if hosting == "xai-realtime" {
		return socket + "/realtime?" + url.Values{"model": {c.Model(model)}}.Encode(), nil
	}
	if !strings.Contains(base, "/v1/projects/") {
		return socket + "/ws/" + vertexLiveMethod, nil
	}
	origin, err := url.Parse(socket)
	if err != nil {
		return "", err
	}
	return origin.Scheme + "://" + origin.Host + "/ws/" + vertexLiveMethod, nil
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
	prefix := vertexResource(c.CloudProject, c.CloudRegion) + hostings["vertex-google"].vertexPath + "/models/"
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

func (c Config) bareEndpoint() (string, bool) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || c.validateProfileEndpoint(u) != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(c.Endpoint, "/"), true
}
