package usage

import (
	"encoding/json"
	"regexp"
)

const (
	AttributionHeader      = "X-OLP-Attribution"
	AttributionHeaderBytes = 4096
	AttributionMaxKeys     = 4
	AttributionMaxBytes    = 1024
)

var (
	AttributionKeyPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)
	AttributionValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

type AttributionError struct{ Reason string }

func (e *AttributionError) Error() string { return e.Reason }

func ValidateAttribution(attribution map[string]string) error {
	if len(attribution) > AttributionMaxKeys {
		return &AttributionError{Reason: "too_many_keys"}
	}
	for key, value := range attribution {
		if !AttributionKeyPattern.MatchString(key) {
			return &AttributionError{Reason: "invalid_key"}
		}
		if !AttributionValuePattern.MatchString(value) {
			return &AttributionError{Reason: "invalid_value"}
		}
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		return &AttributionError{Reason: "invalid_encoding"}
	}
	if len(data) > AttributionMaxBytes {
		return &AttributionError{Reason: "too_large"}
	}
	return nil
}

func AttributionJSON(attribution map[string]string) []byte {
	if len(attribution) == 0 {
		return []byte("{}")
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		return []byte("{}")
	}
	return data
}
