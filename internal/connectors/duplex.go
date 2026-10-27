package connectors

import (
	"errors"
	"net/url"
	"strings"
)

func (c Config) DuplexURL(model string) (string, error) {
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	switch c.Hosting() {
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
