package connectors

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Gemini Interactions and Live have distinct addresses from GenerateContent.
// Only a registered lifecycle profile can construct either address.
const GeminiLiveMethod = "google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

var geminiInteractionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,511}$`)

func (c Config) geminiLifecycleBase(hosting string) (string, error) {
	if err := c.ValidateProfile(); err != nil || c.Hosting() != hosting {
		return "", errors.New("provider profile does not support this Gemini lifecycle")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || c.validateProfileEndpoint(u) != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Gemini lifecycle endpoint must be an absolute /v1beta URL")
	}
	return strings.TrimRight(c.Endpoint, "/"), nil
}

// InteractionsURL addresses the create, retrieve, cancel and delete methods.
// The resource ID is a single provider-owned segment and never selects a host.
func (c Config) InteractionsURL(id, action string) (string, error) {
	base, err := c.geminiLifecycleBase("direct-gemini-interactions")
	if err != nil {
		return "", err
	}
	if id == "" {
		if action != "" {
			return "", errors.New("interaction action requires an ID")
		}
		return base + "/interactions", nil
	}
	if !geminiInteractionID.MatchString(id) {
		return "", errors.New("invalid Gemini interaction ID")
	}
	path := base + "/interactions/" + url.PathEscape(id)
	switch action {
	case "":
		return path, nil
	case "cancel":
		return path + "/cancel", nil
	default:
		return "", errors.New("unsupported Gemini interaction action")
	}
}

func (c Config) GeminiLiveURL() (string, error) {
	base, err := c.geminiLifecycleBase("direct-gemini-live")
	if err != nil {
		return "", err
	}
	base = strings.TrimSuffix(base, "/v1beta")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return "", errors.New("Gemini Live endpoint requires HTTP(S) hosting")
	}
	return base + "/ws/" + GeminiLiveMethod, nil
}
