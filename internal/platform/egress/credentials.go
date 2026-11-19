package egress

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// ApplyCredentialHeaders decodes the secret header values and requires an exact,
// case-insensitive match with the configured names before changing the request.
func ApplyCredentialHeaders(header http.Header, names []string, credential []byte) error {
	var values map[string]*string
	if err := json.Unmarshal(credential, &values); err != nil {
		return errors.New("Credential must be a JSON object of header values.")
	}
	if len(values) == 0 || len(values) != len(names) {
		return errors.New("Credential headers must exactly match the configured names.")
	}
	normalized := make(map[string]string, len(values))
	for name, value := range values {
		if value == nil || strings.ContainsAny(*value, "\r\n\x00") {
			return errors.New("Credential header values must be strings.")
		}
		name = textproto.CanonicalMIMEHeaderKey(name)
		if _, duplicate := normalized[name]; duplicate {
			return errors.New("Credential header names must be unique.")
		}
		normalized[name] = *value
	}
	for name := range normalized {
		if !slices.ContainsFunc(names, func(configured string) bool {
			return textproto.CanonicalMIMEHeaderKey(configured) == name
		}) {
			return errors.New("Credential headers must exactly match the configured names.")
		}
	}
	for name, value := range normalized {
		header.Set(name, value)
	}
	return nil
}
