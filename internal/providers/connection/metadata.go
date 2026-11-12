package connection

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"

	"github.com/tyk-swe/olp/internal/routing"
)

// MergeDiscoveredFacts places upstream-listed facts under the operator's
// metadata for model, reporting false when nothing is to be stored: facts that
// would leave the configuration invalid, or a new entry beyond the metadata
// bound, so discovery can never make the saved configuration fail its own
// validation.
func MergeDiscoveredFacts(models map[string]json.RawMessage, model string, discovered map[string]json.RawMessage) (json.RawMessage, bool, error) {
	stored, known := models[model]
	if len(discovered) == 0 || !known && len(models) >= maxModelEntries {
		return nil, false, nil
	}
	existing, err := ModelFacts(stored)
	if err != nil {
		return nil, false, err
	}
	facts := maps.Clone(discovered)
	maps.Copy(facts, existing)
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, false, err
	}
	if ValidateMetadata(encoded) != nil {
		return nil, false, nil
	}
	return encoded, true, nil
}

func ModelFacts(stored json.RawMessage) (map[string]json.RawMessage, error) {
	if len(stored) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	return metadataObject(stored)
}

func metadataObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var facts map[string]json.RawMessage
	if err := json.Unmarshal(raw, &facts); err != nil || facts == nil {
		return nil, errors.New("model metadata must be an object")
	}
	return facts, nil
}

func ValidateMetadata(raw json.RawMessage) error {
	var m routing.ModelMetadata
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("model metadata contains invalid or unknown fields")
	}
	return m.Validate()
}

// MetadataJSON applies the public contract's defaults without inventing facts
// or rounding exact integer metadata through float64.
func MetadataJSON(raw []byte) (map[string]json.RawMessage, error) {
	var facts map[string]json.RawMessage
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nil, err
	}
	if facts == nil {
		return nil, nil
	}
	for _, name := range []string{"canonical_model", "context_length", "max_output_tokens", "supported_parameters", "quantization", "region", "data_collection", "zero_data_retention", "deployment", "source", "observed_at"} {
		if _, ok := facts[name]; !ok {
			facts[name] = json.RawMessage("null")
		}
	}
	for _, name := range []string{"input_modalities", "output_modalities"} {
		if len(facts[name]) == 0 || bytes.Equal(facts[name], []byte("null")) {
			facts[name] = json.RawMessage("[]")
		}
	}
	return facts, nil
}
