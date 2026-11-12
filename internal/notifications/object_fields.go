package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

func objectFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("must be a JSON object")
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("%s is not supported", key)
		}
		if string(fields[key]) == "null" {
			return nil, fmt.Errorf("%s must not be null", key)
		}
	}
	return fields, nil
}
