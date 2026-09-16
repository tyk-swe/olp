package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/tyk-swe/olp/internal/runtime"
)

func validateMetadata(raw json.RawMessage) error {
	var m runtime.ModelMetadata
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("model metadata contains invalid or unknown fields")
	}
	return m.Validate()
}

// metadataJSON applies the public contract's defaults without inventing facts
// or rounding exact integer metadata through float64.
func metadataJSON(raw []byte) (map[string]json.RawMessage, error) {
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
