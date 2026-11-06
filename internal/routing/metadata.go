package routing

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

type decodedMetadata struct {
	value ModelMetadata
	valid bool
}

func decodeMetadata(raw json.RawMessage) decodedMetadata {
	if len(raw) == 0 {
		return decodedMetadata{}
	}
	var value ModelMetadata
	err := json.Unmarshal(raw, &value)
	return decodedMetadata{value: value, valid: err == nil}
}

func (p Provider) Metadata(model string) (ModelMetadata, bool) {
	if p.metadata != nil {
		metadata := p.metadata[model]
		return metadata.value.clone(), metadata.valid
	}
	metadata := decodeMetadata(p.Models[model])
	return metadata.value, metadata.valid
}

func compileProviderMetadata(providers map[string]Provider) map[string]Provider {
	if providers == nil {
		return nil
	}
	compiled := make(map[string]Provider, len(providers))
	for id, provider := range providers {
		metadata := make(map[string]decodedMetadata, len(provider.Models))
		for model, raw := range provider.Models {
			metadata[model] = decodeMetadata(raw)
		}
		provider.metadata = metadata
		compiled[id] = provider
	}
	return compiled
}

// ModelMetadata keeps absent facts distinct from affirmative declarations.
// Privacy declarations always name their evidence and observation time.
type ModelMetadata struct {
	CanonicalModel      *string    `json:"canonical_model"`
	InputModalities     []string   `json:"input_modalities"`
	OutputModalities    []string   `json:"output_modalities"`
	ContextLength       *int64     `json:"context_length"`
	MaxOutputTokens     *int64     `json:"max_output_tokens"`
	SupportedParameters *[]string  `json:"supported_parameters"`
	Quantization        *string    `json:"quantization"`
	Region              *string    `json:"region"`
	DataCollection      *bool      `json:"data_collection"`
	ZeroDataRetention   *bool      `json:"zero_data_retention"`
	Deployment          *string    `json:"deployment"`
	Source              *string    `json:"source"`
	ObservedAt          *time.Time `json:"observed_at"`
}

func (m ModelMetadata) Validate() error {
	if m.ContextLength != nil && *m.ContextLength < 1 || m.MaxOutputTokens != nil && *m.MaxOutputTokens < 1 {
		return errors.New("model token limits must be positive")
	}
	if m.SupportedParameters != nil && len(*m.SupportedParameters) > 128 {
		return errors.New("use at most 128 supported parameters")
	}
	if m.Deployment != nil && (*m.Deployment == "" || len(*m.Deployment) > 200 || strings.ContainsAny(*m.Deployment, "\r\n\x00")) {
		return errors.New("invalid model deployment")
	}
	if (m.DataCollection != nil || m.ZeroDataRetention != nil) && (m.Source == nil || strings.TrimSpace(*m.Source) == "" || m.ObservedAt == nil) {
		return errors.New("privacy declarations require a source and observation time")
	}
	return nil
}

func (m ModelMetadata) clone() ModelMetadata {
	parameters := func() *[]string {
		if m.SupportedParameters == nil {
			return nil
		}
		return new(slices.Clone(*m.SupportedParameters))
	}()
	return ModelMetadata{
		CanonicalModel: copyPointer(m.CanonicalModel), InputModalities: slices.Clone(m.InputModalities), OutputModalities: slices.Clone(m.OutputModalities),
		ContextLength: copyPointer(m.ContextLength), MaxOutputTokens: copyPointer(m.MaxOutputTokens), SupportedParameters: parameters,
		Quantization: copyPointer(m.Quantization), Region: copyPointer(m.Region), DataCollection: copyPointer(m.DataCollection),
		ZeroDataRetention: copyPointer(m.ZeroDataRetention), Deployment: copyPointer(m.Deployment), Source: copyPointer(m.Source), ObservedAt: copyPointer(m.ObservedAt),
	}
}
