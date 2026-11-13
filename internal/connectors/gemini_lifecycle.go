package connectors

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const GeminiLiveMethod = "google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

var geminiInteractionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,511}$`)

func (c Config) geminiLifecycleBase(hosting string) (string, error) {
	if err := c.ValidateProfile(); err != nil || c.Hosting() != hosting {
		return "", errors.New("provider profile does not support this Gemini lifecycle")
	}
	base, ok := c.bareEndpoint()
	if !ok {
		return "", errors.New("Gemini lifecycle endpoint must be an absolute /v1beta URL")
	}
	return base, nil
}

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
	socket, ok := websocketURL(strings.TrimSuffix(base, "/v1beta"))
	if !ok {
		return "", errors.New("Gemini Live endpoint requires HTTP(S) hosting")
	}
	return socket + "/ws/" + GeminiLiveMethod, nil
}
