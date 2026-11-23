package ingress

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

func ParseAttribution(r *http.Request, authority keyauthority.Authority) (map[string]string, *fault.Error) {
	return ParseAttributionValues(r.Header.Values(attribution.Header), authority)
}

func ParseAttributionValues(values []string, authority keyauthority.Authority) (map[string]string, *fault.Error) {
	labels, err := parseCallerAttribution(values, authority)
	if err != nil {
		return nil, err
	}
	resolved, problem := authority.ResolveAttribution(labels)
	if problem != nil {
		failure, known := errors.AsType[*attribution.Error](problem)
		if known && failure.Reason == "missing_attribution" {
			return nil, fault.InvalidRequest(failure.Reason, "Required attribution labels are missing.", nil)
		}
		if known && failure.Reason == "pinned_attribution_override" {
			return nil, fault.InvalidRequest(failure.Reason, "Operator-pinned attribution labels cannot be overridden.", nil)
		}
		return nil, fault.InvalidRequest("invalid_attribution", "Attribution does not satisfy the key and project policies.", nil)
	}
	return resolved, nil
}

func parseCallerAttribution(values []string, authority keyauthority.Authority) (map[string]string, *fault.Error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, fault.InvalidRequest("invalid_attribution", "Provide at most one "+attribution.Header+" header.", nil)
	}
	raw := values[0]
	if len(raw) > attribution.HeaderBytes {
		return nil, fault.InvalidRequest("invalid_attribution", "The attribution header exceeds 4096 bytes.", nil)
	}
	var entries map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var trailing any
	if err := decoder.Decode(&entries); err != nil || entries == nil ||
		decoder.Decode(&trailing) != io.EOF {
		return nil, fault.InvalidRequest("invalid_attribution", "The attribution header must be a single JSON object.", nil)
	}
	if len(entries) > attribution.MaxKeys {
		return nil, fault.InvalidRequest("invalid_attribution", "The attribution header allows at most 4 entries.", nil)
	}
	labels := make(map[string]string, len(entries))
	for key, rawValue := range entries {
		if !slices.Contains(authority.Policy.AllowedAttributionKeys, key) && authority.Policy.AttributionDefaults[key] == "" && authority.ProjectAttributionPolicy.Defaults[key] == "" {
			return nil, fault.InvalidRequest("invalid_attribution", "The attribution key `"+key+"` is not allowed for this API key.", nil)
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, fault.InvalidRequest("invalid_attribution", "Attribution values must be strings.", nil)
		}
		labels[key] = value
	}
	if err := attribution.Validate(labels); err != nil {
		return nil, fault.InvalidRequest("invalid_attribution", "Attribution keys and values must be short machine tokens.", nil)
	}
	return labels, nil
}
