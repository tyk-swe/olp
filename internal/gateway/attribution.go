package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) parseAttribution(r *http.Request, authority access.Authority) (map[string]string, *Error) {
	return parseAttributionValues(r.Header.Values(usage.AttributionHeader), authority)
}

func parseAttributionValues(values []string, authority access.Authority) (map[string]string, *Error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, invalidRequest("invalid_attribution", "Provide at most one "+usage.AttributionHeader+" header.", nil)
	}
	raw := values[0]
	if len(raw) > usage.AttributionHeaderBytes {
		return nil, invalidRequest("invalid_attribution", "The attribution header exceeds 4096 bytes.", nil)
	}
	var entries map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var trailing any
	if err := decoder.Decode(&entries); err != nil || entries == nil ||
		decoder.Decode(&trailing) != io.EOF {
		return nil, invalidRequest("invalid_attribution", "The attribution header must be a single JSON object.", nil)
	}
	if len(entries) > usage.AttributionMaxKeys {
		return nil, invalidRequest("invalid_attribution", "The attribution header allows at most 4 entries.", nil)
	}
	labels := make(map[string]string, len(entries))
	for key, rawValue := range entries {
		if !slices.Contains(authority.Policy.AllowedAttributionKeys, key) {
			return nil, invalidRequest("invalid_attribution", "The attribution key `"+key+"` is not allowed for this API key.", nil)
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, invalidRequest("invalid_attribution", "Attribution values must be strings.", nil)
		}
		labels[key] = value
	}
	if err := usage.ValidateAttribution(labels); err != nil {
		return nil, invalidRequest("invalid_attribution", "Attribution keys and values must be short machine tokens.", nil)
	}
	return labels, nil
}
