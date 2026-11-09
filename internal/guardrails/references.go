package guardrails

import (
	"encoding/json"
	"maps"
)

func RenameCredentials(raw json.RawMessage, rename func(string) (string, error)) (json.RawMessage, error) {
	fields, err := renamed(raw, "credential_ref", rename)
	if err != nil {
		return nil, err
	}
	network, ok := fields["network"]
	if !ok || string(network) == "null" {
		return json.Marshal(fields)
	}
	options, err := renamed(network, "credential_id", rename)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	return json.Marshal(with(fields, "network", encoded))
}

func renamed(raw json.RawMessage, key string, rename func(string) (string, error)) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	current, ok := fields[key]
	if !ok {
		return fields, nil
	}
	var value string
	if err := json.Unmarshal(current, &value); err != nil {
		return nil, err
	}
	if value == "" {
		return fields, nil
	}
	next, err := rename(value)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	return with(fields, key, encoded), nil
}

func with(fields map[string]json.RawMessage, key string, value json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(fields)
	out[key] = value
	return out
}
