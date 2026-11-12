package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("must be a JSON object")
	}
	return fields, nil
}

func onlyFields(fields map[string]json.RawMessage, allowed ...string) error {
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%s is not supported", key)
		}
		if string(fields[key]) == "null" {
			return fmt.Errorf("%s must not be null", key)
		}
	}
	return nil
}

func objectFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := onlyFields(fields, allowed...); err != nil {
		return nil, err
	}
	return fields, nil
}
